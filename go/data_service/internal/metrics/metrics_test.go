package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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

// TestPrimeRouteCreatesAllSeries 锁死归属映射告警的前提:PrimeRoute 之后,route_register_total{outcome} 的 7 条与
// route_read_total{path,result} 的 5 条子序列**全部存在且为 0**,且没有多出不会出现的组合(batch 没有 not_found)。
// store_error 这类结局可能一辈子只发生几次,序列不预先存在,`increase(...) > 0` 恰好漏掉第一次。
func TestPrimeRouteCreatesAllSeries(t *testing.T) {
	PrimeRoute()

	wantRead := map[string]bool{}
	for _, s := range routeReadSeries {
		wantRead[s[0]+"/"+s[1]] = true
	}
	if len(wantRead) != 5 || wantRead[RouteReadPathBatch+"/"+RouteReadResultNotFound] {
		t.Fatalf("routeReadSeries = %v, want 5 combinations without batch/not_found", wantRead)
	}
	wantRegister := map[string]bool{}
	for _, outcome := range routeRegisterOutcomes {
		wantRegister[outcome] = true
	}
	if len(wantRegister) != 7 {
		t.Fatalf("routeRegisterOutcomes = %v, want 7 distinct outcomes", wantRegister)
	}

	for _, spec := range []struct {
		family string
		labels []string
		want   map[string]bool
	}{
		{"data_service_route_register_total", []string{"outcome"}, wantRegister},
		{"data_service_route_read_total", []string{"path", "result"}, wantRead},
	} {
		seen := gatherFamily(t, spec.family)
		if len(seen) != len(spec.want) {
			t.Fatalf("%s has %d series after priming, want %d", spec.family, len(seen), len(spec.want))
		}
		for _, m := range seen {
			key := labelKey(m, spec.labels...)
			if !spec.want[key] {
				t.Fatalf("%s{%s} is not in the closed set", spec.family, key)
			}
			if got := m.GetCounter().GetValue(); got != 0 {
				t.Fatalf("%s{%s} = %v after priming, want 0", spec.family, key, got)
			}
		}
	}
}

// TestPrimeKafkaConsumerCreatesAllSeries 锁死审计管道告警(deploy/k8s/data-service-audit-alerts.yaml)的前提:
// PrimeKafkaConsumer 之后,这条消费者的 messages_total 6 个 outcome、lag_probe_total 2 个 result、
// isolation_total 2 个 result **全部存在且为 0**。坏消息跳过 / 批量兜底停机这类结局可能一辈子只发生几次,
// 序列不预先存在,`increase(...) > 0` 恰好漏掉第一次(同 TestPrimeRouteCreatesAllSeries)。
// 用一个只属于本用例的 consumer 名,免得同包其它用例打过的点让"恰好为 0"失真。
func TestPrimeKafkaConsumerCreatesAllSeries(t *testing.T) {
	const consumer = "metrics_test_prime_consumer"
	PrimeKafkaConsumer(consumer)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]struct {
		label  string
		values []string
		count  int
	}{
		"data_service_kafka_consumer_messages_total":  {"outcome", kafkaConsumerOutcomes, 6},
		"data_service_kafka_consumer_lag_probe_total": {"result", kafkaLagProbeResults, 2},
		"data_service_kafka_consumer_isolation_total": {"result", kafkaIsolationResults, 2},
	}
	found := map[string]bool{}
	for _, mf := range families {
		spec, ok := want[mf.GetName()]
		if !ok {
			continue
		}
		found[mf.GetName()] = true
		seen := map[string]float64{}
		for _, m := range mf.GetMetric() {
			var mine bool
			var value string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "consumer":
					mine = l.GetValue() == consumer
				case spec.label:
					value = l.GetValue()
				}
			}
			if mine {
				seen[value] = m.GetCounter().GetValue()
			}
		}
		if len(spec.values) != spec.count || len(seen) != spec.count {
			t.Fatalf("%s{consumer=%q} has %d series after priming (closed set lists %d), want %d: %v",
				mf.GetName(), consumer, len(seen), len(spec.values), spec.count, seen)
		}
		for _, v := range spec.values {
			got, ok := seen[v]
			if !ok {
				t.Fatalf("%s{consumer=%q,%s=%q} missing after priming", mf.GetName(), consumer, spec.label, v)
			}
			if got != 0 {
				t.Fatalf("%s{consumer=%q,%s=%q} = %v after priming, want 0", mf.GetName(), consumer, spec.label, v, got)
			}
		}
	}
	for name := range want {
		if !found[name] {
			t.Fatalf("metric family %s is not registered", name)
		}
	}
}

// TestSetKafkaConsumerLagWritesAllThreeGauges 锁死 lag 探针一次成功探测写出的三条 gauge:合计积压、单分区最大积压、
// 保留期缺口,各落在自己的序列上(三个 int64 参数挨着,串位了编译器不会报)。DataServiceAuditLagHigh 比的是单分区最大值:
// broker 的 retention.bytes 按分区执行,拿合计去比会在积压集中于一个分区时漏报。
// 三条都不由 PrimeKafkaConsumer 预置:探针没成功过,就不该有一个看着像「积压为 0」的数。
func TestSetKafkaConsumerLagWritesAllThreeGauges(t *testing.T) {
	const consumer = "metrics_test_lag_consumer"
	SetKafkaConsumerLag(consumer, 1751, 1750, 7)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]float64{
		"data_service_kafka_consumer_lag_messages":               1751,
		"data_service_kafka_consumer_lag_max_partition_messages": 1750,
		"data_service_kafka_consumer_retention_gap_messages":     7,
	}
	found := map[string]bool{}
	for _, mf := range families {
		wantValue, ok := want[mf.GetName()]
		if !ok {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != "consumer" || l.GetValue() != consumer {
					continue
				}
				found[mf.GetName()] = true
				if got := m.GetGauge().GetValue(); got != wantValue {
					t.Fatalf("%s{consumer=%q} = %v, want %v", mf.GetName(), consumer, got, wantValue)
				}
			}
		}
	}
	for name := range want {
		if !found[name] {
			t.Fatalf("%s has no series for consumer=%q", name, consumer)
		}
	}
}

// TestSQLPoolCollectorDeclaresEveryStorePool 钉住 data_service 连接池类别的声明集合(G2b):四个 store 各一个池,
// 常量 label sql_client="data_service"。
func TestSQLPoolCollectorDeclaresEveryStorePool(t *testing.T) {
	want := []string{PoolSnapshot, PoolTransactionLog, PoolPlayerName, PoolPlayerRoute}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(NewSQLPoolCollector(nil)); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]bool{}
	for _, mf := range families {
		if mf.GetName() != "mmorpg_sql_pool_pools" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var pool, client string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "pool":
					pool = l.GetValue()
				case "sql_client":
					client = l.GetValue()
				}
			}
			if client != "data_service" {
				t.Fatalf("pool %q has sql_client=%q, want data_service", pool, client)
			}
			seen[pool] = true
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("declared pools = %v, want %v", seen, want)
	}
	for _, pool := range want {
		if !seen[pool] {
			t.Fatalf("pool %q not declared (got %v)", pool, seen)
		}
	}
}

// gatherFamily 按名字取一个指标族;不存在回 nil(没有任何子序列的 *Vec 不出现在 Gather 结果里)。
func gatherFamily(t *testing.T, name string) []*dto.Metric {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == name {
			return mf.GetMetric()
		}
	}
	return nil
}

// labelKey 把一条序列的 label 值按名字顺序拼成 "a/b"(只取 names 里列的)。
func labelKey(m *dto.Metric, names ...string) string {
	vals := make([]string, len(names))
	for _, l := range m.GetLabel() {
		for i, n := range names {
			if l.GetName() == n {
				vals[i] = l.GetValue()
			}
		}
	}
	return strings.Join(vals, "/")
}
