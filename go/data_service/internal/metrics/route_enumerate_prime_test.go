package metrics

// PrimeRouteEnumerate(M1-6,2026-10-23):db_first 下 R7 一律读库、与读方开关无关,它的两条序列必须在开关关着时也预置,
// 否则 `increase(route_read_total{path="enumerate",result="sql_error"}[...]) > 0` 漏掉第一次。

import "testing"

// TestRouteEnumerateSeriesIsTheEnumerateSubset:筛出来的恰好是 routeReadSeries 里 enumerate 的 sql / sql_error 两条
// (取值的唯一出处是 routeReadSeries;enumerate 不会有 cache / not_found_sql)。纯函数,不碰注册表。
func TestRouteEnumerateSeriesIsTheEnumerateSubset(t *testing.T) {
	got := map[string]bool{}
	for _, s := range routeEnumerateSeries() {
		if s[0] != RouteReadPathEnumerate {
			t.Fatalf("routeEnumerateSeries returned a non-enumerate series %v", s)
		}
		got[s[1]] = true
	}
	if len(got) != 2 || !got[RouteReadResultSQL] || !got[RouteReadResultSQLError] {
		t.Fatalf("enumerate series = %v, want exactly sql and sql_error", got)
	}
}

// TestPrimeRouteEnumerateCreatesEnumerateSeries:调用之后 route_read_total{path="enumerate"} 的 sql / sql_error 两条都存在。
// 只断言存在(同一进程里别的用例可能已经预置过整组,-count=N 时前一轮也可能计过数);可重复调用。
func TestPrimeRouteEnumerateCreatesEnumerateSeries(t *testing.T) {
	PrimeRouteEnumerate()
	PrimeRouteEnumerate() // 可重复调用

	seen := map[string]bool{}
	for _, m := range gatherFamily(t, "data_service_route_read_total") {
		seen[labelKey(m, "path", "result")] = true
	}
	for _, result := range []string{RouteReadResultSQL, RouteReadResultSQLError} {
		if key := RouteReadPathEnumerate + "/" + result; !seen[key] {
			t.Fatalf("route_read_total{%s} missing after PrimeRouteEnumerate (got %v)", key, seen)
		}
	}
}
