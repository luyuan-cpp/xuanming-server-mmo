package store

import (
	"context"
	"fmt"

	"shared/placement"
)

// 路由表的只读接口:按 zone / 落点枚举
// (docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.2、§13.2)。
//
// 本文件**只读**:全部是事务外的一致读(不开事务、不加锁),不写任何表 —— 路由表的写入方仍只有 route_store.go
// (TestNoPlayerRouteWritesOutsideRouteStore / TestRouteMetaAndReconcileWritesOnlyInStoreFiles 用 AST 守住;
// 这里的语句也在它们的扫描范围内,写了就会被拦下)。
//
// SQL 照提案 §11.2 G1a「后续批次的读方模板」:
//   - 按 id 的读带行片(placement.RouteRowShard),走聚簇主键点查;
//   - 按 zone / 落点枚举走 idx_route_home / idx_route_storage,键集游标写成
//     `route_shard > ? OR (route_shard = ? AND player_id > ?)`(不用行比较:MySQL 不对行比较做 range 优化),
//     结果按聚簇键 (route_shard, player_id) 排序,**不是** player_id 序;
//   - 时间一律取库时钟(dbNowMsSQL)。

// 本文件的语句(编号 RD2–RD3,与 route_store.go 的 R1–R22、第 1b-2 批预留的 R23–R30 分开;RD1、RD4 随合服围栏删除)。
// 抽成常量是为了让契约用例 / 真库用例执行**同一段文本**。
const (
	// RD2 按 home zone 枚举(R7 GetAllPlayerIDsInZone、第 2 批 W10 的读形态):等值前缀 home_zone + 片前缀键集,
	// 走 idx_route_home (home_zone, route_shard, player_id),ORDER BY 与索引序一致、无 filesort。
	// 实参:zone、游标片、游标片、游标 player_id、limit。
	routeScanByHomeSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE home_zone = ? AND (route_shard > ? OR (route_shard = ? AND player_id > ?))" +
		" ORDER BY route_shard, player_id LIMIT ?"

	// RD3 按落点枚举(relocate 清单等):同 RD2,home_zone 换 storage_id,走 idx_route_storage。
	// 实参:storage、游标片、游标片、游标 player_id、limit。
	routeScanByStorageSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE storage_id = ? AND (route_shard > ? OR (route_shard = ? AND player_id > ?))" +
		" ORDER BY route_shard, player_id LIMIT ?"
)

// ── RD2 / RD3:按 zone / 落点键集翻页 ─────────────────────────────────────────────

// ScanRoutesByHome 按 home zone 键集翻页(RD2,事务外一致读):返回 home_zone = zone 且聚簇键 > after 的至多 limit 行,
// 按 (route_shard, player_id) 升序。下一页从 RouteScanCursorAt(最后一行) 接着翻,起点是零值游标;
// 一页不足 limit 行即翻完。
//
// zone 必须在 [1, RouteHomeZoneMax]、limit 在 [1, RouteBatchLimit]、after.Shard < placement.RouteRowShards,
// 否则 ErrRouteInvalidArgument、不发语句。返回的每一行都校验:库里存的片必须等于 player_id 的片
// (否则 ErrRouteShardMismatch)、home_zone 必须等于 zone、键必须严格大于 after 且严格递增 —— 任一不符整页作废
// (fail-closed:错的一页交给调用方,就是一份静默缺人或重复的名单)。
func (s *RouteStore) ScanRoutesByHome(ctx context.Context, zone uint32, after RouteScanCursor, limit int) ([]RouteRow, error) {
	if zone == 0 || zone > RouteHomeZoneMax {
		return nil, fmt.Errorf("%w: scan by home_zone %d: not in [1, %d]", ErrRouteInvalidArgument, zone, RouteHomeZoneMax)
	}
	return s.scanRoutesByIndex(ctx, "home_zone", routeScanByHomeSQL, uint64(zone), after, limit,
		func(row RouteRow) bool { return row.HomeZone == zone })
}

// ScanRoutesByStorage 按落点键集翻页(RD3):同 ScanRoutesByHome,条件换成 storage_id = storage(storage 必须非 0),
// 每一行校验 storage_id 等于 storage。
func (s *RouteStore) ScanRoutesByStorage(ctx context.Context, storage uint32, after RouteScanCursor, limit int) ([]RouteRow, error) {
	if storage == 0 {
		return nil, fmt.Errorf("%w: scan by storage_id 0", ErrRouteInvalidArgument)
	}
	return s.scanRoutesByIndex(ctx, "storage_id", routeScanByStorageSQL, uint64(storage), after, limit,
		func(row RouteRow) bool { return row.StorageID == storage })
}

// scanRoutesByIndex 是 RD2 / RD3 的公共部分。column 只进错误信息;matches 校验每一行确实满足等值条件。
func (s *RouteStore) scanRoutesByIndex(ctx context.Context, column, query string, key uint64, after RouteScanCursor, limit int,
	matches func(RouteRow) bool) ([]RouteRow, error) {
	if limit < 1 || limit > RouteBatchLimit {
		return nil, fmt.Errorf("%w: scan by %s limit %d not in [1, %d]", ErrRouteInvalidArgument, column, limit, RouteBatchLimit)
	}
	if after.Shard >= placement.RouteRowShards {
		// 片 >= 64 的游标会让查询静默回空页,调用方会当作翻完(同 ScanRoutes)。
		return nil, fmt.Errorf("%w: scan by %s cursor (shard, player_id)=(%d, %d): shard not in [0, %d)",
			ErrRouteInvalidArgument, column, after.Shard, after.PlayerID, placement.RouteRowShards)
	}
	rows, err := s.db.QueryContext(ctx, query, key, uint64(after.Shard), uint64(after.Shard), after.PlayerID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan player_route by %s=%d after (shard, player_id)=(%d, %d): %w",
			column, key, after.Shard, after.PlayerID, err)
	}
	defer rows.Close()

	out := make([]RouteRow, 0, limit)
	prev := after
	for rows.Next() {
		var row RouteRow
		if err := scanRouteRow(rows, &row, true); err != nil {
			return nil, fmt.Errorf("scan player_route row by %s=%d: %w", column, key, err)
		}
		if !matches(row) {
			return nil, fmt.Errorf("scan player_route by %s=%d returned player_id=%d with home_zone=%d storage_id=%d (query contract broken)",
				column, key, row.PlayerID, row.HomeZone, row.StorageID)
		}
		cur := RouteScanCursorAt(row)
		if !prev.Less(cur) {
			return nil, fmt.Errorf("scan player_route by %s=%d: row (shard, player_id)=(%d, %d) not after (%d, %d) (keyset order broken)",
				column, key, cur.Shard, cur.PlayerID, prev.Shard, prev.PlayerID)
		}
		prev = cur
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate player_route scan by %s=%d: %w", column, key, err)
	}
	if len(out) > limit {
		return nil, fmt.Errorf("scan player_route by %s=%d returned %d rows for limit %d", column, key, len(out), limit)
	}
	return out, nil
}
