package kafkautil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	kafkago "github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/drainproof"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

// groupOffsetFake 是 groupOffsetReader 的假实现:按分区给 committed / start / end,并记下调用顺序。
type groupOffsetFake struct {
	committed map[int32]int64 // 分区 → 已提交 offset(-1 = 没提交过);不在 map 里 = 响应里没有这个 block
	blockErr  map[int32]sarama.KError
	start     map[int32]int64
	end       map[int32]int64

	topErr    sarama.KError // 响应顶层 Err(OffsetFetch v2+)
	listErr   error         // ListConsumerGroupOffsets 调用本身失败
	offsetErr error         // GetOffset 调用失败

	calls []string
}

func (f *groupOffsetFake) ListConsumerGroupOffsets(group string, tps map[string][]int32) (*sarama.OffsetFetchResponse, error) {
	f.calls = append(f.calls, "list")
	if f.listErr != nil {
		return nil, f.listErr
	}
	resp := &sarama.OffsetFetchResponse{Err: f.topErr, Blocks: map[string]map[int32]*sarama.OffsetFetchResponseBlock{}}
	for topic, partitions := range tps {
		resp.Blocks[topic] = map[int32]*sarama.OffsetFetchResponseBlock{}
		for _, p := range partitions {
			c, ok := f.committed[p]
			if !ok {
				continue
			}
			resp.Blocks[topic][p] = &sarama.OffsetFetchResponseBlock{Offset: c, Err: f.blockErr[p]}
		}
	}
	return resp, nil
}

func (f *groupOffsetFake) GetOffset(_ string, partition int32, at int64) (int64, error) {
	f.calls = append(f.calls, fmt.Sprintf("offset:%d", partition))
	if f.offsetErr != nil {
		return 0, f.offsetErr
	}
	switch at {
	case sarama.OffsetNewest:
		return f.end[partition], nil
	case sarama.OffsetOldest:
		return f.start[partition], nil
	}
	return 0, fmt.Errorf("unexpected offset query %d", at)
}

// TestComputeGroupLag 覆盖 lag 探针的算法与 fail-closed 口径:
//   - 正常 / 没提交过(-1,从日志起点算)/ 已提交位点早于日志起点(保留期删掉了未消费的消息 = RetentionGap);
//   - MaxPartitionLag 是单分区最大值而不是合计(broker 的 retention.bytes 按分区执行,积压告警比的是它);
//   - committed > end(topic 被删了重建)、某分区 block 带错 / 缺失、响应顶层 Err、调用本身失败 —— 一律报错,不静默当 0;
//   - 调用顺序:先读组的已提交 offset,再读任何分区的 end / start。反过来时消费者在两次读之间提交一次
//     就会得到 committed > end,繁忙 topic 上探针会周期性误报。
func TestComputeGroupLag(t *testing.T) {
	const group, topic = "g", "player_snapshot_topic_g1"

	t.Run("正常", func(t *testing.T) {
		f := &groupOffsetFake{
			committed: map[int32]int64{0: 10, 1: 20, 2: 30},
			start:     map[int32]int64{0: 0, 1: 0, 2: 0},
			end:       map[int32]int64{0: 15, 1: 20, 2: 40},
		}
		got, err := computeGroupLag(f, group, topic, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (GroupLag{Lag: 15, MaxPartitionLag: 10}) {
			t.Fatalf("got %+v, want Lag=15 (5+0+10), MaxPartitionLag=10, RetentionGap=0, Uncommitted=0", got)
		}
		if len(f.calls) == 0 || f.calls[0] != "list" {
			t.Fatalf("committed offsets must be read before any partition offset, calls=%v", f.calls)
		}
		for _, c := range f.calls[1:] {
			if c == "list" {
				t.Fatalf("committed offsets must be read exactly once, first, calls=%v", f.calls)
			}
		}
	})

	t.Run("没提交过的分区从日志起点算", func(t *testing.T) {
		f := &groupOffsetFake{
			committed: map[int32]int64{0: -1, 1: 8},
			start:     map[int32]int64{0: 5, 1: 0},
			end:       map[int32]int64{0: 12, 1: 10},
		}
		got, err := computeGroupLag(f, group, topic, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (GroupLag{Lag: 7 + 2, MaxPartitionLag: 7, Uncommitted: 1}) {
			t.Fatalf("got %+v, want Lag=9 MaxPartitionLag=7 (the uncommitted partition counts too) Uncommitted=1 RetentionGap=0", got)
		}
	})

	t.Run("积压集中在一个分区时 MaxPartitionLag 取单分区最大值", func(t *testing.T) {
		f := &groupOffsetFake{
			committed: map[int32]int64{0: 100, 1: 100, 2: 0},
			start:     map[int32]int64{0: 0, 1: 0, 2: 0},
			end:       map[int32]int64{0: 100, 1: 101, 2: 1750},
		}
		got, err := computeGroupLag(f, group, topic, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (GroupLag{Lag: 1751, MaxPartitionLag: 1750}) {
			t.Fatalf("got %+v, want Lag=1751 (0+1+1750) MaxPartitionLag=1750 (retention is enforced per partition)", got)
		}
	})

	t.Run("已提交位点早于日志起点 = 保留期已删掉未消费的消息", func(t *testing.T) {
		f := &groupOffsetFake{
			committed: map[int32]int64{0: 3},
			start:     map[int32]int64{0: 10},
			end:       map[int32]int64{0: 25},
		}
		got, err := computeGroupLag(f, group, topic, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (GroupLag{Lag: 15, MaxPartitionLag: 15, RetentionGap: 7}) {
			t.Fatalf("got %+v, want Lag=15 MaxPartitionLag=15 (end-start) RetentionGap=7 (start-committed)", got)
		}
	})

	t.Run("committed 越过 end 必须报错", func(t *testing.T) {
		f := &groupOffsetFake{
			committed: map[int32]int64{0: 50, 1: 5},
			start:     map[int32]int64{0: 0, 1: 0},
			end:       map[int32]int64{0: 20, 1: 9},
		}
		got, err := computeGroupLag(f, group, topic, 2)
		if err == nil || !strings.Contains(err.Error(), "beyond the log end") {
			t.Fatalf("committed > end must be reported, got err=%v", err)
		}
		if got.Lag != 4 || got.MaxPartitionLag != 4 {
			t.Fatalf("the recreated partition counts 0 lag, the healthy one still counts: got %+v", got)
		}
	})

	for _, tc := range []struct {
		name string
		f    *groupOffsetFake
		n    int32
	}{
		{"分区数非法", &groupOffsetFake{}, 0},
		{"block 带错", &groupOffsetFake{
			committed: map[int32]int64{0: 1},
			blockErr:  map[int32]sarama.KError{0: sarama.ErrUnknownTopicOrPartition},
			start:     map[int32]int64{0: 0},
			end:       map[int32]int64{0: 1},
		}, 1},
		{"block 缺失", &groupOffsetFake{
			committed: map[int32]int64{},
			start:     map[int32]int64{0: 0},
			end:       map[int32]int64{0: 1},
		}, 1},
		{"响应顶层 Err", &groupOffsetFake{
			committed: map[int32]int64{0: 1},
			topErr:    sarama.ErrNotCoordinatorForConsumer,
			start:     map[int32]int64{0: 0},
			end:       map[int32]int64{0: 1},
		}, 1},
		{"ListConsumerGroupOffsets 失败", &groupOffsetFake{listErr: errors.New("broker down")}, 1},
		{"GetOffset 失败", &groupOffsetFake{
			committed: map[int32]int64{0: 1},
			offsetErr: errors.New("leader not available"),
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := computeGroupLag(tc.f, group, topic, tc.n); err == nil {
				t.Fatal("must return an error, not a silent zero")
			}
		})
	}
}

// committedOffsetsFunc 把一个函数适配成 CommittedOffsetsReader,用来造 groupOffsetFake 造不出的响应(nil 响应)。
type committedOffsetsFunc func(group string, tps map[string][]int32) (*sarama.OffsetFetchResponse, error)

func (f committedOffsetsFunc) ListConsumerGroupOffsets(group string, tps map[string][]int32) (*sarama.OffsetFetchResponse, error) {
	return f(group, tps)
}

// TestCommittedOffsets 钉住抽出来的位点读取(lag 探针与 go/db 交接闸共用)的错误语义:
//   - -1(没提交过)原样返回,不当成错误,也不改写成 0;
//   - 顶层 ErrGroupIDNotFound 用 %w 包住,errors.Is 可识别(交接闸据此把「组不存在」当成全部 -1);
//   - nil 响应、block 缺失、block 带错、调用本身失败,一律返回 error。
func TestCommittedOffsets(t *testing.T) {
	const group, topic = "db_rpc_consumer_group_z1", "db_task_zone_1"

	t.Run("正常与 -1 原样返回", func(t *testing.T) {
		f := &groupOffsetFake{committed: map[int32]int64{0: 42, 1: -1, 2: 0}}
		got, err := CommittedOffsets(f, group, topic, []int32{0, 1, 2})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[int32]int64{0: 42, 1: -1, 2: 0}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for p, o := range want {
			if got[p] != o {
				t.Fatalf("partition %d = %d, want %d (got %v)", p, got[p], o, got)
			}
		}
	})

	t.Run("只查请求的分区", func(t *testing.T) {
		f := &groupOffsetFake{committed: map[int32]int64{0: 1, 3: 7}}
		got, err := CommittedOffsets(f, group, topic, []int32{3})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[3] != 7 {
			t.Fatalf("got %v, want only partition 3 = 7", got)
		}
	})

	t.Run("顶层 ErrGroupIDNotFound 可被 errors.Is 识别", func(t *testing.T) {
		f := &groupOffsetFake{committed: map[int32]int64{0: -1}, topErr: sarama.ErrGroupIDNotFound}
		_, err := CommittedOffsets(f, group, topic, []int32{0})
		if err == nil {
			t.Fatal("a top-level error must not be swallowed")
		}
		if !errors.Is(err, sarama.ErrGroupIDNotFound) {
			t.Fatalf("errors.Is(err, ErrGroupIDNotFound) = false, err=%v", err)
		}
	})

	t.Run("nil 响应", func(t *testing.T) {
		r := committedOffsetsFunc(func(string, map[string][]int32) (*sarama.OffsetFetchResponse, error) { return nil, nil })
		if _, err := CommittedOffsets(r, group, topic, []int32{0}); err == nil {
			t.Fatal("a nil response must be an error, not an empty map")
		}
	})

	t.Run("block 缺失", func(t *testing.T) {
		f := &groupOffsetFake{committed: map[int32]int64{0: 5}}
		if _, err := CommittedOffsets(f, group, topic, []int32{0, 1}); err == nil {
			t.Fatal("a missing block must be an error, not -1")
		}
	})

	t.Run("block 带错", func(t *testing.T) {
		f := &groupOffsetFake{
			committed: map[int32]int64{0: 5},
			blockErr:  map[int32]sarama.KError{0: sarama.ErrNotCoordinatorForConsumer},
		}
		_, err := CommittedOffsets(f, group, topic, []int32{0})
		if err == nil || !errors.Is(err, sarama.ErrNotCoordinatorForConsumer) {
			t.Fatalf("a block error must be returned and wrapped, got %v", err)
		}
	})

	t.Run("调用本身失败", func(t *testing.T) {
		f := &groupOffsetFake{listErr: errors.New("broker down")}
		if _, err := CommittedOffsets(f, group, topic, []int32{0}); err == nil {
			t.Fatal("a failed call must be an error")
		}
	})

	t.Run("computeGroupLag 的错误文本与抽取前逐字相同", func(t *testing.T) {
		f := &groupOffsetFake{committed: map[int32]int64{}}
		_, err := computeGroupLag(f, group, topic, 1)
		want := "kafka lag probe: group " + group + " has no offset block for " + topic + "[0]"
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})
}

// TestNewGroupLagProberConfig 钉住探针的 sarama 配置(负责人 2026-10-15 按「最标准」定:只读探针不得对被监控系统产生副作用):
//   - Metadata.AllowAutoTopicCreation 必须是 false。修复前探针用的是 sarama.NewConfig() 的默认值 true(v1.43.1 config.go:524),
//     GetOffset 按 topic 刷新元数据时带上它,broker 开着 auto.create、num.partitions=1 时会把未预建的 topic 建成 1 分区;
//   - 其余关键项与修复前逐项相同:Version = V3_0_0_0(与 EnsureTopics 一致)、拨号 / 读 / 写超时 5s、Metadata.Retry.Max = 1;
//   - 配置能过 sarama 自己的 Validate(NewClient 的第一步就是它),不连 Kafka。
func TestNewGroupLagProberConfig(t *testing.T) {
	cfg := newGroupLagProberConfig()
	if cfg.Metadata.AllowAutoTopicCreation {
		t.Fatal("the lag probe must not let the broker auto-create topics: Metadata.AllowAutoTopicCreation must be false")
	}
	if cfg.Version != sarama.V3_0_0_0 {
		t.Fatalf("Version = %v, want %v (same as EnsureTopics)", cfg.Version, sarama.V3_0_0_0)
	}
	for _, tc := range []struct {
		name string
		got  time.Duration
	}{
		{"Net.DialTimeout", cfg.Net.DialTimeout},
		{"Net.ReadTimeout", cfg.Net.ReadTimeout},
		{"Net.WriteTimeout", cfg.Net.WriteTimeout},
	} {
		if tc.got != 5*time.Second {
			t.Fatalf("%s = %v, want 5s", tc.name, tc.got)
		}
	}
	if cfg.Metadata.Retry.Max != 1 {
		t.Fatalf("Metadata.Retry.Max = %d, want 1", cfg.Metadata.Retry.Max)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("sarama rejects the probe config: %v", err)
	}
}

// TestComputeGroupLagUnknownTopic 钉住「topic 不存在」被如实报成探测失败,不当成 0 lag。
// 探针关掉自动建 topic 后,topic 不存在时 sarama 的 GetOffset 回 ErrUnknownTopicOrPartition(见 newGroupLagProberConfig);
// 读 committed 那一步按 broker 语义可能对每个分区回 -1,这里按这种情形造(另一种「block 带错」已在 TestComputeGroupLag 覆盖)。
// 此时必须返回 error(调用方据此记 result="error",持续 15 分钟即 DataServiceAuditLagProbeBlind)、errors.Is 认得出原因,
// 且不带回任何部分数字。本用例修复前后都应为绿:不是回归用例,只钉住调用方依赖的这条错误路径。
func TestComputeGroupLagUnknownTopic(t *testing.T) {
	f := &groupOffsetFake{
		committed: map[int32]int64{0: -1, 1: -1},
		offsetErr: sarama.ErrUnknownTopicOrPartition,
	}
	got, err := computeGroupLag(f, "g", "player_snapshot_topic_g2", 2)
	if err == nil {
		t.Fatalf("a missing topic must be a probe failure, not lag %+v", got)
	}
	if !errors.Is(err, sarama.ErrUnknownTopicOrPartition) {
		t.Fatalf("errors.Is(err, ErrUnknownTopicOrPartition) = false, err=%v", err)
	}
	if got != (GroupLag{}) {
		t.Fatalf("a failed probe must not hand back partial numbers, got %+v", got)
	}
}

func TestStableDrainCatalogExcludesOnlyCaptureWindow(t *testing.T) {
	first := []byte(`{"schema":"SQL_DRAIN_KAFKA_CATALOG_V2","observed_start_ms":1,"observed_end_ms":2,"cluster_id":"Yw==","topic_partitions":{},"topic_ids":{},"topic_names":[],"group_names":[],"broker_configs":{},"topic_configs":{}}`)
	second := bytes.ReplaceAll(bytes.ReplaceAll(first, []byte(`"observed_start_ms":1`), []byte(`"observed_start_ms":3`)), []byte(`"observed_end_ms":2`), []byte(`"observed_end_ms":4`))
	a, err := StableDrainCatalogDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := StableDrainCatalogDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("观察时间改变了稳定目录摘要")
	}
	changed := bytes.ReplaceAll(second, []byte("Yw=="), []byte("ZA=="))
	c, err := StableDrainCatalogDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("换cluster未改变稳定目录摘要")
	}
}

func TestBoundConsumerGroupRejectsImplicitOrUnboundedOwnership(t *testing.T) {
	config := BoundConsumerGroupConfig{Group: kafkago.ConsumerGroupConfig{ID: "results", Brokers: []string{"localhost:9092"}, Topics: []string{"results"}}, Reader: kafkago.ReaderConfig{MinBytes: 1, MaxBytes: 1024}, HandlerTimeout: time.Second}
	handler := func(context.Context, kafkago.Message) error { return nil }
	if _, err := NewBoundConsumerGroup(config, nil, handler); err == nil {
		t.Fatal("未给真实 relay owner 却接纳构造")
	}
	if _, err := NewBoundConsumerGroup(config, drainproof.New(), nil); err == nil {
		t.Fatal("未给实际处理函数却接纳构造")
	}
	invalid := config
	invalid.Group.StartOffset = kafkago.LastOffset
	if _, err := NewBoundConsumerGroup(invalid, drainproof.New(), handler); err == nil {
		t.Fatal("允许 latest 跳过无提交历史")
	}
	invalid = config
	invalid.Reader.GroupID = "second-group"
	if _, err := NewBoundConsumerGroup(invalid, drainproof.New(), handler); err == nil {
		t.Fatal("允许第二个隐式 consumer group")
	}
	invalid = config
	invalid.HandlerTimeout = 0
	if _, err := NewBoundConsumerGroup(invalid, drainproof.New(), handler); err == nil {
		t.Fatal("允许没有时间预算的 accepted handler")
	}
	owner, err := NewBoundConsumerGroup(config, drainproof.New(), handler)
	if err != nil {
		t.Fatal(err)
	}
	owner.StopFetching()
	// 故意移除 broker：若 Run 越过终局分支，构造校验立即失败，不允许回归用例连接网络。
	owner.config.Group.Brokers = nil
	if err := owner.Run(context.Background()); err != nil {
		t.Fatalf("未启动已停止属于已知零接纳终局: %v", err)
	}
	raw, err := owner.CloseAndReport(context.Background())
	if err != nil {
		t.Fatalf("Stop→Run→Close 必须完成: %v", err)
	}
	if owner.started || owner.generation != nil || len(owner.history) != 0 || !bytes.Contains(raw, []byte(`"Started":false`)) {
		t.Fatalf("未启动终局伪造了实际运行: %s", raw)
	}
	again, err := owner.CloseAndReport(context.Background())
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("重复关闭必须保留同一零接纳原件: %v", err)
	}
}

func TestMergeConsumerReadinessBindsActualBootAndRejectsDuplicateGroup(t *testing.T) {
	makeReport := func(group string, instance byte) []byte {
		t.Helper()
		report := &pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{Groups: []*pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness{{OperationId: bytes.Repeat([]byte{1}, 16), ManifestDigest: bytes.Repeat([]byte{2}, 32), IssuerId: bytes.Repeat([]byte{3}, 16), InstanceId: bytes.Repeat([]byte{instance}, 16), Group: group, GenerationId: 5, MemberId: "actual-member", ExpectedPartitions: []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{{Group: group, TopicName: "results", Partition: 0}}}}}
		raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	first, second := makeReport("a", 4), makeReport("b", 4)
	digest, raw, err := MergeConsumerReadiness(second, first)
	if err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(raw)
	if !bytes.Equal(digest, expected[:]) {
		t.Fatal("摘要没有绑定实际合并原件")
	}
	decoded := &pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Groups) != 2 || decoded.Groups[0].Group != "a" || len(decoded.Groups[0].AssignedPartitions) != 0 {
		t.Fatal("合并改变了真实组集合或为空 assignment 伪造分区")
	}
	if _, _, err := MergeConsumerReadiness(first, first); err == nil {
		t.Fatal("重复 consumer group 未拒绝")
	}
	if _, _, err := MergeConsumerReadiness(first, makeReport("b", 9)); err == nil {
		t.Fatal("跨 boot 的报告被合并")
	}
	if _, _, err := MergeConsumerReadiness(); err == nil {
		t.Fatal("无实际消费者的空报告被当作 readiness")
	}
}

// 代次/提交边界替身不连接 Kafka，验证批 SQL caller 可观察的交付、确定性重放与元数据提交合同。
func TestBoundMessageReaderReplaysSameTicketAndAcceptsMetadataCommit(t *testing.T) {
	relay := drainproof.New()
	owner, err := NewBoundMessageReader(BoundConsumerGroupConfig{Group: kafkago.ConsumerGroupConfig{ID: "snapshots", Brokers: []string{"localhost:9092"}, Topics: []string{"snapshots"}}, Reader: kafkago.ReaderConfig{MinBytes: 1, MaxBytes: 1024}}, relay)
	if err != nil {
		t.Fatal(err)
	}
	install := func(id int32) *boundPartition {
		generation := &kafkago.Generation{ID: id, GroupID: "snapshots", MemberID: "member"}
		partition := &boundPartition{topic: "snapshots", partition: 0, initial: 10, committed: 10, pending: map[int64]*boundDelivery{}}
		owner.generation = generation
		owner.generationContext = context.Background()
		owner.generationCancel = func() {}
		owner.partitions = map[string]*boundPartition{"snapshots/0": partition}
		owner.initialized = true
		return partition
	}
	message := kafkago.Message{Topic: "snapshots", Partition: 0, Offset: 10, Key: []byte("key"), Value: []byte("actual-payload")}
	first := install(1)
	owner.messages <- &boundDelivery{generation: owner.generation, partition: first, message: message}
	delivered, err := owner.FetchMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if relay.Pending() != 1 {
		t.Fatal("实际交付没有登记 accepted")
	}
	metadata := kafkago.Message{Topic: delivered.Topic, Partition: delivered.Partition, Offset: delivered.Offset}
	if err = owner.ReportProcessingFailure(context.Background(), ProcessingNotCommitted, metadata); err != nil {
		t.Fatal(err)
	}
	if relay.Pending() != 1 {
		t.Fatal("已证明未提交仍须等实际重放，不能伪造 settled")
	}
	second := install(2)
	owner.messages <- &boundDelivery{generation: owner.generation, partition: second, message: message}
	if _, err = owner.FetchMessage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if relay.Pending() != 1 {
		t.Fatal("同 T/P/O 重放重复创建原票据")
	}
	committed := false
	owner.commitOffsets = func(generation *kafkago.Generation, offsets map[string]map[int]int64) error {
		if generation.ID != 2 || offsets["snapshots"][0] != 11 {
			t.Fatalf("错误的代次或 exclusive commit: %v", offsets)
		}
		committed = true
		return nil
	}
	if err = owner.CommitMessages(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	if !committed || relay.Pending() != 0 {
		t.Fatal("实际新代次提交之后原票据没有 settled")
	}
}

func TestBoundMessageReaderUnknownCannotBecomeReplaySuccess(t *testing.T) {
	relay := drainproof.New()
	owner, err := NewBoundMessageReader(BoundConsumerGroupConfig{Group: kafkago.ConsumerGroupConfig{ID: "g", Brokers: []string{"localhost:9092"}, Topics: []string{"t"}}, Reader: kafkago.ReaderConfig{MinBytes: 1, MaxBytes: 1024}}, relay)
	if err != nil {
		t.Fatal(err)
	}
	generation := &kafkago.Generation{ID: 1, GroupID: "g", MemberID: "member"}
	partition := &boundPartition{topic: "t", partition: 0, pending: map[int64]*boundDelivery{}}
	owner.generation = generation
	owner.generationContext = context.Background()
	owner.partitions = map[string]*boundPartition{"t/0": partition}
	owner.initialized = true
	owner.messages <- &boundDelivery{generation: generation, partition: partition, message: kafkago.Message{Topic: "t", Partition: 0, Offset: 0, Value: []byte("x")}}
	message, err := owner.FetchMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.ReportProcessingFailure(context.Background(), ProcessingOutcomeUnknown, message); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("UNKNOWN 被接受成确定性恢复: %v", err)
	}
	owner.waitDelivered(context.Background(), generation, owner.partitions)
	if err = relay.WaitIdle(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("UNKNOWN 没有保持 HOLD: %v", err)
	}
	if err = owner.CommitMessages(context.Background(), message); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("UNKNOWN 被后一次 commit 洗白: %v", err)
	}
}

// P9 复核和本机门禁提交必须共用真实 owner 锁，不能留下预检后换代的窗口。
func TestWithConsumerReadinessKeepsGenerationLockedThroughOpen(t *testing.T) {
	owner, err := NewBoundMessageReader(BoundConsumerGroupConfig{Group: kafkago.ConsumerGroupConfig{ID: "g", Brokers: []string{"localhost:9092"}, Topics: []string{"t"}}, Reader: kafkago.ReaderConfig{MinBytes: 1, MaxBytes: 1024}}, drainproof.New())
	if err != nil {
		t.Fatal(err)
	}
	owner.generation = &kafkago.Generation{ID: 7, GroupID: "g", MemberID: "member"}
	owner.generationContext = context.Background()
	owner.initialized = true
	owner.partitions = map[string]*boundPartition{"t/0": {topic: "t", partition: 0, committed: 12}}
	report := &pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{Groups: []*pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness{{Group: "g", GenerationId: 7, MemberId: "member", AssignedPartitions: []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{{Group: "g", TopicName: "t", Partition: 0, FinalCommitted: 12}}}}}
	raw, err := protojson.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	commit := func() ([]byte, error) {
		calls++
		if owner.mu.TryLock() {
			owner.mu.Unlock()
			t.Fatal("本机 open 没有持有实际代次锁")
		}
		return []byte("opened"), nil
	}
	result, err := WithConsumerReadiness(context.Background(), raw, []*BoundConsumerGroup{owner}, commit)
	if err != nil || string(result) != "opened" || calls != 1 {
		t.Fatalf("同代次开放失败: %q %v", result, err)
	}
	owner.generation = &kafkago.Generation{ID: 8, GroupID: "g", MemberID: "member"}
	if _, err = WithConsumerReadiness(context.Background(), raw, []*BoundConsumerGroup{owner}, commit); err == nil || calls != 1 {
		t.Fatal("换代后仍执行旧 open")
	}
	owner.generation = &kafkago.Generation{ID: 7, GroupID: "g", MemberID: "member"}
	report.ManualAssignments = []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{{}}
	raw, err = protojson.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = WithConsumerReadiness(context.Background(), raw, []*BoundConsumerGroup{owner}, commit); err == nil || calls != 1 {
		t.Fatal("group owner 忽略了未拥有的 manual assignment")
	}
	if _, _, err = MergeConsumerReadiness(raw); err == nil {
		t.Fatal("合并悄悄丢弃 manual assignment")
	}
}

func TestManualAssignmentKeepsNativeIdentityAndPositionFloor(t *testing.T) {
	base := &pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{IssuerId: []byte("scene-source"), InstanceId: bytes.Repeat([]byte{1}, 16), ConsumerId: "scene-rpc", ClusterId: []byte("cluster"), TopicId: bytes.Repeat([]byte{2}, 16), TopicName: "scene-cmd", Partition: 0, PartitionCount: 2, TargetNodeId: 9, TargetInstanceId: bytes.Repeat([]byte{1}, 16), AssignmentRevision: 3, StartOffset: 4, Position: 10, Earliest: 4, ExclusiveEnd: 12, ObservedMs: 1}
	clone := func() *pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment {
		return proto.Clone(base).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
	}
	settled := clone()
	settled.Position = 12
	settled.ExclusiveEnd = 12
	settled.ObservedMs = 2
	if !sameManualAssignment(base, settled) {
		t.Fatal("同一本机 assignment 的真实位置推进被拒绝")
	}
	changed := clone()
	changed.AssignmentRevision++
	if sameManualAssignment(base, changed) {
		t.Fatal("换 assignment 被接纳")
	}
	changed = clone()
	changed.StartOffset++
	if sameManualAssignment(base, changed) {
		t.Fatal("改写本机起点被接纳")
	}
	changed = clone()
	changed.Position--
	if sameManualAssignment(base, changed) {
		t.Fatal("本机位置回退被接纳")
	}
	changed = clone()
	changed.Earliest = 11
	changed.Position = 12
	if sameManualAssignment(base, changed) {
		t.Fatal("已丢失 P0 保护范围仍被接纳")
	}
}

// 已启动的重复 Run 与未完成的非法停止状态仍拒绝，不能广泛吞掉 ErrClosed。
func TestBoundConsumerGroupRunRejectsDuplicateAndIncompleteStop(t *testing.T) {
	for _, state := range []struct {
		name                        string
		started, stopping, complete bool
	}{
		{name: "重复启动", started: true},
		{name: "已启动后停止", started: true, stopping: true, complete: true},
		{name: "未完成的停止", stopping: true},
	} {
		t.Run(state.name, func(t *testing.T) {
			owner := &BoundConsumerGroup{started: state.started, stopping: state.stopping, complete: state.complete}
			if err := owner.Run(context.Background()); !errors.Is(err, drainproof.ErrClosed) {
				t.Fatalf("非法生命周期必须拒绝: %v", err)
			}
		})
	}
}
