package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// TestSQLPoolCollectorDeclaresDbPools 钉住 go/db 的连接池类别:没有任何池时(nil source)三类各输出一组序列,
// 常量 label sql_client="db"。告警 DbExtraStorePoolsBeyondPlan 按 pool="extra_zone" / "extra_non_zone" 写死,
// 类别名一改,告警会静默落空 —— 这条用例让改名先在这里红。
func TestSQLPoolCollectorDeclaresDbPools(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(NewSQLPoolCollector(nil)); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "mmorpg_sql_pool_pools" {
			continue
		}
		seen := map[string]float64{}
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
			if client != "db" {
				t.Fatalf("pool %q has sql_client=%q, want db", pool, client)
			}
			seen[pool] = m.GetGauge().GetValue()
		}
		want := []string{PoolHome, PoolExtraZone, PoolExtraNonZone}
		if len(seen) != len(want) {
			t.Fatalf("pools = %v, want exactly %v", seen, want)
		}
		for _, pool := range want {
			if v, ok := seen[pool]; !ok || v != 0 {
				t.Fatalf("mmorpg_sql_pool_pools{pool=%q} = %v (present=%v), want 0", pool, v, ok)
			}
		}
		if PoolExtraZone != "extra_zone" || PoolExtraNonZone != "extra_non_zone" {
			t.Fatalf("pool class names are pinned by deploy/k8s/mysql-connection-alerts.yaml, got %q / %q",
				PoolExtraZone, PoolExtraNonZone)
		}
		return
	}
	t.Fatal("mmorpg_sql_pool_pools was not emitted")
}

// TestConsumerGroupHandoffSeriesPrecreated 钉住交接闸指标的预建序列:告警 DbConsumerGroupHandoffOwnBehindPeer /
// DbConsumerGroupHandoffConflict / DbConsumerGroupHandoffSeedAborted 用 increase(),序列从无到 1 时会漏掉第一次,
// 所以 register() 之后这些序列必须已经以 0 存在。outcome 集合是修订稿 R5 / R12 之后的 8 个(含 wait_peer_state_unknown,
// 不含已删除的 peer_unreachable_own_complete)。
func TestConsumerGroupHandoffSeriesPrecreated(t *testing.T) {
	register()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	// 按名字索引(存下标,不必引入 client_model 的类型名)。
	byName := map[string]int{}
	for i, mf := range families {
		byName[mf.GetName()] = i
	}
	labelValues := func(name, label string) map[string]float64 {
		i, ok := byName[name]
		if !ok {
			t.Fatalf("%s was not registered", name)
		}
		mf := families[i]
		out := map[string]float64{}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label {
					out[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
		return out
	}

	conflictIdx, ok := byName["db_consumer_group_handoff_conflict_total"]
	if !ok || len(families[conflictIdx].GetMetric()) != 1 {
		t.Fatal("db_consumer_group_handoff_conflict_total must exist as a single unlabeled series")
	}
	conflict := families[conflictIdx]
	if v := conflict.GetMetric()[0].GetCounter().GetValue(); v != 0 {
		t.Fatalf("db_consumer_group_handoff_conflict_total = %v, want 0", v)
	}

	gates := labelValues("db_consumer_group_handoff_gate_total", "outcome")
	wantGates := []string{
		"no_peer_offsets", "peer_released", "wait_peer_holding", "wait_peer_rebalancing",
		"wait_peer_state_unknown", "wait_own_behind_peer", "wait_peer_unavailable", "wait_own_unavailable",
	}
	if len(gates) != len(wantGates) {
		t.Fatalf("gate outcomes = %v, want exactly %v", gates, wantGates)
	}
	for _, outcome := range wantGates {
		if v, ok := gates[outcome]; !ok || v != 0 {
			t.Fatalf("db_consumer_group_handoff_gate_total{outcome=%q} = %v (present=%v), want 0", outcome, v, ok)
		}
	}
	if _, ok := gates["peer_unreachable_own_complete"]; ok {
		t.Fatal("peer_unreachable_own_complete was removed (addendum R5) and must not be pre-created")
	}

	results := labelValues("db_consumer_group_handoff_total", "result")
	for _, result := range []string{"seeded", "own_present", "no_offsets", "aborted"} {
		if v, ok := results[result]; !ok || v != 0 {
			t.Fatalf("db_consumer_group_handoff_total{result=%q} = %v (present=%v), want 0", result, v, ok)
		}
	}
	if len(results) != 4 {
		t.Fatalf("handoff results = %v, want exactly 4", results)
	}

	for _, name := range []string{
		"db_consumer_group_rebalance_gap_seconds",
		"db_consumer_group_handoff_waiting_seconds",
		"db_consumer_group_handoff_wait_seconds",
	} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("%s was not registered", name)
		}
	}

	// 常量与 Help 一一对应(告警按这些字面量写):常量一改,这里先红。
	if HandoffGateWaitOwnBehindPeer != "wait_own_behind_peer" || HandoffSeedAborted != "aborted" {
		t.Fatalf("alert-pinned label values changed: %q / %q", HandoffGateWaitOwnBehindPeer, HandoffSeedAborted)
	}
}

// gatherByLabel 从注册表收集 family 的序列,按 label 的值索引(值取仪表或计数器)。
func gatherByLabel(t *testing.T, reg *prometheus.Registry, family, label string) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != label {
					continue
				}
				if g := m.GetGauge(); g != nil {
					out[l.GetValue()] = g.GetValue()
				} else if c := m.GetCounter(); c != nil {
					out[l.GetValue()] = c.GetValue()
				}
			}
		}
	}
	return out
}

// TestStoreCooldownCollector_EmitsAllPools(launch-todo §2.3 第 11 条):三类连接池每次都各出一条序列,
// 没有冷却记录的类别为 0;source 返回 nil map 时三类全为 0。告警按 pool 的取值写,少一条就会静默落空。
func TestStoreCooldownCollector_EmitsAllPools(t *testing.T) {
	cases := []struct {
		name   string
		source map[string]int
		want   map[string]float64
	}{
		{name: "home cooling", source: map[string]int{PoolHome: 2},
			want: map[string]float64{PoolHome: 2, PoolExtraZone: 0, PoolExtraNonZone: 0}},
		{name: "nil map", source: nil,
			want: map[string]float64{PoolHome: 0, PoolExtraZone: 0, PoolExtraNonZone: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewPedanticRegistry()
			source := tc.source
			if err := reg.Register(NewStoreCooldownCollector(func() map[string]int { return source })); err != nil {
				t.Fatalf("register: %v", err)
			}
			got := gatherByLabel(t, reg, "db_store_cooldown_stores", "pool")
			if len(got) != len(tc.want) {
				t.Fatalf("pools = %v, want %v", got, tc.want)
			}
			for pool, want := range tc.want {
				if v, ok := got[pool]; !ok || v != want {
					t.Fatalf("db_store_cooldown_stores{pool=%q} = %v (present=%v), want %v", pool, v, ok, want)
				}
			}
		})
	}
}

// fakeRedisPoolClient 返回固定的 PoolStats 与 Options。
type fakeRedisPoolClient struct {
	stats redis.PoolStats
	opts  redis.Options
}

func (f *fakeRedisPoolClient) PoolStats() *redis.PoolStats { s := f.stats; return &s }
func (f *fakeRedisPoolClient) Options() *redis.Options     { return &f.opts }

var redisPoolFamilies = []string{
	"db_redis_pool_hits_total", "db_redis_pool_misses_total", "db_redis_pool_timeouts_total", "db_redis_pool_wait_total",
	"db_redis_pool_wait_seconds_total", "db_redis_pool_unusable_total", "db_redis_pool_stale_conns_total",
	"db_redis_pool_conns", "db_redis_pool_idle_conns", "db_redis_pool_pending_requests", "db_redis_pool_max_conns",
}

// placement 与 main 是同一个对象(Placement.Redis 缺省)时只导出 redis_client=main 一份:汇总脚本跨 redis_client
// 求和,导出两份会把等待次数翻倍。
func TestRedisPoolCollector_SharedClientExportedOnce(t *testing.T) {
	shared := &fakeRedisPoolClient{stats: redis.PoolStats{Hits: 3, WaitCount: 5}, opts: redis.Options{PoolSize: 10}}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(NewRedisPoolCollector(shared, shared)); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, family := range redisPoolFamilies {
		got := gatherByLabel(t, reg, family, "redis_client")
		if len(got) != 1 {
			t.Fatalf("%s = %v, want exactly one series (redis_client=main)", family, got)
		}
		if _, ok := got[RedisClientMain]; !ok {
			t.Fatalf("%s = %v, want the series labelled redis_client=main", family, got)
		}
	}
	if v := gatherByLabel(t, reg, "db_redis_pool_wait_total", "redis_client")[RedisClientMain]; v != 5 {
		t.Fatalf("db_redis_pool_wait_total{main} = %v, want 5 (not doubled)", v)
	}
}

// 两个不同的 client 各出一组;计数、等待秒数(纳秒换算成秒)、池大小都按快照导出。nil 的 placement 不导出。
func TestRedisPoolCollector_SeparatePlacementClient(t *testing.T) {
	mainClient := &fakeRedisPoolClient{
		stats: redis.PoolStats{Hits: 7, Misses: 2, Timeouts: 1, WaitCount: 4, WaitDurationNs: 1_500_000_000,
			Unusable: 1, StaleConns: 3, TotalConns: 9, IdleConns: 6, PendingRequests: 2},
		opts: redis.Options{PoolSize: 10},
	}
	placementClient := &fakeRedisPoolClient{stats: redis.PoolStats{Hits: 11, WaitDurationNs: 250_000_000}, opts: redis.Options{PoolSize: 4}}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(NewRedisPoolCollector(mainClient, placementClient)); err != nil {
		t.Fatalf("register: %v", err)
	}
	expect := func(family, client string, want float64) {
		t.Helper()
		got, ok := gatherByLabel(t, reg, family, "redis_client")[client]
		if !ok || got != want {
			t.Fatalf("%s{redis_client=%q} = %v (present=%v), want %v", family, client, got, ok, want)
		}
	}
	expect("db_redis_pool_hits_total", RedisClientMain, 7)
	expect("db_redis_pool_hits_total", RedisClientPlacement, 11)
	expect("db_redis_pool_wait_seconds_total", RedisClientMain, 1.5)
	expect("db_redis_pool_wait_seconds_total", RedisClientPlacement, 0.25)
	expect("db_redis_pool_max_conns", RedisClientMain, 10)
	expect("db_redis_pool_max_conns", RedisClientPlacement, 4)
	expect("db_redis_pool_conns", RedisClientMain, 9)
	expect("db_redis_pool_idle_conns", RedisClientMain, 6)
	expect("db_redis_pool_pending_requests", RedisClientMain, 2)
	expect("db_redis_pool_stale_conns_total", RedisClientMain, 3)

	var nilPlacement *fakeRedisPoolClient
	reg2 := prometheus.NewPedanticRegistry()
	if err := reg2.Register(NewRedisPoolCollector(mainClient, nilPlacement)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := gatherByLabel(t, reg2, "db_redis_pool_hits_total", "redis_client"); len(got) != 1 {
		t.Fatalf("a nil placement client must not be exported, got %v", got)
	}
}

// writePathRedisSteps 钉住 v0.4 的固定列集合;条件路径可能跳过某步,cache_set 只用于读路径 SET NX。
var writePathRedisSteps = []string{
	"lock_acquire", "cursor_get", "epoch_get", "owner_epoch_probe", "placement_lookup", "expand_get", "placement_recheck",
	"lease_verify", "cache_publish", "cache_set", "applied_advance", "lock_release",
}

// TestRedisStepNamesPinned 钉住 12 个 step 的名字与顺序:tools/scripts/stress_summarize.ps1 的 2e 段按这些名字
// 与顺序出列,改名 / 合并 step 必须同改脚本与 test_stress_summarize.ps1。
func TestRedisStepNamesPinned(t *testing.T) {
	if len(RedisSteps) != 12 {
		t.Fatalf("RedisSteps has %d entries, want 12: %v", len(RedisSteps), RedisSteps)
	}
	seen := map[string]bool{}
	for _, step := range RedisSteps {
		if seen[step] {
			t.Fatalf("duplicate step %q in %v", step, RedisSteps)
		}
		seen[step] = true
	}
	for i, want := range writePathRedisSteps {
		if RedisSteps[i] != want {
			t.Fatalf("RedisSteps[%d] = %q, want %q (write-path order %v)", i, RedisSteps[i], want, writePathRedisSteps)
		}
	}
}

// TestRedisStepNamesMatchStressSummarize 读 stress_summarize.ps1 的 $redisSteps 列表,与 RedisSteps 逐项比对:
// 两边不一致时脚本整列静默变成 n/a,只有这里能先红。
func TestRedisStepNamesMatchStressSummarize(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$redisSteps\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$redisSteps list not found in %s", path)
	}
	var names []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		names = append(names, string(m[1]))
	}
	if len(names) != len(RedisSteps) {
		t.Fatalf("stress_summarize.ps1 lists %v, RedisSteps is %v", names, RedisSteps)
	}
	for i := range names {
		if names[i] != RedisSteps[i] {
			t.Fatalf("stress_summarize.ps1 step %d = %q, RedisSteps[%d] = %q", i, names[i], i, RedisSteps[i])
		}
	}
}

// TestPipelineCountersPrecreated:告警与汇总脚本用 increase() 读的计数器取值在 registerPipeline 之后必须已经存在。
func TestPipelineCountersPrecreated(t *testing.T) {
	registerPipeline()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	present := map[string]map[string]bool{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if present[mf.GetName()] == nil {
					present[mf.GetName()] = map[string]bool{}
				}
				present[mf.GetName()][l.GetName()+"="+l.GetValue()] = true
			}
			if len(m.GetLabel()) == 0 {
				if present[mf.GetName()] == nil {
					present[mf.GetName()] = map[string]bool{}
				}
				present[mf.GetName()][""] = true
			}
		}
	}
	want := map[string][]string{
		"db_ordering_guard_total": {"outcome=apply", "outcome=apply_no_cursor", "outcome=stale", "outcome=incomparable",
			"outcome=error", "outcome=stale_no_epoch", "outcome=epoch_override"},
		"db_ordering_lease_lost_total": {"reason=owner_changed", "reason=renew_exhausted", "reason=renew_deadline",
			"reason=verify_error"},
		"db_ordering_lock_release_total": {"outcome=released", "outcome=not_owner", "outcome=error",
			"outcome=skipped_sql_abandoned"},
		"db_ordering_lease_renew_retry_total": {""},
	}
	for family, series := range want {
		for _, s := range series {
			if !present[family][s] {
				t.Fatalf("%s{%s} was not pre-created", family, s)
			}
		}
	}
}

// TestFenceSeriesPrecreated 钉住 v0.4 的封闭词表与首抓取可见性,避免 critical 告警漏掉从无到 1 的第一次。
func TestFenceSeriesPrecreated(t *testing.T) {
	register()
	registerPipeline()

	var writes []string
	for _, class := range []string{"seq", "legacy_epoch", "legacy_zero"} {
		for _, outcome := range []string{"updated", "inserted", "unchanged", "equal", "older_seq", "stale_owner", "owner_epoch_regressed", "legacy_after_seq", "race", "error", "schema_regression"} {
			writes = append(writes, class+"|"+outcome)
		}
	}
	for _, outcome := range []string{"updated", "inserted", "unchanged", "error"} {
		writes = append(writes, "unfenced|"+outcome)
	}
	assertExactSeries(t, "db_fence_write_total", []string{"class", "outcome"}, writes, true)
	assertExactSeries(t, "db_fence_cache_publish_total", []string{"path", "result"}, []string{
		"write|written", "write|not_owner", "write|evidence_ahead", "write|evidence_conflict", "read|written", "read|exists",
	}, true)
	assertExactSeries(t, "db_fence_body_mismatch_total", nil, []string{""}, true)
	assertExactSeries(t, "db_fence_rejected_bytes_total", []string{"outcome"}, []string{
		"equal", "older_seq", "stale_owner", "owner_epoch_regressed", "legacy_after_seq", "epoch_override", "schema_regression",
	}, true)
	// G2 回退证据(docs/design/zone-redis-regression-guard-20261022.md §7):告警按绝对值 > 0 读,首抓取就必须有 0;
	// 取值经 SaveBarrierAck* 常量注册,这里用字面量钉住,常量一改名先红。
	assertExactSeries(t, "db_save_barrier_ack_ahead_total", []string{"kind"}, []string{"above_pending", "pending_missing"}, true)
	assertExactSeries(t, "db_owner_epoch_guard_total", []string{"outcome"}, []string{
		"match", "legacy_zero", "first", "advance", "stale", "read_error", "regressed",
	}, true)
	if OwnerEpochRegressed != "regressed" {
		t.Fatalf("owner counter regression label = %q, want regressed", OwnerEpochRegressed)
	}
	for _, family := range []struct {
		name    string
		labels  []string
		entries []string
	}{
		{"db_task_deferred_total", []string{"op", "reason"}, []string{"read|fence_race", "write|fence_race", "read|fence_evidence_conflict", "write|fence_evidence_conflict"}},
		{"db_task_dead_letter_total", []string{"reason"}, []string{"fence_stale_owner", "fence_legacy_after_seq", "owner_epoch_regressed", "persist_unstamped", "schema_regression"}},
		{"db_task_deferred_total", []string{"op", "reason"}, []string{"read|schema_too_new", "write|schema_too_new"}},
		{"db_task_read_abandoned_total", []string{"reason"}, []string{"schema_too_new"}},
	} {
		got, found := seriesValues(t, family.name, family.labels...)
		if !found {
			t.Fatalf("%s was not registered", family.name)
		}
		for _, key := range family.entries {
			if value, ok := got[key]; !ok || value != 0 {
				t.Fatalf("%s{%s} = %v (found=%v), want a pre-created 0", family.name, key, value, ok)
			}
		}
	}
}

// seriesValues 从默认注册表取 family 的全部序列,键是按 labels 顺序取出的 label 值用 "|" 连接(无 label 时为 ""),
// 值取计数器或仪表。family 不存在时 found=false。
func seriesValues(t *testing.T, family string, labels ...string) (values map[string]float64, found bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
		values = map[string]float64{}
		for _, m := range mf.GetMetric() {
			byName := map[string]string{}
			for _, l := range m.GetLabel() {
				byName[l.GetName()] = l.GetValue()
			}
			if len(byName) != len(labels) {
				t.Fatalf("%s has a series with labels %v, want exactly %v", family, byName, labels)
			}
			key := ""
			for i, name := range labels {
				v, ok := byName[name]
				if !ok {
					t.Fatalf("%s has a series without label %q: %v", family, name, byName)
				}
				if i > 0 {
					key += "|"
				}
				key += v
			}
			switch {
			case m.GetCounter() != nil:
				values[key] = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				values[key] = m.GetGauge().GetValue()
			}
		}
		return values, true
	}
	return nil, false
}

// assertExactSeries 断言 family 恰好有 want 这些序列(按 seriesValues 的键);requireZero 时每条都必须是 0
// (本包里没有别的用例会动它)。
func assertExactSeries(t *testing.T, family string, labels []string, want []string, requireZero bool) {
	t.Helper()
	got, found := seriesValues(t, family, labels...)
	if !found {
		t.Fatalf("%s was not registered", family)
	}
	if len(got) != len(want) {
		t.Fatalf("%s has %d series %v, want exactly %d: %v", family, len(got), got, len(want), want)
	}
	for _, key := range want {
		v, ok := got[key]
		if !ok {
			t.Fatalf("%s{%s} was not pre-created", family, key)
		}
		if requireZero && v != 0 {
			t.Fatalf("%s{%s} = %v, want a pre-created 0", family, key, v)
		}
	}
}

// TestRetryRoutingSeriesPrecreated 钉住重试路由与入队落盘确认的预建序列(docs/design/db-retry-owner-routing.md §11):
// 告警用 increase(),序列从无到 1 时会漏掉第一次,所以 register() 之后这些序列必须已经以 0 存在;
// 取值集合用字面量写死 —— deploy/k8s/db-task-retry-alerts.yaml 与 stress_summarize.ps1 的 2f 段按这些字符串匹配,
// 常量一改名,这里先红。数量:retry_route 2×6=12、ordering_lock_busy 2、durable_enqueue 2×4=8、retry_routing_mode 2。
func TestRetryRoutingSeriesPrecreated(t *testing.T) {
	register()

	var routes []string
	for _, mode := range []string{"shared", "owner"} {
		for _, action := range []string{"executed", "executed_nonowner", "forwarded", "requeued_revoked", "misrouted", "dead"} {
			routes = append(routes, mode+"|"+action)
		}
	}
	assertExactSeries(t, "db_retry_route_total", []string{"mode", "action"}, routes, true)
	assertExactSeries(t, "db_ordering_lock_busy_total", []string{"origin"}, []string{"kafka", "retry"}, true)

	var enqueues []string
	for _, target := range []string{"retry", "dead"} {
		for _, result := range []string{"ok", "not_durable", "error", "canceled"} {
			enqueues = append(enqueues, target+"|"+result)
		}
	}
	// 只断言存在:TestObserveDurableEnqueueSkipsCanceledInHistogram 会往这里记数(用例顺序不固定,如 -shuffle)。
	assertExactSeries(t, "db_durable_enqueue_total", []string{"target", "result"}, enqueues, false)
	// 两条都要存在(DbZoneNoPartitionClaims 数 mode="owner" 序列的条数);没人调 SetRetryRoutingMode 时都是 0。
	assertExactSeries(t, "db_retry_routing_mode", []string{"mode"}, []string{"shared", "owner"}, true)
	assertExactSeries(t, "db_claim_acquired_total", nil, []string{""}, true)
	if _, found := seriesValues(t, "db_owned_partitions"); !found {
		t.Fatal("db_owned_partitions was not registered")
	}

	// 常量与上面的字面量一一对应(用切片而不是 map:几个常量取值相同,作 map 键会编译不过)。
	for _, pin := range [][2]string{
		{RetryModeShared, "shared"}, {RetryModeOwner, "owner"},
		{RetryRouteExecuted, "executed"}, {RetryRouteExecutedNonOwner, "executed_nonowner"},
		{RetryRouteForwarded, "forwarded"}, {RetryRouteRequeuedRevoked, "requeued_revoked"},
		{RetryRouteMisrouted, "misrouted"}, {RetryRouteDead, "dead"},
		{OriginKafka, "kafka"}, {OriginRetry, "retry"},
		{DurableTargetRetry, "retry"}, {DurableTargetDead, "dead"},
		{DurableOK, "ok"}, {DurableNotDurable, "not_durable"}, {DurableError, "error"}, {DurableCanceled, "canceled"},
		{RetryQueueReady, "ready"}, {RetryQueuePartitioned, "partitioned"},
		{RetryQueueProcessing, "processing"}, {RetryQueueDead, "dead"},
	} {
		if pin[0] != pin[1] {
			t.Fatalf("alert / stress_summarize pinned label value changed: got %q, want %q", pin[0], pin[1])
		}
	}
}

// TestRetryRoutingNamesMatchStressSummarize 读 stress_summarize.ps1 2f 段的 $retryQueues / $retryActions,与本包常量
// 逐项比对:两边不一致时脚本整列静默变成 n/a,只有这里能先红。
func TestRetryRoutingNamesMatchStressSummarize(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	listOf := func(variable string) []string {
		t.Helper()
		block := regexp.MustCompile(`(?s)\$` + variable + `\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
		if block == nil {
			t.Fatalf("$%s list not found in %s", variable, path)
		}
		var names []string
		for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
			names = append(names, string(m[1]))
		}
		return names
	}
	for _, tc := range []struct {
		variable string
		want     []string
	}{
		{"retryQueues", []string{RetryQueueReady, RetryQueuePartitioned, RetryQueueProcessing, RetryQueueDead}},
		{"retryActions", retryRouteActions},
	} {
		got := listOf(tc.variable)
		if len(got) != len(tc.want) {
			t.Fatalf("stress_summarize.ps1 $%s = %v, want %v", tc.variable, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("stress_summarize.ps1 $%s[%d] = %q, want %q", tc.variable, i, got[i], tc.want[i])
			}
		}
	}
	for _, mode := range retryModes {
		if !regexp.MustCompile(`'db_retry_routing_mode'\s+'mode="` + mode + `"'`).Match(raw) {
			t.Fatalf("stress_summarize.ps1 does not read db_retry_routing_mode{mode=%q}", mode)
		}
	}
}

// canceled 只计数、不进直方图;其余结果两边都记。
func TestObserveDurableEnqueueSkipsCanceledInHistogram(t *testing.T) {
	register()
	histogramCount := func() uint64 {
		t.Helper()
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, mf := range families {
			if mf.GetName() != "db_durable_enqueue_seconds" {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "target" && l.GetValue() == DurableTargetDead {
						return m.GetHistogram().GetSampleCount()
					}
				}
			}
		}
		return 0
	}
	counter := func(result string) float64 {
		t.Helper()
		got, _ := seriesValues(t, "db_durable_enqueue_total", "target", "result")
		return got[DurableTargetDead+"|"+result]
	}

	beforeHist, beforeCanceled, beforeOK := histogramCount(), counter(DurableCanceled), counter(DurableOK)
	ObserveDurableEnqueue(DurableTargetDead, DurableCanceled, 3*time.Second)
	if histogramCount() != beforeHist {
		t.Fatal("a canceled enqueue must not be observed in db_durable_enqueue_seconds")
	}
	if counter(DurableCanceled) != beforeCanceled+1 {
		t.Fatal("a canceled enqueue must still be counted")
	}
	ObserveDurableEnqueue(DurableTargetDead, DurableOK, 10*time.Millisecond)
	if histogramCount() != beforeHist+1 || counter(DurableOK) != beforeOK+1 {
		t.Fatal("an ok enqueue must be counted and observed")
	}
}

// TestStrandedAndConsumerGroupErrorSeriesPrecreated 钉住 R7 复核新增的两个指标:db_retry_stranded_receipts(无 label 的
// 仪表)与 db_consumer_group_errors_total{source} 的三个取值。告警按这些名字与取值写,register() 之后必须已以 0 存在;
// 常量与字面量一一对应,改名先红。
func TestStrandedAndConsumerGroupErrorSeriesPrecreated(t *testing.T) {
	register()
	assertExactSeries(t, "db_retry_stranded_receipts", nil, []string{""}, false)
	assertExactSeries(t, "db_consumer_group_errors_total", []string{"source"}, []string{"heartbeat", "partition", "claim"}, false)
	for _, pin := range [][2]string{
		{ConsumerGroupErrorHeartbeat, "heartbeat"}, {ConsumerGroupErrorPartition, "partition"}, {ConsumerGroupErrorClaim, "claim"},
	} {
		if pin[0] != pin[1] {
			t.Fatalf("alert pinned label value changed: got %q, want %q", pin[0], pin[1])
		}
	}
}

// TestRouteReadySeriesPrimed 钉住 D1 第 3 批读方门禁的指标(spec_D1_batch1b2_2_3.md §13.2 第 1 条):PrimeRouteReady 之后
// db_route_ready_verdict_total 的 7 个 verdict 都以 0 存在、db_route_ready_gate 为 0;计数与仪表按调用变化;
// route_not_ready 进 db_task_deferred_total 的预建集合。verdict 用字面量写死:告警按这些字符串写,改名先红。
func TestRouteReadySeriesPrimed(t *testing.T) {
	verdicts := []string{"trusted", "absent", "malformed", "newer_version", "identity_unavailable", "not_master", "identity_mismatch"}
	PrimeRouteReady(verdicts)
	PrimeRouteReady(verdicts) // 可重复调用
	// 只断言存在(-count=N 时同一进程里前一轮已经计过数);仪表由 PrimeRouteReady 置 0。
	assertExactSeries(t, "db_route_ready_verdict_total", []string{"verdict"}, verdicts, false)
	assertExactSeries(t, "db_route_ready_gate", nil, []string{""}, true)

	before, _ := seriesValues(t, "db_route_ready_verdict_total", "verdict")
	CountRouteReadyVerdict("absent")
	CountRouteReadyVerdict("absent")
	SetRouteReadyGate(true)
	got, _ := seriesValues(t, "db_route_ready_verdict_total", "verdict")
	if got["absent"] != before["absent"]+2 || got["trusted"] != before["trusted"] {
		t.Fatalf("verdict counts = %v (before %v), want absent +2 and trusted unchanged", got, before)
	}
	gate, _ := seriesValues(t, "db_route_ready_gate")
	if gate[""] != 1 {
		t.Fatalf("db_route_ready_gate = %v after SetRouteReadyGate(true), want 1", gate)
	}
	SetRouteReadyGate(false)
	gate, _ = seriesValues(t, "db_route_ready_gate")
	if gate[""] != 0 {
		t.Fatalf("db_route_ready_gate = %v after SetRouteReadyGate(false), want 0", gate)
	}

	register()
	deferred, found := seriesValues(t, "db_task_deferred_total", "op", "reason")
	if !found {
		t.Fatal("db_task_deferred_total was not registered")
	}
	for _, op := range []string{"read", "write"} {
		if _, ok := deferred[op+"|route_not_ready"]; !ok {
			t.Fatalf("db_task_deferred_total{op=%q,reason=\"route_not_ready\"} was not pre-created", op)
		}
	}
	if DeferRouteNotReady != "route_not_ready" || PlacementRouteNotReady != "route_not_ready" {
		t.Fatalf("route_not_ready label values changed: defer=%q placement=%q", DeferRouteNotReady, PlacementRouteNotReady)
	}
}

// TestPlacementSeriesPrimed 钉住落点计数器的预置(deploy/k8s/player-placement-alerts.yaml 的「出现一次就报」四条告警据此
// 去掉 `unless … offset 10m` 第二支):PrimePlacement 之后 db_placement_guard_total 的 op×outcome 18 条、
// db_placement_store_open_total 的 3 条都已存在,且没有封闭集合之外的取值。取值用字面量写死:告警按这些字符串写,改名先红。
// 只断言存在(-count=N 时同一进程里前一轮可能已经计过数)。
func TestPlacementSeriesPrimed(t *testing.T) {
	PrimePlacement()
	PrimePlacement() // 可重复调用

	var guards []string
	for _, op := range []string{"read", "write"} {
		for _, outcome := range []string{"placed", "home", "legacy", "stale_topic", "frozen_deferred", "missing_required",
			"lookup_error", "recheck_moved", "route_not_ready"} {
			guards = append(guards, op+"|"+outcome)
		}
	}
	assertExactSeries(t, "db_placement_guard_total", []string{"op", "outcome"}, guards, false)
	assertExactSeries(t, "db_placement_store_open_total", []string{"result"}, []string{"ok", "error", "rejected"}, false)

	before, _ := seriesValues(t, "db_placement_guard_total", "op", "outcome")
	CountPlacementGuard("write", PlacementStaleTopic)
	got, _ := seriesValues(t, "db_placement_guard_total", "op", "outcome")
	if got["write|stale_topic"] != before["write|stale_topic"]+1 || got["write|placed"] != before["write|placed"] {
		t.Fatalf("placement guard counts = %v (before %v), want write|stale_topic +1 and write|placed unchanged", got, before)
	}
}
