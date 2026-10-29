package kafka

import (
	"testing"

	"db/internal/metrics"
	db_proto "proto/db"
	"shared/kafkautil"

	"github.com/IBM/sarama"
	"github.com/prometheus/client_golang/prometheus"
)

const producerPartitionMetricName = "db_task_producer_partition_total"

// producerPartitionSeries 从默认注册表读出 db_task_producer_partition_total 的全部序列,
// 返回 "op/verdict" → 计数。不依赖 testutil。
func producerPartitionSeries(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	series := map[string]float64{}
	for _, family := range families {
		if family.GetName() != producerPartitionMetricName {
			continue
		}
		for _, m := range family.GetMetric() {
			var op, verdict string
			for _, label := range m.GetLabel() {
				switch label.GetName() {
				case "op":
					op = label.GetValue()
				case "verdict":
					verdict = label.GetValue()
				}
			}
			series[op+"/"+verdict] = m.GetCounter().GetValue()
		}
	}
	return series
}

func producerPartitionCount(t *testing.T, op, verdict string) float64 {
	t.Helper()
	return producerPartitionSeries(t)[op+"/"+verdict]
}

func TestObserveProducerPartitionVerdicts(t *testing.T) {
	const n = int32(10)
	exp, err := kafkautil.DBTaskPartition([]byte("1001"), n)
	if err != nil {
		t.Fatalf("DBTaskPartition: %v", err)
	}

	for _, opCase := range []struct {
		taskOp db_proto.DBOp
		label  string
	}{
		{db_proto.DBOp_DB_OP_READ, metrics.ProducerPartitionOpRead},
		{db_proto.DBOp_DB_OP_WRITE, metrics.ProducerPartitionOpWrite},
		{db_proto.DBOp(99), metrics.ProducerPartitionOpOther},
	} {
		for _, tc := range []struct {
			name      string
			key       []byte
			partition int32
			count     int32
			want      string
		}{
			{"ok", []byte("1001"), exp, n, metrics.ProducerPartitionOK},
			{"partition_mismatch", []byte("1001"), (exp + 1) % n, n, metrics.ProducerPartitionMismatch},
			{"key_mismatch", []byte("1002"), exp, n, metrics.ProducerPartitionKeyMismatch},
			{"nil_key", nil, exp, n, metrics.ProducerPartitionKeyMismatch},
			{"invalid_partition_count", []byte("1001"), exp, 0, metrics.ProducerPartitionInvalid},
		} {
			t.Run(opCase.taskOp.String()+"/"+tc.name, func(t *testing.T) {
				msg := &sarama.ConsumerMessage{Topic: "db_task_zone_1", Partition: tc.partition, Offset: 7, Key: tc.key}
				task := &db_proto.DBTask{Key: 1001, Op: opCase.taskOp, MsgType: "player_database", TaskId: "audit"}

				before := producerPartitionCount(t, opCase.label, tc.want)
				if got := observeProducerPartition(msg, task, tc.count); got != tc.want {
					t.Fatalf("verdict = %q, want %q", got, tc.want)
				}
				if after := producerPartitionCount(t, opCase.label, tc.want); after != before+1 {
					t.Fatalf("db_task_producer_partition_total{op=%q,verdict=%q} went %v -> %v, want +1",
						opCase.label, tc.want, before, after)
				}
			})
		}
	}
}

func TestObserveProducerPartitionPreRegistersAllSeries(t *testing.T) {
	msg := &sarama.ConsumerMessage{Topic: "db_task_zone_1", Partition: 0, Key: []byte("1001")}
	observeProducerPartition(msg, &db_proto.DBTask{Key: 1001, Op: db_proto.DBOp_DB_OP_WRITE}, 10)

	series := producerPartitionSeries(t)
	if len(series) != 12 {
		t.Fatalf("%s has %d series, want exactly 12 (3 ops x 4 verdicts): %v", producerPartitionMetricName, len(series), series)
	}
	for _, op := range []string{metrics.ProducerPartitionOpRead, metrics.ProducerPartitionOpWrite, metrics.ProducerPartitionOpOther} {
		for _, verdict := range []string{metrics.ProducerPartitionOK, metrics.ProducerPartitionMismatch,
			metrics.ProducerPartitionKeyMismatch, metrics.ProducerPartitionInvalid} {
			if _, ok := series[op+"/"+verdict]; !ok {
				t.Errorf("series op=%q verdict=%q is missing", op, verdict)
			}
		}
	}
}

func TestObserveProducerPartitionNilInputs(t *testing.T) {
	task := &db_proto.DBTask{Key: 1001, Op: db_proto.DBOp_DB_OP_WRITE}
	msg := &sarama.ConsumerMessage{Topic: "db_task_zone_1", Key: []byte("1001")}
	if got := observeProducerPartition(nil, task, 10); got != metrics.ProducerPartitionInvalid {
		t.Errorf("nil msg: verdict = %q, want invalid", got)
	}
	if got := observeProducerPartition(msg, nil, 10); got != metrics.ProducerPartitionInvalid {
		t.Errorf("nil task: verdict = %q, want invalid", got)
	}
}

func TestAllowProducerPartitionLogBudget(t *testing.T) {
	resetBudget := func() {
		for i := range producerPartitionLogged {
			producerPartitionLogged[i].Store(0)
		}
	}
	resetBudget()
	t.Cleanup(resetBudget)

	for i := 1; i <= 25; i++ {
		got := allowProducerPartitionLog(metrics.ProducerPartitionOpWrite)
		if want := i <= producerPartitionLogBudget; got != want {
			t.Fatalf("write call %d: allow = %v, want %v", i, got, want)
		}
	}
	// read 与 other 各有独立额度,不受 write 用尽的影响。
	if !allowProducerPartitionLog(metrics.ProducerPartitionOpRead) {
		t.Fatal("read budget must be independent of the exhausted write budget")
	}
	if !allowProducerPartitionLog(metrics.ProducerPartitionOpOther) {
		t.Fatal("other budget must be independent of the exhausted write budget")
	}
}
