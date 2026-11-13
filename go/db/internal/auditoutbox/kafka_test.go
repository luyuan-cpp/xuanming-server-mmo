package auditoutbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/segmentio/kafka-go"
)

// kafka-go 的逐条错误按下标摊到每一行;别的错误(拿不到元数据、ctx 到期)算整批失败。
func TestSplitWriteError(t *testing.T) {
	if errs := splitWriteError(nil, 3); errs[0] != nil || errs[1] != nil || errs[2] != nil {
		t.Fatalf("nil error must mean every message was acknowledged: %v", errs)
	}
	rejected := errors.New("rejected")
	errs := splitWriteError(kafka.WriteErrors{nil, rejected, nil}, 3)
	if errs[0] != nil || !errors.Is(errs[1], rejected) || errs[2] != nil {
		t.Fatalf("per-message errors must keep their positions: %v", errs)
	}
	down := errors.New("no leader")
	for _, err := range []error{down, kafka.WriteErrors{nil, rejected}} {
		errs := splitWriteError(err, 3)
		for i, got := range errs {
			if got == nil {
				t.Fatalf("whole-call failure %v must fail message %d", err, i)
			}
		}
	}
}

// 装不进一次请求的行在本地就判失败,不交给 kafka-go:否则它会以 MessageTooLargeError 拒掉整次调用,一条坏行拖住整批。
// 这里所有行都超限,所以根本不连 broker(地址故意写成连不上的)。
func TestKafkaProducer_OversizedRowsFailLocally(t *testing.T) {
	producer, err := NewKafkaProducer([]string{"127.0.0.1:1"}, "transaction_log_topic_g1", "player_snapshot_topic_g1")
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	big := make([]byte, txlogBatchBytes)
	errs := producer.Produce(context.Background(), txlogKind, []Row{{PlayerID: 7, RecordID: 1, Payload: big}})
	if len(errs) != 1 || !errors.Is(errs[0], errRowTooLarge) {
		t.Fatalf("errs = %v, want errRowTooLarge", errs)
	}
	errs = producer.Produce(context.Background(), snapshotKind, []Row{{PlayerID: 7, RecordID: 2, Payload: make([]byte, snapshotBatchBytes)}})
	if len(errs) != 1 || !errors.Is(errs[0], errRowTooLarge) {
		t.Fatalf("errs = %v, want errRowTooLarge for the snapshot topic too", errs)
	}
}

func TestNewKafkaProducer_RejectsBadConfig(t *testing.T) {
	cases := map[string]func() error{
		"no brokers":  func() error { _, err := NewKafkaProducer(nil, "a_g1", "b_g1"); return err },
		"empty topic": func() error { _, err := NewKafkaProducer([]string{"k:9092"}, "", "b_g1"); return err },
		"same topic":  func() error { _, err := NewKafkaProducer([]string{"k:9092"}, "a_g1", "a_g1"); return err },
	}
	for name, build := range cases {
		if err := build(); err == nil {
			t.Fatalf("%s: NewKafkaProducer must fail", name)
		}
	}
	producer, err := NewKafkaProducer([]string{"k:9092"}, "a_g1", "b_g1")
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	errs := producer.Produce(context.Background(), 99, []Row{{PlayerID: 1, RecordID: 1}})
	if len(errs) != 1 || errs[0] == nil || !strings.Contains(errs[0].Error(), "unknown audit kind") {
		t.Fatalf("an unknown kind must fail every row, got %v", errs)
	}
}

// 两个 Writer 按 topic 契约配置:acks=all、CRC32(与 C++ librdkafka consistent_random 同一个分区算法)、不自动建 topic、
// 批上限分别等于两个 topic 的 max.message.bytes。
func TestKafkaProducer_WriterSettings(t *testing.T) {
	producer, err := NewKafkaProducer([]string{"k:9092"}, "transaction_log_topic_g1", "player_snapshot_topic_g1")
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	for i, kind := range relayKinds {
		w := producer.writers[i]
		if w.RequiredAcks != kafka.RequireAll || w.Async || w.AllowAutoTopicCreation {
			t.Fatalf("kind %v: acks=%v async=%v autoCreate=%v", kind, w.RequiredAcks, w.Async, w.AllowAutoTopicCreation)
		}
		if _, ok := w.Balancer.(*kafka.CRC32Balancer); !ok {
			t.Fatalf("kind %v: balancer %T, want CRC32", kind, w.Balancer)
		}
		if w.BatchSize != batchSize(kind) {
			t.Fatalf("kind %v: batch size %d", kind, w.BatchSize)
		}
	}
	if producer.writers[0].Topic != "transaction_log_topic_g1" || producer.writers[0].BatchBytes != txlogBatchBytes {
		t.Fatalf("transaction log writer = %s / %d", producer.writers[0].Topic, producer.writers[0].BatchBytes)
	}
	if producer.writers[1].Topic != "player_snapshot_topic_g1" || producer.writers[1].BatchBytes != snapshotBatchBytes {
		t.Fatalf("snapshot writer = %s / %d", producer.writers[1].Topic, producer.writers[1].BatchBytes)
	}
}
