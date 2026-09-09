package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestSetKafkaConsumerUpCreatesChildSeries 锁死告警的前提:
// kafka_consumer_up{consumer=...} 这条子序列在**消费者还没起来**时就必须存在、且值为 0。
//
// Prometheus 的 *Vec 只有 WithLabelValues 之后才有 child series。data_service 最该告警的
// 故障态恰恰是"EnsureTopics 一直失败 → 两条消费者一条都没起来":如果那时序列根本不存在,
// `data_service_kafka_consumer_up == 0` 的告警永远不会触发,监控上与"服务没部署"无从区分。
// 所以 startKafkaConsumers 在第一次 Start 尝试之前先按 false 打一次点。
func TestSetKafkaConsumerUpCreatesChildSeries(t *testing.T) {
	const consumer = "metrics_test_consumer"
	SetKafkaConsumerUp(consumer, false)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "data_service_kafka_consumer_up" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "consumer" && l.GetValue() == consumer {
					if got := m.GetGauge().GetValue(); got != 0 {
						t.Fatalf("kafka_consumer_up{consumer=%q} = %v, want 0", consumer, got)
					}
					return
				}
			}
		}
		t.Fatalf("kafka_consumer_up has no child series for consumer=%q", consumer)
	}
	t.Fatal("metric family data_service_kafka_consumer_up is not registered")
}
