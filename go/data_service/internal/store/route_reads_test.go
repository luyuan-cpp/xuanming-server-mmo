package store

import (
	"regexp"
	"strings"
	"testing"
)

// 第 3 批读方语句(route_reads.go 的 RD2–RD3)的形状守卫:只看文本,不连库。
// 执行语义(LEFT JOIN 的 NULL 口径、索引是否命中)由 route_reads_integration_test.go 在真库上证。

// routeReadSQLForTest 是本文件全部读语句,按编号索引。
func routeReadSQLForTest() map[string]string {
	return map[string]string{
		"RD2": routeScanByHomeSQL,
		"RD3": routeScanByStorageSQL,
	}
}

// TestRouteReadSQLIsReadOnly:读方语句一律是事务外一致读 —— 不加锁(FOR UPDATE / FOR SHARE / LOCK IN SHARE MODE)、
// 不写任何表。读方加锁会与 W1 的玩家行锁排队,把登录读拖进建角的锁等待里。
func TestRouteReadSQLIsReadOnly(t *testing.T) {
	write := regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|REPLACE|TRUNCATE|ALTER|DROP|CREATE)\b`)
	for name, stmt := range routeReadSQLForTest() {
		upper := strings.ToUpper(stmt)
		if !strings.HasPrefix(upper, "SELECT ") {
			t.Fatalf("%s must be a SELECT: %q", name, stmt)
		}
		for _, lock := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
			if strings.Contains(upper, lock) {
				t.Fatalf("%s must not take locks (%s): %q", name, lock, stmt)
			}
		}
		// 列名 updated_by / updated_at_ms 含 "updated",按词边界匹配不会误中;真出现写关键字就是写语句混进了读文件。
		if m := write.FindString(stmt); m != "" {
			t.Fatalf("%s contains the write keyword %q: %q", name, m, stmt)
		}
	}
}

// TestRouteScanByIndexSQLShape:RD2 / RD3 照提案 §11.2 G1a 模板 —— 等值前缀 + `a > x OR (a = x AND b > y)` 键集
// (不是行比较)、按聚簇键排序、带 LIMIT;选出的列与 ScanRoutes 相同(scanRouteRow withKey 的列序)。
func TestRouteScanByIndexSQLShape(t *testing.T) {
	for name, tc := range map[string]struct {
		stmt, column string
	}{
		"RD2": {routeScanByHomeSQL, "home_zone"},
		"RD3": {routeScanByStorageSQL, "storage_id"},
	} {
		for _, want := range []string{
			"SELECT route_shard, player_id, " + routeRowColumnsSQL + " FROM player_route",
			"WHERE " + tc.column + " = ? AND (route_shard > ? OR (route_shard = ? AND player_id > ?))",
			" ORDER BY route_shard, player_id LIMIT ?",
		} {
			if !strings.Contains(tc.stmt, want) {
				t.Fatalf("%s must contain %q: %q", name, want, tc.stmt)
			}
		}
		if strings.Contains(tc.stmt, "(route_shard, player_id) >") {
			t.Fatalf("%s must not use a row comparison (MySQL does not range-optimise it): %q", name, tc.stmt)
		}
		if got := strings.Count(tc.stmt, "?"); got != 5 {
			t.Fatalf("%s has %d placeholders, want 5: %q", name, got, tc.stmt)
		}
	}
}

// TestRouteReadsFileHasNoTableWrites 把 route_store_test.go 的两条 AST 守卫在本文件上的结论钉一遍:route_reads.go 的
// 字符串字面量里不得出现路由表的写语句(那两条守卫扫的是整个 module,这里只为让失败点直接指向读文件)。
func TestRouteReadsFileHasNoTableWrites(t *testing.T) {
	write := regexp.MustCompile("(?i)(INSERT\\s+INTO|UPDATE|DELETE\\s+FROM|REPLACE\\s+INTO|TRUNCATE\\s+TABLE)\\s+`?(" +
		PlayerRouteTableName + "|" + ZoneMergedIntoTableName + "|" +
		RouteMetaTableName + "|" + RouteReconcileTableName + ")\\b")
	for name, stmt := range routeReadSQLForTest() {
		if write.MatchString(stmt) {
			t.Fatalf("%s writes a route table: %q", name, stmt)
		}
	}
}
