package main

// 本文件是本工具读「玩家归属映射」的全部读法:读全局库 player_route,它是映射唯一的真源
// (docs/design/player-route-mapping-durability-proposal.md §15)。活区在 idx_route_home 上逐个跳取 home_zone,
// 玩家数在 idx_route_home 上一条 COUNT(*) 数完;按 id 点查走主键 (route_shard, player_id)(提案 §11.2 G1a「读方模板」)。
//
// 读失败一律报错,绝不把「没读到」当成「没有归属」;值形状不合规的条目计数并在报告里列出,不静默跳过。
//
// 本 module 不依赖 go/shared(避免 go.mod / go.sum 引入 shared 的整棵依赖树):行片数与 home_zone 上限镜像在这里,
// 由 route_source_test.go 的 TestMirroredRouteConstantsMatchSources 逐字核对源头。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// routeRowShards 镜像 shared/placement.RouteRowShards:player_route.route_shard = player_id % 64(chk_route_shard 钉住)。
const routeRowShards uint64 = 64

// maxHomeZone 镜像 player_route 的 chk_route_home(home_zone BETWEEN 1 AND 999999)。
// CHECK 在 MySQL < 8.0.16 / TiDB 未开 tidb_enable_check_constraint 时不生效,所以读方仍按它判畸形。
const maxHomeZone uint32 = 999999

// shapeSampleLimit 是报告里列出的畸形样本上限。
const shapeSampleLimit = 10

// routeSource 是各检查对「玩家归属」的全部读法。
type routeSource interface {
	// Describe 是报告里的 "route source: ..." 一行(不含口令)。
	Describe() string
	// LiveZones 枚举出现过的归属区。值形状不合规的条目不计入活区,计进 zoneShapeReport。
	LiveZones(ctx context.Context) (map[uint32]struct{}, zoneShapeReport, error)
	// CountPlayers 返回有归属记录的玩家数。
	CountPlayers(ctx context.Context) (int64, error)
	// HasHome 报告 playerID 是否有归属记录。error 时调用方整条检查判失败,绝不当成「没有归属」。
	HasHome(ctx context.Context, playerID uint64) (bool, error)
}

// zoneShapeReport 是 LiveZones 顺带做的值形状检查。
type zoneShapeReport struct {
	// Subject 说明计的是什么。
	Subject   string
	Scanned   int64
	Malformed int64
	Samples   []string
}

func (r *zoneShapeReport) addMalformed(sample string) {
	r.Malformed++
	if len(r.Samples) < shapeSampleLimit {
		r.Samples = append(r.Samples, sample)
	}
}

// routeTable 是用到的三条只读查询;sqlRouteTable 是生产实现,单测用假实现驱动 dbRouteSource 的循环与判定。
type routeTable interface {
	// NextHomeZone 返回不小于 from 的最小 home_zone(from 取 0 表示从头);没有了回 ok=false。
	// 参数取「不小于」而不是「大于 -1」:不让负数常量去比 int unsigned 列,两种引擎的有无符号比较口径不必再核对。
	NextHomeZone(ctx context.Context, from uint64) (zone uint32, ok bool, err error)
	// CountWithHome 返回 home_zone 非 NULL 的行数(含越界 zone)。
	CountWithHome(ctx context.Context) (int64, error)
	// HomeOf 点查 playerID 的 home_zone;无行或 home_zone 为 NULL 回 present=false。
	HomeOf(ctx context.Context, playerID uint64) (zone uint32, present bool, err error)
}

// SQL 按提案 §11.2 G1a「后续批次的读方模板」的索引口径。
//
//   - sqlNextHomeZone:在 idx_route_home 上跳着取下一个 home_zone(每次一个范围起点 + LIMIT 1,次数 = zone 数),
//     不用 SELECT DISTINCT:TiDB 没有 loose index scan,DISTINCT 会扫完整条索引;
//   - sqlCountWithHome:idx_route_home 上一次范围计数(MySQL 只扫索引不回表,TiDB 把 COUNT 下推到存储层)。
//     2026-10-20 之前按 zone 逐页拉主键(每页 2000 行)再在进程里数,大表要几千次往返,在单项 60s 预算内跑不完;
//     那时失败只报 warn,改成读失败报 fail 之后必须一起换成单条计数,否则大服每次都会 exit 1;
//   - sqlHomeOf:「按 id 回源」的不加锁形态,主键点查。
const (
	sqlNextHomeZone  = "SELECT home_zone FROM player_route WHERE home_zone >= ? ORDER BY home_zone LIMIT 1"
	sqlCountWithHome = "SELECT COUNT(*) FROM player_route WHERE home_zone IS NOT NULL"
	sqlHomeOf        = "SELECT home_zone FROM player_route WHERE route_shard = ? AND player_id = ?"
)

type sqlRouteTable struct {
	db *sql.DB
}

func (t sqlRouteTable) NextHomeZone(ctx context.Context, from uint64) (uint32, bool, error) {
	var zone uint32
	err := t.db.QueryRowContext(ctx, sqlNextHomeZone, from).Scan(&zone)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("next home_zone from %d: %w", from, err)
	}
	return zone, true, nil
}

func (t sqlRouteTable) CountWithHome(ctx context.Context) (int64, error) {
	var n int64
	if err := t.db.QueryRowContext(ctx, sqlCountWithHome).Scan(&n); err != nil {
		return 0, fmt.Errorf("count player_route rows with home_zone: %w", err)
	}
	return n, nil
}

func (t sqlRouteTable) HomeOf(ctx context.Context, playerID uint64) (uint32, bool, error) {
	var home sql.NullInt64
	err := t.db.QueryRowContext(ctx, sqlHomeOf, playerID%routeRowShards, playerID).Scan(&home)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("point read player_route player_id=%d: %w", playerID, err)
	}
	if !home.Valid {
		return 0, false, nil
	}
	return uint32(home.Int64), true, nil
}

type dbRouteSource struct {
	t      routeTable
	dbName string
}

func (s dbRouteSource) Describe() string {
	return fmt.Sprintf("route source: db (player_route in database %q)", s.dbName)
}

// homeZones 依次取出全部不同的 home_zone(含越界值,由调用方判)。返回值严格递增,否则报错(防排序规则 / 驱动异常时死循环)。
func (s dbRouteSource) homeZones(ctx context.Context) ([]uint32, error) {
	var zones []uint32
	var from uint64
	for {
		zone, ok, err := s.t.NextHomeZone(ctx, from)
		if err != nil {
			return nil, err
		}
		if !ok {
			return zones, nil
		}
		if uint64(zone) < from {
			return nil, fmt.Errorf("next home_zone from %d returned %d (not increasing)", from, zone)
		}
		zones = append(zones, zone)
		from = uint64(zone) + 1
	}
}

// LiveZones:home_zone 为 0 或大于 maxHomeZone 的取值是畸形(CHECK 未生效的库才可能出现),不计入活区。
func (s dbRouteSource) LiveZones(ctx context.Context) (map[uint32]struct{}, zoneShapeReport, error) {
	rep := zoneShapeReport{Subject: "distinct player_route.home_zone values"}
	zones, err := s.homeZones(ctx)
	if err != nil {
		return nil, rep, err
	}
	out := map[uint32]struct{}{}
	for _, z := range zones {
		rep.Scanned++
		if z == 0 || z > maxHomeZone {
			rep.addMalformed(fmt.Sprintf("home_zone=%d", z))
			continue
		}
		out[z] = struct{}{}
	}
	return out, rep, nil
}

// CountPlayers 数 home_zone 非 NULL 的行(含越界 zone)。
func (s dbRouteSource) CountPlayers(ctx context.Context) (int64, error) {
	n, err := s.t.CountWithHome(ctx)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("count player_route rows with home_zone returned %d", n)
	}
	return n, nil
}

func (s dbRouteSource) HasHome(ctx context.Context, playerID uint64) (bool, error) {
	_, present, err := s.t.HomeOf(ctx, playerID)
	return present, err
}
