package kafkautil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

func TestPartitionContractMarkerIsStableAndUnambiguous(t *testing.T) {
	prefixA, markerA10 := partitionContractMarker("db_task_zone_1", 10)
	prefixAAgain, markerA20 := partitionContractMarker("db_task_zone_1", 20)
	prefixB, markerB10 := partitionContractMarker("db_task_zone_1_p10", 10)

	if prefixA != prefixAAgain || !strings.HasPrefix(markerA10, prefixA) || !strings.HasPrefix(markerA20, prefixA) {
		t.Fatalf("same topic must share one stable marker prefix")
	}
	if markerA10 == markerA20 {
		t.Fatalf("different partition contracts must produce different markers")
	}
	if prefixA == prefixB || markerA10 == markerB10 {
		t.Fatalf("different topic names must not share an ambiguous marker namespace")
	}
	if len(markerA10) > 249 {
		t.Fatalf("Kafka topic marker exceeds the 249-byte topic-name limit: %d", len(markerA10))
	}
}

// 用显式虚拟时钟推进截止，所有测试既不休眠也不连接 broker。
type topicMetadataReply struct {
	topics  map[string]sarama.TopicDetail
	err     error
	elapsed time.Duration
}

type topicAdminFake struct {
	t             *testing.T
	now           time.Time
	replies       []topicMetadataReply
	listCalls     int
	createErr     error
	created       map[string]sarama.TopicDetail
	retentionName string
	retention     map[string]sarama.IncrementalAlterConfigsEntry
	alterCalls    int
	sleeps        []time.Duration
	// grown 记下每次 CreatePartitions 的 topic → 目标分区数;growErr 是它的返回值(2026-11-06)。
	grown   map[string]int32
	growErr error
}

func newTopicAdminFake(t *testing.T, replies ...topicMetadataReply) *topicAdminFake {
	return &topicAdminFake{t: t, now: time.Unix(0, 0), replies: replies, created: make(map[string]sarama.TopicDetail)}
}

func (a *topicAdminFake) ListTopics() (map[string]sarama.TopicDetail, error) {
	a.listCalls++
	if a.listCalls > 1000 {
		a.t.Fatal("metadata polling did not stop within its budget")
	}
	index := a.listCalls - 1
	if index >= len(a.replies) {
		index = len(a.replies) - 1
	}
	reply := a.replies[index]
	a.now = a.now.Add(reply.elapsed)
	return reply.topics, reply.err
}

func (a *topicAdminFake) CreateTopic(name string, detail *sarama.TopicDetail, validateOnly bool) error {
	if _, exists := a.created[name]; exists {
		a.t.Fatalf("topic %s was created more than once", name)
	}
	if validateOnly {
		a.t.Fatal("initialization must create the topic")
	}
	a.created[name] = *detail
	return a.createErr
}

func (a *topicAdminFake) CreatePartitions(topic string, count int32, assignment [][]int32, validateOnly bool) error {
	if validateOnly || assignment != nil {
		a.t.Fatal("growing partitions must be a real request with broker-chosen replica placement")
	}
	if a.grown == nil {
		a.grown = make(map[string]int32)
	}
	if _, again := a.grown[topic]; again {
		a.t.Fatalf("partitions of %s were grown more than once", topic)
	}
	a.grown[topic] = count
	return a.growErr
}

func (a *topicAdminFake) IncrementalAlterConfig(resource sarama.ConfigResourceType, name string, entries map[string]sarama.IncrementalAlterConfigsEntry, validateOnly bool) error {
	if resource != sarama.TopicResource || validateOnly {
		a.t.Fatal("retention must use a topic-level incremental update")
	}
	a.alterCalls++
	a.retentionName, a.retention = name, entries
	return nil
}

func (a *topicAdminFake) clock() time.Time { return a.now }
func (a *topicAdminFake) sleep(delay time.Duration) {
	if delay <= 0 {
		a.t.Fatalf("metadata retry delay must be positive: %s", delay)
	}
	a.sleeps = append(a.sleeps, delay)
	a.now = a.now.Add(delay)
}

func TestEnsureTopicsWaitsForCreatedTopicMetadata(t *testing.T) {
	for _, createErr := range []error{nil, sarama.ErrTopicAlreadyExists} {
		t.Run(fmtCreateResult(createErr), func(t *testing.T) {
			spec := TopicSpec{Name: "db_task_zone_1", Partitions: 10, RetentionMs: 86400000}
			admin := newTopicAdminFake(t, topicMetadataReply{}, topicMetadataReply{}, topicMetadataReply{
				topics: map[string]sarama.TopicDetail{spec.Name: {NumPartitions: 10}},
			})
			admin.createErr = createErr
			if err := ensureTopics(admin, []TopicSpec{spec}, 1, admin.clock, admin.sleep); err != nil {
				t.Fatalf("delayed metadata must not reject successful creation: %v", err)
			}
			if admin.listCalls != 3 || len(admin.sleeps) == 0 {
				t.Fatalf("must refresh until metadata is visible: calls=%d sleeps=%v", admin.listCalls, admin.sleeps)
			}
			_, marker := partitionContractMarker(spec.Name, spec.Partitions)
			if len(admin.created) != 2 || admin.created[spec.Name].NumPartitions != 10 || admin.created[marker].NumPartitions != 1 {
				t.Fatalf("data topic and immutable marker must retain their partition contracts: %+v", admin.created)
			}
			if policy := admin.created[marker].ConfigEntries["cleanup.policy"]; policy == nil || *policy != "compact" {
				t.Fatal("partition marker must remain compacted")
			}
			entry := admin.retention["retention.ms"]
			if admin.retentionName != spec.Name || len(admin.retention) != 1 || entry.Operation != sarama.IncrementalAlterConfigsOperationSet || entry.Value == nil || *entry.Value != "86400000" {
				t.Fatalf("validated topic must receive only incremental retention SET: %+v", admin.retention)
			}
		})
	}
}

func fmtCreateResult(err error) string {
	if err == nil {
		return "created"
	}
	return "already_exists"
}

func TestEnsureTopicsRejectsDelayedPartitionOrMarkerConflict(t *testing.T) {
	const topic = "db_task_zone_1"
	_, wrongMarker := partitionContractMarker(topic, 20)
	for _, tc := range []struct {
		name, message string
		topics        map[string]sarama.TopicDetail
	}{
		{"partitions", "partition contract mismatch", map[string]sarama.TopicDetail{topic: {NumPartitions: 20}}},
		{"marker", "immutable partition marker conflicts", map[string]sarama.TopicDetail{topic: {NumPartitions: 10}, wrongMarker: {NumPartitions: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := newTopicAdminFake(t, topicMetadataReply{}, topicMetadataReply{}, topicMetadataReply{topics: tc.topics})
			admin.createErr = sarama.ErrTopicAlreadyExists
			err := ensureTopics(admin, []TopicSpec{{Name: topic, Partitions: 10, RetentionMs: 86400000}}, 1, admin.clock, admin.sleep)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("delayed conflicting metadata must fail closed with %q: %v", tc.message, err)
			}
			if len(admin.created) != 1 || admin.retention != nil {
				t.Fatal("conflicting metadata must not create a marker or alter retention")
			}
		})
	}
}

// 只许加的 topic(2026-11-06,缺口清单第 15 条):broker 上分区少于配置就原地加到配置值,多于配置就接受;
// 都不建不可变分区标记。严格 topic 的同一种分区差仍判 mismatch(见上一条),且绝不调用 CreatePartitions。
func TestEnsureTopicsGrowablePartitions(t *testing.T) {
	const topic = "transaction_log_topic_g1"
	_, staleMarker := partitionContractMarker(topic, 6)
	for _, tc := range []struct {
		name      string
		onBroker  int32
		wantGrown map[string]int32
	}{
		{"fewer on broker grows in place", 6, map[string]int32{topic: 12}},
		{"equal leaves it alone", 12, nil},
		{"more on broker is kept", 20, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 存量 topic 还挂着改动前的标记(6 分区),只许加的 topic 不再核它,也不建新的。
			admin := newTopicAdminFake(t, topicMetadataReply{topics: map[string]sarama.TopicDetail{
				topic: {NumPartitions: tc.onBroker}, staleMarker: {NumPartitions: 1},
			}})
			spec := TopicSpec{Name: topic, Partitions: 12, RetentionMs: 2592000000, AllowPartitionGrowth: true}
			if err := ensureTopics(admin, []TopicSpec{spec}, 1, admin.clock, admin.sleep); err != nil {
				t.Fatalf("growable topic with %d partitions on the broker must be accepted: %v", tc.onBroker, err)
			}
			if fmt.Sprint(admin.grown) != fmt.Sprint(tc.wantGrown) {
				t.Fatalf("CreatePartitions calls = %v, want %v", admin.grown, tc.wantGrown)
			}
			if len(admin.created) != 0 {
				t.Fatalf("a growable topic must not create a partition marker: %+v", admin.created)
			}
			if admin.retentionName != topic || admin.alterCalls != 1 {
				t.Fatalf("retention must still be managed: name=%q calls=%d", admin.retentionName, admin.alterCalls)
			}
		})
	}
}

// 两个副本同时去加:后到的被 Kafka 拒绝(分区数不大于当前值),重读元数据看到已经加够就当成功;
// 仍然不够才回错误,且不往下改保留期。
func TestEnsureTopicsGrowablePartitionsRace(t *testing.T) {
	const topic = "player_snapshot_topic_g1"
	spec := TopicSpec{Name: topic, Partitions: 8, RetentionMs: 2592000000, AllowPartitionGrowth: true}
	for _, tc := range []struct {
		name    string
		reread  int32
		wantErr bool
	}{
		{"another instance already grew it", 8, false},
		{"still short after the failure", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := newTopicAdminFake(t,
				topicMetadataReply{topics: map[string]sarama.TopicDetail{topic: {NumPartitions: 3}}},
				topicMetadataReply{topics: map[string]sarama.TopicDetail{topic: {NumPartitions: tc.reread}}},
			)
			admin.growErr = sarama.ErrInvalidPartitions
			err := ensureTopics(admin, []TopicSpec{spec}, 1, admin.clock, admin.sleep)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "grow topic") {
					t.Fatalf("growth that did not happen must fail, got %v", err)
				}
				if admin.alterCalls != 0 {
					t.Fatal("a failed growth must not go on to alter retention")
				}
				return
			}
			if err != nil {
				t.Fatalf("growth done by another instance must be accepted: %v", err)
			}
			if admin.listCalls != 2 {
				t.Fatalf("metadata must be re-read once after the rejected growth, calls=%d", admin.listCalls)
			}
		})
	}
}

func TestEnsureTopicsMetadataVisibilityTimeout(t *testing.T) {
	admin := newTopicAdminFake(t, topicMetadataReply{})
	err := ensureTopics(admin, []TopicSpec{{Name: "db_task_zone_1", Partitions: 10, RetentionMs: 86400000}}, 1, admin.clock, admin.sleep)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "db_task_zone_1") {
		t.Fatalf("invisible metadata must report the topic and deadline: %v", err)
	}
	if admin.listCalls < 3 || len(admin.sleeps) == 0 || admin.now.Sub(time.Unix(0, 0)) > 10*time.Second {
		t.Fatalf("metadata polling must stop at its ten-second budget: calls=%d elapsed=%s", admin.listCalls, admin.now.Sub(time.Unix(0, 0)))
	}
	if len(admin.created) != 1 || admin.retention != nil {
		t.Fatal("timeout must not recreate the topic, create a marker, or alter retention")
	}
}

func TestEnsureTopicsRejectsMetadataArrivingAfterDeadline(t *testing.T) {
	const topic = "db_task_zone_1"
	admin := newTopicAdminFake(t, topicMetadataReply{}, topicMetadataReply{
		topics: map[string]sarama.TopicDetail{topic: {NumPartitions: 10}}, elapsed: 11 * time.Second,
	})
	err := ensureTopics(admin, []TopicSpec{{Name: topic, Partitions: 10, RetentionMs: 86400000}}, 1, admin.clock, admin.sleep)
	if !errors.Is(err, context.DeadlineExceeded) || admin.listCalls != 2 || len(admin.sleeps) != 0 || admin.retention != nil || len(admin.created) != 1 {
		t.Fatalf("an in-flight query exceeding the deadline must not permit follow-up work: err=%v calls=%d", err, admin.listCalls)
	}
}

func TestEnsureTopicsMetadataErrorStopsImmediately(t *testing.T) {
	cause := sarama.ErrTopicAuthorizationFailed
	admin := newTopicAdminFake(t, topicMetadataReply{}, topicMetadataReply{err: cause})
	err := ensureTopics(admin, []TopicSpec{{Name: "db_task_zone_1", Partitions: 10}}, 1, admin.clock, admin.sleep)
	if !errors.Is(err, cause) || admin.listCalls != 2 || len(admin.sleeps) != 0 || len(admin.created) != 1 {
		t.Fatalf("metadata errors must retain their cause without retrying: err=%v calls=%d", err, admin.listCalls)
	}
}

// 副本数是部署契约:spec 不写时取调用方给的默认值(生产路径 = KAFKA_TOPIC_REPLICATION_FACTOR),
// 数据 topic 与它的不可变分区 marker 用同一个副本数;spec 显式写了的以 spec 为准。
func TestEnsureTopicsCreatesWithDeploymentReplicationFactor(t *testing.T) {
	for _, tc := range []struct {
		name           string
		specReplica    int16
		defaultReplica int16
		want           int16
	}{
		{"deployment_default", 0, 3, 3},
		{"spec_overrides_default", 2, 3, 2},
		{"unset_default_falls_back_to_one", 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := TopicSpec{Name: "db_task_zone_1", Partitions: 10, ReplicaFactor: tc.specReplica}
			admin := newTopicAdminFake(t, topicMetadataReply{}, topicMetadataReply{
				topics: map[string]sarama.TopicDetail{spec.Name: {NumPartitions: 10, ReplicationFactor: tc.want}},
			})
			if err := ensureTopics(admin, []TopicSpec{spec}, tc.defaultReplica, admin.clock, admin.sleep); err != nil {
				t.Fatalf("creation with the deployment replication factor must succeed: %v", err)
			}
			_, marker := partitionContractMarker(spec.Name, spec.Partitions)
			if got := admin.created[spec.Name].ReplicationFactor; got != tc.want {
				t.Fatalf("data topic replication factor = %d, want %d", got, tc.want)
			}
			if got := admin.created[marker].ReplicationFactor; got != tc.want {
				t.Fatalf("partition marker replication factor = %d, want %d", got, tc.want)
			}
		})
	}
}

// 已存在的 topic 副本数低于契约 = 假冗余,启动期 fail-closed;高于契约、或元数据不带副本信息(0)不拦。
func TestEnsureTopicsRejectsUnderReplicatedExistingTopic(t *testing.T) {
	const topic = "db_task_zone_1"
	_, marker := partitionContractMarker(topic, 10)
	for _, tc := range []struct {
		name       string
		existing   int16
		wantReject bool
	}{
		{"under_replicated", 1, true},
		{"exact", 3, false},
		{"over_replicated", 5, false},
		{"unknown_replication", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := newTopicAdminFake(t, topicMetadataReply{topics: map[string]sarama.TopicDetail{
				topic:  {NumPartitions: 10, ReplicationFactor: tc.existing},
				marker: {NumPartitions: 1},
			}})
			err := ensureTopics(admin, []TopicSpec{{Name: topic, Partitions: 10, RetentionMs: 86400000}}, 3, admin.clock, admin.sleep)
			if tc.wantReject {
				if err == nil || !strings.Contains(err.Error(), "replication contract mismatch") {
					t.Fatalf("an under-replicated topic must fail closed: %v", err)
				}
				if admin.retention != nil || len(admin.created) != 0 {
					t.Fatal("a rejected topic must not be altered and nothing may be created")
				}
				return
			}
			if err != nil {
				t.Fatalf("replication at or above the contract must pass: %v", err)
			}
			if len(admin.created) != 0 {
				t.Fatalf("an existing topic with its marker must not trigger any creation: %+v", admin.created)
			}
		})
	}
}

// retention.bytes(S5b):建 topic 与每次启动的增量 SET 都带上,且与 retention.ms 合成一次调用。
func TestEnsureTopicsSetsRetentionBytesOnCreateAndAlter(t *testing.T) {
	spec := TopicSpec{Name: "db_task_zone_1", Partitions: 10, RetentionMs: 86400000, RetentionBytes: 2785017856}
	admin := newTopicAdminFake(t, topicMetadataReply{}, topicMetadataReply{
		topics: map[string]sarama.TopicDetail{spec.Name: {NumPartitions: 10}},
	})
	if err := ensureTopics(admin, []TopicSpec{spec}, 1, admin.clock, admin.sleep); err != nil {
		t.Fatalf("creating a topic with retention.bytes must succeed: %v", err)
	}
	created := admin.created[spec.Name].ConfigEntries
	if ms := created["retention.ms"]; ms == nil || *ms != "86400000" {
		t.Fatalf("created topic must carry retention.ms: %+v", created)
	}
	if bytes := created["retention.bytes"]; bytes == nil || *bytes != "2785017856" {
		t.Fatalf("created topic must carry retention.bytes: %+v", created)
	}
	if admin.alterCalls != 1 || admin.retentionName != spec.Name || len(admin.retention) != 2 {
		t.Fatalf("retention must be applied in exactly one incremental call with two entries: calls=%d entries=%+v", admin.alterCalls, admin.retention)
	}
	for key, want := range map[string]string{"retention.ms": "86400000", "retention.bytes": "2785017856"} {
		entry := admin.retention[key]
		if entry.Operation != sarama.IncrementalAlterConfigsOperationSet || entry.Value == nil || *entry.Value != want {
			t.Fatalf("%s must be an incremental SET of %s: %+v", key, want, entry)
		}
	}
}

func TestEnsureTopicsLeavesRetentionBytesUnmanagedWhenZero(t *testing.T) {
	spec := TopicSpec{Name: "db_task_zone_1", Partitions: 10, RetentionMs: 86400000}
	admin := newTopicAdminFake(t, topicMetadataReply{}, topicMetadataReply{
		topics: map[string]sarama.TopicDetail{spec.Name: {NumPartitions: 10}},
	})
	if err := ensureTopics(admin, []TopicSpec{spec}, 1, admin.clock, admin.sleep); err != nil {
		t.Fatalf("unmanaged retention.bytes must keep the old behavior: %v", err)
	}
	if _, managed := admin.created[spec.Name].ConfigEntries["retention.bytes"]; managed {
		t.Fatal("RetentionBytes=0 must not put retention.bytes into the created topic")
	}
	if _, managed := admin.retention["retention.bytes"]; managed || len(admin.retention) != 1 {
		t.Fatalf("RetentionBytes=0 must only SET retention.ms: %+v", admin.retention)
	}
}

func TestEnsureTopicsRejectsNegativeRetentionBytes(t *testing.T) {
	spec := TopicSpec{Name: "db_task_zone_1", Partitions: 10, RetentionMs: 86400000, RetentionBytes: -1}
	admin := newTopicAdminFake(t, topicMetadataReply{})
	err := ensureTopics(admin, []TopicSpec{spec}, 1, admin.clock, admin.sleep)
	if err == nil || !strings.Contains(err.Error(), "retention.bytes") {
		t.Fatalf("negative retention.bytes (Kafka's unlimited) must be rejected: %v", err)
	}
	if len(admin.created) != 0 || admin.alterCalls != 0 {
		t.Fatalf("a rejected spec must not create or alter anything: created=%+v calls=%d", admin.created, admin.alterCalls)
	}
}

func TestEnsureTopicsRetentionBytesWithoutRetentionMs(t *testing.T) {
	const topic = "db_task_zone_1"
	_, marker := partitionContractMarker(topic, 10)
	admin := newTopicAdminFake(t, topicMetadataReply{topics: map[string]sarama.TopicDetail{
		topic:  {NumPartitions: 10},
		marker: {NumPartitions: 1},
	}})
	if err := ensureTopics(admin, []TopicSpec{{Name: topic, Partitions: 10, RetentionBytes: 1 << 30}}, 1, admin.clock, admin.sleep); err != nil {
		t.Fatalf("retention.bytes alone must be applied: %v", err)
	}
	if admin.alterCalls != 1 || len(admin.retention) != 1 {
		t.Fatalf("alter must contain only retention.bytes: calls=%d entries=%+v", admin.alterCalls, admin.retention)
	}
	if entry := admin.retention["retention.bytes"]; entry.Value == nil || *entry.Value != "1073741824" {
		t.Fatalf("retention.bytes must be SET verbatim: %+v", entry)
	}
}

func TestParseReplicationFactor(t *testing.T) {
	for raw, want := range map[string]int16{"": 1, "  ": 1, "1": 1, " 3 ": 3, "0": 1, "-2": 1, "abc": 1, "70000": 1} {
		if got := parseReplicationFactor(raw); got != want {
			t.Fatalf("parseReplicationFactor(%q) = %d, want %d", raw, got, want)
		}
	}
}
