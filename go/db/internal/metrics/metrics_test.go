package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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

func TestPlacementStoreOpenSeriesPrimed(t *testing.T) {
	register()
	assertExactSeries(t, "db_placement_store_open_total", []string{"result"}, []string{StoreOpenOK, StoreOpenError, StoreOpenRejected}, false)
}
