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
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"math/rand/v2"
	"path/filepath"
	pb "proto/common/database"
	"shared/drainproof"
	"shared/safego"
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
	// CommitStall 是有积压的分区里,已提交位点最久多长时间没动(CommitStall.Observe)。要跨轮记状态,
	// 只由 RunGroupLagProbe 填;单次探测(Probe / ProbeOffsets)恒为 0。
	CommitStall time.Duration
}

// PartitionOffsets 是一个分区在一次探测里读到的位点。Committed = -1 表示这个组在该分区没提交过(消费者从 Start 读起)。
// 只供进程内按分区判停滞(CommitStall),调用方不按分区出指标。
type PartitionOffsets struct {
	Partition             int32
	Committed, Start, End int64
}

// Backlog 是这个组在该分区上还没提交到的条数,口径同 GroupLag.Lag 里该分区那一项(committed 越过 End 记 0)。
func (p PartitionOffsets) Backlog() int64 {
	if from := max(p.Committed, p.Start); p.End > from {
		return p.End - from
	}
	return 0
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
	lag, _, err := computeGroupOffsets(r, group, topic, partitions)
	return lag, err
}

// computeGroupOffsets 同 computeGroupLag,另按分区号升序返回各分区读到的位点(CommitStall 按分区判停滞要用)。
// 返回 error 时位点不可用。
func computeGroupOffsets(r groupOffsetReader, group, topic string, partitions int32) (GroupLag, []PartitionOffsets, error) {
	if partitions <= 0 {
		return GroupLag{}, nil, fmt.Errorf("kafka lag probe: topic %s has invalid partition count %d", topic, partitions)
	}
	ids := make([]int32, partitions)
	for i := range ids {
		ids[i] = int32(i)
	}

	// 先读 committed(见上面的读取顺序说明);错误语义与文本由 CommittedOffsets 统一给出,这里只补前缀。
	committedByPartition, err := CommittedOffsets(r, group, topic, ids)
	if err != nil {
		return GroupLag{}, nil, fmt.Errorf("kafka lag probe: %w", err)
	}
	committed := make([]int64, partitions)
	for i, p := range ids {
		committed[i] = committedByPartition[p]
	}

	var (
		out      GroupLag
		offsets  = make([]PartitionOffsets, 0, partitions)
		mismatch error
	)
	for i, p := range ids {
		end, err := r.GetOffset(topic, p, sarama.OffsetNewest)
		if err != nil {
			return GroupLag{}, nil, fmt.Errorf("kafka lag probe: end offset of %s[%d]: %w", topic, p, err)
		}
		start, err := r.GetOffset(topic, p, sarama.OffsetOldest)
		if err != nil {
			return GroupLag{}, nil, fmt.Errorf("kafka lag probe: start offset of %s[%d]: %w", topic, p, err)
		}
		c := committed[i]
		offsets = append(offsets, PartitionOffsets{Partition: p, Committed: c, Start: start, End: end})
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
	return out, offsets, mismatch
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

// ProbeOffsets 同 Probe,另返回各分区读到的位点(见 computeGroupOffsets),喂给 CommitStall。
func (p *GroupLagProber) ProbeOffsets(group, topic string, partitions int32) (GroupLag, []PartitionOffsets, error) {
	return computeGroupOffsets(p, group, topic, partitions)
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
// 回调（与 reader 模式的批处理落库）必须幂等：暂时失败或代次结束时 owner 不提交，同一条会被原地重调或由下一代次重投。
// 设计见 docs/design/kafka-bound-consumer-group.md。
type BoundConsumerGroupConfig struct {
	Group          kafkago.ConsumerGroupConfig
	Reader         kafkago.ReaderConfig
	HandlerTimeout time.Duration
	// DeadLetter 打开 handler 模式的「有界重试后转死信」（bound_consumer_dead_letter.go）。nil = 不转死信：回调失败一律原地无限重试，
	// 毒丸会让所在分区停在那一条（match 结果消费按负责人 2026-11-06 的决定取这一档：保留位点、告警、人工处置）。reader 模式不支持。
	DeadLetter *DeadLetterConfig
}

type boundPartition struct {
	topic     string
	partition int
	initial   int64
	committed int64
	accepted  uint64
	settled   uint64
	replayed  uint64
	// released 是没有提交、交还 Kafka 重投的已接纳记录：代次已结束、提交被拒，或 owner 终止时还没结清。
	released uint64
	// handedOff 是 released 里 owner 终止时交还给新属主的那部分：票据留到 CloseAndReport 核到组内已提交位点越过它才结清。
	handedOff uint64
	// deadLettered 是 settled 里先写成死信、再提交越过的那部分。
	deadLettered uint64
	reader       boundPartitionReader
	pending  map[int64]*boundDelivery
	// groupCommitted：initial 是组内已提交位点（assignment.Offset >= 0）；false = 这个组在该分区从没提交过，initial 只是日志起点。
	groupCommitted bool
}

// boundGroupClient 是 owner 对消费组的全部要求；*kafkago.ConsumerGroup 满足它。单测换成替身，不连真实 Kafka。
type boundGroupClient interface {
	Next(context.Context) (*kafkago.Generation, error)
	Close() error
}

// boundPartitionReader 是分区 worker 对分区 reader 的全部要求；*kafkago.Reader 满足它。
type boundPartitionReader interface {
	FetchMessage(context.Context) (kafkago.Message, error)
	Close() error
}

// BoundConsumerGroup 只在 Generation.CommitOffsets 成功后推进本地前沿。
// 一个 Generation.Start 回调拥有全部分区 worker；单分区退出不能提前结束其他分区的提交代次。
//
// 一个 owner 跨越本进程的全部代次（设计见 docs/design/kafka-bound-consumer-group.md）：
//   - 再均衡、入组失败、分区打开或拉取失败、回调失败、提交失败都不结束 owner。代次还在就有界指数退避后原地重试同一条；
//     代次已结束就不提交、把这条交还 Kafka（released），由下一代次的属主（可能还是本 owner）从组内已提交位点重投。
//   - 打开了死信（DeadLetter）时，回调失败到预算或判成 ErrUnprocessable 的记录写进死信存储，写成功才提交越过它。
//   - owner 终止（控制面 StopFetching 或 Run 的 ctx 结束）时还没结清的记录不再重试，交还给新属主（handedOff）；
//     票据留到 CloseAndReport，owner 离组之后按组内已提交位点核到新属主提交越过它才结清，核不到才 HOLD。
//   - 不能自愈的情形才 fail：记 HOLD 并永久停止取新（不变量破坏、提交点越过保留范围、保留期删掉了本组还没处理的记录）。
//     排空收据 CloseAndReport 的判据不变：每条已接纳记录都要有确定结局。
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
	// runContext 是 Run 的 owner 生命周期：StopFetching 或调用方 ctx 结束即取消，见 terminating。
	runContext context.Context
	// compacted / replayCompacted 汇总滚出窗口的旧原件，见 appendHistoryLocked / appendResolvedReplayLocked。
	compacted       boundHistoryCompaction
	replayCompacted boundReplayCompaction
	// deadLetter 是归一化之后的死信配置；nil = 不转死信。
	deadLetter *DeadLetterConfig
	// handoffs 是 owner 终止时交还给新属主、还没核到结局的记录；resolvedHandoffs 是收尾核对结清的原件（bound_consumer_tickets.go）。
	handoffs         []*boundHandoff
	resolvedHandoffs []boundHandoffObservation

	// 以下是与 Kafka 及时间的接缝：生产取 NewBoundConsumerGroup 装配的实现，单测换成替身（不连 Kafka、不依赖墙钟）。
	newGroup        func(kafkago.ConsumerGroupConfig) (boundGroupClient, error)
	startGeneration func(*kafkago.Generation, func(context.Context))
	openReader      func(context.Context, string, kafkago.PartitionAssignment) (boundPartitionReader, int64, error)
	// sleep 可中断地等待 d（生产实现带抖动）；ctx 结束返回 false。
	sleep func(context.Context, time.Duration) bool
	// readLogStart 读分区当前的日志起点（取到的记录前面有缺口时核保留期，见 admitGap）。
	readLogStart func(ctx context.Context, topic string, partition int) (int64, error)
	// readCommitted 读本组在 topic 这些分区上的已提交位点（-1 = 没提交过），收尾核对交还与恢复票据用。
	readCommitted func(ctx context.Context, topic string, partitions []int32) (map[int32]int64, error)
	// now 给死信原件打时间戳。
	now func() time.Time
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
	deadLetter, err := normalizeDeadLetter(config.DeadLetter)
	if err != nil {
		return nil, err
	}
	config.DeadLetter = deadLetter
	owner := &BoundConsumerGroup{config: config, relay: relay, handler: handler, commitOffsets: func(generation *kafkago.Generation, offsets map[string]map[int]int64) error {
		return generation.CommitOffsets(offsets)
	}, changed: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}), replay: map[string]*boundDelivery{}, deadLetter: deadLetter}
	owner.newGroup = func(groupConfig kafkago.ConsumerGroupConfig) (boundGroupClient, error) {
		group, err := kafkago.NewConsumerGroup(groupConfig)
		if err != nil {
			return nil, err
		}
		return group, nil
	}
	owner.startGeneration = func(generation *kafkago.Generation, run func(context.Context)) { generation.Start(run) }
	owner.openReader = owner.dialPartitionReader
	owner.sleep = sleepWithJitter
	owner.readLogStart = owner.dialLogStart
	owner.readCommitted = owner.fetchGroupCommitted
	owner.now = time.Now
	primeBoundConsumerMetrics(config.Group.ID, deadLetter != nil)
	return owner, nil
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
	observeBoundHold(c.config.Group.ID)
}

// terminating 报告 owner 是否已终止：控制面调了 StopFetching，或 Run 的 ctx 已结束。
// 终止之后本 owner 不再重试、也不再取下一代次；还没结清的已接纳记录交还给新属主（handedOff），收尾时核结局。
func (c *BoundConsumerGroup) terminating() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminatingLocked()
}

// terminatingLocked 同 terminating，调用方持有 c.mu。
func (c *BoundConsumerGroup) terminatingLocked() bool {
	return c.stopping || c.runContext != nil && c.runContext.Err() != nil
}

// boundOutcome 是一条已接纳记录的结局。
type boundOutcome uint8

const (
	boundSettled   boundOutcome = iota + 1 // 已以本代次提交（含先写成死信再提交）
	boundReleased                          // 未提交，已交还 Kafka 重投（owner 仍在运行，代次已结束）
	boundHandedOff                         // owner 终止时未提交，交还 Kafka 给新属主；票据留到 CloseAndReport 核组内已提交位点
	boundHeld                              // 结局未知，已 HOLD 并停止取新
)

// 暂时失败的原地重试退避：1s 起、逐次翻倍、封顶 30s；生产等待带抖动（sleepWithJitter），避免多副本同拍重试。
const (
	boundRetryDelayMin = time.Second
	boundRetryDelayMax = 30 * time.Second
)

// nextBoundRetryDelay 给出下一次退避时长；prev 为 0 表示第一次失败。
func nextBoundRetryDelay(prev time.Duration) time.Duration {
	if prev <= 0 {
		return boundRetryDelayMin
	}
	return min(prev*2, boundRetryDelayMax)
}

// sleepWithJitter 可中断地等待 [d/2, d] 内的随机时长；ctx 结束返回 false。
func sleepWithJitter(ctx context.Context, d time.Duration) bool {
	if half := d / 2; half > 0 {
		d = half + rand.N(half+1)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// boundHandlerPanicPoint 是回调 panic 的 safego 点位名（直接成为 safego_panic_total 的 label，必须是常量）。
const boundHandlerPanicPoint = "kafkautil.bound_consumer.handle"

var errBoundHandlerPanicked = errors.New("Kafka 已接纳记录的处理回调 panic（栈见 goroutine_panic 日志），本次未结清")

// errBoundAssignmentOutOfRange：组内已提交位点早于日志起点（保留期已删掉未消费的消息）或晚于日志终点（topic 重建 / 日志截断）。
// 不是暂时失败：继续读等于静默跳过或读错数据，只能 HOLD 等人工处理。
var errBoundAssignmentOutOfRange = errors.New("Kafka assignment 提交点未知或已越过保留范围")

// errBoundRetentionSkipped：代次中途取到的记录前面有缺口，而日志起点已越过本组应到的位点 —— 中间那段没被本组处理就被保留期删掉了。
// kafka-go 的分区 reader 遇 OffsetOutOfRange 会自己跳到日志起点，（重新）初始化时也会静默改到起点；照常接纳等于静默跳过，只能 HOLD。
var errBoundRetentionSkipped = errors.New("Kafka 保留期删掉了本组还没处理的记录")

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
	group, err := c.newGroup(c.config.Group)
	if err != nil {
		c.fail(err)
		return err
	}
	fetchContext, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.runContext = fetchContext
	c.mu.Unlock()
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
	// 入组失败（协调者不可达、join / sync / 读位点失败、RebalanceInProgress）都是暂时的：kafka-go 的组循环自己带
	// JoinGroupBackoff 重新入组，这里只计数、记日志并有界退避后再等下一代次，不结束 owner（r2-kc#1）。
	var joinDelay time.Duration
	for {
		generation, err := group.Next(fetchContext)
		if err != nil {
			if fetchContext.Err() != nil {
				c.mu.Lock()
				held := c.failure
				c.mu.Unlock()
				return held
			}
			if errors.Is(err, kafkago.ErrGroupClosed) {
				// 组只在本函数退出时关；提前关闭说明所有权被破坏，不能再假装在消费。
				c.fail(err)
				return err
			}
			joinDelay = nextBoundRetryDelay(joinDelay)
			observeBoundRetry(c.config.Group.ID, boundStageJoin)
			logx.Errorf("[kafka-bound] group=%s: waiting for the next generation failed; consumer keeps running, retry in about %v: %v",
				c.config.Group.ID, joinDelay, err)
			c.sleep(fetchContext, joinDelay)
			continue
		}
		joinDelay = 0
		generations.Add(1)
		c.startGeneration(generation, func(generationContext context.Context) {
			defer generations.Done()
			c.runGeneration(fetchContext, generationContext, generation)
		})
	}
}

// openPartition 打开本代次分到的一个分区。暂时失败（leader 不可达、读水位失败）有界退避后重试，直到成功、代次结束或
// owner 终止（后两种返回 nil，没有已接纳记录，不用 HOLD）；提交点越过保留范围不是暂时的，fail 并停止取新。
func (c *BoundConsumerGroup) openPartition(ctx context.Context, topic string, assigned kafkago.PartitionAssignment) *boundPartition {
	var delay time.Duration
	for {
		reader, offset, err := c.openReader(ctx, topic, assigned)
		if err == nil {
			return &boundPartition{topic: topic, partition: assigned.ID, initial: offset, committed: offset, groupCommitted: assigned.Offset >= 0, reader: reader, pending: map[int64]*boundDelivery{}}
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errBoundAssignmentOutOfRange) {
			c.fail(err)
			c.StopFetching()
			return nil
		}
		delay = nextBoundRetryDelay(delay)
		observeBoundRetry(c.config.Group.ID, boundStageOpen)
		logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d: opening the assigned partition failed; retry in about %v: %v",
			c.config.Group.ID, topic, assigned.ID, delay, err)
		if !c.sleep(ctx, delay) {
			return nil
		}
	}
}

// readPartitionBounds 向分区 leader 读日志起止（earliest, end），单次 10s 预算。
func (c *BoundConsumerGroup) readPartitionBounds(ctx context.Context, topic string, partition int) (int64, int64, error) {
	dialer := c.config.Reader.Dialer
	if dialer == nil {
		dialer = kafkago.DefaultDialer
	}
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var connection *kafkago.Conn
	var err error
	for _, broker := range c.config.Group.Brokers {
		connection, err = dialer.DialLeader(budget, "tcp", broker, topic, partition)
		if err == nil {
			break
		}
		if budget.Err() != nil {
			break
		}
	}
	if err != nil || connection == nil {
		return 0, 0, errors.Join(errors.New("Kafka 分区 leader 不可读"), err)
	}
	deadline, _ := budget.Deadline()
	err = connection.SetDeadline(deadline)
	var earliest, end int64
	if err == nil {
		earliest, end, err = connection.ReadOffsets()
	}
	err = errors.Join(err, connection.Close())
	if err != nil {
		return 0, 0, err
	}
	return earliest, end, nil
}

// dialLogStart 是 readLogStart 的生产实现。
func (c *BoundConsumerGroup) dialLogStart(ctx context.Context, topic string, partition int) (int64, error) {
	earliest, _, err := c.readPartitionBounds(ctx, topic, partition)
	return earliest, err
}

// dialPartitionReader 是 openReader 的生产实现：向 leader 读日志起止，核对组内已提交位点仍在保留范围内，再建分区 reader。
func (c *BoundConsumerGroup) dialPartitionReader(ctx context.Context, topic string, assigned kafkago.PartitionAssignment) (boundPartitionReader, int64, error) {
	earliest, end, err := c.readPartitionBounds(ctx, topic, assigned.ID)
	if err != nil {
		return nil, 0, err
	}
	offset := assigned.Offset
	if offset == kafkago.FirstOffset {
		offset = earliest
	}
	if offset < earliest || offset > end || earliest < 0 {
		return nil, 0, fmt.Errorf("%w: %s[%d] offset=%d earliest=%d end=%d", errBoundAssignmentOutOfRange, topic, assigned.ID, offset, earliest, end)
	}
	config := c.config.Reader
	config.Topic = topic
	config.Partition = assigned.ID
	reader := kafkago.NewReader(config)
	if err = reader.SetOffset(offset); err != nil {
		return nil, 0, errors.Join(err, reader.Close())
	}
	return reader, offset, nil
}

// runGeneration 拥有一个代次的全部分区 worker；owner 是 Run 的生命周期（StopFetching 或 ctx 结束即取消）。
func (c *BoundConsumerGroup) runGeneration(owner, generationContext context.Context, generation *kafkago.Generation) {
	fetchContext, cancel := context.WithCancel(owner)
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
			c.appendHistoryLocked(c.observationLocked("SQL_DRAIN_KAFKA_GENERATION_CLOSED_V1"))
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
			key := boundPartitionKey(topic, assignment.ID)
			if assignment.ID < 0 {
				c.fail(errors.New("Kafka assignment 分区为负"))
				c.StopFetching()
				return
			}
			if _, exists := partitions[key]; exists {
				c.fail(errors.New("Kafka 实际 assignment 重复"))
				c.StopFetching()
				return
			}
			partition := c.openPartition(fetchContext, topic, assignment)
			if partition == nil {
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
	resolved, lost := c.resolveReplayPassedLocked(generation)
	c.signalLocked()
	c.mu.Unlock()
	if lost != nil {
		// 先记 HOLD 再结束丢失票据，排空方不会在两步之间看到「没有在途、也没有未知」。
		c.fail(lost)
		c.StopFetching()
	}
	for _, finish := range resolved {
		finish()
	}
	if lost != nil {
		return
	}
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
	var fetchDelay time.Duration
	// expected 是下一条应到的 offset：reader 从 initial 起读，之后每取到一条就推进到它的下一位（缺口核对见 admitGap）。
	expected := partition.initial
	for {
		message, err := partition.reader.FetchMessage(fetchContext)
		if err != nil {
			if fetchContext.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) {
				// reader 只在本代次全部 worker 返回之后才关；worker 还在却读到 EOF，说明所有权被破坏。
				c.fail(errors.New("Kafka 分区 reader 在 worker 退出前被关闭"))
				c.StopFetching()
				return
			}
			// 分区 reader 在后台自己重连 leader，这里报出的是它这一轮的失败：退避后接着取，不结束 owner。
			fetchDelay = nextBoundRetryDelay(fetchDelay)
			observeBoundRetry(c.config.Group.ID, boundStageFetch)
			logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d: fetch failed; retry in about %v: %v",
				c.config.Group.ID, partition.topic, partition.partition, fetchDelay, err)
			if !c.sleep(fetchContext, fetchDelay) {
				return
			}
			continue
		}
		fetchDelay = 0
		if message.Offset < expected {
			// reader 只会顺序前进；回退说明它被别处重新定位过，接纳就会重复处理或把提交位点往回拨。
			c.fail(fmt.Errorf("Kafka 分区 reader 回退：%s[%d] 应到 %d，取到 %d", partition.topic, partition.partition, expected, message.Offset))
			c.StopFetching()
			return
		}
		if message.Offset > expected && !c.admitGap(fetchContext, partition, expected, message.Offset) {
			return
		}
		expected = message.Offset + 1
		if c.manual {
			delivery := &boundDelivery{generation: generation, partition: partition, message: message}
			select {
			case c.messages <- delivery:
			case <-fetchContext.Done():
				return
			}
			continue
		}
		// 代次已结束时拿到的记录不接纳：它不属于下一代次的本 owner，组内已提交位点之后会重投给新属主。
		// 只是 StopFetching 而代次仍在时照常接纳——Fetch 已返回即归本 owner，与停止竞争时也不能丢弃已拿到的记录。
		if generationContext.Err() != nil {
			return
		}
		finish, err := c.relay.Enter()
		if err != nil {
			c.fail(err)
			c.StopFetching()
			return
		}
		c.mu.Lock()
		partition.accepted++
		c.mu.Unlock()
		if c.settleGuarded(fetchContext, generation, partition, message, finish) != boundSettled {
			return
		}
	}
}

// settleGuarded 结清一条已接纳记录并处置它的票据：交还给新属主（boundHandedOff）的票据留到 CloseAndReport 核结局，
// 其余结局在这里结束票据。结清过程 panic 记 HOLD；先记 HOLD 再结束票据（defer 逆序），排空方不会在两步之间看到「没有在途、也没有未知」。
func (c *BoundConsumerGroup) settleGuarded(fetchContext context.Context, generation *kafkago.Generation, partition *boundPartition, message kafkago.Message, finish func()) (outcome boundOutcome) {
	keepTicket := false
	defer func() {
		if !keepTicket {
			finish()
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			c.fail(errors.New("Kafka 已接纳记录结清过程 panic，结局未知"))
			c.StopFetching()
			outcome = boundHeld
		}
	}()
	outcome = c.settleAccepted(fetchContext, generation, partition, message)
	if outcome == boundHandedOff {
		c.mu.Lock()
		c.handoffs = append(c.handoffs, &boundHandoff{topic: partition.topic, partition: partition.partition, offset: message.Offset, generation: generation.ID, finish: finish})
		c.signalLocked()
		c.mu.Unlock()
		keepTicket = true
	}
	return outcome
}

// settleAccepted 把一条已接纳记录推进到确定结局：
//   - boundSettled：回调成功（或已写成死信）且以本代次提交成功。代次的 ctx 已结束也照常提交：eager 再均衡准备期 broker 仍接受
//     旧代次的提交，本代次的协调者连接要等全部 Start 回调返回才关；真被拒（IllegalGeneration、UnknownMemberId 等）就按下面处理。
//   - boundReleased：没结清，owner 仍在运行而本代次已不再消费。不提交，交还 Kafka，由下一代次的属主从组内已提交位点重投。
//   - boundHandedOff：没结清而 owner 已终止。不再重试、不提交，交还给新属主；票据由调用方留到 CloseAndReport 核结局。
//   - boundHeld：记录与代次不符。fail（HOLD）并停止取新。
//
// 代次还在、owner 也没终止时，失败按有界指数退避原地重试同一条：回调成功过（或死信写成功过）就只重试提交，不再重复调用；
// 退避被代次结束或 owner 终止打断时不再调用回调、不再写死信（新属主可能已在处理同一条）。
// 没打开死信时回调失败不设次数上限：跳过这一条等于丢结果（match 结果消费的取舍：保留位点、告警、人工处置，负责人 2026-11-06 定）。
// 打开了死信时，回调失败（返回错误、超过 HandlerTimeout、panic）累计到 MaxAttempts 次，或回调返回 ErrUnprocessable，就写死信；
// 死信写成功才提交越过它，写失败照样退避重试写，不越过、不跳过。每次调用有 HandlerTimeout，退避封顶 30s 且带抖动。
func (c *BoundConsumerGroup) settleAccepted(fetchContext context.Context, generation *kafkago.Generation, partition *boundPartition, message kafkago.Message) boundOutcome {
	if message.Topic != partition.topic || message.Partition != partition.partition || message.Offset < partition.committed {
		c.fail(errors.New("Kafka reader 返回分区或 offset 与代次不符"))
		c.StopFetching()
		return boundHeld
	}
	offsets := map[string]map[int]int64{partition.topic: {partition.partition: message.Offset + 1}}
	var (
		handled      bool  // 回调成功，或死信已写成功：只差提交
		deadLettered bool  // handled 是因为死信写成功
		attempts     int   // 回调失败的次数（返回错误、超时、panic）
		handlerErr   error // 最后一次回调失败的原因，写进死信原件
		interrupted  bool  // 上一次退避被代次结束或 owner 终止打断
		delay        time.Duration
	)
	for {
		var cause error
		stage := boundStageCommit
		if !handled {
			if interrupted {
				// 退避被打断之后不再调用回调、不再写死信：代次已结束时新属主可能已在处理同一条，owner 终止后本 owner 不再重试。
				if outcome, done := c.unsettled(fetchContext, partition, message, handlerErr); done {
					return outcome
				}
				interrupted = false
			}
			if c.deadLetterDue(attempts, handlerErr) {
				stage = boundStageDeadLetter
				if cause = c.writeDeadLetter(fetchContext, partition, message, attempts, handlerErr); cause == nil {
					handled, deadLettered = true, true
				}
			} else {
				stage = boundStageHandle
				if cause = c.invokeHandler(fetchContext, message); cause == nil {
					handled = true
				} else {
					attempts++
					handlerErr = cause
				}
			}
		}
		if handled {
			stage = boundStageCommit
			if cause = c.commitOffsets(generation, offsets); cause == nil {
				c.mu.Lock()
				partition.committed = message.Offset + 1
				partition.settled++
				if deadLettered {
					partition.deadLettered++
				}
				c.signalLocked()
				c.mu.Unlock()
				return boundSettled
			}
		}
		if outcome, done := c.unsettled(fetchContext, partition, message, cause); done {
			return outcome
		}
		if stage == boundStageHandle && c.deadLetterDue(attempts, handlerErr) {
			// 预算刚用完，或回调判成无法处理：下一轮直接写死信，不再白等一轮退避。
			continue
		}
		delay = nextBoundRetryDelay(delay)
		observeBoundRetry(c.config.Group.ID, stage)
		logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d offset=%d: %s failed (handler attempts so far: %d); offset not committed, retrying the same record in about %v: %v",
			c.config.Group.ID, partition.topic, partition.partition, message.Offset, stage, attempts, delay, cause)
		interrupted = !c.sleep(fetchContext, delay)
	}
}

// unsettled 判定一条没结清的已接纳记录：owner 终止 → 交还给新属主（handedOff），本 owner 不再重试；
// 本代次已不再消费（fetchContext 结束：代次结束）→ 交还重投（released）；否则 done=false，由调用方退避重试。
// 交还给新属主不记 HOLD：没提交就是确定结局，组内已提交位点停在它前面，owner 离组后新属主从那里重投（回调幂等）；
// 新属主结清了它，CloseAndReport 从组内已提交位点核到，计入同一次排空（bound_consumer_tickets.go）。
func (c *BoundConsumerGroup) unsettled(fetchContext context.Context, partition *boundPartition, message kafkago.Message, cause error) (boundOutcome, bool) {
	c.mu.Lock()
	terminating := c.terminatingLocked()
	if !terminating && fetchContext.Err() == nil {
		c.mu.Unlock()
		return 0, false
	}
	partition.released++
	outcome := boundReleased
	if terminating {
		partition.handedOff++
		outcome = boundHandedOff
	}
	c.signalLocked()
	c.mu.Unlock()
	observeBoundReleased(c.config.Group.ID, 1)
	if outcome == boundHandedOff {
		logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d offset=%d: consumer stopping before the record was committed; handed back to Kafka for the next owner, the drain receipt waits for the group's committed offset to pass it: %v",
			c.config.Group.ID, partition.topic, partition.partition, message.Offset, cause)
	} else {
		logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d offset=%d: generation ended before the record was committed; returned to Kafka for redelivery: %v",
			c.config.Group.ID, partition.topic, partition.partition, message.Offset, cause)
	}
	return outcome, true
}

// invokeHandler 调一次回调：单次 HandlerTimeout，不继承代次结束、StopFetching 与 Run ctx 的取消（进行中的事务不被打断）。
// panic 由 safego 记栈与计数后折成本次失败，与回调返回错误同样处理（不提交，按结局判定）。
func (c *BoundConsumerGroup) invokeHandler(fetchContext context.Context, message kafkago.Message) error {
	work, cancel := context.WithTimeout(context.WithoutCancel(fetchContext), c.config.HandlerTimeout)
	defer cancel()
	var err error
	if !safego.Run(boundHandlerPanicPoint, func() { err = c.handler(work, message) }) {
		return errBoundHandlerPanicked
	}
	if err != nil {
		return err
	}
	return work.Err()
}

// CloseAndReport 只在全部实际 worker 返回、reader 和 consumer group 关闭后给出原件。
// 超时或未知结局返回 HOLD，不能用 cancel 或空本地队列伪造 settled。
// owner 离组之后，先按组内已提交位点核一次终止时交还给新属主的记录与漂走的恢复票据（confirmOutstandingTickets）：
// 新属主提交越过了的结清、计入本次收据，预算内核不到的才 HOLD。
func (c *BoundConsumerGroup) CloseAndReport(ctx context.Context) ([]byte, error) {
	c.StopFetching()
	select {
	case <-ctx.Done():
		return nil, errors.Join(drainproof.ErrHold, ctx.Err())
	case <-c.done:
	}
	c.confirmOutstandingTickets(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.complete || c.failure != nil {
		return nil, errors.Join(drainproof.ErrHold, c.failure)
	}
	// 每条已接纳记录都要有确定结局：本 owner 提交（Settled，含先写成死信的）、确定未提交后交同 owner 重放（Replayed），
	// 或交还 Kafka 重投（Released：代次结束时由下一代次重投；owner 终止时交给新属主，收尾已核到新属主提交越过它）。
	for _, generation := range c.history {
		for _, partition := range generation.Partitions {
			if partition.Accepted != partition.Settled+partition.Replayed+partition.Released {
				return nil, drainproof.ErrHold
			}
		}
	}
	if compacted := c.compacted; compacted.Accepted != compacted.Settled+compacted.Replayed+compacted.Released {
		return nil, drainproof.ErrHold
	}
	if len(c.replay) != 0 || len(c.handoffs) != 0 {
		return nil, drainproof.ErrHold
	}
	return json.Marshal(struct {
		Schema           string
		Group            string
		Started          bool
		Generations      []*boundGroupObservation
		ResolvedReplay   []boundReplayObservation
		Compacted        boundHistoryCompaction
		CompactedReplay  boundReplayCompaction
		ResolvedHandoffs []boundHandoffObservation
	}{"SQL_DRAIN_KAFKA_OWNER_CLOSED_V1", c.config.Group.ID, c.started, c.history, c.resolvedReplay, c.compacted, c.replayCompacted, c.resolvedHandoffs})
}

// boundHistoryWindow 是逐代保留原件的代次数；更早的代次滚出窗口、折叠进 compacted（只累加计数）。
// owner 跨越本进程的全部再均衡（r2-kc#1），不能再用「代次数超限即 HOLD」来限内存。
const boundHistoryWindow = 1024

// boundHistoryCompaction 汇总滚出窗口的已关闭代次。HandedOff ⊆ Released、DeadLettered ⊆ Settled，只作原件，不参与结局平衡。
type boundHistoryCompaction struct {
	Generations     uint64
	FirstGeneration int32
	LastGeneration  int32
	Accepted        uint64
	Settled         uint64
	Replayed        uint64
	Released        uint64
	HandedOff       uint64
	DeadLettered    uint64
}

// fold 把一个已关闭代次累加进汇总；该代次有分区结局不平时返回 false，由调用方 HOLD，不把不平藏进合计。
func (s *boundHistoryCompaction) fold(observation *boundGroupObservation) bool {
	if s.Generations == 0 {
		s.FirstGeneration = observation.Generation
	}
	s.Generations++
	s.LastGeneration = observation.Generation
	balanced := true
	for _, partition := range observation.Partitions {
		s.Accepted += partition.Accepted
		s.Settled += partition.Settled
		s.Replayed += partition.Replayed
		s.Released += partition.Released
		s.HandedOff += partition.HandedOff
		s.DeadLettered += partition.DeadLettered
		if partition.Accepted != partition.Settled+partition.Replayed+partition.Released {
			balanced = false
		}
	}
	return balanced
}

// appendHistoryLocked 记一个已关闭代次的原件；窗口满时最老的一代折叠进 compacted。调用方持有 c.mu。
func (c *BoundConsumerGroup) appendHistoryLocked(observation *boundGroupObservation) {
	if len(c.history) >= boundHistoryWindow {
		oldest := c.history[0]
		copy(c.history, c.history[1:])
		c.history = c.history[:len(c.history)-1]
		if !c.compacted.fold(oldest) {
			// 锁序与 FetchMessage 相同（c.mu → relay）。
			c.failure = errors.Join(c.failure, drainproof.ErrHold, errors.New("Kafka 滚出窗口的代次原件结局不平"))
			c.relay.Hold()
		}
	}
	c.history = append(c.history, observation)
}

// 以下原件只记录 Kafka API 实际返回值；分区的历史身份与 floor 仍引用唯一正式 proto。
// 结局平衡只看 Accepted = Settled + Replayed + Released；HandedOff ⊆ Released、DeadLettered ⊆ Settled。
type boundPartitionObservation struct {
	Topic        string
	Partition    int
	Initial      int64
	Committed    int64
	Accepted     uint64
	Settled      uint64
	Replayed     uint64
	Released     uint64
	HandedOff    uint64
	DeadLettered uint64
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
		report.Partitions = append(report.Partitions, boundPartitionObservation{Topic: partition.topic, Partition: partition.partition, Initial: partition.initial, Committed: partition.committed, Accepted: partition.accepted, Settled: partition.settled, Replayed: partition.replayed, Released: partition.released, HandedOff: partition.handedOff, DeadLettered: partition.deadLettered})
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
// 调用方仍须只调用一次 Run。FetchMessage 返回才接纳，CommitMessages 表示该分区已持久前缀；
// 它返回 *UncommittedError 时这批已交还 Kafka 重投（owner 仍在运行时下一代次重投，终止时交给新属主），
// 调用方摘掉在途记录、继续消费，不当成未知结局。
// 不支持死信：坏消息由批处理 owner 自己按业务口径处置（data_service 跳过并提交、记日志与计数）。
func NewBoundMessageReader(config BoundConsumerGroupConfig, relay *drainproof.Session) (*BoundConsumerGroup, error) {
	if config.DeadLetter != nil {
		return nil, errors.New("Kafka reader 模式不支持死信")
	}
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
//
// 代次的 ctx 已结束（再均衡）也照常用这个代次提交：本代次要等这些已交付记录结清才关（waitDelivered），
// 协调者连接仍在，eager 再均衡准备期 broker 仍接受旧代次的提交。提交成功就是确定结局，调用方 ctx 到点不改写它。
// 有界重试后仍失败时，这次覆盖的已交付记录不提交、交还 Kafka，返回 *UncommittedError（落库幂等，调用方只记日志、继续消费），
// 见 releaseUncommitted：owner 仍在运行就结束本代次、让下一代次立即重投；owner 已终止就交给新属主，收尾核结局。
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
	generationContext := c.generationContext
	if generation == nil || generationContext == nil || c.failure != nil {
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
	if err := c.commitWithRetry(ctx, generationContext, generation, offsets); err != nil {
		return c.releaseUncommitted(generation, offsets, err)
	}
	c.mu.Lock()
	if c.generation != generation {
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
						c.appendResolvedReplayLocked(boundReplayObservation{Topic: topic, Partition: id, Offset: offset, FirstGeneration: delivery.originGeneration, CommittedGeneration: generation.ID, PayloadDigest: delivery.payloadDigest[:], Resolution: boundReplayCommitted})
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

// 批提交的有界重试：含第一次共 boundCommitAttempts 次，间隔 boundCommitRetryDelay 起翻倍（持 commitMu，所以要短）。
// 只在代次仍在时重试：代次已结束的提交被拒不会因重试变好。
const (
	boundCommitAttempts   = 3
	boundCommitRetryDelay = 200 * time.Millisecond
)

func (c *BoundConsumerGroup) commitWithRetry(ctx, generationContext context.Context, generation *kafkago.Generation, offsets map[string]map[int]int64) error {
	delay := boundCommitRetryDelay
	for attempt := 1; ; attempt++ {
		err := c.commitOffsets(generation, offsets)
		if err == nil || attempt == boundCommitAttempts || generationContext.Err() != nil || c.terminating() {
			return err
		}
		observeBoundRetry(c.config.Group.ID, boundStageCommit)
		logx.Errorf("[kafka-bound] group=%s generation=%d: commit failed (attempt %d/%d); retry in about %v: %v",
			c.config.Group.ID, generation.ID, attempt, boundCommitAttempts, delay, err)
		if !c.sleep(ctx, delay) {
			return err
		}
		delay *= 2
	}
}

// ErrUncommitted 表示一批已交付记录没有提交、已交还 Kafka 重投（提交失败，常见于再均衡或排空时逐个离组）。
// 落库幂等的调用方只记日志、继续消费；不能把它当成落库失败，也不能据此 HOLD。
var ErrUncommitted = errors.New("Kafka 已交付记录没有提交，已交还重投")

// UncommittedError 列出被交还的记录（只含 T/P/O，按 T/P/O 排序）。errors.Is(err, ErrUncommitted) 与 errors.Is(err, 原因) 都成立。
type UncommittedError struct {
	Released []kafkago.Message
	Cause    error
}

func (e *UncommittedError) Error() string {
	return fmt.Sprintf("%v（%d 条）: %v", ErrUncommitted, len(e.Released), e.Cause)
}

func (e *UncommittedError) Unwrap() []error { return []error{ErrUncommitted, e.Cause} }

// releaseUncommitted 把一次失败提交覆盖的已交付记录交还 Kafka：不推进本地前沿，计入 released，按 owner 状态分两种：
//   - owner 仍在运行：结束这些记录的票据，并结束本代次（与 ReportProcessingFailure 同一做法）。交还的记录已落库却没提交，
//     本代次不会再投它们，安静分区上它们会一直挂着积压，直到同分区来了新消息或下一代次才被提交；结束本代次让下一代次从组内
//     已提交位点立即重投（落库幂等），积压不靠新消息到达才消失，提交停滞告警也就不必等新消息（第 7 条）。
//   - owner 已终止（排空时逐个离组，提交被拒）：交给新属主（handedOff），票据留到 CloseAndReport 核到组内已提交位点越过它们（第 1 条）。
func (c *BoundConsumerGroup) releaseUncommitted(generation *kafkago.Generation, offsets map[string]map[int]int64, cause error) error {
	released := &UncommittedError{Cause: cause}
	var finished []func()
	c.mu.Lock()
	if c.generation != generation {
		c.mu.Unlock()
		c.fail(errors.Join(drainproof.ErrHold, cause))
		c.StopFetching()
		return drainproof.ErrHold
	}
	handoff := c.terminatingLocked()
	for topic, partitions := range offsets {
		for id, next := range partitions {
			partition := c.partitions[boundPartitionKey(topic, id)]
			if partition == nil {
				continue
			}
			for offset, delivery := range partition.pending {
				if offset < next && delivery.generation == generation {
					delete(partition.pending, offset)
					partition.released++
					released.Released = append(released.Released, kafkago.Message{Topic: topic, Partition: id, Offset: offset})
					if handoff {
						partition.handedOff++
						c.handoffs = append(c.handoffs, &boundHandoff{topic: topic, partition: id, offset: offset, generation: generation.ID, finish: delivery.finish})
					} else {
						finished = append(finished, delivery.finish)
					}
				}
			}
		}
	}
	var restart context.CancelFunc
	if !handoff {
		c.initialized = false
		restart = c.generationCancel
	}
	c.signalLocked()
	c.mu.Unlock()
	if restart != nil {
		restart()
	}
	for _, finish := range finished {
		finish()
	}
	sort.Slice(released.Released, func(i, j int) bool {
		a, b := released.Released[i], released.Released[j]
		if a.Topic != b.Topic {
			return a.Topic < b.Topic
		}
		if a.Partition != b.Partition {
			return a.Partition < b.Partition
		}
		return a.Offset < b.Offset
	})
	observeBoundReleased(c.config.Group.ID, len(released.Released))
	if handoff {
		logx.Errorf("[kafka-bound] group=%s generation=%d: commit failed while the consumer is stopping; %d delivered record(s) handed back to Kafka for the next owner, the drain receipt waits for the group's committed offset to pass them: %v",
			c.config.Group.ID, generation.ID, len(released.Released), cause)
	} else {
		logx.Errorf("[kafka-bound] group=%s generation=%d: commit failed; %d delivered record(s) returned to Kafka uncommitted, ending this generation so the next one redelivers them now (consumer keeps running): %v",
			c.config.Group.ID, generation.ID, len(released.Released), cause)
	}
	return released
}

// 确定性恢复票据的结清方式。
const (
	boundReplayCommitted             = "COMMITTED"                     // 原 T/P/O 重新交付给本 owner 并以新代次提交
	boundReplayGroupCommittedPast    = "GROUP_COMMITTED_PAST"          // 分区回到本 owner 时组内已提交位点已越过它
	boundReplayGroupCommittedAtClose = "GROUP_COMMITTED_PAST_AT_CLOSE" // 分区没回来，收尾核到组内已提交位点越过它（别的副本处理并提交了）
)

// boundReplayWindow 是逐条保留的已结清恢复票据原件数；更早的滚出窗口，折叠进 replayCompacted（计数 + 链式摘要）。
const boundReplayWindow = 16384

// boundReplayCompaction 汇总滚出窗口的已结清恢复票据：Digest = SHA-256(前一 Digest ‖ 条目定长编码)。
type boundReplayCompaction struct {
	Count  uint64
	Digest []byte
}

func (s *boundReplayCompaction) fold(observation boundReplayObservation) {
	hash := sha256.New()
	var value [8]byte
	write := func(raw []byte) {
		binary.BigEndian.PutUint64(value[:], uint64(len(raw)))
		hash.Write(value[:])
		hash.Write(raw)
	}
	write(s.Digest)
	write([]byte(observation.Topic))
	for _, n := range []int64{int64(observation.Partition), observation.Offset, int64(observation.FirstGeneration), int64(observation.CommittedGeneration)} {
		binary.BigEndian.PutUint64(value[:], uint64(n))
		hash.Write(value[:])
	}
	write(observation.PayloadDigest)
	write([]byte(observation.Resolution))
	s.Digest = hash.Sum(nil)
	s.Count++
}

// appendResolvedReplayLocked 记一条已结清的恢复票据原件；窗口满时最老的一条折叠进 replayCompacted。调用方持有 c.mu。
func (c *BoundConsumerGroup) appendResolvedReplayLocked(observation boundReplayObservation) {
	if len(c.resolvedReplay) >= boundReplayWindow {
		c.replayCompacted.fold(c.resolvedReplay[0])
		copy(c.resolvedReplay, c.resolvedReplay[1:])
		c.resolvedReplay = c.resolvedReplay[:len(c.resolvedReplay)-1]
	}
	c.resolvedReplay = append(c.resolvedReplay, observation)
}

// errBoundReplayLost：确定性恢复票据所在分区回到本 owner 时，组在该分区从没提交过，日志起点却已越过票据——
// 这条没被任何属主提交就被保留期删掉了。按 GROUP_COMMITTED_PAST 结清等于把丢失记成已处理，只能 HOLD。
var errBoundReplayLost = errors.New("Kafka 确定性恢复票据没有任何提交就被保留期删掉")

// resolveReplayPassedLocked 结清组内已提交位点越过的确定性恢复票据：本代次分到的分区从 initial 起读，早于它的票据已由组内某个属主提交
// （位点只在处理之后才推进），不会再交付给本 owner。不结清，它会永久悬着，之后同分区更高位点的提交还会被
// 「越过尚未恢复的已交付前缀」拒绝。分区不在本代次的票据原样保留：分区漂到别的副本、不再回来时，由 CloseAndReport
// 在 owner 离组之后读组内已提交位点核一次（bound_consumer_tickets.go），核不到才 HOLD。
// 只有 initial 确是组内已提交位点（groupCommitted）才能这样结清；组在该分区从没提交过时 initial 只是日志起点，
// 越过票据说明它没被处理就被保留期删掉：不记结清，返回 lost，由调用方先 HOLD 再结束票据（同 waitDelivered 的失败分支）。
// 调用方持有 c.mu，且 c.partitions 已是本代次；返回要在锁外调用的票据结束函数。
func (c *BoundConsumerGroup) resolveReplayPassedLocked(generation *kafkago.Generation) (finished []func(), lost error) {
	for key, delivery := range c.replay {
		partition := c.partitions[boundPartitionKey(delivery.message.Topic, delivery.message.Partition)]
		if partition == nil || delivery.message.Offset >= partition.initial {
			continue
		}
		delete(c.replay, key)
		finished = append(finished, delivery.finish)
		if !partition.groupCommitted {
			lost = errors.Join(lost, fmt.Errorf("%w: %s[%d]@%d，组在该分区没有已提交位点，日志起点已是 %d", errBoundReplayLost, delivery.message.Topic, delivery.message.Partition, delivery.message.Offset, partition.initial))
			continue
		}
		c.appendResolvedReplayLocked(boundReplayObservation{Topic: delivery.message.Topic, Partition: delivery.message.Partition, Offset: delivery.message.Offset, FirstGeneration: delivery.originGeneration, CommittedGeneration: generation.ID, PayloadDigest: delivery.payloadDigest[:], Resolution: boundReplayGroupCommittedPast})
	}
	return finished, lost
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
	// Resolution 取 boundReplayCommitted / boundReplayGroupCommittedPast / boundReplayGroupCommittedAtClose
	// （最后一种没有新代次，CommittedGeneration 记 0）。
	Resolution string
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
	// 只限尚未结清的票据；已结清的原件是滚动窗口（appendResolvedReplayLocked），长寿 owner 不会因此 HOLD。
	if len(c.replay)+len(selected) > 16384 {
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
