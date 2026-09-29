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

// TestPrimeRollbackGuildCheckCreatesAllSeries 锁死 M1(docs/design/guild-phase2/07-rollback-fail-closed.md §7.9.1):
// PrimeRollbackGuildCheck 之后,rollback_guild_check_total 的 scope × result 封闭集合 27 条子序列**全部存在且为 0**。
//
// 放行回档(divergent_accepted)与写后分歧(post_write_*)是进程一辈子可能只发生一次的事件,
// 告警是 `increase(...) > 0`:序列在第一次 Inc 之前不存在的话,那一次恰好只有一个样本,告警是哑的。
func TestPrimeRollbackGuildCheckCreatesAllSeries(t *testing.T) {
	PrimeRollbackGuildCheck()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "data_service_rollback_guild_check_total" {
			continue
		}
		seen := make(map[string]float64, len(mf.GetMetric()))
		for _, m := range mf.GetMetric() {
			var scope, result string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "scope":
					scope = l.GetValue()
				case "result":
					result = l.GetValue()
				}
			}
			seen[scope+"/"+result] = m.GetCounter().GetValue()
		}
		if len(seen) != len(rollbackGuildScopes)*len(rollbackGuildResults) || len(seen) != 27 {
			t.Fatalf("rollback_guild_check_total has %d series after priming, want 27: %v", len(seen), seen)
		}
		for _, scope := range rollbackGuildScopes {
			for _, result := range rollbackGuildResults {
				v, ok := seen[scope+"/"+result]
				if !ok {
					t.Fatalf("series scope=%q result=%q missing after priming", scope, result)
				}
				if v != 0 {
					t.Fatalf("series scope=%q result=%q = %v after priming, want 0", scope, result, v)
				}
			}
		}
		if _, ok := seen[RollbackGuildScopePlayer+"/"+RollbackGuildResultDivergentAccepted]; !ok {
			t.Fatal(`series scope="player",result="divergent_accepted" missing`)
		}
		return
	}
	t.Fatal("metric family data_service_rollback_guild_check_total is not registered")
}
