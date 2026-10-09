package kafkautil

import (
	"context"
	"errors"
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
	sleeps        []time.Duration
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

func (a *topicAdminFake) IncrementalAlterConfig(resource sarama.ConfigResourceType, name string, entries map[string]sarama.IncrementalAlterConfigsEntry, validateOnly bool) error {
	if resource != sarama.TopicResource || validateOnly {
		a.t.Fatal("retention must use a topic-level incremental update")
	}
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

func TestParseReplicationFactor(t *testing.T) {
	for raw, want := range map[string]int16{"": 1, "  ": 1, "1": 1, " 3 ": 3, "0": 1, "-2": 1, "abc": 1, "70000": 1} {
		if got := parseReplicationFactor(raw); got != want {
			t.Fatalf("parseReplicationFactor(%q) = %d, want %d", raw, got, want)
		}
	}
}
