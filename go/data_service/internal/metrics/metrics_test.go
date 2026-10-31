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

// TestPrimeRouteCreatesAllSeries 锁死映射持久化第 1a 批的两组告警前提:PrimeRoute 之后,
// route_register_total{outcome} 的 10 条与 route_publish_total{result} 的 7 条子序列**全部存在且为 0**。
// refused_unknown_row / legacy_conflict 这类结局可能一辈子只发生几次,序列不预先存在,
// `increase(...) > 0` 恰好漏掉第一次。
func TestPrimeRouteCreatesAllSeries(t *testing.T) {
	PrimeRoute()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]struct {
		label  string
		values []string
		count  int
	}{
		"data_service_route_register_total": {"outcome", routeRegisterOutcomes, 10},
		"data_service_route_publish_total":  {"result", routePublishResults, 7},
	}
	found := map[string]bool{}
	for _, mf := range families {
		spec, ok := want[mf.GetName()]
		if !ok {
			continue
		}
		found[mf.GetName()] = true
		seen := make(map[string]float64, len(mf.GetMetric()))
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == spec.label {
					seen[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
		if len(spec.values) != spec.count || len(seen) != spec.count {
			t.Fatalf("%s has %d series after priming (closed set lists %d), want %d: %v",
				mf.GetName(), len(seen), len(spec.values), spec.count, seen)
		}
		for _, v := range spec.values {
			got, ok := seen[v]
			if !ok {
				t.Fatalf("%s{%s=%q} missing after priming", mf.GetName(), spec.label, v)
			}
			if got != 0 {
				t.Fatalf("%s{%s=%q} = %v after priming, want 0", mf.GetName(), spec.label, v, got)
			}
		}
	}
	for name := range want {
		if !found[name] {
			t.Fatalf("metric family %s is not registered", name)
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

// TestSQLPoolCollectorDeclaresRouteOnlyUnderDBFirst 钉住 data_service 连接池类别的声明集合(G2b):
// redis 模式(includeRoute=false)只声明四个 store 的池,db_first 才多出 player_route —— 理由同 PrimeRoute,
// redis 模式下不装配 route store,不要一组恒为 0、看着像在跑的序列。常量 label sql_client="data_service"。
func TestSQLPoolCollectorDeclaresRouteOnlyUnderDBFirst(t *testing.T) {
	for _, tc := range []struct {
		includeRoute bool
		want         []string
	}{
		{includeRoute: false, want: []string{PoolSnapshot, PoolTransactionLog, PoolPlayerName}},
		{includeRoute: true, want: []string{PoolSnapshot, PoolTransactionLog, PoolPlayerName, PoolPlayerRoute}},
	} {
		reg := prometheus.NewPedanticRegistry()
		if err := reg.Register(NewSQLPoolCollector(nil, tc.includeRoute)); err != nil {
			t.Fatalf("includeRoute=%v: register: %v", tc.includeRoute, err)
		}
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("includeRoute=%v: gather: %v", tc.includeRoute, err)
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
					t.Fatalf("includeRoute=%v: pool %q has sql_client=%q, want data_service", tc.includeRoute, pool, client)
				}
				seen[pool] = true
			}
		}
		if len(seen) != len(tc.want) {
			t.Fatalf("includeRoute=%v: declared pools = %v, want %v", tc.includeRoute, seen, tc.want)
		}
		for _, pool := range tc.want {
			if !seen[pool] {
				t.Fatalf("includeRoute=%v: pool %q not declared (got %v)", tc.includeRoute, pool, seen)
			}
		}
	}
}

// TestPrimeRouteReadCreatesAllSeries 锁死第 3 批读方门禁告警(RouteReadSQLFallbackErrors 等,规格 §7 第 2 条)的前提:
// PrimeRouteRead 之后,route_read_total 会出现的 7 个 path × result 组合与 route_ready_verdict_total 的 8 个 verdict
// **全部存在且为 0**,且没有多出不会出现的组合(batch 没有 not_found_sql)。
// sql_error 可能一辈子只发生几次,序列不预先存在,`increase(...) > 0` 恰好漏掉第一次(同 TestPrimeRouteCreatesAllSeries)。
func TestPrimeRouteReadCreatesAllSeries(t *testing.T) {
	PrimeRouteRead()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	wantRead := map[string]bool{}
	for _, s := range routeReadSeries {
		wantRead[s[0]+"/"+s[1]] = true
	}
	if len(wantRead) != 7 {
		t.Fatalf("routeReadSeries lists %d distinct combinations, want 7", len(wantRead))
	}
	for _, absent := range []string{
		RouteReadPathBatch + "/" + RouteReadResultNotFoundSQL,
	} {
		if wantRead[absent] {
			t.Fatalf("%s can never happen and must not be primed", absent)
		}
	}
	wantVerdict := map[string]bool{}
	for _, v := range routeReadyVerdicts() {
		wantVerdict[v] = true
	}
	if len(wantVerdict) != 8 || !wantVerdict["trusted"] || !wantVerdict[RouteReadyVerdictScriptError] {
		t.Fatalf("route_ready_verdict_total closed set = %v, want the 7 placement verdicts + script_error", wantVerdict)
	}

	found := map[string]bool{}
	for _, mf := range families {
		var want map[string]bool
		switch mf.GetName() {
		case "data_service_route_read_total":
			want = wantRead
		case "data_service_route_ready_verdict_total":
			want = wantVerdict
		default:
			continue
		}
		found[mf.GetName()] = true
		seen := map[string]float64{}
		for _, m := range mf.GetMetric() {
			var path, result, verdict string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "path":
					path = l.GetValue()
				case "result":
					result = l.GetValue()
				case "verdict":
					verdict = l.GetValue()
				}
			}
			key := verdict
			if key == "" {
				key = path + "/" + result
			}
			seen[key] = m.GetCounter().GetValue()
		}
		if len(seen) != len(want) {
			t.Fatalf("%s has %d series after priming, want %d: %v", mf.GetName(), len(seen), len(want), seen)
		}
		for key := range want {
			got, ok := seen[key]
			if !ok {
				t.Fatalf("%s{%s} missing after priming", mf.GetName(), key)
			}
			if got != 0 {
				t.Fatalf("%s{%s} = %v after priming, want 0", mf.GetName(), key, got)
			}
		}
	}
	for _, name := range []string{"data_service_route_read_total", "data_service_route_ready_verdict_total"} {
		if !found[name] {
			t.Fatalf("metric family %s is not registered", name)
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

// TestPrimeRouteReconcileSeries 锁死对账 leader 告警的前提(规格 §13.1 第 9 条、§13.4 第 2 条):PrimeRouteReconcile 之后
// tick 6 条、event 15 条、push 4×10=40 条、mirror 4 条全部存在且为 0;task_rows 每个 phase 一条;装配型 gauge
// assembled=1、leader=0、arm_enabled 按入参;leader 观测型 gauge 不预置(只有现任 leader 导出)。
func TestPrimeRouteReconcileSeries(t *testing.T) {
	ClearRouteLeaderObservations()
	PrimeRouteReconcile(true)

	counters := []struct {
		family string
		labels []string
		want   map[string]bool
		count  int
	}{
		{"data_service_route_reconcile_tick_total", []string{"result"}, setOf(routeReconcileTickResults), 6},
		{"data_service_route_reconcile_event_total", []string{"event"}, setOf(routeReconcileEvents), 15},
		{"data_service_route_reconcile_mirror_total", []string{"result"}, setOf(routeReconcileMirrorResults), 4},
	}
	push := map[string]bool{}
	for _, p := range routeReconcilePhases {
		for _, r := range routeReconcilePushResults {
			push[p+"/"+r] = true
		}
	}
	counters = append(counters, struct {
		family string
		labels []string
		want   map[string]bool
		count  int
	}{"data_service_route_reconcile_push_total", []string{"phase", "result"}, push, 40})

	for _, c := range counters {
		if len(c.want) != c.count {
			t.Fatalf("%s closed set lists %d values, want %d", c.family, len(c.want), c.count)
		}
		metrics := gatherFamily(t, c.family)
		if len(metrics) != c.count {
			t.Fatalf("%s has %d series after priming, want %d", c.family, len(metrics), c.count)
		}
		for _, m := range metrics {
			key := labelKey(m, c.labels...)
			if !c.want[key] {
				t.Fatalf("%s{%s} is not in the closed set", c.family, key)
			}
			if v := m.GetCounter().GetValue(); v != 0 {
				t.Fatalf("%s{%s} = %v after priming, want 0", c.family, key, v)
			}
		}
	}
	for _, ev := range []string{"marker_dropped_disarmed", "arming_refused_debug_enabled", "lease_released_redis_unreachable"} {
		if !setOf(routeReconcileEvents)[ev] {
			t.Fatalf("event %q must be in the closed set (spec §13.1)", ev)
		}
	}
	if rows := gatherFamily(t, "data_service_route_reconcile_task_rows"); len(rows) != 4 {
		t.Fatalf("route_reconcile_task_rows has %d series after priming, want one per phase (4)", len(rows))
	}
	for name, want := range map[string]float64{
		"data_service_route_reconcile_assembled":   1,
		"data_service_route_reconcile_leader":      0,
		"data_service_route_reconcile_arm_enabled": 1,
	} {
		m := gatherFamily(t, name)
		if len(m) != 1 || m[0].GetGauge().GetValue() != want {
			t.Fatalf("%s after priming = %v, want exactly one series at %v", name, m, want)
		}
	}
	for _, name := range []string{"data_service_route_ready_marker", "data_service_route_writable",
		"data_service_route_cache_verified_age_seconds", "data_service_route_ready_selfcheck"} {
		if m := gatherFamily(t, name); len(m) != 0 {
			t.Fatalf("%s must not be primed (only the current leader exports it), got %d series", name, len(m))
		}
	}
}

// TestRouteLeaderObservationsClearedOnDemote(核实意见 should「gauge 与告警口径」):leader 观测型 gauge 只由现任 leader 导出;
// 掉主(ClearRouteLeaderObservations)之后四个族一条序列都不剩 —— 非 leader 的 0 与前任的旧值都会把 WritesFrozen / ReadyMissing 带偏。
// 自检 gauge 恰好一个 verdict 为 1。
func TestRouteLeaderObservationsClearedOnDemote(t *testing.T) {
	SetRouteLeaderObservations(true, false, 12.5, "identity_mismatch")
	if m := gatherFamily(t, "data_service_route_ready_marker"); len(m) != 1 || m[0].GetGauge().GetValue() != 1 {
		t.Fatalf("route_ready_marker = %v, want one series at 1", m)
	}
	if m := gatherFamily(t, "data_service_route_writable"); len(m) != 1 || m[0].GetGauge().GetValue() != 0 {
		t.Fatalf("route_writable = %v, want one series at 0", m)
	}
	if m := gatherFamily(t, "data_service_route_cache_verified_age_seconds"); len(m) != 1 || m[0].GetGauge().GetValue() != 12.5 {
		t.Fatalf("route_cache_verified_age_seconds = %v, want one series at 12.5", m)
	}
	sc := gatherFamily(t, "data_service_route_ready_selfcheck")
	if len(sc) != len(routeReadySelfCheckVerdicts()) || len(sc) != 8 {
		t.Fatalf("route_ready_selfcheck has %d series, want the 7 placement verdicts + script_error", len(sc))
	}
	ones := 0
	for _, m := range sc {
		if m.GetGauge().GetValue() == 1 {
			ones++
			if v := labelKey(m, "verdict"); v != "identity_mismatch" {
				t.Fatalf("route_ready_selfcheck{verdict=%q} = 1, want only identity_mismatch", v)
			}
		}
	}
	if ones != 1 {
		t.Fatalf("route_ready_selfcheck has %d verdicts at 1, want exactly 1", ones)
	}

	ClearRouteLeaderObservations()
	for _, name := range []string{"data_service_route_ready_marker", "data_service_route_writable",
		"data_service_route_cache_verified_age_seconds", "data_service_route_ready_selfcheck"} {
		if m := gatherFamily(t, name); len(m) != 0 {
			t.Fatalf("%s still has %d series after demotion; only the current leader may export it", name, len(m))
		}
	}
	// 装配型 gauge 不受影响:每个装配了 reconciler 的副本恒导出 leader 0/1。
	SetRouteReconcileLeader(false)
	if m := gatherFamily(t, "data_service_route_reconcile_leader"); len(m) != 1 || m[0].GetGauge().GetValue() != 0 {
		t.Fatalf("route_reconcile_leader must stay exported at 0 after demotion, got %v", m)
	}
}

func setOf(vals []string) map[string]bool {
	out := make(map[string]bool, len(vals))
	for _, v := range vals {
		out[v] = true
	}
	return out
}
