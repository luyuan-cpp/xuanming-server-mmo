// Package placement提供路由真源与提交前缀，业务域必须在获取角色P之前调用完整集合。
package placement

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
)

// ErrRoleRouteFenced 表示这名玩家的路由行不允许本次事务:没有归属、被冻结(搬库中),或预读之后被改过。
// 名字保留「Fenced」是调用方的既有判据;合服围栏已经删除(docs/design/player-data-tidb-scaling-plan.md §7.1),
// 合服不改路由行,所以它不再表示「合服中」。
var ErrRoleRouteFenced = errors.New("角色全局路由未允许本次事务")
var roleRouteSchemaPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

type roleOriginalRoute struct {
	player, revision uint64
	home             uint32
}

// LockRoleRoutes 预读完整路由集合,再按聚簇键 (route_shard, player_id) 升序逐行 FOR UPDATE,
// 锁住后复核 home / rev / frozen 与预读一致。返回后调用者才可取得全部角色行锁(P)。
//
// 每名玩家只锁自己的路由行,互不排队,这条路径不再有全局或按区共享的热点:
//   - 合服不改路由行(别名合服,docs/design/player-data-tidb-scaling-plan.md §7.1),所以不需要围栏:
//     合服过程中在源区建号也不丢,归属按 ResolveMergedZone 解析到目标区;
//   - route_meta 也移出了这条路径:可写开关管的是路由写者(RegisterHome、导入、修库),这里只读路由行、不改。
//
// 拿到的 home 是原始值。按区分组必须先解析成最终区,见 ResolveMergedZone / ResolveMergedZoneSQL。
//
// 拒绝一律回 ErrRoleRouteFenced:没有归属、被冻结(搬库中)、预读与锁住之后不一致。SQL 错误(含缺行)原样返回。
// 同一事务重入只允许同一完整玩家集合;禁止调用者持 P 之后再锁新的路由行或新增参与者(锁序会倒置)。
func LockRoleRoutes(ctx context.Context, tx *sql.Tx, schema string, players []uint64) error {
	if tx == nil || !roleRouteSchemaPattern.MatchString(schema) || len(players) == 0 {
		return ErrRoleRouteFenced
	}
	prefix := "`" + schema + "`."
	routes := make([]roleOriginalRoute, 0, len(players))
	seen := map[uint64]bool{}
	for _, player := range players {
		if player == 0 {
			return ErrRoleRouteFenced
		}
		if seen[player] {
			continue
		}
		seen[player] = true
		var home sql.NullInt64
		var revision uint64
		var frozen sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT home_zone,rev,frozen_run_id FROM "+prefix+"player_route WHERE route_shard=? AND player_id=?", RouteRowShard(player), player).Scan(&home, &revision, &frozen); err != nil {
			return err
		}
		if !allowedRoleRoute(home, revision, frozen) {
			return ErrRoleRouteFenced
		}
		routes = append(routes, roleOriginalRoute{player: player, home: uint32(home.Int64), revision: revision})
	}
	sort.Slice(routes, func(i, j int) bool {
		left, right := RouteRowShard(routes[i].player), RouteRowShard(routes[j].player)
		if left != right {
			return left < right
		}
		return routes[i].player < routes[j].player
	})
	for _, route := range routes {
		var home sql.NullInt64
		var revision uint64
		var frozen sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT home_zone,rev,frozen_run_id FROM "+prefix+"player_route WHERE route_shard=? AND player_id=? FOR UPDATE", RouteRowShard(route.player), route.player).Scan(&home, &revision, &frozen); err != nil {
			return err
		}
		if !home.Valid || home.Int64 != int64(route.home) || revision != route.revision || frozen.Valid {
			return ErrRoleRouteFenced
		}
	}
	return nil
}

// allowedRoleRoute 是「这一行路由允许事务」的唯一判据:有归属、rev 非 0、没有冻结。
func allowedRoleRoute(home sql.NullInt64, revision uint64, frozen sql.NullString) bool {
	return home.Valid && home.Int64 > 0 && home.Int64 <= int64(^uint32(0)) && revision != 0 && !frozen.Valid
}

// CheckRoleRoute 是 LockRoleRoutes 的不加锁形态:普通读这名玩家的路由行,判据相同(allowedRoleRoute)。
// 给不写角色资产的身份校验用(聊天)。它不和任何写者串行,读到的只是语句开始时已提交的值,提交点也不再复核,
// 所以不能拿它保护资产,要保护资产用 LockRoleRoutes。
//
// 拒绝回 ErrRoleRouteFenced;SQL 错误(含缺行)原样返回,与 LockRoleRoutes 同口径。
func CheckRoleRoute(ctx context.Context, q MergedZoneQueryer, schema string, player uint64) error {
	if q == nil || !roleRouteSchemaPattern.MatchString(schema) || player == 0 {
		return ErrRoleRouteFenced
	}
	var home sql.NullInt64
	var revision uint64
	var frozen sql.NullString
	if err := q.QueryRowContext(ctx, "SELECT home_zone,rev,frozen_run_id FROM `"+schema+"`.player_route WHERE route_shard=? AND player_id=?", RouteRowShard(player), player).Scan(&home, &revision, &frozen); err != nil {
		return err
	}
	if !allowedRoleRoute(home, revision, frozen) {
		return ErrRoleRouteFenced
	}
	return nil
}

// CheckRoleRouteContract 只读核实际使用的正式列与聚簇键顺序,不创建或修复其它服务的表。
func CheckRoleRouteContract(ctx context.Context, db *sql.DB, schema string) error {
	if db == nil || !roleRouteSchemaPattern.MatchString(schema) {
		return ErrRoleRouteFenced
	}
	prefix := "`" + schema + "`."
	columns, err := db.QueryContext(ctx, "SELECT route_shard,player_id,home_zone,rev,frozen_run_id FROM "+prefix+"player_route LIMIT 0")
	if err != nil {
		return err
	}
	if err = columns.Close(); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT column_name FROM information_schema.statistics WHERE table_schema=? AND table_name='player_route' AND index_name='PRIMARY' ORDER BY seq_in_index", schema)
	if err != nil {
		return err
	}
	var primaryKey []string
	for rows.Next() {
		var column string
		if err = rows.Scan(&column); err != nil {
			rows.Close()
			return err
		}
		primaryKey = append(primaryKey, column)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if strings.Join(primaryKey, ",") != "route_shard,player_id" {
		return ErrRoleRouteFenced
	}
	return nil
}
