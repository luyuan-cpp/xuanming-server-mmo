package kafkautil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	kafkago "github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"path/filepath"
	pb "proto/common/database"
	"shared/drainproof"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/IBM/sarama"
)

// GroupLag 是一个消费组在一个 topic 上的积压(Lag / RetentionGap 是全部分区求和,MaxPartitionLag 是单分区最大值;
// 调用方不按分区出指标,基数恒定)。
type GroupLag struct {
	// Lag = Σ max(0, end − max(committed, start)):end = 下一条要写的 offset(OffsetNewest),start = 日志起点(OffsetOldest)。
	// 没提交过的分区按「从 start 开始」算(消费者都是 StartOffset=FirstOffset)。
	Lag int64
	// MaxPartitionLag 是上式里单个分区那一项的最大值(没提交过的分区同样计入)。
	// broker 的 retention.bytes 按**分区**执行:积压集中在少数分区时(持有它的实例卡住、毒丸行卡住所在分区),
	// 合计 Lag 还远没到线,那个分区已经开始被删 —— 「离删除线还有多远」只能拿它比,不能拿合计比。
	MaxPartitionLag int64
	// RetentionGap = Σ max(0, start − committed),只算提交过的分区:保留期(retention.ms 或 retention.bytes)
	// 已经删掉、而这个组还没提交到的消息条数 —— 通常意味着这些数据已经丢了。也含已被消费者读入、尚未提交的消息
	// 所在段被删的情形(消费者预取在前、处理完才提交):此时未必丢,消费者照常处理并提交越过 start 后它自行归零,
	// 提交之前停机 / 重建时其中没处理完的才真丢。丢没丢、丢了多少以 [kafka-go] 日志为准。
	RetentionGap int64
	// Uncommitted 是还没提交过 offset 的分区数(新组 / 新一代 topic 刚建)。
	Uncommitted int
}

// CommittedOffsetsReader 是读一个组已提交位点要的唯一调用;sarama.ClusterAdmin 满足它。
// 单独成接口是为了让 lag 探针(本文件)与 go/db 的消费组交接闸(go/db/internal/kafka/group_handoff.go)
// 共用同一份 fail-closed 的位点读取,不各写一份错误语义。
type CommittedOffsetsReader interface {
	ListConsumerGroupOffsets(group string, topicPartitions map[string][]int32) (*sarama.OffsetFetchResponse, error)
}

// groupOffsetReader 只暴露算 lag 要的两个调用,单测注入 fake,不连真实 Kafka(同 topicAdmin 的做法)。
type groupOffsetReader interface {
	CommittedOffsetsReader
	GetOffset(topic string, partition int32, at int64) (int64, error)
}

// CommittedOffsets 读 group 在 topic 的 partitions 上的已提交位点,返回 分区 → offset;-1 原样返回,表示该分区没提交过。
//
// fail-closed:调用本身出错、响应为 nil、响应顶层 Err 非 0(OffsetFetch v2+)、某分区 block 缺失或 block.Err 非 0,
// 都返回 error,不把读不到当成「没有位点」。sarama.KError 用 %w 包住,调用方可以
// errors.Is(err, sarama.ErrGroupIDNotFound) 区分「组不存在」(交接闸按全部 -1 处理)。
// 错误文本不带前缀,由调用方补(computeGroupLag 补 "kafka lag probe: ",与抽取之前逐字相同)。
func CommittedOffsets(r CommittedOffsetsReader, group, topic string, partitions []int32) (map[int32]int64, error) {
	resp, err := r.ListConsumerGroupOffsets(group, map[string][]int32{topic: partitions})
	if err != nil {
		return nil, fmt.Errorf("fetch offsets of group %s on %s: %w", group, topic, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("fetch offsets of group %s on %s: empty response", group, topic)
	}
	if resp.Err != sarama.ErrNoError {
		return nil, fmt.Errorf("fetch offsets of group %s on %s: %w", group, topic, resp.Err)
	}
	out := make(map[int32]int64, len(partitions))
	for _, p := range partitions {
		block := resp.GetBlock(topic, p)
		if block == nil {
			return nil, fmt.Errorf("group %s has no offset block for %s[%d]", group, topic, p)
		}
		if block.Err != sarama.ErrNoError {
			return nil, fmt.Errorf("group %s offset of %s[%d]: %w", group, topic, p, block.Err)
		}
		out[p] = block.Offset
	}
	return out, nil
}

// computeGroupLag 算一个组在一个 topic 上的 GroupLag。
//
// **读取顺序是正确性要求**:先读组的已提交 offset,再逐分区读 end / start。反过来(先 end 后 committed)时,
// 消费者在两次读之间提交一次就会出现 committed > end,繁忙 topic 上探针会周期性误报;先读 committed,
// end 单调不减,committed <= end 恒成立(除非 topic 真被删了重建)。代价:start 也是后读的,保留期恰在两次读之间推进、
// 而消费者其实已越过它时,会算出一个**瞬时**的 RetentionGap > 0 —— 对应告警 DataServiceAuditRetentionGap 自 2026-10-15
// 起不带 for(max_over_time[15m]、for: 0m),这种正值也会呼叫,按丢数据处置后以 `[kafka-go]` 日志确认条数(可能为 0),
// 见 deploy/k8s/data-service-audit-alerts.yaml 与 docs/ops/audit-topic-generation-bump-runbook.md §7。
//
// 任何一处读失败、响应顶层 Err(OffsetFetch v2+)或某分区 block 缺失 / 带错,都返回 error;
// committed > end(topic 被删了重建过)时该分区 lag 记 0,并返回 error 让调用方看见,不静默当 0。
func computeGroupLag(r groupOffsetReader, group, topic string, partitions int32) (GroupLag, error) {
	if partitions <= 0 {
		return GroupLag{}, fmt.Errorf("kafka lag probe: topic %s has invalid partition count %d", topic, partitions)
	}
	ids := make([]int32, partitions)
	for i := range ids {
		ids[i] = int32(i)
	}

	// 先读 committed(见上面的读取顺序说明);错误语义与文本由 CommittedOffsets 统一给出,这里只补前缀。
	committedByPartition, err := CommittedOffsets(r, group, topic, ids)
	if err != nil {
		return GroupLag{}, fmt.Errorf("kafka lag probe: %w", err)
	}
	committed := make([]int64, partitions)
	for i, p := range ids {
		committed[i] = committedByPartition[p]
	}

	var (
		out      GroupLag
		mismatch error
	)
	for i, p := range ids {
		end, err := r.GetOffset(topic, p, sarama.OffsetNewest)
		if err != nil {
			return GroupLag{}, fmt.Errorf("kafka lag probe: end offset of %s[%d]: %w", topic, p, err)
		}
		start, err := r.GetOffset(topic, p, sarama.OffsetOldest)
		if err != nil {
			return GroupLag{}, fmt.Errorf("kafka lag probe: start offset of %s[%d]: %w", topic, p, err)
		}
		c := committed[i]
		if c < 0 {
			// 没提交过(Kafka 回 -1):消费者从日志起点开始读。
			out.Uncommitted++
			if end > start {
				out.Lag += end - start
				out.MaxPartitionLag = max(out.MaxPartitionLag, end-start)
			}
			continue
		}
		if c > end {
			// 先读 committed 后读 end,正常情况下不可能:topic 被删了重建过(或换了集群)。lag 记 0,报出来。
			mismatch = errors.Join(mismatch, fmt.Errorf("kafka lag probe: group %s committed offset %d of %s[%d] is beyond the log end %d (topic recreated?)",
				group, c, topic, p, end))
			continue
		}
		if c < start {
			out.RetentionGap += start - c
		}
		if from := max(c, start); end > from {
			out.Lag += end - from
			out.MaxPartitionLag = max(out.MaxPartitionLag, end-from)
		}
	}
	return out, mismatch
}

// groupLagProbeNetTimeout:探针的拨号 / 读 / 写超时。sarama 调用不收 ctx,默认 30s 的超时会让一轮探测在 broker
// 半死时挂半分钟以上、并拖慢关停;探针只是观测,宁可快速失败、下一轮再来。
const groupLagProbeNetTimeout = 5 * time.Second

// GroupLagProber 持有一对长连接的 sarama Client + ClusterAdmin(admin 由 client 派生,admin.Close 会一并关 client),
// 自己实现 groupOffsetReader:GetOffset 转 client、ListConsumerGroupOffsets 转 admin。
// 连接坏了不就地自愈:调用方(data_service 的 lag 探针)探失败就 Close、下一轮重建。
type GroupLagProber struct {
	client sarama.Client
	admin  sarama.ClusterAdmin
}

// newGroupLagProberConfig 是探针连 broker 用的 sarama 配置;单独成函数是为了让单测钉住各项取值(不连 Kafka)。
//
// Metadata.AllowAutoTopicCreation = false(负责人 2026-10-15 按「最标准」定:只读的监控探针不得对被监控系统产生副作用)。
// 下列 sarama 行为按本机模块缓存里的 v1.43.1 源码核对:
//   - 默认值是 true(config.go:524)。探针里唯一按 topic 拉元数据的是 GetOffset:元数据缓存里没有该 topic / 分区时先按 topic
//     刷新(client.go:496-510),出错后再按 topic 刷新一次重试(:563-577);按 topic 的 Metadata 请求带上这个标志(:1022-1031,
//     拉全量元数据时 sarama 自己强制为 false,:1025-1026;V3_0_0_0 下是 Metadata v10,标志在 v>3 时编码,metadata_request.go:95-96)。
//     默认 true 时,broker 开着 auto.create.topics.enable、num.partitions=1(本仓 compose / K8s 均如此),探针打到尚未预建或已被删的
//     topic 就可能把它建成 1 分区,data_service 的分区契约随即永久失败。
//   - ListConsumerGroupOffsets 只向 coordinator 发 OffsetFetch,不拉 topic 元数据(admin.go:1009-1018),与这个标志无关。
//   - 关掉之后 topic 不存在时:元数据响应里该 topic 的错误若是 UNKNOWN_TOPIC_OR_PARTITION,sarama 记为 ErrUnknownTopicOrPartition
//     并按 Metadata.Retry.Max 重试(client.go:1130-1133、:1048-1051),仍失败即由 GetOffset 返回 (-1, ErrUnknownTopicOrPartition)。
//     computeGroupLag 把它包成 "kafka lag probe: end offset of <topic>[0]: ..." 返回 error(读 committed 那一步对不存在的 topic
//     回 -1 还是分区错,属 broker 语义、未核;两种情形最终都是 error),调用方记 lag_probe_total{result="error"}、关连接下一轮重建,
//     持续 15 分钟即 DataServiceAuditLagProbeBlind —— 不会当成 0 lag。broker 收到 false 时对不存在的 topic 回哪个错误码属 Kafka
//     语义,本注未实测。
func newGroupLagProberConfig() *sarama.Config {
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_0_0_0 // 与 EnsureTopics 一致
	cfg.Net.DialTimeout = groupLagProbeNetTimeout
	cfg.Net.ReadTimeout = groupLagProbeNetTimeout
	cfg.Net.WriteTimeout = groupLagProbeNetTimeout
	cfg.Metadata.Retry.Max = 1
	cfg.Metadata.AllowAutoTopicCreation = false
	return cfg
}

// NewGroupLagProber 连 broker(会拨号并拉一次元数据,失败即返回 error)。配置见 newGroupLagProberConfig。
func NewGroupLagProber(brokers []string) (*GroupLagProber, error) {
	cfg := newGroupLagProberConfig()

	client, err := sarama.NewClient(brokers, cfg)
	if err != nil {
		return nil, fmt.Errorf("kafka lag probe connect: %w", err)
	}
	admin, err := sarama.NewClusterAdminFromClient(client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("kafka lag probe admin: %w", err)
	}
	return &GroupLagProber{client: client, admin: admin}, nil
}

// GetOffset 实现 groupOffsetReader(转 sarama Client)。
func (p *GroupLagProber) GetOffset(topic string, partition int32, at int64) (int64, error) {
	return p.client.GetOffset(topic, partition, at)
}

// ListConsumerGroupOffsets 实现 groupOffsetReader(转 sarama ClusterAdmin)。
func (p *GroupLagProber) ListConsumerGroupOffsets(group string, topicPartitions map[string][]int32) (*sarama.OffsetFetchResponse, error) {
	return p.admin.ListConsumerGroupOffsets(group, topicPartitions)
}

// Probe 算一个组在一个 topic 上的积压,见 computeGroupLag。
func (p *GroupLagProber) Probe(group, topic string, partitions int32) (GroupLag, error) {
	return computeGroupLag(p, group, topic, partitions)
}

// Close 关 admin(连同它底下的 client)。
func (p *GroupLagProber) Close() error {
	return p.admin.Close()
}

// DrainKafkaCatalog是实际管理API的目录/有效配置原件，分区事实仍只用正式proto。
// 这份原件必须被SourcePin保留，portable核验器核首尾完全相同，不能只pin摘要而丢正文。
type DrainKafkaCatalog struct {
	Schema          string                       `json:"schema"`
	ObservedStartMS uint64                       `json:"observed_start_ms"`
	ObservedEndMS   uint64                       `json:"observed_end_ms"`
	ClusterID       []byte                       `json:"cluster_id"`
	TopicPartitions map[string]int               `json:"topic_partitions"`
	TopicIDs        map[string][]byte            `json:"topic_ids"`
	TopicNames      []string                     `json:"topic_names"`
	GroupNames      []string                     `json:"group_names"`
	BrokerConfigs   map[string]map[string]string `json:"broker_configs"`
	TopicConfigs    map[string]map[string]string `json:"topic_configs"`
}

func (p *GroupLagProber) CaptureDrainCatalog(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	observedStart := uint64(time.Now().UnixMilli())
	controller, err := p.client.Controller()
	if err != nil {
		return nil, err
	}
	request := sarama.NewMetadataRequest(sarama.V3_0_0_0, nil)
	request.Version = 10
	request.AllowAutoTopicCreation = false
	metadata, err := controller.GetMetadata(request)
	if err != nil || metadata == nil || metadata.ClusterID == nil || *metadata.ClusterID == "" {
		return nil, errors.New("Kafka cluster身份未知")
	}
	out := DrainKafkaCatalog{Schema: "SQL_DRAIN_KAFKA_CATALOG_V2", ObservedStartMS: observedStart, ClusterID: []byte(*metadata.ClusterID), TopicIDs: map[string][]byte{}, TopicPartitions: map[string]int{}, BrokerConfigs: map[string]map[string]string{}, TopicConfigs: map[string]map[string]string{}}
	readConfig := func(resource sarama.ConfigResource, required []string) (map[string]string, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := p.admin.DescribeConfig(resource)
		if err != nil {
			return nil, err
		}
		result := map[string]string{}
		for _, entry := range entries {
			for _, name := range required {
				if entry.Name == name {
					if entry.Sensitive || entry.Value == "" {
						return nil, errors.New("Kafka保留策略不可读")
					}
					if _, exists := result[name]; exists {
						return nil, errors.New("Kafka配置重复")
					}
					result[name] = entry.Value
				}
			}
		}
		if len(result) != len(required) {
			return nil, errors.New("Kafka有效保留配置缺项")
		}
		return result, nil
	}
	topicRequired := []string{"retention.ms", "retention.bytes", "cleanup.policy", "segment.ms", "segment.bytes", "delete.retention.ms", "min.insync.replicas", "unclean.leader.election.enable"}
	for _, topic := range metadata.Topics {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if topic == nil || topic.Err != sarama.ErrNoError || topic.Name == "" {
			return nil, errors.New("Kafka topic目录未知")
		}
		if _, exists := out.TopicIDs[topic.Name]; exists {
			return nil, errors.New("Kafka topic目录重复")
		}
		if bytes.Equal(topic.Uuid[:], make([]byte, 16)) {
			return nil, errors.New("Kafka topic物理UUID不可读")
		}
		out.TopicNames = append(out.TopicNames, topic.Name)
		out.TopicIDs[topic.Name] = bytes.Clone(topic.Uuid[:])
		out.TopicPartitions[topic.Name] = len(topic.Partitions)
		seenPartitions := map[int32]bool{}
		for _, partition := range topic.Partitions {
			if partition == nil || partition.Err != sarama.ErrNoError || partition.ID < 0 || int(partition.ID) >= len(topic.Partitions) || seenPartitions[partition.ID] {
				return nil, errors.New("Kafka实际partition目录不完整")
			}
			seenPartitions[partition.ID] = true
		}
		config, err := readConfig(sarama.ConfigResource{Type: sarama.TopicResource, Name: topic.Name}, topicRequired)
		if err != nil {
			return nil, err
		}
		out.TopicConfigs[topic.Name] = config
	}
	for _, broker := range metadata.Brokers {
		if broker == nil {
			return nil, errors.New("Kafka broker目录未知")
		}
		name := strconv.FormatInt(int64(broker.ID()), 10)
		if _, exists := out.BrokerConfigs[name]; exists {
			return nil, errors.New("Kafka broker目录重复")
		}
		config, err := readConfig(sarama.ConfigResource{Type: sarama.BrokerResource, Name: name}, []string{"log.retention.ms", "log.retention.bytes", "log.segment.bytes", "log.retention.check.interval.ms", "unclean.leader.election.enable"})
		if err != nil {
			return nil, err
		}
		config["address"] = broker.Addr()
		out.BrokerConfigs[name] = config
	}
	groups, err := p.admin.ListConsumerGroups()
	if err != nil {
		return nil, err
	}
	for group := range groups {
		out.GroupNames = append(out.GroupNames, group)
	}
	sort.Strings(out.TopicNames)
	sort.Strings(out.GroupNames)
	if len(out.TopicNames) == 0 || len(out.TopicNames) > 129 || len(out.GroupNames) > 1024 || len(out.BrokerConfigs) == 0 || len(out.BrokerConfigs) > 128 {
		return nil, errors.New("Kafka目录超过预算")
	}
	out.ObservedEndMS = uint64(time.Now().UnixMilli())
	if out.ObservedEndMS < out.ObservedStartMS || out.ObservedEndMS-out.ObservedStartMS > 60000 {
		return nil, errors.New("Kafka目录采集超过60秒")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// DrainPartitions只采集固定历史全集，先committed再end/earliest；末尾重读固定end和committed，不接受retention越过P0保护floor。
// 调用前必须已有全部queue mutator QUIESCED；返回目录首尾原bytes由调用者写SourcePin。
func (p *GroupLagProber) DrainPartitions(ctx context.Context, expected []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure) ([]*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure, []byte, []byte, error) {
	budget, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if len(expected) == 0 || len(expected) > 16384 {
		return nil, nil, nil, errors.New("Kafka历史分区全集缺失或超限")
	}
	first, err := p.CaptureDrainCatalog(budget)
	if err != nil {
		return nil, nil, nil, err
	}
	var catalog DrainKafkaCatalog
	if json.Unmarshal(first, &catalog) != nil {
		return nil, nil, nil, errors.New("Kafka目录编码错误")
	}
	rows := make([]*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure, 0, len(expected))
	seen := map[string]bool{}
	for _, base := range expected {
		if err = budget.Err(); err != nil {
			return nil, nil, nil, err
		}
		if base == nil || base.Group == "" || base.TopicName == "" || base.InitialEarliest < 0 || base.ProtectedCommittedFloor < base.InitialEarliest || !bytes.Equal(base.ClusterId, catalog.ClusterID) || !bytes.Equal(base.TopicId, catalog.TopicIDs[base.TopicName]) {
			return nil, nil, nil, errors.New("Kafka历史物理身份/floor不符")
		}
		key := fmt.Sprintf("%s/%s/%d", base.Group, base.TopicName, base.Partition)
		if seen[key] {
			return nil, nil, nil, errors.New("Kafka历史订阅重复")
		}
		seen[key] = true
		committed, err := CommittedOffsets(p, base.Group, base.TopicName, []int32{int32(base.Partition)})
		if err != nil {
			return nil, nil, nil, err
		}
		end, err := p.GetOffset(base.TopicName, int32(base.Partition), sarama.OffsetNewest)
		if err != nil {
			return nil, nil, nil, err
		}
		earliest, err := p.GetOffset(base.TopicName, int32(base.Partition), sarama.OffsetOldest)
		if err != nil {
			return nil, nil, nil, err
		}
		current := committed[int32(base.Partition)]
		if current == -1 && base.InitialEarliest == 0 && base.ProtectedCommittedFloor == 0 && earliest == 0 && end == 0 {
			current = 0
		}
		if earliest < base.InitialEarliest || earliest > base.ProtectedCommittedFloor || current != end || end < base.ProtectedCommittedFloor {
			return nil, nil, nil, errors.New("Kafka未排空或保护范围被retention越过")
		}
		row := proto.Clone(base).(*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure)
		row.FinalEarliest = earliest
		row.FinalExclusiveEnd = end
		row.FinalCommitted = end
		rows = append(rows, row)
	}
	for _, row := range rows {
		if err = budget.Err(); err != nil {
			return nil, nil, nil, err
		}
		end, e := p.GetOffset(row.TopicName, int32(row.Partition), sarama.OffsetNewest)
		if e != nil || end != row.FinalExclusiveEnd {
			return nil, nil, nil, errors.New("Kafka已关闭relay后end仍漂移")
		}
		offsets, e := CommittedOffsets(p, row.Group, row.TopicName, []int32{int32(row.Partition)})
		earliest, eStart := p.GetOffset(row.TopicName, int32(row.Partition), sarama.OffsetOldest)
		if eStart != nil || earliest < row.FinalEarliest || earliest > row.ProtectedCommittedFloor {
			return nil, nil, nil, errors.New("Kafka最终earliest越过保护floor")
		}
		row.FinalEarliest = earliest
		current := offsets[int32(row.Partition)]
		if current == -1 && row.InitialEarliest == 0 && row.ProtectedCommittedFloor == 0 && row.FinalEarliest == 0 && end == 0 {
			current = 0
		}
		if e != nil || current != end {
			return nil, nil, nil, errors.New("Kafka最终提交水位漂移")
		}
	}
	last, err := p.CaptureDrainCatalog(budget)
	if err != nil {
		return nil, nil, nil, err
	}
	a, aErr := StableDrainCatalogDigest(first)
	b, bErr := StableDrainCatalogDigest(last)
	if aErr != nil || bErr != nil || !bytes.Equal(a, b) {
		return nil, nil, nil, errors.New("Kafka历史目录/broker/有效retention配置漂移")
	}
	if err = budget.Err(); err != nil {
		return nil, nil, nil, err
	}
	return rows, first, last, nil
}

// DrainKafkaBaseline保存P0实际C0/L0原件，typed分区行不由手填上界替代。
type DrainKafkaBaseline struct {
	Schema        string                                                  `json:"schema"`
	CatalogSHA256 []byte                                                  `json:"catalog_sha256"`
	ObservedMS    uint64                                                  `json:"observed_ms"`
	Partitions    []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure `json:"partitions"`
}

func (p *GroupLagProber) CaptureDrainBaseline(ctx context.Context, expected []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure) ([]*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure, []byte, []byte, error) {
	began := time.Now()
	if len(expected) == 0 || len(expected) > 16384 {
		return nil, nil, nil, errors.New("P0历史订阅预算非法")
	}
	catalogRaw, err := p.CaptureDrainCatalog(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var catalog DrainKafkaCatalog
	if json.Unmarshal(catalogRaw, &catalog) != nil {
		return nil, nil, nil, errors.New("P0目录无法解码")
	}
	result := &DrainKafkaBaseline{Schema: "SQL_DRAIN_KAFKA_BASELINE_V2", ObservedMS: uint64(time.Now().UnixMilli())}
	sum := sha256.Sum256(catalogRaw)
	result.CatalogSHA256 = sum[:]
	seen := map[string]bool{}
	for _, seed := range expected {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		if seed == nil || seed.Group == "" || seed.TopicName == "" || int(seed.Partition) >= catalog.TopicPartitions[seed.TopicName] || !bytes.Equal(seed.ClusterId, catalog.ClusterID) || !bytes.Equal(seed.TopicId, catalog.TopicIDs[seed.TopicName]) {
			return nil, nil, nil, errors.New("P0订阅物理身份不符")
		}
		key := fmt.Sprintf("%s/%s/%d", seed.Group, seed.TopicName, seed.Partition)
		if seen[key] {
			return nil, nil, nil, errors.New("P0订阅重复")
		}
		seen[key] = true
		committed, err := CommittedOffsets(p, seed.Group, seed.TopicName, []int32{int32(seed.Partition)})
		if err != nil {
			return nil, nil, nil, err
		}
		end, err := p.GetOffset(seed.TopicName, int32(seed.Partition), sarama.OffsetNewest)
		if err != nil {
			return nil, nil, nil, err
		}
		start, err := p.GetOffset(seed.TopicName, int32(seed.Partition), sarama.OffsetOldest)
		if err != nil {
			return nil, nil, nil, err
		}
		floor := committed[int32(seed.Partition)]
		if floor == -1 && start == 0 && end == 0 {
			floor = 0
		} // 确知从未含记录的空分区；不是以未提交推断历史已处理。
		if start < 0 || floor < start || floor > end {
			return nil, nil, nil, errors.New("P0保护范围未知或已被retention越过")
		}
		row := proto.Clone(seed).(*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure)
		row.InitialEarliest = start
		row.ProtectedCommittedFloor = floor
		row.FinalEarliest = start
		row.FinalExclusiveEnd = end
		row.FinalCommitted = committed[int32(seed.Partition)]
		result.Partitions = append(result.Partitions, row)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if time.Since(began) > 60*time.Second {
		return nil, nil, nil, errors.New("P0水位采集超过60秒")
	}
	result.ObservedMS = uint64(time.Now().UnixMilli())
	raw, err := json.Marshal(result)
	return result.Partitions, raw, catalogRaw, err
}

func StableDrainCatalogDigest(raw []byte) ([]byte, error) {
	catalog := &DrainKafkaCatalog{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(catalog) != nil || catalog.Schema != "SQL_DRAIN_KAFKA_CATALOG_V2" || catalog.ObservedStartMS == 0 || catalog.ObservedEndMS < catalog.ObservedStartMS || catalog.ObservedEndMS-catalog.ObservedStartMS > 60000 {
		return nil, errors.New("Kafka实际目录窗口缺失")
	}
	catalog.ObservedStartMS = 0
	catalog.ObservedEndMS = 0
	stable, err := json.Marshal(catalog)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(stable)
	return sum[:], nil
}

// DrainManualKafkaLeader 保留 P4 leader 采样原件；消费者条目只引用 P3 原件，避免自身摘要循环。
// Consumers 是正式 ManualConsumerEvidence 的 protojson，不是第二套分区结构。
type DrainManualKafkaLeader struct {
	Schema             string            `json:"schema"`
	ObservedStartMS    uint64            `json:"observed_start_ms"`
	ObservedEndMS      uint64            `json:"observed_end_ms"`
	CatalogFirstSHA256 []byte            `json:"catalog_first_sha256"`
	CatalogLastSHA256  []byte            `json:"catalog_last_sha256"`
	Consumers          []json.RawMessage `json:"consumers"`
}
type DrainManualKafkaBaseline struct {
	CatalogSource   *pb.SqlWriterMaintenanceReceipt_SourcePin `json:"catalog_source,omitempty"`
	Schema          string                                    `json:"schema"`
	CatalogSHA256   []byte                                    `json:"catalog_sha256"`
	ObservedStartMS uint64                                    `json:"observed_start_ms"`
	ObservedEndMS   uint64                                    `json:"observed_end_ms"`
	Assignments     []json.RawMessage                         `json:"assignments"`
}

func manualAssignmentKey(row *pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) string {
	return fmt.Sprintf("%x/%x/%s/%s/%d", row.IssuerId, row.InstanceId, row.ConsumerId, row.TopicName, row.Partition)
}
func validManualAssignment(row *pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) bool {
	return row != nil && len(row.IssuerId) > 0 && len(row.InstanceId) == 16 && row.ConsumerId != "" && len(row.ClusterId) > 0 && len(row.TopicId) == 16 && row.TopicName != "" && row.PartitionCount > 0 && row.Partition < row.PartitionCount && row.TargetNodeId > 0 && len(row.TargetInstanceId) == 16 && bytes.Equal(row.InstanceId, row.TargetInstanceId) && row.AssignmentRevision > 0 && row.StartOffset >= 0 && row.Position >= row.StartOffset && row.Earliest >= 0 && row.Position >= row.Earliest && row.ExclusiveEnd >= row.Position && row.ObservedMs > 0 && len(row.ProtoReflect().GetUnknown()) == 0
}
func sameManualAssignment(base, row *pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) bool {
	if !validManualAssignment(base) || !validManualAssignment(row) {
		return false
	}
	a := proto.Clone(base).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
	b := proto.Clone(row).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
	a.Position = 0
	a.Earliest = 0
	a.ExclusiveEnd = 0
	a.ObservedMs = 0
	b.Position = 0
	b.Earliest = 0
	b.ExclusiveEnd = 0
	b.ObservedMs = 0
	return proto.Equal(a, b) && row.Position >= base.Position && row.Earliest >= base.Earliest && row.Earliest <= base.Position
}
func manualCatalogMatches(row *pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment, catalog *DrainKafkaCatalog) bool {
	return validManualAssignment(row) && bytes.Equal(row.ClusterId, catalog.ClusterID) && bytes.Equal(row.TopicId, catalog.TopicIDs[row.TopicName]) && int(row.PartitionCount) == catalog.TopicPartitions[row.TopicName]
}

// CaptureManualBaseline 仅采样 leader；Position/StartOffset/assignment 必须来自本机 P0 原件。
// 调用者仍须把该本机原件精确 pin 到 Plan；本方法从不把 leader end 当作已消费位置。
func (p *GroupLagProber) CaptureManualBaseline(ctx context.Context, expected []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) ([]*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment, []byte, []byte, error) {
	budget, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if len(expected) == 0 || len(expected) > 16384 {
		return nil, nil, nil, errors.New("manual P0 assignment 全集缺失或超限")
	}
	began := uint64(time.Now().UnixMilli())
	catalogRaw, err := p.CaptureDrainCatalog(budget)
	if err != nil {
		return nil, nil, nil, err
	}
	var catalog DrainKafkaCatalog
	if json.Unmarshal(catalogRaw, &catalog) != nil {
		return nil, nil, nil, errors.New("manual P0 目录不能解码")
	}
	sum := sha256.Sum256(catalogRaw)
	report := DrainManualKafkaBaseline{Schema: "SQL_DRAIN_MANUAL_KAFKA_BASELINE_V1", CatalogSHA256: sum[:], ObservedStartMS: began}
	rows := make([]*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment, 0, len(expected))
	seen := map[string]bool{}
	for _, base := range expected {
		if err = budget.Err(); err != nil {
			return nil, nil, nil, err
		}
		if !manualCatalogMatches(base, &catalog) {
			return nil, nil, nil, errors.New("manual P0 实际物理目录不符")
		}
		key := manualAssignmentKey(base)
		if seen[key] {
			return nil, nil, nil, errors.New("manual P0 assignment 重复")
		}
		seen[key] = true
		end, e := p.GetOffset(base.TopicName, int32(base.Partition), sarama.OffsetNewest)
		if e != nil {
			return nil, nil, nil, e
		}
		earliest, e := p.GetOffset(base.TopicName, int32(base.Partition), sarama.OffsetOldest)
		if e != nil {
			return nil, nil, nil, e
		}
		if earliest < base.Earliest || earliest > base.Position || end < base.ExclusiveEnd || end < base.Position {
			return nil, nil, nil, errors.New("manual P0 leader 保护范围已丢失或回退")
		}
		row := proto.Clone(base).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
		row.Earliest = earliest
		row.ExclusiveEnd = end
		row.ObservedMs = uint64(time.Now().UnixMilli())
		rows = append(rows, row)
		raw, e := protojson.MarshalOptions{UseProtoNames: true}.Marshal(row)
		if e != nil {
			return nil, nil, nil, e
		}
		report.Assignments = append(report.Assignments, raw)
	}
	last, err := p.CaptureDrainCatalog(budget)
	if err != nil {
		return nil, nil, nil, err
	}
	firstDigest, e1 := StableDrainCatalogDigest(catalogRaw)
	lastDigest, e2 := StableDrainCatalogDigest(last)
	if e1 != nil || e2 != nil || !bytes.Equal(firstDigest, lastDigest) {
		return nil, nil, nil, errors.New("manual P0 物理目录或有效配置漂移")
	}
	if err = budget.Err(); err != nil {
		return nil, nil, nil, err
	}
	report.ObservedEndMS = uint64(time.Now().UnixMilli())
	raw, err := json.Marshal(report)
	return rows, raw, catalogRaw, err
}

func readManualClosures(expected []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment, pins []*pb.SqlWriterMaintenanceReceipt_SourcePin) ([]*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, error) {
	if len(expected) == 0 || len(expected) > 16384 || len(pins) == 0 || len(pins) > 1024 {
		return nil, errors.New("manual P3 全集或原件数量非法")
	}
	bases := map[string]*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
	for _, base := range expected {
		if !validManualAssignment(base) {
			return nil, errors.New("manual P0 assignment 非法")
		}
		key := manualAssignmentKey(base)
		if bases[key] != nil {
			return nil, errors.New("manual P0 assignment 重复")
		}
		bases[key] = base
	}
	seen := map[string]bool{}
	seenPins := map[string]bool{}
	seenOwners := map[string]bool{}
	var operation, manifest []byte
	result := make([]*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, 0, len(pins))
	for _, pin := range pins {
		if pin == nil || filepath.Base(pin.AbsolutePath) != "manual-closure.report.json" || seenPins[pin.AbsolutePath] {
			return nil, errors.New("manual P3 原件必须是唯一固定文件")
		}
		seenPins[pin.AbsolutePath] = true
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return nil, err
		}
		receipt := &pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{}
		if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '{' {
			err = protojson.Unmarshal(raw, receipt)
		} else {
			err = proto.Unmarshal(raw, receipt)
		}
		if err != nil || len(receipt.ProtoReflect().GetUnknown()) != 0 || len(receipt.OperationId) == 0 || len(receipt.ManifestDigest) != 32 || !receipt.AcceptingClosed || !receipt.CallbacksJoined || receipt.Accepted != receipt.Settled || receipt.UnresolvedPublications != 0 || len(receipt.Assignments) == 0 {
			return nil, errors.New("manual P3 原件没有真实关闭结局")
		}
		if operation == nil {
			operation = bytes.Clone(receipt.OperationId)
			manifest = bytes.Clone(receipt.ManifestDigest)
		} else if !bytes.Equal(operation, receipt.OperationId) || !bytes.Equal(manifest, receipt.ManifestDigest) {
			return nil, errors.New("manual P3 原件 run/manifest 不一致")
		}
		owner := ""
		for _, row := range receipt.Assignments {
			if !validManualAssignment(row) {
				return nil, errors.New("manual P3 assignment 非法")
			}
			currentOwner := fmt.Sprintf("%x/%x", row.IssuerId, row.InstanceId)
			if owner == "" {
				owner = currentOwner
			} else if owner != currentOwner {
				return nil, errors.New("manual P3 文件混入多个 boot")
			}
			key := manualAssignmentKey(row)
			base := bases[key]
			if seen[key] || !sameManualAssignment(base, row) || row.Position != row.ExclusiveEnd {
				return nil, errors.New("manual P3 实际配置/位置与 P0 不符")
			}
			seen[key] = true
		}
		if seenOwners[owner] {
			return nil, errors.New("manual P3 同 boot 原件重复")
		}
		seenOwners[owner] = true
		receipt.SourcePins = []*pb.SqlWriterMaintenanceReceipt_SourcePin{proto.Clone(pin).(*pb.SqlWriterMaintenanceReceipt_SourcePin)}
		result = append(result, receipt)
	}
	if len(seen) != len(bases) {
		return nil, errors.New("manual P3 没覆盖全部 P0 assignment")
	}
	return result, nil
}

// DrainManualConsumers 强读 P4 leader，完全不查询或伪造 consumer group committed。
// 返回 typed 条目仅 pin P3；CLI 保存 leaderRaw 后给每条 SourcePins 追加唯一 leader pin，
// 并把同一 pin 加入 NonSqlClosureEvidence.SourceEvidence。IssuerClosure 的 P0 pins 不变。
func (p *GroupLagProber) DrainManualConsumers(ctx context.Context, expected []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment, p3Pins []*pb.SqlWriterMaintenanceReceipt_SourcePin) ([]*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, []byte, []byte, []byte, error) {
	budget, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	receipts, err := readManualClosures(expected, p3Pins)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	report := DrainManualKafkaLeader{Schema: "SQL_DRAIN_MANUAL_KAFKA_LEADER_V1", ObservedStartMS: uint64(time.Now().UnixMilli())}
	first, err := p.CaptureDrainCatalog(budget)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var catalog DrainKafkaCatalog
	if json.Unmarshal(first, &catalog) != nil {
		return nil, nil, nil, nil, errors.New("manual P4 目录无法解码")
	}
	bases := map[string]*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
	for _, base := range expected {
		bases[manualAssignmentKey(base)] = base
	}
	// 两轮 leader 采样确认关闭后 end 没有漂移；保留范围始终不能越过 P0 position。
	for pass := 0; pass < 2; pass++ {
		for _, receipt := range receipts {
			for _, row := range receipt.Assignments {
				if err = budget.Err(); err != nil {
					return nil, nil, nil, nil, err
				}
				if !manualCatalogMatches(row, &catalog) {
					return nil, nil, nil, nil, errors.New("manual P4 物理目录不符")
				}
				end, e := p.GetOffset(row.TopicName, int32(row.Partition), sarama.OffsetNewest)
				if e != nil {
					return nil, nil, nil, nil, e
				}
				earliest, e := p.GetOffset(row.TopicName, int32(row.Partition), sarama.OffsetOldest)
				if e != nil {
					return nil, nil, nil, nil, e
				}
				base := bases[manualAssignmentKey(row)]
				if end != row.Position || earliest < row.Earliest || earliest > base.Position {
					return nil, nil, nil, nil, errors.New("manual P4 未排空、end 漂移或 retention 越过 P0 位置")
				}
				row.Earliest = earliest
				row.ExclusiveEnd = end
				row.ObservedMs = uint64(time.Now().UnixMilli())
			}
		}
	}
	last, err := p.CaptureDrainCatalog(budget)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	a, e1 := StableDrainCatalogDigest(first)
	b, e2 := StableDrainCatalogDigest(last)
	if e1 != nil || e2 != nil || !bytes.Equal(a, b) {
		return nil, nil, nil, nil, errors.New("manual P4 首尾物理目录或配置漂移")
	}
	for _, receipt := range receipts {
		raw, e := protojson.MarshalOptions{UseProtoNames: true}.Marshal(receipt)
		if e != nil {
			return nil, nil, nil, nil, e
		}
		report.Consumers = append(report.Consumers, raw)
	}
	firstSum, lastSum := sha256.Sum256(first), sha256.Sum256(last)
	report.CatalogFirstSHA256 = firstSum[:]
	report.CatalogLastSHA256 = lastSum[:]
	report.ObservedEndMS = uint64(time.Now().UnixMilli())
	if err = budget.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	if report.ObservedEndMS < report.ObservedStartMS || report.ObservedEndMS-report.ObservedStartMS > 60000 {
		return nil, nil, nil, nil, errors.New("manual P4 采样超出窗口")
	}
	raw, err := json.Marshal(report)
	return receipts, raw, first, last, err
}

// BoundConsumerGroupConfig 把组代次与分区 reader 的所有权集中在一个实际消费 owner。
// HandlerTimeout 只约束已接纳处理，不继承 StopFetching 的取消；回调必须返回真实持久结局。
type BoundConsumerGroupConfig struct {
	Group          kafkago.ConsumerGroupConfig
	Reader         kafkago.ReaderConfig
	HandlerTimeout time.Duration
}

type boundPartition struct {
	topic     string
	partition int
	initial   int64
	committed int64
	accepted  uint64
	settled   uint64
	replayed  uint64
	reader    *kafkago.Reader
	pending   map[int64]*boundDelivery
}

// BoundConsumerGroup 只在 Generation.CommitOffsets 成功后推进本地前沿。
// 一个 Generation.Start 回调拥有全部分区 worker；单分区退出不能提前结束其他分区的提交代次。
type BoundConsumerGroup struct {
	config            BoundConsumerGroupConfig
	relay             *drainproof.Session
	handler           func(context.Context, kafkago.Message) error
	manual            bool
	messages          chan *boundDelivery
	commitMu          sync.Mutex
	commitOffsets     func(*kafkago.Generation, map[string]map[int]int64) error
	mu                sync.Mutex
	started           bool
	stopping          bool
	complete          bool
	generation        *kafkago.Generation
	generationContext context.Context
	generationCancel  context.CancelFunc
	replay            map[string]*boundDelivery
	resolvedReplay    []boundReplayObservation
	partitions        map[string]*boundPartition
	history           []*boundGroupObservation
	initialized       bool
	failure           error
	changed           chan struct{}
	stop              chan struct{}
	done              chan struct{}
}

func NewBoundConsumerGroup(config BoundConsumerGroupConfig, relay *drainproof.Session, handler func(context.Context, kafkago.Message) error) (*BoundConsumerGroup, error) {
	if relay == nil || handler == nil || config.HandlerTimeout <= 0 || config.HandlerTimeout > 5*time.Minute {
		return nil, errors.New("Kafka owner 缺少 relay、处理函数或有界处理预算")
	}
	if err := config.Group.Validate(); err != nil {
		return nil, err
	}
	if config.Group.Timeout <= 0 || config.Group.Timeout > 10*time.Second {
		return nil, errors.New("Kafka owner 组请求预算必须在十秒内")
	}
	if config.Group.StartOffset != kafkago.FirstOffset {
		return nil, errors.New("Kafka owner 不能以 latest 跳过无提交的历史")
	}
	if config.Reader.GroupID != "" || len(config.Reader.GroupTopics) != 0 || config.Reader.Partition != 0 || config.Reader.Topic != "" {
		return nil, errors.New("Kafka owner 的 reader 模板不能另设组或分区")
	}
	topics := map[string]bool{}
	for _, topic := range config.Group.Topics {
		if topic == "" || topics[topic] {
			return nil, errors.New("Kafka owner 订阅为空或重复")
		}
		topics[topic] = true
	}
	config.Group.Brokers = append([]string(nil), config.Group.Brokers...)
	config.Group.Topics = append([]string(nil), config.Group.Topics...)
	config.Reader.Brokers = append([]string(nil), config.Group.Brokers...)
	if config.Reader.Dialer == nil {
		config.Reader.Dialer = config.Group.Dialer
	}
	check := config.Reader
	check.Topic = config.Group.Topics[0]
	check.Partition = 0
	if err := check.Validate(); err != nil {
		return nil, err
	}
	return &BoundConsumerGroup{config: config, relay: relay, handler: handler, commitOffsets: func(generation *kafkago.Generation, offsets map[string]map[int]int64) error {
		return generation.CommitOffsets(offsets)
	}, changed: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}), replay: map[string]*boundDelivery{}}, nil
}

func boundPartitionKey(topic string, partition int) string {
	return fmt.Sprintf("%s/%d", topic, partition)
}
func (c *BoundConsumerGroup) signalLocked() { close(c.changed); c.changed = make(chan struct{}) }
func (c *BoundConsumerGroup) fail(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.failure = errors.Join(c.failure, err)
	c.signalLocked()
	c.mu.Unlock()
	c.relay.Hold()
}

// StopFetching 永久停止本 owner 取新；已拿到的记录仍要处理、提交并 join。
func (c *BoundConsumerGroup) StopFetching() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopping {
		c.stopping = true
		close(c.stop)
		if !c.started {
			c.complete = true
			close(c.done)
		}
		c.signalLocked()
	}
}

func (c *BoundConsumerGroup) Run(ctx context.Context) (resultErr error) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return drainproof.ErrClosed
	}
	// Stop 已先完成零接纳终局；稍后抵达的 Run 不创建 group，也不把已知停止升级为 UNKNOWN。
	if c.stopping {
		stopped := c.complete
		c.mu.Unlock()
		if stopped {
			return nil
		}
		return drainproof.ErrClosed
	}
	c.started = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.complete = true
		c.signalLocked()
		close(c.done)
		c.mu.Unlock()
	}()
	group, err := kafkago.NewConsumerGroup(c.config.Group)
	if err != nil {
		c.fail(err)
		return err
	}
	fetchContext, cancel := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-c.stop:
			cancel()
		case <-fetchContext.Done():
		}
	}()
	var generations sync.WaitGroup
	defer func() {
		cancel()
		<-watchDone
		generations.Wait()
		if err := group.Close(); err != nil {
			c.fail(err)
			resultErr = errors.Join(resultErr, err)
		}
		c.mu.Lock()
		resultErr = errors.Join(resultErr, c.failure)
		c.mu.Unlock()
	}()
	for {
		generation, err := group.Next(fetchContext)
		if err != nil {
			if fetchContext.Err() != nil {
				c.mu.Lock()
				held := c.failure
				c.mu.Unlock()
				return held
			}
			c.fail(err)
			return err
		}
		generations.Add(1)
		generation.Start(func(generationContext context.Context) {
			defer generations.Done()
			c.runGeneration(fetchContext, generationContext, generation)
		})
	}
}

func (c *BoundConsumerGroup) openPartition(ctx context.Context, topic string, assigned kafkago.PartitionAssignment) (*boundPartition, error) {
	if assigned.ID < 0 {
		return nil, errors.New("Kafka assignment 分区为负")
	}
	dialer := c.config.Reader.Dialer
	if dialer == nil {
		dialer = kafkago.DefaultDialer
	}
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var connection *kafkago.Conn
	var err error
	for _, broker := range c.config.Group.Brokers {
		connection, err = dialer.DialLeader(budget, "tcp", broker, topic, assigned.ID)
		if err == nil {
			break
		}
		if budget.Err() != nil {
			break
		}
	}
	if err != nil || connection == nil {
		return nil, errors.Join(errors.New("Kafka 分区 leader 不可读"), err)
	}
	deadline, _ := budget.Deadline()
	err = connection.SetDeadline(deadline)
	var earliest, end int64
	if err == nil {
		earliest, end, err = connection.ReadOffsets()
	}
	err = errors.Join(err, connection.Close())
	if err != nil {
		return nil, err
	}
	offset := assigned.Offset
	if offset == kafkago.FirstOffset {
		offset = earliest
	}
	if offset < earliest || offset > end || earliest < 0 {
		return nil, errors.New("Kafka assignment 提交点未知或已越过保留范围")
	}
	config := c.config.Reader
	config.Topic = topic
	config.Partition = assigned.ID
	reader := kafkago.NewReader(config)
	if err = reader.SetOffset(offset); err != nil {
		return nil, errors.Join(err, reader.Close())
	}
	return &boundPartition{topic: topic, partition: assigned.ID, initial: offset, committed: offset, reader: reader, pending: map[int64]*boundDelivery{}}, nil
}

func (c *BoundConsumerGroup) runGeneration(parent, generationContext context.Context, generation *kafkago.Generation) {
	fetchContext, cancel := context.WithCancel(parent)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-generationContext.Done():
			cancel()
		case <-fetchContext.Done():
		}
	}()
	defer func() { cancel(); <-watcherDone }()
	partitions := map[string]*boundPartition{}
	defer func() {
		c.mu.Lock()
		if c.generation == generation {
			c.history = append(c.history, c.observationLocked("SQL_DRAIN_KAFKA_GENERATION_CLOSED_V1"))
			if len(c.history) > 1024 {
				c.failure = errors.Join(c.failure, drainproof.ErrHold)
			}
		}
		c.initialized = false
		c.signalLocked()
		c.mu.Unlock()
		for _, partition := range partitions {
			if err := partition.reader.Close(); err != nil {
				c.fail(err)
			}
		}
	}()
	for topic, assigned := range generation.Assignments {
		for _, assignment := range assigned {
			partition, err := c.openPartition(fetchContext, topic, assignment)
			if err != nil {
				if fetchContext.Err() == nil {
					c.fail(err)
					c.StopFetching()
				}
				return
			}
			key := boundPartitionKey(topic, assignment.ID)
			if _, exists := partitions[key]; exists {
				c.fail(errors.New("Kafka 实际 assignment 重复"))
				c.StopFetching()
				_ = partition.reader.Close()
				return
			}
			partitions[key] = partition
		}
	}
	c.mu.Lock()
	c.generation = generation
	c.generationContext = generationContext
	c.generationCancel = cancel
	c.partitions = partitions
	c.initialized = true
	c.signalLocked()
	c.mu.Unlock()
	var workers sync.WaitGroup
	for _, partition := range partitions {
		workers.Add(1)
		go func(partition *boundPartition) {
			defer workers.Done()
			c.consumePartition(fetchContext, generationContext, generation, partition)
		}(partition)
	}
	// 空 assignment 也保持该真实代次，不能热循环创建不存在的准备证据。
	if len(partitions) == 0 {
		<-fetchContext.Done()
	}
	workers.Wait()
	if c.manual {
		c.waitDelivered(generationContext, generation, partitions)
	}
}

func (c *BoundConsumerGroup) consumePartition(fetchContext, generationContext context.Context, generation *kafkago.Generation, partition *boundPartition) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.fail(errors.New("Kafka 已接纳处理 panic，结局未知"))
			c.StopFetching()
		}
	}()
	for {
		message, err := partition.reader.FetchMessage(fetchContext)
		if err != nil {
			if fetchContext.Err() == nil {
				c.fail(err)
				c.StopFetching()
			}
			return
		}
		if c.manual {
			delivery := &boundDelivery{generation: generation, partition: partition, message: message}
			select {
			case c.messages <- delivery:
			case <-fetchContext.Done():
				return
			}
			continue
		}
		// Fetch 已返回即归本 owner；与 StopFetching 竞争时也不能丢弃已拿到的记录。
		finish, err := c.relay.Enter()
		if err != nil {
			c.fail(err)
			c.StopFetching()
			return
		}
		c.mu.Lock()
		partition.accepted++
		c.mu.Unlock()
		err = func() (resultErr error) {
			defer finish()
			defer func() {
				if recovered := recover(); recovered != nil {
					resultErr = errors.New("Kafka 已接纳 handler panic，结局未知")
				}
			}()
			if message.Topic != partition.topic || message.Partition != partition.partition || message.Offset < partition.committed {
				return errors.New("Kafka reader 返回分区或 offset 与代次不符")
			}
			work, cancel := context.WithTimeout(context.WithoutCancel(fetchContext), c.config.HandlerTimeout)
			defer cancel()
			if err := c.handler(work, message); err != nil {
				return err
			}
			if err := work.Err(); err != nil {
				return err
			}
			if generationContext.Err() != nil {
				return errors.New("Kafka handler 返回前代次已失效，提交结局未知")
			}
			if err := c.commitOffsets(generation, map[string]map[int]int64{partition.topic: {partition.partition: message.Offset + 1}}); err != nil {
				return err
			}
			c.mu.Lock()
			partition.committed = message.Offset + 1
			partition.settled++
			c.signalLocked()
			c.mu.Unlock()
			return nil
		}()
		if err != nil {
			c.fail(err)
			c.StopFetching()
			return
		}
	}
}

// CloseAndReport 只在全部实际 worker 返回、reader 和 consumer group 关闭后给出原件。
// 超时或未知结局返回 HOLD，不能用 cancel 或空本地队列伪造 settled。
func (c *BoundConsumerGroup) CloseAndReport(ctx context.Context) ([]byte, error) {
	c.StopFetching()
	select {
	case <-ctx.Done():
		return nil, errors.Join(drainproof.ErrHold, ctx.Err())
	case <-c.done:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.complete || c.failure != nil {
		return nil, errors.Join(drainproof.ErrHold, c.failure)
	}
	for _, generation := range c.history {
		for _, partition := range generation.Partitions {
			if partition.Accepted != partition.Settled+partition.Replayed {
				return nil, drainproof.ErrHold
			}
		}
	}
	if len(c.replay) != 0 {
		return nil, drainproof.ErrHold
	}
	return json.Marshal(struct {
		Schema         string
		Group          string
		Started        bool
		Generations    []*boundGroupObservation
		ResolvedReplay []boundReplayObservation
	}{"SQL_DRAIN_KAFKA_OWNER_CLOSED_V1", c.config.Group.ID, c.started, c.history, c.resolvedReplay})
}

// 以下原件只记录 Kafka API 实际返回值；分区的历史身份与 floor 仍引用唯一正式 proto。
type boundPartitionObservation struct {
	Topic     string
	Partition int
	Initial   int64
	Committed int64
	Accepted  uint64
	Settled   uint64
	Replayed  uint64
}
type boundGroupObservation struct {
	Schema     string
	Group      string
	Generation int32
	Member     string
	ObservedMS uint64
	Partitions []boundPartitionObservation
}

func (c *BoundConsumerGroup) observationLocked(schema string) *boundGroupObservation {
	report := &boundGroupObservation{Schema: schema, Group: c.config.Group.ID, ObservedMS: uint64(time.Now().UnixMilli())}
	if c.generation != nil {
		report.Generation = c.generation.ID
		report.Member = c.generation.MemberID
	}
	for _, partition := range c.partitions {
		report.Partitions = append(report.Partitions, boundPartitionObservation{Topic: partition.topic, Partition: partition.partition, Initial: partition.initial, Committed: partition.committed, Accepted: partition.accepted, Settled: partition.settled, Replayed: partition.replayed})
	}
	sort.Slice(report.Partitions, func(i, j int) bool {
		a, b := report.Partitions[i], report.Partitions[j]
		if a.Topic != b.Topic {
			return a.Topic < b.Topic
		}
		return a.Partition < b.Partition
	})
	return report
}

func (c *BoundConsumerGroup) bindExpected(runtime *drainproof.Runtime, role string, expected []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure) (*pb.SqlWriterMaintenanceReceipt_IssuerClosure, error) {
	if runtime == nil || runtime.Plan == nil || runtime.Request == nil || runtime.Request.Evidence == nil || runtime.Request.RunPhase != pb.SqlWriterDrainRun_SQL_RELEASED || len(expected) == 0 || len(expected) > 16384 {
		return nil, drainproof.ErrHold
	}
	seed := runtime.Seed(role)
	if seed == nil {
		return nil, drainproof.ErrHold
	}
	topics := map[string]bool{}
	for _, topic := range c.config.Group.Topics {
		topics[topic] = true
	}
	planned := map[string]*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
	for _, partition := range runtime.Plan.KafkaPartitions {
		if partition != nil && partition.Group == c.config.Group.ID && topics[partition.TopicName] {
			key := boundPartitionKey(partition.TopicName, int(partition.Partition))
			if planned[key] != nil {
				return nil, drainproof.ErrHold
			}
			planned[key] = partition
		}
	}
	if len(planned) != len(expected) {
		return nil, drainproof.ErrHold
	}
	for topic := range topics {
		found := false
		for _, partition := range planned {
			if partition.TopicName == topic {
				found = true
				break
			}
		}
		if !found {
			return nil, drainproof.ErrHold
		}
	}
	seen := map[string]bool{}
	for _, partition := range expected {
		if partition == nil {
			return nil, drainproof.ErrHold
		}
		key := boundPartitionKey(partition.TopicName, int(partition.Partition))
		if seen[key] || !proto.Equal(planned[key], partition) {
			return nil, drainproof.ErrHold
		}
		seen[key] = true
	}
	return seed, nil
}

// PreparedFrontier 记录本实例真实 live 代次及 assigned 子集；全局 P8 核验器负责多实例联合覆盖。
// expected 必须是本 configured group/topics 在正式 Plan 中的完整集合，不能按当前 assignment 缩小。
func (c *BoundConsumerGroup) PreparedFrontier(ctx context.Context, runtime *drainproof.Runtime, role string, expected []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure) ([]byte, []byte, error) {
	seed, err := c.bindExpected(runtime, role, expected)
	if err != nil {
		return nil, nil, err
	}
	expectedByKey := map[string]*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
	for _, base := range expected {
		expectedByKey[boundPartitionKey(base.TopicName, int(base.Partition))] = base
	}
	var generation *kafkago.Generation
	var assigned []boundPartitionObservation
	for {
		c.mu.Lock()
		if c.failure != nil || c.stopping {
			c.mu.Unlock()
			return nil, nil, drainproof.ErrHold
		}
		ready := c.initialized && c.generation != nil && c.generationContext != nil && c.generationContext.Err() == nil && c.generation.MemberID != ""
		if ready {
			for key, partition := range c.partitions {
				base := expectedByKey[key]
				if base == nil || partition.initial < base.ProtectedCommittedFloor || partition.committed < base.ProtectedCommittedFloor {
					c.mu.Unlock()
					return nil, nil, drainproof.ErrHold
				}
			}
			generation = c.generation
			assigned = c.observationLocked("").Partitions
			c.mu.Unlock()
			break
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, errors.Join(drainproof.ErrHold, ctx.Err())
		case <-changed:
		}
	}
	prober, err := NewGroupLagProber(c.config.Group.Brokers)
	if err != nil {
		return nil, nil, err
	}
	catalogRaw, err := prober.CaptureDrainCatalog(ctx)
	var catalog DrainKafkaCatalog
	if err == nil {
		err = json.Unmarshal(catalogRaw, &catalog)
	}
	report := &pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness{OperationId: bytes.Clone(runtime.Plan.RunId), ManifestDigest: bytes.Clone(runtime.Request.Evidence.ManifestDigest), IssuerId: bytes.Clone(seed.IssuerId), InstanceId: bytes.Clone(seed.InstanceId), Group: c.config.Group.ID, GenerationId: generation.ID, MemberId: generation.MemberID}
	if err == nil {
		for _, base := range expected {
			if ctx.Err() != nil {
				err = ctx.Err()
				break
			}
			if !bytes.Equal(base.ClusterId, catalog.ClusterID) || !bytes.Equal(base.TopicId, catalog.TopicIDs[base.TopicName]) || int(base.Partition) >= catalog.TopicPartitions[base.TopicName] {
				err = drainproof.ErrHold
				break
			}
			report.ExpectedPartitions = append(report.ExpectedPartitions, proto.Clone(base).(*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure))
		}
	}
	if err == nil {
		for _, local := range assigned {
			if ctx.Err() != nil {
				err = ctx.Err()
				break
			}
			base := expectedByKey[boundPartitionKey(local.Topic, local.Partition)]
			offsets, e := CommittedOffsets(prober, base.Group, base.TopicName, []int32{int32(base.Partition)})
			if e != nil {
				err = e
				break
			}
			end, e := prober.GetOffset(base.TopicName, int32(base.Partition), sarama.OffsetNewest)
			if e != nil {
				err = e
				break
			}
			earliest, e := prober.GetOffset(base.TopicName, int32(base.Partition), sarama.OffsetOldest)
			if e != nil {
				err = e
				break
			}
			committed := offsets[int32(base.Partition)]
			if committed == -1 && base.InitialEarliest == 0 && base.ProtectedCommittedFloor == 0 && earliest == 0 && end == 0 {
				committed = 0
			}
			if earliest < base.InitialEarliest || earliest > base.ProtectedCommittedFloor || committed < base.ProtectedCommittedFloor || committed < local.Committed || committed > end {
				err = drainproof.ErrHold
				break
			}
			row := proto.Clone(base).(*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure)
			row.FinalEarliest = earliest
			row.FinalExclusiveEnd = end
			row.FinalCommitted = committed
			report.AssignedPartitions = append(report.AssignedPartitions, row)
		}
	}
	err = errors.Join(err, prober.Close())
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	valid := c.generation == generation && c.initialized && !c.stopping && c.failure == nil && c.generationContext.Err() == nil
	c.mu.Unlock()
	if !valid || ctx.Err() != nil {
		return nil, nil, drainproof.ErrHold
	}
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(&pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{Groups: []*pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness{report}})
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], raw, nil
}

// MergeConsumerReadiness 合并同 boot 多个实际 consumer owner 的正式原件，不填充虚构分区。
func MergeConsumerReadiness(reports ...[]byte) ([]byte, []byte, error) {
	if len(reports) == 0 || len(reports) > 128 {
		return nil, nil, drainproof.ErrHold
	}
	merged := &pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{}
	seen := map[string]bool{}
	var first *pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness
	for _, raw := range reports {
		part := &pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{}
		if len(raw) == 0 || len(raw) > 16<<20 || (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, part) != nil || len(part.Groups) == 0 || len(part.ManualAssignments) != 0 {
			return nil, nil, drainproof.ErrHold
		}
		for _, group := range part.Groups {
			if group == nil || group.Group == "" || group.MemberId == "" || seen[group.Group] || len(group.ExpectedPartitions) == 0 {
				return nil, nil, drainproof.ErrHold
			}
			if first == nil {
				first = group
			} else if !bytes.Equal(first.OperationId, group.OperationId) || !bytes.Equal(first.ManifestDigest, group.ManifestDigest) || !bytes.Equal(first.IssuerId, group.IssuerId) || !bytes.Equal(first.InstanceId, group.InstanceId) {
				return nil, nil, drainproof.ErrHold
			}
			seen[group.Group] = true
			merged.Groups = append(merged.Groups, group)
		}
	}
	sort.Slice(merged.Groups, func(i, j int) bool { return merged.Groups[i].Group < merged.Groups[j].Group })
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(merged)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], raw, nil
}

// ValidatePrepared 在实际 open 前复核同一活代次；P8 后 rebalance 必须 HOLD，不能重签旧请求。
func (c *BoundConsumerGroup) ValidatePrepared(raw []byte) error {
	report := &pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{}
	if len(raw) == 0 || len(raw) > 16<<20 || (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, report) != nil || len(report.ManualAssignments) != 0 {
		return drainproof.ErrHold
	}
	var selected *pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness
	for _, group := range report.Groups {
		if group != nil && group.Group == c.config.Group.ID {
			if selected != nil {
				return drainproof.ErrHold
			}
			selected = group
		}
	}
	if selected == nil {
		return drainproof.ErrHold
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized || c.stopping || c.failure != nil || c.generation == nil || c.generationContext == nil || c.generationContext.Err() != nil || c.generation.ID != selected.GenerationId || c.generation.MemberID != selected.MemberId || len(c.partitions) != len(selected.AssignedPartitions) {
		return drainproof.ErrHold
	}
	seen := map[string]bool{}
	for _, row := range selected.AssignedPartitions {
		if row == nil || row.Group != c.config.Group.ID {
			return drainproof.ErrHold
		}
		key := boundPartitionKey(row.TopicName, int(row.Partition))
		partition := c.partitions[key]
		if seen[key] || partition == nil || partition.committed < row.FinalCommitted {
			return drainproof.ErrHold
		}
		seen[key] = true
	}
	return nil
}

// ExpectedPartitions 只从已绑定的正式 Plan 提取本 owner 的配置范围，返回副本给 PreparedFrontier。
func (c *BoundConsumerGroup) ExpectedPartitions(runtime *drainproof.Runtime) ([]*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure, error) {
	if runtime == nil || runtime.Plan == nil {
		return nil, drainproof.ErrHold
	}
	topics := map[string]bool{}
	for _, topic := range c.config.Group.Topics {
		topics[topic] = false
	}
	var expected []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure
	for _, row := range runtime.Plan.KafkaPartitions {
		if row == nil || row.Group != c.config.Group.ID {
			continue
		}
		if _, configured := topics[row.TopicName]; configured {
			topics[row.TopicName] = true
			expected = append(expected, proto.Clone(row).(*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure))
		}
	}
	for _, found := range topics {
		if !found {
			return nil, drainproof.ErrHold
		}
	}
	if len(expected) == 0 {
		return nil, drainproof.ErrHold
	}
	return expected, nil
}

// boundDelivery 保存实际交付的原件和代次；提交不接受调用方合成的消息或跨代次旧消息。
type boundDelivery struct {
	generation       *kafkago.Generation
	partition        *boundPartition
	message          kafkago.Message
	finish           func()
	payloadDigest    [32]byte
	originGeneration int32
	replayed         bool
}

// NewBoundMessageReader 与 handler 模式共享一个 ConsumerGroup；供原有批 SQL owner 保留批处理。
// 调用方仍须只调用一次 Run。FetchMessage 返回才接纳，CommitMessages 表示该分区已持久前缀。
func NewBoundMessageReader(config BoundConsumerGroupConfig, relay *drainproof.Session) (*BoundConsumerGroup, error) {
	// reader 模式的 SQL/事务预算由现有批处理 owner 负责；此值不改变其事务语义。
	if config.HandlerTimeout == 0 {
		config.HandlerTimeout = time.Minute
	}
	owner, err := NewBoundConsumerGroup(config, relay, func(context.Context, kafkago.Message) error { return drainproof.ErrHold })
	if err != nil {
		return nil, err
	}
	capacity := config.Reader.QueueCapacity
	if capacity == 0 {
		capacity = 100
	}
	if capacity < 1 || capacity > 16384 {
		return nil, errors.New("Kafka 交付队列超过固定预算")
	}
	owner.manual = true
	owner.messages = make(chan *boundDelivery, capacity)
	return owner, nil
}

// FetchMessage 不提交 offset。停止取新后缓冲中的未交付消息不计入 accepted，也不会提交。
func (c *BoundConsumerGroup) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	if !c.manual {
		return kafkago.Message{}, errors.New("handler 模式不允许另取消息")
	}
	for {
		c.mu.Lock()
		stopped := c.stopping
		failure := c.failure
		c.mu.Unlock()
		if stopped || failure != nil {
			return kafkago.Message{}, errors.Join(drainproof.ErrClosed, failure)
		}
		select {
		case <-ctx.Done():
			return kafkago.Message{}, ctx.Err()
		case <-c.stop:
			return kafkago.Message{}, drainproof.ErrClosed
		case <-c.done:
			return kafkago.Message{}, drainproof.ErrClosed
		case delivery := <-c.messages:
			c.mu.Lock()
			if c.stopping || c.failure != nil {
				c.mu.Unlock()
				return kafkago.Message{}, drainproof.ErrHold
			}
			if c.generation != delivery.generation || !c.initialized || c.generationContext.Err() != nil {
				c.mu.Unlock()
				continue
			}
			message := delivery.message
			partition := delivery.partition
			if message.Topic != partition.topic || message.Partition != partition.partition || message.Offset < partition.committed || partition.pending[message.Offset] != nil {
				c.mu.Unlock()
				c.fail(errors.New("Kafka 实际交付重复或分区不符"))
				c.StopFetching()
				return kafkago.Message{}, drainproof.ErrHold
			}
			key := boundDeliveryKey(message)
			digest := boundMessageDigest(message)
			if original := c.replay[key]; original != nil {
				if original.payloadDigest != digest {
					c.mu.Unlock()
					c.fail(errors.New("Kafka 同 T/P/O 重放原件改变"))
					c.StopFetching()
					return kafkago.Message{}, drainproof.ErrHold
				}
				delivery.finish = original.finish
				delivery.payloadDigest = digest
				delivery.originGeneration = original.originGeneration
				delivery.replayed = true
				delete(c.replay, key)
			} else {
				finish, err := c.relay.Enter()
				if err != nil {
					c.mu.Unlock()
					c.fail(err)
					c.StopFetching()
					return kafkago.Message{}, err
				}
				delivery.finish = finish
				delivery.payloadDigest = digest
				delivery.originGeneration = delivery.generation.ID
			}
			// SQL 批处理 owner 可以清空 payload 后以元数据提交；本层不额外钉住第二份大消息。
			delivery.message = kafkago.Message{Topic: message.Topic, Partition: message.Partition, Offset: message.Offset}
			partition.pending[message.Offset] = delivery
			partition.accepted++
			c.signalLocked()
			c.mu.Unlock()
			return message, nil
		}
	}
}

// CommitMessages 只推进本代次实际交付且已被批 SQL owner 确认的前缀。
// 最高 offset 隐含该分区此前交付的所有记录已持久；错误/超时不能被后续提交洗成成功。
func (c *BoundConsumerGroup) CommitMessages(ctx context.Context, messages ...kafkago.Message) error {
	if !c.manual || len(messages) == 0 {
		return drainproof.ErrHold
	}
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	if err := ctx.Err(); err != nil {
		c.fail(err)
		c.StopFetching()
		return err
	}
	c.mu.Lock()
	generation := c.generation
	if generation == nil || c.generationContext == nil || c.generationContext.Err() != nil || c.failure != nil {
		c.mu.Unlock()
		c.fail(drainproof.ErrHold)
		c.StopFetching()
		return drainproof.ErrHold
	}
	offsets := map[string]map[int]int64{}
	for _, message := range messages {
		partition := c.partitions[boundPartitionKey(message.Topic, message.Partition)]
		var delivery *boundDelivery
		if partition != nil {
			delivery = partition.pending[message.Offset]
		}
		if delivery == nil || delivery.generation != generation {
			c.mu.Unlock()
			c.fail(errors.New("Kafka 批提交不是本代次已交付原件"))
			c.StopFetching()
			return drainproof.ErrHold
		}
		if offsets[message.Topic] == nil {
			offsets[message.Topic] = map[int]int64{}
		}
		if next := message.Offset + 1; next > offsets[message.Topic][message.Partition] {
			offsets[message.Topic][message.Partition] = next
		}
	}
	for _, replay := range c.replay {
		if next := offsets[replay.message.Topic][replay.message.Partition]; next > replay.message.Offset {
			c.mu.Unlock()
			c.fail(errors.New("Kafka 批提交越过尚未恢复的已交付前缀"))
			c.StopFetching()
			return drainproof.ErrHold
		}
	}
	c.mu.Unlock()
	err := c.commitOffsets(generation, offsets)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		c.fail(err)
		c.StopFetching()
		return err
	}
	c.mu.Lock()
	if c.generation != generation || c.generationContext.Err() != nil {
		c.mu.Unlock()
		c.fail(drainproof.ErrHold)
		c.StopFetching()
		return drainproof.ErrHold
	}
	var finished []func()
	for topic, partitions := range offsets {
		for id, next := range partitions {
			partition := c.partitions[boundPartitionKey(topic, id)]
			if next > partition.committed {
				partition.committed = next
			}
			for offset, delivery := range partition.pending {
				if offset < next {
					delete(partition.pending, offset)
					partition.settled++
					if delivery.replayed {
						c.resolvedReplay = append(c.resolvedReplay, boundReplayObservation{Topic: topic, Partition: id, Offset: offset, FirstGeneration: delivery.originGeneration, CommittedGeneration: generation.ID, PayloadDigest: delivery.payloadDigest[:]})
					}
					finished = append(finished, delivery.finish)
				}
			}
		}
	}
	c.signalLocked()
	c.mu.Unlock()
	for _, finish := range finished {
		finish()
	}
	return nil
}

func (c *BoundConsumerGroup) waitDelivered(_ context.Context, _ *kafkago.Generation, partitions map[string]*boundPartition) {
	for {
		c.mu.Lock()
		pending := 0
		for _, partition := range partitions {
			pending += len(partition.pending)
		}
		if pending == 0 {
			c.mu.Unlock()
			return
		}
		if c.failure != nil {
			// 只响应实际处理 owner 明确报告的 UNKNOWN，不把 rebalance/取消本身伪造为 SQL 结局。
			// 这里只释放本地等待资源，HOLD 永久保留；unknown 不增加 settled。
			var finished []func()
			for _, partition := range partitions {
				for offset, delivery := range partition.pending {
					finished = append(finished, delivery.finish)
					delete(partition.pending, offset)
				}
			}
			c.failure = errors.Join(c.failure, errors.New("Kafka 已交付批次没有本代次确认提交"))
			c.signalLocked()
			c.mu.Unlock()
			c.relay.Hold()
			for _, finish := range finished {
				finish()
			}
			return
		}
		changed := c.changed
		c.mu.Unlock()
		<-changed
	}
}

func (c *BoundConsumerGroup) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.CloseAndReport(ctx)
	return err
}

// WithConsumerReadiness 按固定 group 顺序持有全部真实 owner 的代次锁，核验与本机 open 共用临界区。
// commit 只允许同步提交根门禁及读取前沿；不能做网络 I/O，也不能回调 consumer owner。
func WithConsumerReadiness(ctx context.Context, raw []byte, owners []*BoundConsumerGroup, commit func() ([]byte, error)) ([]byte, error) {
	if commit == nil || len(owners) == 0 || len(owners) > 128 {
		return nil, drainproof.ErrHold
	}
	report := &pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{}
	if len(raw) == 0 || len(raw) > 16<<20 || (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, report) != nil || len(report.Groups) != len(owners) || len(report.ManualAssignments) != 0 {
		return nil, drainproof.ErrHold
	}
	ordered := append([]*BoundConsumerGroup(nil), owners...)
	for _, owner := range ordered {
		if owner == nil {
			return nil, drainproof.ErrHold
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].config.Group.ID < ordered[j].config.Group.ID })
	for i, owner := range ordered {
		if i > 0 && owner.config.Group.ID == ordered[i-1].config.Group.ID {
			return nil, drainproof.ErrHold
		}
	}
	selected := map[string]*pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness{}
	for _, group := range report.Groups {
		if group == nil || selected[group.Group] != nil {
			return nil, drainproof.ErrHold
		}
		selected[group.Group] = group
	}
	for _, owner := range ordered {
		owner.mu.Lock()
	}
	defer func() {
		for i := len(ordered) - 1; i >= 0; i-- {
			ordered[i].mu.Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, owner := range ordered {
		group := selected[owner.config.Group.ID]
		if group == nil || !owner.initialized || owner.stopping || owner.failure != nil || owner.generation == nil || owner.generationContext == nil || owner.generationContext.Err() != nil || owner.generation.ID != group.GenerationId || owner.generation.MemberID != group.MemberId || len(owner.partitions) != len(group.AssignedPartitions) {
			return nil, drainproof.ErrHold
		}
		seen := map[string]bool{}
		for _, row := range group.AssignedPartitions {
			if row == nil || row.Group != group.Group {
				return nil, drainproof.ErrHold
			}
			key := boundPartitionKey(row.TopicName, int(row.Partition))
			partition := owner.partitions[key]
			if seen[key] || partition == nil || partition.committed < row.FinalCommitted {
				return nil, drainproof.ErrHold
			}
			seen[key] = true
		}
	}
	return commit()
}

// ProcessingFailure 明确区分已证明未提交与 SQL 结果不明；取消或错误文字不能充当回滚证明。
type ProcessingFailure uint8

const (
	ProcessingNotCommitted ProcessingFailure = iota + 1
	ProcessingOutcomeUnknown
)

type boundReplayObservation struct {
	Topic               string
	Partition           int
	Offset              int64
	FirstGeneration     int32
	CommittedGeneration int32
	PayloadDigest       []byte
}

func boundDeliveryKey(message kafkago.Message) string {
	return fmt.Sprintf("%s/%d/%d", message.Topic, message.Partition, message.Offset)
}
func boundMessageDigest(message kafkago.Message) [32]byte {
	hash := sha256.New()
	var length [8]byte
	write := func(raw []byte) {
		binary.BigEndian.PutUint64(length[:], uint64(len(raw)))
		hash.Write(length[:])
		hash.Write(raw)
	}
	write(message.Key)
	write(message.Value)
	binary.BigEndian.PutUint64(length[:], uint64(len(message.Headers)))
	hash.Write(length[:])
	for _, header := range message.Headers {
		write([]byte(header.Key))
		write(header.Value)
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

// ReportProcessingFailure 必须由实际批 SQL owner 在交回失败批次之前调用，messages 可只带 T/P/O。
// NotCommitted 只用于已有明确未提交/回滚证明的整个持久前缀；保留原 token，换代后同原件真实 commit 才结束。
// 返回 nil 后继续使用本 owner Fetch，不能 Close 后另建 group 丢弃原票据。UNKNOWN 永久 HOLD。
// 本方法不做 I/O；处理 ctx 取消后也允许记录已取得的事务结局，取消本身不改变 outcome。
func (c *BoundConsumerGroup) ReportProcessingFailure(ctx context.Context, outcome ProcessingFailure, messages ...kafkago.Message) error {
	if ctx == nil || !c.manual || len(messages) == 0 || (outcome != ProcessingNotCommitted && outcome != ProcessingOutcomeUnknown) {
		return drainproof.ErrHold
	}
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	c.mu.Lock()
	generation := c.generation
	if generation == nil || c.failure != nil {
		c.mu.Unlock()
		return drainproof.ErrHold
	}
	selected := map[string]*boundDelivery{}
	for _, message := range messages {
		partition := c.partitions[boundPartitionKey(message.Topic, message.Partition)]
		var delivery *boundDelivery
		if partition != nil {
			delivery = partition.pending[message.Offset]
		}
		key := boundDeliveryKey(message)
		if delivery == nil || delivery.generation != generation || selected[key] != nil {
			c.mu.Unlock()
			c.fail(errors.New("Kafka 失败批次不是本代次实际已交付票据"))
			c.StopFetching()
			return drainproof.ErrHold
		}
		selected[key] = delivery
	}
	if outcome == ProcessingOutcomeUnknown {
		c.mu.Unlock()
		c.fail(errors.New("SQL 批次提交结局未知，保留 T/P/O 原件并 HOLD"))
		c.StopFetching()
		return drainproof.ErrHold
	}
	if len(c.replay)+len(selected) > 16384 || len(c.resolvedReplay) > 16384 {
		c.mu.Unlock()
		c.fail(errors.New("Kafka 确定性恢复票据超过固定预算"))
		c.StopFetching()
		return drainproof.ErrHold
	}
	for key, delivery := range selected {
		delete(delivery.partition.pending, delivery.message.Offset)
		delivery.partition.replayed++
		c.replay[key] = delivery
	}
	// 只停本代次取新；其它已交付分区仍可提交。所有 pending 有实际结局后才结束代次并重新分配。
	c.initialized = false
	cancel := c.generationCancel
	c.signalLocked()
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}
