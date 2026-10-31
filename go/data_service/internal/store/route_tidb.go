package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"shared/placement"
)

// TiDB-only 的路由表预切与防合并(2026-10-12 G1a,docs/design/player-route-mapping-durability-proposal.md
// §11.2「第 1a 批热点修复」;由 `route_tool tidb-prepare` 调用)。
//
// 为什么需要:player_route 改成聚簇 (route_shard, player_id) 之后,每条单调索引从 1 条写入尾部变成 64 条
// (placement.RouteRowShards,2026-10-13 由 16 改为 64);但空表只有 1 个 Region,开服首日数据量到不了按大小分裂的
// 阈值,64 条尾部仍压在同一个 Region leader 上 —— **只建片前缀、不预切,在 TiDB 上等于没做**。
//
// 为什么是显式运维命令而不是迁移里自动做:按 zone 预切要真实 zone 清单,data_service 的 yaml 里没有;
// 开新 zone、BR / PITR 恢复之后、TRUNCATE 之后(换表 ID,预切与 table attributes 都不跟过去)都要重放,
// 一个显式、幂等、可 dry-run 的命令最容易写进 runbook。MySQL 没有 Region,整步跳过。
//
// 先设 merge_option=deny 再切:免得刚切出的空 Region 被 PD 合回去。
//
// 【未核实,本批无法联网对照官方文档,由真 TiDB 用例取证】组合聚簇主键 / 多列索引的 SPLIT … BY 元组写法;
// 对已存在的边界再次 SPLIT 是否 no-op;ATTRIBUTES 是否覆盖索引 Region;SPLIT / ATTRIBUTES 需要的权限;
// SPLIT 结果集列(TOTAL_SPLIT_REGION, SCATTER_FINISH_RATIO);TIDB_PK_TYPE 列名;ALTER … ATTRIBUTES 是 DDL,
// 在线重跑是否让在途 W1 悲观事务以 8028 失败(W1 不重试 8028,那一次建角回 Unavailable,由客户端重试)。
// 任何一项不成立都只改本文件的语句生成,DDL 不需要动。
//
// 本文件只发 ALTER TABLE … ATTRIBUTES 与 SPLIT TABLE,不写任何一行数据(player_route 的写入只许在 route_store.go)。

// routeTiDBPKTypeSQL 是 tidb-prepare 的前置核对(T1):要按值切的表必须是聚簇主键。只读。
const routeTiDBPKTypeSQL = "SELECT TABLE_NAME, TIDB_PK_TYPE FROM information_schema.TABLES" +
	" WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN ('player_route')"

// routeTiDBClusteredTables 是 T1 要求为 CLUSTERED 的表(按名排序,报错顺序稳定)。
var routeTiDBClusteredTables = []string{PlayerRouteTableName}

// RouteTiDBPrepareRequest 是 tidb-prepare 的入参。
type RouteTiDBPrepareRequest struct {
	Zones    []uint32 // 必填,每个在 [1, RouteHomeZoneMax];去重升序后使用;上限 RouteBatchLimit 个
	Storages []uint32 // 可空,每个 ≥ 1;去重升序(Phase 2 PinOnCreate 指向 1000000 时传 1000000)
	DryRun   bool     // true:只判引擎、核对聚簇、列出语句,一条 ALTER / SPLIT 都不发
}

// RouteTiDBPrepareStep 是一条语句及其执行结果。
type RouteTiDBPrepareStep struct {
	SQL          string
	Executed     bool
	SplitRegions int64 // SPLIT 返回的首列(TOTAL_SPLIT_REGION);ALTER 为 0
}

// RouteTiDBPrepareReport:Engine 为 "mysql" 或 "tidb";MySQL 上 Skipped=true、Steps 为空。
// 中途失败时 Steps 仍是完整的语句清单,Executed 标出已经执行成功的那些(全部语句幂等,重跑即可)。
type RouteTiDBPrepareReport struct {
	Engine  string
	Version string
	Skipped bool
	Steps   []RouteTiDBPrepareStep
}

// SplitRegions 是各条 SPLIT 切出的 Region 数之和。
func (r RouteTiDBPrepareReport) SplitRegions() int64 {
	var n int64
	for _, st := range r.Steps {
		n += st.SplitRegions
	}
	return n
}

// PrepareTiDB:校验入参 → 判引擎(MySQL 直接 Skipped)→ 核对表是聚簇主键(ErrRouteTiDBNotClustered,
// dry-run 同样核对,只读)→ ATTRIBUTES ×1 → SPLIT ×(2 + zones + storages)。
// 入参非法一律 ErrRouteInvalidArgument,一条语句都不发。中途失败返回已执行的步骤 + error(带第几步与语句)。
func (s *RouteStore) PrepareTiDB(ctx context.Context, req RouteTiDBPrepareRequest) (RouteTiDBPrepareReport, error) {
	zones, storages, err := normalizeRouteTiDBPrepareIDs(req.Zones, req.Storages)
	if err != nil {
		return RouteTiDBPrepareReport{}, err
	}
	isTiDB, version, err := serverIsTiDB(ctx, s.db)
	if err != nil {
		return RouteTiDBPrepareReport{}, fmt.Errorf("tidb-prepare: %w", err)
	}
	if !isTiDB {
		return RouteTiDBPrepareReport{Engine: "mysql", Version: version, Skipped: true}, nil
	}
	rep := RouteTiDBPrepareReport{Engine: "tidb", Version: version}
	if err := assertRouteTablesClustered(ctx, s.db); err != nil {
		return rep, err
	}

	stmts := routeTiDBPrepareStatements(zones, storages)
	rep.Steps = make([]RouteTiDBPrepareStep, len(stmts))
	for i, q := range stmts {
		rep.Steps[i].SQL = q
	}
	if req.DryRun {
		return rep, nil
	}
	for i, q := range stmts {
		n, err := execRouteTiDBPrepareStep(ctx, s.db, q)
		if err != nil {
			return rep, fmt.Errorf("tidb-prepare step %d/%d: %s: %w", i+1, len(stmts), q, err)
		}
		rep.Steps[i].Executed = true
		rep.Steps[i].SplitRegions = n
	}
	return rep, nil
}

// normalizeRouteTiDBPrepareIDs 校验并规范化入参:zones 非空、每个 ∈ [1, RouteHomeZoneMax]、去重后不超过
// RouteBatchLimit 个;storages 每个 ≥ 1;两者都去重升序。非法一律 ErrRouteInvalidArgument(一条语句都不发)。
func normalizeRouteTiDBPrepareIDs(zones, storages []uint32) ([]uint32, []uint32, error) {
	if len(zones) == 0 {
		return nil, nil, fmt.Errorf("%w: tidb-prepare needs at least one zone", ErrRouteInvalidArgument)
	}
	for _, z := range zones {
		if z == 0 || z > RouteHomeZoneMax {
			return nil, nil, fmt.Errorf("%w: tidb-prepare zone %d not in [1, %d]", ErrRouteInvalidArgument, z, RouteHomeZoneMax)
		}
	}
	for _, sid := range storages {
		if sid == 0 {
			return nil, nil, fmt.Errorf("%w: tidb-prepare storage id must be >= 1", ErrRouteInvalidArgument)
		}
	}
	nz := sortedUniqueU32(zones)
	if len(nz) > RouteBatchLimit {
		// 每个 zone 一条 SPLIT;上限只为挡住误传(例如把 player_id 清单当 zone 传进来)。
		return nil, nil, fmt.Errorf("%w: tidb-prepare got %d zones, more than %d", ErrRouteInvalidArgument, len(nz), RouteBatchLimit)
	}
	return nz, sortedUniqueU32(storages), nil
}

// sortedUniqueU32 返回 in 去重升序后的新切片(不改 in);in 为空时回 nil。
func sortedUniqueU32(in []uint32) []uint32 {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// tidb-prepare 在 idx_route_home / idx_route_storage 上的切点密度(2026-10-13,行片 16 → 64 时一并定)。
//
// 这两条索引是「每个 zone / 每个落点值」各切一遍,Region 数随 zone 数成倍涨:每片一段的话是 64·Z + 64·S,
// 上百个 zone 就是数千到上万个靠 merge_option=deny 永久留住的空 Region。所以每个前缀值只切 routeTiDBPrefixSplitPoints
// 个点,每段装 routeTiDBPrefixSplitStride 片的写入尾部(容量模型 docs/design/capacity-model-hok-scale.md §4.1
// 理由 3:「把每 zone 预切改成 8 个点」;提案 §11.2 偏离 H8 预留的改法)。只改这里,不需要动 DDL。
const (
	// routeTiDBPrefixSplitPoints 是每个前缀值(一个 zone 或一个落点值)上的切点数,也就是它被切成的段数。
	routeTiDBPrefixSplitPoints uint32 = 8
	// routeTiDBPrefixSplitStride 是相邻两个切点隔的片数:切点落在片 0、stride、2·stride、…(64 片时是 0、8、…、56)。
	routeTiDBPrefixSplitStride = placement.RouteRowShards / routeTiDBPrefixSplitPoints
)

// 编译期断言:切点数必须整除行片数。整除时「间隔 × 切点数 = 行片数」,每段恰好 routeTiDBPrefixSplitStride 片、
// 切点恰好 routeTiDBPrefixSplitPoints 个;不整除时要么最后一段比别的段窄(切点数取 7:间隔 9,最后一段只剩 1 片),
// 要么切点数对不上(切点数取 24:间隔 2,循环切出 32 个点 —— 只断言「间隔整除行片数」拦不住这一种)。
// 不整除时下标不是 0,是「常量下标越界」的编译错误(切点数比行片数还多时余数就是行片数本身,同样越界);
// 切点数为 0 时上面的间隔是「除以零」的编译错误。
var _ = [1]struct{}{}[placement.RouteRowShards%routeTiDBPrefixSplitPoints]

// routeTiDBPrepareStatements 是纯函数:zones / storages 已规范化(去重升序、范围合法)。顺序即执行顺序:
//
//	ATTRIBUTES player_route(先防合并再切)
//	player_route 行数据:(s, 0),s = 1..63                               → 64 段,每片一段
//	idx_route_updated:(s, 0, 0),s = 1..63                               → 64 段,每片一段
//	idx_route_home:每个 zone 一条 (z, s, 0),s = 0、8、…、56            → 每 zone 8 段、每段 8 片(s=0 那个点同时隔开相邻 zone)
//	idx_route_storage:每个 storage 一条 (sid, s, 0),s = 0、8、…、56    → 每个落点值 8 段、每段 8 片
//
// 行数据与 idx_route_updated 为什么仍然每片一段:这两条的切点数是常数(各 63 个),不乘 zone 数;容量模型 §4.1
// 点名要降密度的只有「每 zone 预切」,提案 H8 担心的也只是 Z 上百时 idx_route_home 的空 Region 数。对这两条没有
// 现成的结论,取「每条写入尾部独占一个 Region」这一档(与 16 片时相同)。Region 总数 ≈ 63 + 63 + 8·Z + 8·S。
//
// 不切 idx_route_storage 的 NULL 前缀段(PinOnCreate=false 时新号 storage 为 NULL):`SPLIT … BY (NULL, …)` 的写法
// 未核实,且这一段要等 Phase 2 打开 PinOnCreate 后才有写入尾部,届时按 -storages 1000000 切。
// 片的上界只用 placement.RouteRowShards、间隔只用 routeTiDBPrefixSplitStride,不写字面量。
func routeTiDBPrepareStatements(zones, storages []uint32) []string {
	const route = "`" + PlayerRouteTableName + "`"
	stmts := []string{
		"ALTER TABLE " + route + " ATTRIBUTES 'merge_option=deny'",
	}

	tuples := make([]string, 0, placement.RouteRowShards)
	for sh := uint32(1); sh < placement.RouteRowShards; sh++ {
		tuples = append(tuples, "("+u32(sh)+", 0)")
	}
	stmts = append(stmts, "SPLIT TABLE "+route+" BY "+strings.Join(tuples, ", "))

	tuples = tuples[:0]
	for sh := uint32(1); sh < placement.RouteRowShards; sh++ {
		tuples = append(tuples, "("+u32(sh)+", 0, 0)")
	}
	stmts = append(stmts, "SPLIT TABLE "+route+" INDEX `idx_route_updated` BY "+strings.Join(tuples, ", "))

	prefixed := func(index string, lead uint32) string {
		parts := make([]string, 0, routeTiDBPrefixSplitPoints)
		for sh := uint32(0); sh < placement.RouteRowShards; sh += routeTiDBPrefixSplitStride {
			parts = append(parts, "("+u32(lead)+", "+u32(sh)+", 0)")
		}
		return "SPLIT TABLE " + route + " INDEX `" + index + "` BY " + strings.Join(parts, ", ")
	}
	for _, z := range zones {
		stmts = append(stmts, prefixed("idx_route_home", z))
	}
	for _, sid := range storages {
		stmts = append(stmts, prefixed("idx_route_storage", sid))
	}
	return stmts
}

func u32(v uint32) string { return strconv.FormatUint(uint64(v), 10) }

// assertRouteTablesClustered 是 T1:表在 TiDB 上必须是聚簇主键(TIDB_PK_TYPE = CLUSTERED,大小写不敏感)。
// 缺表或不是 → ErrRouteTiDBNotClustered,一条 ALTER / SPLIT 都不发:非聚簇表按主键值切毫无意义,
// 而聚簇属性建表后改不了。
func assertRouteTablesClustered(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, routeTiDBPKTypeSQL)
	if err != nil {
		return fmt.Errorf("tidb-prepare: read information_schema.TABLES.TIDB_PK_TYPE: %w", err)
	}
	defer rows.Close()
	got := make(map[string]string, len(routeTiDBClusteredTables))
	for rows.Next() {
		var name string
		var pkType sql.NullString
		if err := rows.Scan(&name, &pkType); err != nil {
			return fmt.Errorf("tidb-prepare: scan TIDB_PK_TYPE: %w", err)
		}
		got[strings.ToLower(name)] = pkType.String
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tidb-prepare: iterate TIDB_PK_TYPE: %w", err)
	}
	for _, name := range routeTiDBClusteredTables {
		pkType, ok := got[name]
		if !ok {
			return fmt.Errorf("%w: table %s is absent (run `data_service -f <yaml> -migrate` first)", ErrRouteTiDBNotClustered, name)
		}
		if !strings.EqualFold(pkType, "CLUSTERED") {
			return fmt.Errorf("%w: table %s has TIDB_PK_TYPE=%q", ErrRouteTiDBNotClustered, name, pkType)
		}
	}
	return nil
}

// execRouteTiDBPrepareStep 执行一条语句:ALTER 走 Exec(返回 0);SPLIT 有结果集,取首行首列解析成切出的 Region 数。
// 结果集按列数通用扫描(列名 / 列数随版本可能变);没有列、没有行、首列解析不了一律报错(fail-closed:
// 看不到结果就不能声称切过了)。
func execRouteTiDBPrepareStep(ctx context.Context, db *sql.DB, q string) (int64, error) {
	if !strings.HasPrefix(q, "SPLIT TABLE ") {
		_, err := db.ExecContext(ctx, q)
		return 0, err
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, fmt.Errorf("read SPLIT result columns: %w", err)
	}
	if len(cols) == 0 {
		return 0, errors.New("SPLIT returned no columns")
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("read SPLIT result: %w", err)
		}
		return 0, errors.New("SPLIT returned no rows")
	}
	vals := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return 0, fmt.Errorf("scan SPLIT result: %w", err)
	}
	n, err := strconv.ParseInt(string(vals[0]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("SPLIT result first column %q (%s) is not an integer: %w", string(vals[0]), cols[0], err)
	}
	return n, nil
}
