package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"shared/placement"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// 玩家归属与落点映射的真源(docs/design/player-route-mapping-durability-proposal.md §15):
// player_route 只在全局库一份,data_service 直接 SQL 读写,提交即完成。
//
// 路由表(建表语句在 schema.go 的 bootstrapTables):
//   - player_route:每个玩家一行,home_zone 与落点同行;rev 本行任何列变化 +1,是行版本。
//     聚簇主键 (route_shard, player_id),route_shard = placement.RouteRowShard(player_id)(G1a,TiDB 写热点修复):
//     **按 player_id 的每一条读写都必须同时带 route_shard**,片一律经 placement.RouteRowShard 算;
//   - zone_merged_into:合服映射(源区 → 目标区)的永久记录,由 merge_zone 的合服 / 撤销写入与删除;
//     别名合服之后它就是合服的全部写入:玩家行和路由行都不动(docs/design/player-data-tidb-scaling-plan.md §7.1);
//   - route_meta:开关表,单行(id=1),writable=0 = 全体映射写者拒写;很少写,冻结与解冻走 shared/authoritysql 的 Catalog。
//
// 写入方:data_service 进程内只有本文件(route_store_test.go 的 TestNoPlayerRouteWritesOutsideRouteStore /
// TestRouteMetaWritesOnlyInStoreFiles 用 AST 机械守住);进程外只有 tools/merge_zone/route_store.go
// (它自己的 TestNoRouteTableWritesOutsideMergeRouteStore)。
//
// 行片 rowShard = player_id % placement.RouteRowShards(聚簇主键首列,常量 64,建表后不可改)。
//
// 不变量,按重要性排序:
//  1. **判定只认加锁读**(R5 的 FOR UPDATE)。新行一律 ODKU no-op 插入、撞键 = 受影响 0 行,
//     **不用 INSERT IGNORE**:它会把 CHECK 违例降成 warning,坏值就静默进库了(提案 §4.1-3)。
//  2. **锁顺序**:route_meta(普通读)→ player_route(按聚簇键 (route_shard, player_id);ODKU 或 FOR UPDATE)。W1 只等最多一个玩家行。
//     W1 与建角的初始提交对 route_meta 只做普通读、从不持它的锁(每笔业务事务都锁这一行就是全服排队,
//     提交点的保证靠写者闸门,见 lockInitialPlayerRoute);只有低频的建池探针才锁它,方向同样是 route_meta → player_route。
//  3. **会话级 RC + 锁等待 1s**(buildRouteDSN,建 store 时探针读回校验):锁推演只在 RC 下成立;
//     锁等待封顶后,ctx 到期的请求最多再占 1s 行锁。
//  4. **1213 / 9007 有界重试,1205 不重试**:前两者整个事务已被回滚、重跑安全;1205 已经等满了封顶的 1s,
//     再等一轮只会白白吃掉服务端 2s 的预算,结论不会变(guild 先例 guild_manage_repo.go)。
//  5. 时间戳一律取库时钟(dbNowMsSQL);NOW() 与 UNIX_TIMESTAMP() 都按会话时区算、互相抵消,
//     结果就是真实 epoch 毫秒(只有夏令时切换的那一小时有歧义,Asia/Shanghai 与 UTC 都没有夏令时)。
//
// 前提(与 player_name 同口径,任何一条不成立判定都会安静地错):连接**不**带 CLIENT_FOUND_ROWS
// (buildDSN 不设,TestRouteDSNSetsReadCommittedAndLockWaitWithoutClientFoundRows 守住);binlog_format = ROW
// (RC 在 STATEMENT 格式下会报 1665;本批不探测,与其它 store 同口径)。

const (
	// PlayerRouteTableName 等是路由表名(= proto 的 OptionTableName);建表语句在 schema.go,
	// 形状守卫的事实源是 rollback_database_table.proto 的同名 message(schema.go 的 routeTableMessages)。
	PlayerRouteTableName    = "player_route"
	ZoneMergedIntoTableName = "zone_merged_into"
	RouteMetaTableName      = "route_meta"

	// RouteHomeZoneMax 是 home_zone 的上限,= playerRouteBootstrapDDL 里 chk_route_home 的 999999
	// (与 placement.MaxZoneStorageID 同值:zone id 与 zone 库编号是同一个空间)。
	// 应用层在写之前就拒绝,CHECK 只兜底(TiDB < 7.2 或没开 tidb_enable_check_constraint 时 CHECK 不生效)。
	RouteHomeZoneMax uint32 = 999999

	// RouteBatchLimit 是 GetRoutes 单批上限:一条无上限的 IN 会把单次查询变成事实上的全表读。
	// 占位符:R13 每批 2×N 个(行构造器 (route_shard, player_id) 成对);N = 1000 时最多 2000 个,
	// 远低于 MySQL 预处理语句 65535 的上限。
	RouteBatchLimit = 1000

	// routeMetaRowID 是 route_meta 唯一那一行的 id(R1 / R15 里写死的 1)。
	routeMetaRowID = 1

	// routeTxAttempts 事务尝试上限:1 次 + 1213/9007 重试 3 次。
	// 1205 不在其中(见文件头不变量 4)。最坏多睡 3 × routeLockRetryBackoffMax = 150ms。
	routeTxAttempts = 4

	// routeLockWaitTimeoutSec 会话级 innodb_lock_wait_timeout,由 buildRouteDSN 钉死,探针读回校验。
	routeLockWaitTimeoutSec = 1

	// routeLockRetryBackoffMin / Max:1213/9007 重来之前的抖动等待(jitteredBackoff,可被 ctx 取消)。
	routeLockRetryBackoffMin = 10 * time.Millisecond
	routeLockRetryBackoffMax = 50 * time.Millisecond

	// routeProbeTimeout 是建 store 时探针(会话变量、TiDB 事务模式、route_meta 行、chk_route_shard 实测与模数核对)的总时限,
	// 五步共用(含 P4 + P5 遇 1213 / 9007 的重跑与退避)。
	routeProbeTimeout = 5 * time.Second

	// routeWriterMaxLen = player_route.updated_by 的列宽 varchar(96)。
	routeWriterMaxLen = 96

	// tidbErrWriteConflict 9007:TiDB 写冲突,事务已被整体回滚,与 1213 同样可重跑
	// (与 tools/merge_zone/merge_run.go、go/guild 的事务重试判定同口径)。
	tidbErrWriteConflict = 9007

	// mysqlErrCheckConstraintViolated 3819 ER_CHECK_CONSTRAINT_VIOLATED:探针 P4 期望收到的那一个。
	mysqlErrCheckConstraintViolated = 3819
	// mysqlErrTableAccessDenied 1142 ER_TABLEACCESS_DENIED_ERROR:账号对表没有该权限(P4 / P5 要 player_route 的 INSERT 权限)。
	mysqlErrTableAccessDenied = 1142
)

var (
	// ErrRouteInvalidArgument 入参在库层就被拒:player_id=0、home_zone 越界、一批 id 超限等。
	// 纵深防御:路由层也会先挡一遍,但写进库的垃圾行没有人会再去清。
	ErrRouteInvalidArgument = errors.New("player_route: invalid argument")
	// ErrRouteMetaMissing route_meta 没有 id=1 那一行(迁移没跑过,或库被重置后没重跑迁移)。
	ErrRouteMetaMissing = errors.New("route_meta: row id=1 missing; run `data_service -f <yaml> -migrate`")
	// ErrRouteNotWritable route_meta.writable != 1:映射写者被冻结(维护流程经 Catalog.Freeze 置 0)。
	ErrRouteNotWritable = errors.New("route_meta: writable!=1, route writers are frozen")
	// ErrRouteRowVanished ODKU 撞了键(受影响 0 行),紧接着的加锁读却读不到这一行。
	// 没有删 player_route 的写者,出现即说明有人绕过本 store 删行,fail-closed。
	ErrRouteRowVanished = errors.New("player_route: row vanished between ON DUPLICATE KEY and the locking read")
	// ErrRouteSessionMisconfigured 探针读回的会话隔离级别 / 锁等待 / TiDB 事务模式不符合要求,拒绝建 store。
	ErrRouteSessionMisconfigured = errors.New("route store: session isolation / lock wait / txn mode not as required")
	// ErrRouteCheckNotEnforced 探针 P4 证不了 chk_route_shard 生效(插入成功,或被别的约束拒绝),拒绝建 store。
	// player_id 的唯一性靠这条 CHECK(主键是 (route_shard, player_id)),它不生效时一个算错片的写者就能给同一个
	// player_id 写出两行,而按正确片点查的读者看不到另一行(见 probeRouteShardCheck)。
	ErrRouteCheckNotEnforced = errors.New("player_route: CHECK constraint chk_route_shard is not enforced " +
		"(player_id uniqueness depends on it; needs MySQL >= 8.0.16, or TiDB >= 7.2 with " +
		"`SET GLOBAL tidb_enable_check_constraint=ON` before the table is created)")
	// ErrRouteShardModulusMismatch 正向探针 P5 证明表上 chk_route_shard 的模数不是 placement.RouteRowShards:表是按另一个
	// 模数建的(例如跑过行片还是 16 的 d5cbb8a8e 的迁移),拒绝建 store(见 probeRouteShardModulus)。重跑迁移修不好它
	// (CREATE TABLE IF NOT EXISTS 不改已有的表);也不能改常量迁就旧表 —— 模数是聚簇主键的一部分,改它 = 整表重写。
	ErrRouteShardModulusMismatch = errors.New("player_route: the route_shard modulus in CHECK chk_route_shard differs from " +
		"this binary's placement.RouteRowShards (the table was created with another modulus); after confirming the route " +
		"tables hold no data worth keeping, DROP TABLE player_route, zone_merged_into, route_meta " +
		"by hand (proposal §11.2 HR4), then rerun `data_service -f <yaml> -migrate`; " +
		"do not change the constant to fit the old table")
	// ErrRouteShardMismatch 查回来的一行存的 route_shard 不等于 placement.RouteRowShard(player_id):只有 CHECK 曾经
	// 不生效时被绕过 store 的写者写进来。fail-closed,整批作废。这是纵深防御:按 id 的点读(R5 / R13)按正确片查,
	// 真库上错片行对它们表现为缺席。
	ErrRouteShardMismatch = errors.New("player_route: stored route_shard does not match placement.RouteRowShard(player_id)")
	// ErrRouteTiDBNotClustered tidb-prepare 的前置核对失败:player_route 在 TiDB 上不是聚簇主键。
	// 非聚簇的表按 (route_shard, player_id) 切毫无意义,而且建表后改不了,只能趁表空 DROP 后用 -migrate 重建。
	ErrRouteTiDBNotClustered = errors.New("route tables are not CLUSTERED on TiDB; while they are still empty, " +
		"DROP them and rebuild with `data_service -f <yaml> -migrate`")
)

// dbNowMsSQL 是库时钟的 epoch 毫秒。所有时间戳都取它,不取任何一台应用机的本地时钟。
const dbNowMsSQL = "CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED)"

// routeRowColumnsSQL 是 RouteRow 除聚簇键 (route_shard, player_id) 外的列,顺序与 scanRouteRow 的 Scan 目标一一对应。
const routeRowColumnsSQL = "home_zone, storage_id, placement_version, frozen_run_id, rev, updated_by, created_at_ms, updated_at_ms"

// 本 store 的全部 SQL(G1a 起按 player_id 的读写都带 route_shard)。
// 抽成常量是为了让契约用例 / 真库用例执行**同一段文本**。
const (
	// 探针:会话变量与 TiDB 事务模式。
	routeProbeSessionSQL     = "SELECT @@SESSION.transaction_isolation, @@SESSION.innodb_lock_wait_timeout"
	routeProbeTiDBTxnModeSQL = "SELECT @@tidb_txn_mode"

	// P4 探针:一行**只违反 chk_route_shard** 的插入(player_id=0 → 片应为 0,这里绑 1;其余列都合法:
	// home / 落点 / 冻结全 NULL、rev=1),包在必然回滚的事务里(probeRouteShardCheck)。
	routeProbeShardCheckSQL = "INSERT INTO player_route (route_shard, player_id, rev, updated_by, created_at_ms, updated_at_ms)" +
		" VALUES (1, 0, 1, 'route-probe', 0, 0)"

	// P5 正向探针:一行**按 placement.RouteRowShards 合法**的插入(实参:行片、player_id,取自 routeShardModulusProbeKeys;
	// 其余列与 P4 相同:home / 落点 / 冻结全 NULL、rev=1,满足 player_route 上其余五条 CHECK 与全部 NOT NULL),
	// 与 P4 在同一个必然回滚的事务里(probeRouteShardModulus)。
	routeProbeShardModulusSQL = "INSERT INTO player_route (route_shard, player_id, rev, updated_by, created_at_ms, updated_at_ms)" +
		" VALUES (?, ?, 1, 'route-probe', 0, 0)"

	// R1 读可写开关(普通一致读)。建 store 的探针也用它确认 meta 行在。
	routeReadWritableSQL = "SELECT writable FROM route_meta WHERE id = 1"
	// 建池探针的守卫(lockRouteProbeGuard):同一行,加锁读。
	routeLockProbeGuardSQL = routeReadWritableSQL + " FOR UPDATE"

	// R4 新行。没有落点时 storage / version 都绑 NULL。受影响 1 = 插入,0 = 撞键 no-op。
	// 实参:行片、player_id、home、storage、version、writer。
	routeInsertNewSQL = "INSERT INTO player_route (route_shard, player_id, home_zone, storage_id, placement_version, frozen_run_id," +
		" rev, updated_by, created_at_ms, updated_at_ms)" +
		" VALUES (?, ?, ?, ?, ?, NULL, 1, ?, " + dbNowMsSQL + ", " + dbNowMsSQL + ")" +
		" ON DUPLICATE KEY UPDATE player_id = player_id"

	// R5 加锁读(W1 的判定只认它)。按聚簇键点查:行片、player_id。
	routeLockRowSQL = "SELECT " + routeRowColumnsSQL + " FROM player_route WHERE route_shard = ? AND player_id = ? FOR UPDATE"

	// R6a 只补 home(落点已在,或请求不带 storage)。rev 做 CAS:行已被 R5 锁住,不是恰好 1 行就是不可能状态。
	// 实参:home、writer、行片、player_id、rev。
	routeFillHomeSQL = "UPDATE player_route SET home_zone = ?, rev = rev + 1, updated_by = ?, updated_at_ms = " + dbNowMsSQL +
		" WHERE route_shard = ? AND player_id = ? AND rev = ? AND home_zone IS NULL"

	// R6b 补 home + 落点(落点为 NULL 且请求带 storage)。实参:home、storage、writer、行片、player_id、rev。
	routeFillHomePlacementSQL = "UPDATE player_route SET home_zone = ?, storage_id = ?, placement_version = 1, rev = rev + 1," +
		" updated_by = ?, updated_at_ms = " + dbNowMsSQL +
		" WHERE route_shard = ? AND player_id = ? AND rev = ? AND home_zone IS NULL AND storage_id IS NULL"

	// R13 按 id 批量读的头部,由 routeSelectRowsSQL 拼。
	// 用行构造器 (route_shard, player_id) IN ((?, ?), …):作为聚簇主键的前缀,MySQL 8.0 走 PRIMARY 的 range
	// (由真库 EXPLAIN 用例取证),TiDB 期望 Batch_Point_Get(未核实,只记录计划)。
	routeSelectRowsHeadSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE (route_shard, player_id) IN ("

	// R14 列 merged_into(一致读,不取锁)。
	routeListMergedIntoSQL = "SELECT src_zone, dst_zone, run_id, merged_at_ms FROM zone_merged_into ORDER BY src_zone"

	// R15 迁移种 meta 行(ODKU no-op:已存在时 generation / writable 一个字节都不改)。
	routeMetaSeedSQL = "INSERT INTO route_meta (id, generation, writable)" +
		" VALUES (1, ?, 1) ON DUPLICATE KEY UPDATE id = id"
)

// RouteFreezeTimeoutMin 是冻结 / 解冻(Catalog.Freeze / Release)至少要给的时限。TiDB 把 route_meta 设成缓存表之后
// (提案 §11.2「TiDB 运维步骤」第 6 条,S2 档起必选),对它的写要等当前读租约到期(tidb_table_cache_lease,
// 缺省 3s【未核实】),这段等待不是锁等待、不受会话 innodb_lock_wait_timeout=1 约束,只受 ctx 约束。
// 所以时限取 max(5s, 租约 + 2s);租约调大时调用方(route_tool -freeze-timeout)要同步放大。
// tidb-prepare 不发 `ALTER TABLE route_meta CACHE`,那一步留给 TiDB Phase 1 的部署步骤。
const RouteFreezeTimeoutMin = 5 * time.Second

// routeSelectRowsSQL 拼 n 个 id 的批量读(R13):n 个行构造器 (?, ?),实参按聚簇键升序交错给出
// (行片_i, player_id_i)。ORDER BY route_shard, player_id 让结果顺序确定。
func routeSelectRowsSQL(n int) string {
	var b strings.Builder
	b.WriteString(routeSelectRowsHeadSQL)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?)")
	}
	b.WriteString(") ORDER BY route_shard, player_id")
	return b.String()
}

// routeKey 是 player_route 的聚簇键 (route_shard, player_id)。
type routeKey struct {
	Shard    uint32
	PlayerID uint64
}

// less 按聚簇键序比较(先片、后 player_id)。
func (k routeKey) less(o routeKey) bool {
	if k.Shard != o.Shard {
		return k.Shard < o.Shard
	}
	return k.PlayerID < o.PlayerID
}

// routeKeyOf 返回 player_id 的聚簇键 (route_shard, player_id)。片只经 placement.RouteRowShard 算。
func routeKeyOf(playerID uint64) routeKey {
	return routeKey{Shard: placement.RouteRowShard(playerID), PlayerID: playerID}
}

// routeShardArg 是绑进 SQL 的行片实参。与 uint64(req.HomeZone) 同口径交给驱动 uint64(见 nullU32 的注释)。
func routeShardArg(playerID uint64) uint64 {
	return uint64(placement.RouteRowShard(playerID))
}

// sortRouteIDsByKey 把 ids 原地按聚簇键升序排。
func sortRouteIDsByKey(ids []uint64) {
	sort.Slice(ids, func(i, j int) bool { return routeKeyOf(ids[i]).less(routeKeyOf(ids[j])) })
}

// nullU32 把「0 = NULL」的领域约定翻成 SQL 参数。
//
// 刻意不用 sql.Null[T] 当参数:它的 Value() 随 Go 版本变化 —— 1.24 之前原样回 T(uint32 会被 go-sql-driver 以
// "non-Value type" 拒绝),1.24 起先过 driver.DefaultParameterConverter(uint64 高位为 1 时报错)。
// 两种行为都不是我们想要的绑定形状;这里直接给驱动 uint64 / nil,与 Go 版本无关
// (契约用例 TestRegisterHomeBindsNullForAbsentPlacement 钉住实参形状)。
func nullU32(v uint32) any {
	if v == 0 {
		return nil
	}
	return uint64(v)
}

// ── 建 store 与探针 ─────────────────────────────────────────────────────────────

// RouteStoreOptions 是 RouteStore 的行为参数。
type RouteStoreOptions struct {
	// Writer 写进 updated_by 的写者标识,用 NewRouteWriterID 生成(只含 [A-Za-z0-9._@:-],不超过 96 字节)。
	Writer string
}

func (o RouteStoreOptions) validate() error {
	if !validRouteWriter(o.Writer) {
		return fmt.Errorf("%w: writer %q must be 1..%d bytes of [A-Za-z0-9._@:-] (use NewRouteWriterID)",
			ErrRouteInvalidArgument, o.Writer, routeWriterMaxLen)
	}
	return nil
}

// RouteStore 是路由表的读写。连接池独立于其它 store:它要会话级 RC + 锁等待 1s(buildRouteDSN),
// 与快照 / 流水 / 名字共用池会把这两个会话变量带给别人,也会互相拖慢建角路径。
// 并发安全(sql.DB 与 atomic 计数器都是)。
type RouteStore struct {
	db     *sql.DB
	writer string

	// lockRetries 累计因 1213 / 9007 重来的次数(按实例)。只给日志与真库用例用:
	// 只看"最终结果正确"分不清是写法真的不成环,还是环被重试悄悄吸收了。
	lockRetries atomic.Uint64
}

// NewRouteStore 在全局库上开自己的池(DSN 见 buildRouteDSN),跑探针,任一步失败就关池返回 error(fail-closed)。
// 探针失败最常见的原因是迁移没跑过(route_meta 不存在 → 1146,或没有 id=1 → ErrRouteMetaMissing)。
func NewRouteStore(cfg MySQLConfig, opts RouteStoreOptions) (*RouteStore, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	db, err := openMySQLWithDSN(cfg, buildRouteDSN(cfg))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), routeProbeTimeout)
	defer cancel()
	st, err := newVerifiedRouteStoreFromDB(ctx, db, opts, cfg.DBName)
	if err != nil {
		return nil, fmt.Errorf("route store on %s/%s: %w", cfg.Host, cfg.DBName, err)
	}
	logx.Infof("[RouteStore] connected to %s/%s max_open_conn=%d max_idle_conn=%d writer=%s "+
		"(session READ-COMMITTED, innodb_lock_wait_timeout=%ds)",
		cfg.Host, cfg.DBName, cfg.MaxOpenConn, cfg.MaxIdleConn, opts.Writer, routeLockWaitTimeoutSec)
	return st, nil
}

// newVerifiedRouteStoreFromDB 是常驻/工具的只读启动核验。
// CHECK 的行为探针留给独立 schema/真库验收，normal 不为启动探针取得 metaX。
func newVerifiedRouteStoreFromDB(ctx context.Context, db *sql.DB, opts RouteStoreOptions, schema string) (*RouteStore, error) {
	fail := func(err error) (*RouteStore, error) { _ = db.Close(); return nil, err }
	if err := opts.validate(); err != nil {
		return fail(err)
	}
	if err := probeRouteSession(ctx, db); err != nil {
		return fail(err)
	}
	if err := assertRouteTableShapes(ctx, db, schema); err != nil {
		return fail(err)
	}
	if err := verifyRouteShardConstraint(ctx, db, schema); err != nil {
		return fail(err)
	}
	if err := assertWriterAuthoritySchema(ctx, db, schema); err != nil {
		return fail(err)
	}
	if _, err := readRouteWritable(ctx, db); err != nil {
		return fail(err)
	}
	return &RouteStore{db: db, writer: opts.Writer}, nil
}

// verifyRouteShardConstraint 在正常启动时只读校验，避免用业务写探针绕过写者首锁。
// 表级 ENFORCED、精确表达式及 TiDB 总开关必须同时成立；读取失败一律拒绝启动。
func verifyRouteShardConstraint(ctx context.Context, db *sql.DB, schema string) error {
	var enforced, clause string
	err := db.QueryRowContext(ctx, "SELECT tc.ENFORCED,cc.CHECK_CLAUSE FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc JOIN INFORMATION_SCHEMA.CHECK_CONSTRAINTS cc ON cc.CONSTRAINT_SCHEMA=tc.CONSTRAINT_SCHEMA AND cc.CONSTRAINT_NAME=tc.CONSTRAINT_NAME WHERE tc.TABLE_SCHEMA=? AND tc.TABLE_NAME='player_route' AND tc.CONSTRAINT_NAME='chk_route_shard' AND tc.CONSTRAINT_TYPE='CHECK'", schema).Scan(&enforced, &clause)
	if err != nil {
		return fmt.Errorf("%w: read chk_route_shard metadata: %v", ErrRouteCheckNotEnforced, err)
	}
	if !strings.EqualFold(enforced, "YES") {
		return fmt.Errorf("%w: chk_route_shard ENFORCED=%q", ErrRouteCheckNotEnforced, enforced)
	}
	if !routeShardClauseMatches(clause) {
		return fmt.Errorf("%w: chk_route_shard does not match route_shard = player_id %% %d", ErrRouteShardModulusMismatch, placement.RouteRowShards)
	}
	isTiDB, _, err := serverIsTiDB(ctx, db)
	if err != nil {
		return err
	}
	if isTiDB {
		var enabled string
		if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.tidb_enable_check_constraint").Scan(&enabled); err != nil {
			return fmt.Errorf("%w: read TiDB CHECK switch: %v", ErrRouteCheckNotEnforced, err)
		}
		if enabled != "1" && !strings.EqualFold(enabled, "ON") {
			return fmt.Errorf("%w: TiDB CHECK switch is disabled", ErrRouteCheckNotEnforced)
		}
	}
	return nil
}

// routeShardClauseMatches 只接受同一棵等式/取模表达式树；不能删光括号后比较，否则
// (route_shard=player_id)%64 也会被误认作正确约束。未知函数、标识符和运算符均拒绝。
func routeShardClauseMatches(clause string) bool {
	s := strings.ToLower(clause)
	s = strings.ReplaceAll(s, "`route_shard`", "route_shard")
	s = strings.ReplaceAll(s, "`player_id`", "player_id")
	if strings.ContainsRune(s, '`') {
		return false
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
	strip := func(v string) (string, bool) {
		for {
			depth, firstClose := 0, -1
			for i, ch := range v {
				if ch == '(' {
					depth++
				}
				if ch == ')' {
					depth--
					if depth < 0 {
						return "", false
					}
					if depth == 0 && firstClose < 0 {
						firstClose = i
					}
				}
			}
			if depth != 0 || v == "" {
				return "", false
			}
			if v[0] != '(' || firstClose != len(v)-1 {
				return v, true
			}
			v = v[1 : len(v)-1]
		}
	}
	split := func(v string, operator byte) (string, string, bool) {
		depth, index := 0, -1
		for i := 0; i < len(v); i++ {
			switch v[i] {
			case '(':
				depth++
			case ')':
				depth--
			default:
				if depth == 0 && v[i] == operator {
					if index >= 0 {
						return "", "", false
					}
					index = i
				}
			}
		}
		if index < 1 || index == len(v)-1 {
			return "", "", false
		}
		return v[:index], v[index+1:], true
	}
	s, ok := strip(s)
	if !ok {
		return false
	}
	left, right, ok := split(s, '=')
	if !ok {
		return false
	}
	left, ok = strip(left)
	if !ok || left != "route_shard" {
		return false
	}
	right, ok = strip(right)
	if !ok {
		return false
	}
	value, modulus, ok := split(right, '%')
	if !ok {
		return false
	}
	value, ok = strip(value)
	if !ok || value != "player_id" {
		return false
	}
	modulus, ok = strip(modulus)
	if !ok || modulus != strconv.FormatUint(uint64(placement.RouteRowShards), 10) {
		return false
	}
	return true
}

// OpenRouteStoreHint 是 NewRouteStore 失败时给运维的一句处理提示(英文,拼进日志、错误或 stderr 的括号里),建 route store 的
// 两个入口共用:data_service 装配、route_tool。configPath 是 data_service 的 yaml 路径,
// 不知道时传 "<yaml>"。表按另一个行片模数建的(ErrRouteShardModulusMismatch)不能照常提示「先跑迁移」:
// CREATE TABLE IF NOT EXISTS 不改已有的表,只重跑 -migrate 修不好,要先按报错里的步骤 DROP 全部路由表
// (runbook §9.3 前置第 1 条)。其余原因回原来的「先跑迁移」提示。
func OpenRouteStoreHint(configPath string, err error) string {
	if errors.Is(err, ErrRouteShardModulusMismatch) {
		return "the route tables were created with another route_shard modulus; rerunning -migrate alone does not fix that, " +
			"see merge-zone-runbook §9.3 prerequisite 1"
	}
	return fmt.Sprintf("run `data_service -f %s -migrate` first if route_meta is missing", configPath)
}

// newRouteStoreFromDB 在一个已开好的池上跑探针并装出 store。任一步失败都会关掉 db(调用方不必再 Close)。
// 单独成函数是为了让契约用例注入假驱动、真库用例注入一个会话变量不对的池。
//
// 探针按顺序:
//  1. 会话隔离级别必须是 READ-COMMITTED、innodb_lock_wait_timeout 必须是 routeLockWaitTimeoutSec
//     (DSN 里的会话变量没生效时,锁推演与预算分配都不成立);
//  2. 对端是 TiDB 时 tidb_txn_mode 必须是 pessimistic(RC + FOR UPDATE 的前提,提案 §3.1 / A15);
//  3. route_meta(id=1)必须在:表不存在原样返回(1146),没有行返回 ErrRouteMetaMissing。
//     writable != 1 不拒绝建 store(那是运维显式冻结,写路径会逐次拒绝),只打 Error;
//  4. chk_route_shard 必须生效(probeRouteShardCheck,G1a):player_id 的唯一性靠它,证不了就 ErrRouteCheckNotEnforced。
//     这一步是一次必然回滚的写尝试,要 player_route 的 INSERT 权限;
//  5. chk_route_shard 的模数必须是 placement.RouteRowShards(probeRouteShardModulus,P5):与 P4 同一个必然回滚的事务、
//     同一条权限;按另一个模数建过的表 → ErrRouteShardModulusMismatch。
//     P4 + P5 这个事务遇到 1213 / 9007 时整个重跑(retryRouteTx,与写事务同一预算;1205 不重试)。
func newRouteStoreFromDB(ctx context.Context, db *sql.DB, opts RouteStoreOptions) (*RouteStore, error) {
	fail := func(err error) (*RouteStore, error) {
		_ = db.Close()
		return nil, err
	}
	if err := opts.validate(); err != nil {
		return fail(err)
	}
	if err := probeRouteSession(ctx, db); err != nil {
		return fail(err)
	}
	writable, err := readRouteWritable(ctx, db)
	if err != nil {
		return fail(err)
	}
	if writable != 1 {
		logx.Errorf("[RouteStore] route_meta(id=%d).writable=%d: route writers are frozen, every RegisterPlayerZone "+
			"will be refused until it is set back to 1", routeMetaRowID, writable)
	}
	// P4 + P5 每次尝试都是新开的一个必然回滚的事务,1213 / 9007 整事务重跑(见 probeRouteShardModulus 的「锁」);
	// 1205 与其它错误不重试(文件头不变量 4)。退避受 ctx(routeProbeTimeout)约束。
	if err := retryRouteTx(ctx, "chk_route_shard probes", nil, func() error { return probeRouteShardCheck(ctx, db) }); err != nil {
		return fail(err)
	}
	return &RouteStore{db: db, writer: opts.Writer}, nil
}

// probeRouteShardCheck 是探针第 4 步(P4):player_id 的唯一性现在靠 chk_route_shard(主键是 (route_shard, player_id)),
// CHECK 不生效时一个算错片的写者就能给同一个 player_id 写出两行 —— 而 R5 / R13 都按「正确的片 + id」点查,
// 错片行对它们不可见,读路径兜不住。所以在一个必然回滚的事务里插一行
// 「只违反 chk_route_shard」的行(routeProbeShardCheckSQL),必须被 3819 拒绝、且报错点名 chk_route_shard;
// 插入成功或被别的约束拒绝都说明我们证不了它,拒建 store(fail-closed,AGENTS.md §11.3)。
//
// 为什么实测而不是查元数据:MySQL 的 information_schema.TABLE_CONSTRAINTS.ENFORCED 与 TiDB 的全局变量口径不一,
// 实测在两种引擎上是同一个判据。它只证明「CHECK 生效」,不证明模数是 64(探针行 (片 1, player_id 0) 对任何模数都违例,
// 按旧模数 16 建的表也过得了这一步):P4 通过之后,在同一个事务里接着跑第 5 步 probeRouteShardModulus 核对模数。
//
// 无论哪种结果事务都回滚,CHECK 没生效时那行也不会留下。CHECK 生效时语句在写索引前就被拒、不取锁。
// 整个探针事务的第一句是 lockRouteProbeGuard(route_meta(id=1) FOR UPDATE):多副本同时探针时在这行已提交的守卫上
// 排队、逐个探针,后到的最多等 1s(会话锁等待封顶),不会排在别人必然回滚的探针行后面(见 probeRouteShardModulus 的「锁」)。
func probeRouteShardCheck(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("route store: begin chk_route_shard probe: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := lockRouteProbeGuard(ctx, tx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, routeProbeShardCheckSQL)
	if err == nil {
		return fmt.Errorf("%w: a row violating it was accepted (rolled back)", ErrRouteCheckNotEnforced)
	}
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return fmt.Errorf("route store: chk_route_shard probe: %w", err)
	}
	switch me.Number {
	case mysqlErrCheckConstraintViolated:
		if namesRouteShardCheck(me) {
			// P4 通过:同一个事务里接着核对模数(P5),结束时由上面的 defer 一并回滚。MySQL 只回滚失败的那条语句、
			// 事务仍可用;TiDB 上是否如此未核实(见 probeRouteShardModulus)。
			return probeRouteShardModulus(ctx, tx)
		}
		return fmt.Errorf("%w: the probe row was rejected by another constraint, so chk_route_shard is not proven: %v",
			ErrRouteCheckNotEnforced, err)
	case mysqlErrDupEntry:
		return fmt.Errorf("route store: chk_route_shard probe hit 1062: player_route already holds the row "+
			"(route_shard=1, player_id=0), i.e. a mis-sharded row was written while CHECK was not enforced; "+
			"remove it by hand after review: %w", err)
	case mysqlErrTableAccessDenied:
		return routeProbeInsertDenied(err)
	default:
		return fmt.Errorf("route store: chk_route_shard probe: %w", err)
	}
}

// probeRouteShardModulus 是探针第 5 步(P5,正向探针,2026-10-14):P4 只证明 chk_route_shard 生效,证不了它的模数。
// 按另一个模数建过的表(例如跑过行片还是 16 的 d5cbb8a8e 的迁移)迁移、形状守卫、P4 全都过得去 —— CREATE TABLE
// IF NOT EXISTS 不改已有的表,形状守卫只查列名与主键,P4 的违例行对任何模数都违例 —— 要到运行期 W1 / 导入对大部分
// 新号撞 3819 才暴露。所以在 P4 的同一个必然回滚的事务里(tx 由调用方持有并回滚),按聚簇键升序再插两行「按
// N = placement.RouteRowShards 合法」的行(routeShardModulusProbeKeys;其余列同 P4)。表的模数为 M 时:
//
//	(0, N)      被接受 ⇔ N % M = 0       ⇔ M 整除 N(于是 M ≤ N)
//	(N−1, N−1)  被接受 ⇔ (N−1) % M = N−1 ⇔ M ≥ N
//
// 两行都被接受 ⇔ M = N,对任何 M ≥ 1 都成立,不必为个别模数再补探针行。N = 64 时的判别表
// (TestRouteShardModulusProbeDiscriminates 钉住,并对 M = 1..256 穷举;route_shard 是 tinyint unsigned,
// 提案 H2 的后备模数最大取 256):
//
//	M     (0, 64)  (63, 63)  结论
//	16    收       拒        不一致
//	32    收       拒        不一致
//	64    收       收        一致
//	128   拒       收        不一致
//	192   拒       收        不一致
//	256   拒       收        不一致
//
// 逐行判定:成功或 1062 = 该行被接受;3819 且点名 chk_route_shard → ErrRouteShardModulusMismatch;1142 → 与 P4
// 同一条权限提示;3819 点名别的约束(探针行本该满足其余全部约束)、1205 与其它任何错误 → 原样包装(证不了就不建
// store,fail-closed);其中 1213 / 9007 由调用方整事务重跑(见下「锁」)。不需要 P4 之外的新权限。
// 1062 = 库里已有这一行:CHECK 生效的表上已有的行都满足它,所以撞键同样说明这一行在表的模数下合法,
// 与引擎先判 CHECK 还是先判撞键无关(CHECK 被关掉期间写进来的行除外,那是 HR2 的范畴)。探针键是固定值,
// 真实玩家的 id 恰好等于它时就会撞键(早期号段从 1 发号时这是常态,打散之后几乎不会发生,但判定不变)。
//
// 锁:这两行不存在且模数一致时真的插进去(未提交);已存在时撞键会在那一行上取共享锁(评审按 mysql-server 8.0.36
// 源码 row0ins.cc 核对,未实跑;真库用例 TestRouteStore_ProbeKeysHeldByRealPlayers 取证)。两种都持到回滚(毫秒级),
// 所以 63 / 64 号玩家的 W1 可能在探针期间短暂排队;反过来探针最多等会话锁等待 1s,等满即 1205、这次建池失败
// (不重试,重启即可)。各副本都按聚簇键升序插这两行,两行之间不成环。
// 三个及以上会话并发插同一个**缺席**的探针键时(空表、首次部署,或 63 / 64 号还没建角),先插入者(探针**必然**回滚)
// 回滚后,其余等待者查重取的 S 锁被继承成 gap 锁,重新插入要插入意向锁,互等成 1213 —— 事故文档
// docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md §9.1 现场二,写法与隔离级别都消不掉(2026-10-16 真库在 RC 下
// 复现 10/10)。唯一的消法是让首次插入者先排在一行已提交的守卫行上:所以 P4 之前先 lockRouteProbeGuard 锁住
// route_meta(id=1),并发探针在它上面串成一列,任何时刻至多一个探针持有未提交的探针行(同一真库编排 0/10)。
// newRouteStoreFromDB 对 P4 + P5 的 1213 / 9007 有界重跑(retryRouteTx)只剩最后防线的意义,不再是正确性论证。
// P4 被拒之后事务继续用:MySQL 只回滚失败的那条语句;TiDB 是否同样只回滚语句【未核实】,
// 由真 TiDB 用例(TestRouteStore_TiDBClusteredShardsAndPresplit)取证。
//
// 作用范围:只在建 route store 时跑(data_service、route_tool)。
func probeRouteShardModulus(ctx context.Context, tx *sql.Tx) error {
	for _, key := range routeShardModulusProbeKeys() {
		_, err := tx.ExecContext(ctx, routeProbeShardModulusSQL, uint64(key.Shard), key.PlayerID)
		if err == nil {
			continue
		}
		var me *mysql.MySQLError
		isMySQL := errors.As(err, &me)
		switch {
		case isMySQL && me.Number == mysqlErrDupEntry:
			continue // 库里已有这一行,等同被接受(见上)
		case isMySQL && me.Number == mysqlErrCheckConstraintViolated && namesRouteShardCheck(me):
			return fmt.Errorf("%w: probe row (route_shard=%d, player_id=%d) is legal under %% %d but chk_route_shard rejected it: %v",
				ErrRouteShardModulusMismatch, key.Shard, key.PlayerID, placement.RouteRowShards, err)
		case isMySQL && me.Number == mysqlErrTableAccessDenied:
			return routeProbeInsertDenied(err)
		default:
			return fmt.Errorf("route store: route_shard modulus probe row (route_shard=%d, player_id=%d): %w",
				key.Shard, key.PlayerID, err)
		}
	}
	return nil
}

// routeShardModulusProbeKeys 返回 P5 的两行聚簇键,按聚簇键升序(插入即按这个顺序取锁):(0, N) 与 (N−1, N−1),
// N = placement.RouteRowShards。片经 routeKeyOf(即 placement.RouteRowShard)算,所以两行就是本二进制会写的样子。
func routeShardModulusProbeKeys() []routeKey {
	n := uint64(placement.RouteRowShards)
	return []routeKey{routeKeyOf(n), routeKeyOf(n - 1)}
}

// namesRouteShardCheck 报告一条 3819 的报错是否点名 chk_route_shard(P4 与 P5 共用这一个判据)。只按名字的子串认:
// MySQL 8.0 的原文是 "Check constraint 'chk_route_shard' is violated.",TiDB 的引号格式未核实;
// 其余五条约束(chk_route_home 等)的名字都不含这个子串。
func namesRouteShardCheck(me *mysql.MySQLError) bool {
	return strings.Contains(strings.ToLower(me.Message), "chk_route_shard")
}

// routeProbeInsertDenied 是 P4 / P5 收到 1142 时的报错:两步都是必然回滚的 INSERT,要这条权限。
func routeProbeInsertDenied(err error) error {
	return fmt.Errorf("route store: the chk_route_shard probes need INSERT privilege on player_route "+
		"(P4 and the modulus probe P5 are rolled-back inserts): %w", err)
}

// probeRouteSession 探针第 1、2 步(见 newRouteStoreFromDB)。
func probeRouteSession(ctx context.Context, db *sql.DB) error {
	var isolation string
	var lockWait int64
	if err := db.QueryRowContext(ctx, routeProbeSessionSQL).Scan(&isolation, &lockWait); err != nil {
		return fmt.Errorf("route store: read session transaction_isolation / innodb_lock_wait_timeout: %w", err)
	}
	if !strings.EqualFold(isolation, "READ-COMMITTED") || lockWait != routeLockWaitTimeoutSec {
		return fmt.Errorf("%w: transaction_isolation=%q innodb_lock_wait_timeout=%d, want READ-COMMITTED and %d "+
			"(the DSN session variables of buildRouteDSN did not take effect)",
			ErrRouteSessionMisconfigured, isolation, lockWait, routeLockWaitTimeoutSec)
	}

	isTiDB, version, err := serverIsTiDB(ctx, db)
	if err != nil {
		return fmt.Errorf("route store: %w", err)
	}
	if !isTiDB {
		return nil
	}
	var mode string
	if err := db.QueryRowContext(ctx, routeProbeTiDBTxnModeSQL).Scan(&mode); err != nil {
		return fmt.Errorf("route store: read tidb_txn_mode on %s: %w", version, err)
	}
	if !strings.EqualFold(mode, "pessimistic") {
		return fmt.Errorf("%w: TiDB %s tidb_txn_mode=%q, want pessimistic "+
			"(the FOR UPDATE based decisions need pessimistic locking; proposal §3.1)",
			ErrRouteSessionMisconfigured, version, mode)
	}
	return nil
}

// routeQueryer 是 *sql.DB 与 *sql.Tx 共有的读接口:同一段读代码既给探针 / dry-run(事务外)用,也给事务内用。
type routeQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// lockRouteProbeGuard 是探针事务(P4 + P5)的第一句:对 route_meta(id=1) 完整主键等值 FOR UPDATE。
// 这一行由迁移种好、从不删除,所以并发建池的探针排在一行**已提交**的记录上,而不是排在别人必然回滚的探针行后面
// (原因见 probeRouteShardModulus 的「锁」)。与全局锁序 route_meta → player_route 同向;
// W1 对 route_meta 只做一致读(requireRouteWritable),不受它影响;冻结写者(Catalog.Freeze 把 writable 置 0)最多等一个探针事务。
// 需要对 route_meta 的 UPDATE(或 DELETE / LOCK TABLES)权限 —— MySQL 对 FOR UPDATE 的要求;部署给 appuser 的是库级 ALL。
func lockRouteProbeGuard(ctx context.Context, tx *sql.Tx) error {
	var writable uint64
	err := tx.QueryRowContext(ctx, routeLockProbeGuardSQL).Scan(&writable)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRouteMetaMissing
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == mysqlErrTableAccessDenied {
		return fmt.Errorf("route store: the probe guard needs SELECT ... FOR UPDATE on route_meta "+
			"(UPDATE privilege): %w", err)
	}
	if err != nil {
		return fmt.Errorf("route store: lock route_meta(id=1) as the probe guard: %w", err)
	}
	return nil
}

// readRouteWritable 读 route_meta.writable(R1)。没有 id=1 那一行时回 ErrRouteMetaMissing。
func readRouteWritable(ctx context.Context, q routeQueryer) (uint64, error) {
	var writable uint64
	err := q.QueryRowContext(ctx, routeReadWritableSQL).Scan(&writable)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrRouteMetaMissing
	}
	if err != nil {
		return 0, fmt.Errorf("read route_meta.writable: %w", err)
	}
	return writable, nil
}

// requireRouteWritable 是写事务的第一步:writable != 1 即拒写(提案 §4.1-2 第 1 步)。
// 普通读而非加锁读:把它置 0 时已经过了这一步的事务仍会提交,提交点的保证靠写者闸门(见 lockInitialPlayerRoute)。
func requireRouteWritable(ctx context.Context, q routeQueryer) error {
	writable, err := readRouteWritable(ctx, q)
	if err != nil {
		return err
	}
	if writable != 1 {
		return fmt.Errorf("%w (writable=%d)", ErrRouteNotWritable, writable)
	}
	return nil
}

// Close 关闭连接池。
func (s *RouteStore) Close() error {
	return s.db.Close()
}

// NewRouteWriterID 生成写进 updated_by 的写者标识 "{component}@{host}":只留 [A-Za-z0-9._@:-]
// (列是 ascii,非 ASCII 字节会被库拒绝或改写,这里先洗掉),截到 96 字节;host 洗完为空时用 "unknown"。
// host 由调用方传入(装配层取 os.Hostname),本函数不碰任何环境。
func NewRouteWriterID(component, host string) string {
	c := sanitizeRouteWriterPart(component)
	if c == "" {
		c = "unknown"
	}
	h := sanitizeRouteWriterPart(host)
	if h == "" {
		h = "unknown"
	}
	id := c + "@" + h
	if len(id) > routeWriterMaxLen {
		id = id[:routeWriterMaxLen] // 洗过之后全是单字节 ASCII,按字节截不会截断字符
	}
	return id
}

func sanitizeRouteWriterPart(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if routeWriterByteAllowed(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func routeWriterByteAllowed(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '@', c == ':', c == '-':
		return true
	default:
		return false
	}
}

func validRouteWriter(w string) bool {
	if w == "" || len(w) > routeWriterMaxLen {
		return false
	}
	for i := 0; i < len(w); i++ {
		if !routeWriterByteAllowed(w[i]) {
			return false
		}
	}
	return true
}

// ── 行 ──────────────────────────────────────────────────────────────────────────

// RouteRow 是 player_route 的一行,**领域类型**:带 HasHome / Placement 等判定,是 SQL 扫描的载体。
// 表的列契约是 proto 的 dbpb.PlayerRoute(只作列漂移守卫,不作读写载体,见 schema.go routeTableMessages)。
// 两者逐字段对应(proto 字段名 snake_case → 这里的 CamelCase,类型相同,0 / 空串 = NULL 的约定也相同);
// 唯一的例外是 route_shard:它由 player_id 唯一决定(placement.RouteRowShard,chk_route_shard),这里不存,
// SQL 绑定时现算。新增列 = proto、DDL、这里三处同改,route_store_test 的 TestRouteRowMirrorsPlayerRouteProto 钉住对应关系。
// 「0 / 空串 = NULL」:HomeZone、StorageID(与 PlacementVersion 同 NULL)、FrozenRunID。
type RouteRow struct {
	PlayerID         uint64
	HomeZone         uint32 // 0 = NULL
	StorageID        uint32 // 0 = NULL(与 PlacementVersion 同 NULL,chk_route_place)
	PlacementVersion uint64
	FrozenRunID      string // "" = NULL;非空 = 冻结态(搬库进行中)
	Rev              uint64
	UpdatedBy        string
	// CreatedAtMs / UpdatedAtMs 取库时钟。
	CreatedAtMs uint64
	UpdatedAtMs uint64
}

// HasHome 报告这一行有没有 home_zone。
func (r RouteRow) HasHome() bool { return r.HomeZone != 0 }

// HasPlacement 报告这一行有没有落点记录。
func (r RouteRow) HasPlacement() bool { return r.StorageID != 0 }

// Placement 把落点三列还原成 placement.Record。没有落点时回 (零值, false, nil)。
// 有落点就 Validate,三列不自洽(version 为 0、冻结 run_id 不合法、没有落点却带着 version / run_id)
// 一律回 ErrMalformed —— 读方必须 fail-closed,绝不能按一条坏行选库。
func (r RouteRow) Placement() (placement.Record, bool, error) {
	if r.StorageID == 0 {
		if r.PlacementVersion != 0 || r.FrozenRunID != "" {
			return placement.Record{}, false, fmt.Errorf("%w: player_route player_id=%d has no storage_id but placement_version=%d frozen_run_id=%q",
				placement.ErrMalformed, r.PlayerID, r.PlacementVersion, r.FrozenRunID)
		}
		return placement.Record{}, false, nil
	}
	rec := placement.Record{
		StorageID: r.StorageID,
		Version:   r.PlacementVersion,
		Frozen:    r.FrozenRunID != "",
		RunID:     r.FrozenRunID,
	}
	if err := rec.Validate(); err != nil {
		return placement.Record{}, false, fmt.Errorf("player_route player_id=%d: %w", r.PlayerID, err)
	}
	return rec, true, nil
}

// routeScanner 是 *sql.Row 与 *sql.Rows 共有的 Scan。
type routeScanner interface {
	Scan(dest ...any) error
}

// scanRouteRow 按 routeRowColumnsSQL 的顺序扫一行(withKey 时最前面多两列聚簇键 route_shard, player_id)。
// 可空列用 sql.Null[uint32] / sql.Null[uint64] / sql.NullString 扫:bigint unsigned 可能超过 2^63,NullInt64 装不下。
//
// withKey 时校验库里存的 route_shard == placement.RouteRowShard(player_id),不等即 ErrRouteShardMismatch(fail-closed)。
// R13 按正确片点查,这里只是纵深防御(见 ErrRouteShardMismatch)。RouteRow 不带片:片由 player_id 唯一决定,校验一致后丢弃。
func scanRouteRow(sc routeScanner, row *RouteRow, withKey bool) error {
	var home, storage sql.Null[uint32]
	var version sql.Null[uint64]
	var frozen sql.NullString
	var storedShard uint64
	dest := []any{&home, &storage, &version, &frozen, &row.Rev, &row.UpdatedBy, &row.CreatedAtMs, &row.UpdatedAtMs}
	if withKey {
		dest = append([]any{&storedShard, &row.PlayerID}, dest...)
	}
	if err := sc.Scan(dest...); err != nil {
		return err
	}
	if withKey {
		if want := routeShardArg(row.PlayerID); storedShard != want {
			return fmt.Errorf("%w: player_id=%d route_shard=%d want %d", ErrRouteShardMismatch, row.PlayerID, storedShard, want)
		}
	}
	row.HomeZone, row.StorageID, row.PlacementVersion, row.FrozenRunID = 0, 0, 0, ""
	if home.Valid {
		row.HomeZone = home.V
	}
	if storage.Valid {
		row.StorageID = storage.V
	}
	if version.Valid {
		row.PlacementVersion = version.V
	}
	if frozen.Valid {
		row.FrozenRunID = frozen.String
	}
	return nil
}

// ── W1:RegisterHome ─────────────────────────────────────────────────────────────

// RouteRegisterRequest 是一次 W1(RegisterPlayerZone)的入参。
type RouteRegisterRequest struct {
	PlayerID  uint64
	HomeZone  uint32 // 1..RouteHomeZoneMax
	StorageID uint32 // 0 = 不钉落点
}

// RouteRegisterOutcome 是一次 W1 在库里的终局。业务结果(冲突)走这里,只有故障才走 error。
type RouteRegisterOutcome uint8

const (
	// RouteRegisterInserted 新插入了一行(rev=1)。
	RouteRegisterInserted RouteRegisterOutcome = iota + 1
	// RouteRegisterFilledHome 行已在、home 为 NULL,本次补上 home(以及请求带的落点,如果行里没有落点)。
	RouteRegisterFilledHome
	// RouteRegisterFilledHomePlacementKept 补上了 home,但行里已有落点、请求也带了 storage:落点不覆盖
	// (它指向玩家数据**真正所在**的库),调用方打 ERROR。
	RouteRegisterFilledHomePlacementKept
	// RouteRegisterAlreadySame 行里 home 已等于请求值:幂等,没有写入。
	RouteRegisterAlreadySame
	// RouteRegisterConflict 行里 home 是另一个值,ExistingHome 给出。不写。
	RouteRegisterConflict
)

// String 仅供日志与指标 label(低基数,五个固定值)。
func (o RouteRegisterOutcome) String() string {
	switch o {
	case RouteRegisterInserted:
		return "inserted"
	case RouteRegisterFilledHome:
		return "filled_home"
	case RouteRegisterFilledHomePlacementKept:
		return "filled_home_placement_kept"
	case RouteRegisterAlreadySame:
		return "already_same"
	case RouteRegisterConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// RouteRegisterResult 是 RegisterHome 的结果。
type RouteRegisterResult struct {
	Outcome RouteRegisterOutcome
	// ExistingHome 只在 Conflict 时有值。
	ExistingHome uint32
}

// RegisterHome 是 W1 的库内提交点,提交即完成:
//
//	BEGIN(RC,DSN 已设)
//	R1 普通读 route_meta.writable            缺行 → ErrRouteMetaMissing;!=1 → ErrRouteNotWritable
//	R4 ODKU 插新行                           1 行 → COMMIT,Inserted;0 行 → 往下;其它 → fail-closed
//	R5 加锁读玩家行 (player_id % 64, id)      缺行 → ErrRouteRowVanished
//	   home == 请求 → 回滚,AlreadySame;home 是别的值 → 回滚,Conflict
//	   home 为 NULL → R6b(请求带 storage 且行里没落点)或 R6a → COMMIT,Filled*
//
// 不看合服状态:合服过程中在源区建号是允许的,home_zone 就是请求里的源区,归属解析之后落到目标区
// (docs/design/player-data-tidb-scaling-plan.md §7.1)。
//
// 错误语义:冲突通过 Outcome 返回,err 为 nil;只有故障才返回 err —— 参数非法(ErrRouteInvalidArgument)、
// meta 缺失 / 不可写、行消失、SQL 错误(含 1205)、1213 / 9007 重试用尽。
// COMMIT 出错时结果不确定,按 err 返回;调用方重试会落到幂等分支(AlreadySame),安全。
func (s *RouteStore) RegisterHome(ctx context.Context, req RouteRegisterRequest) (RouteRegisterResult, error) {
	if req.PlayerID == 0 || req.HomeZone == 0 || req.HomeZone > RouteHomeZoneMax {
		return RouteRegisterResult{}, fmt.Errorf("%w: player_id=%d home_zone=%d (want player_id > 0 and home_zone in [1, %d])",
			ErrRouteInvalidArgument, req.PlayerID, req.HomeZone, RouteHomeZoneMax)
	}
	var res RouteRegisterResult
	err := s.runRouteTx(ctx, "player_route register player_id="+strconv.FormatUint(req.PlayerID, 10), func() error {
		var attemptErr error
		res, attemptErr = s.registerHomeTx(ctx, req)
		return attemptErr
	})
	if err != nil {
		return RouteRegisterResult{}, err
	}
	return res, nil
}

// registerHomeTx 一次事务尝试。R4 / R5 / R6 绑的行片 rowShard 在这里由 player_id 算。
func (s *RouteStore) registerHomeTx(ctx context.Context, req RouteRegisterRequest) (RouteRegisterResult, error) {
	rowShard := routeShardArg(req.PlayerID)
	// 参数绑定:落点缺席时 storage / version 都绑 NULL(chk_route_place 要求两者同 NULL)。
	storageArg := nullU32(req.StorageID)
	var versionArg any
	if req.StorageID != 0 {
		versionArg = uint64(1)
	}
	if (storageArg == nil) != (versionArg == nil) {
		return RouteRegisterResult{}, fmt.Errorf("player_route register player_id=%d: storage/version binding out of sync (internal bug)", req.PlayerID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RouteRegisterResult{}, fmt.Errorf("begin player_route register tx player_id=%d: %w", req.PlayerID, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	commit := func() error {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit player_route register player_id=%d (outcome unknown; a retry lands on the idempotent branch): %w",
				req.PlayerID, err)
		}
		committed = true
		return nil
	}

	// R1
	if err := requireRouteWritable(ctx, tx); err != nil {
		return RouteRegisterResult{}, err
	}

	// R4
	r4, err := tx.ExecContext(ctx, routeInsertNewSQL, rowShard, req.PlayerID, uint64(req.HomeZone), storageArg, versionArg, s.writer)
	if err != nil {
		return RouteRegisterResult{}, fmt.Errorf("insert player_route player_id=%d: %w", req.PlayerID, err)
	}
	n, err := r4.RowsAffected()
	if err != nil {
		return RouteRegisterResult{}, fmt.Errorf("insert player_route player_id=%d rows affected: %w", req.PlayerID, err)
	}
	switch n {
	case 1:
		if err := commit(); err != nil {
			return RouteRegisterResult{}, err
		}
		return RouteRegisterResult{Outcome: RouteRegisterInserted}, nil
	case 0:
		// 撞键:行已在,落到 R5 用加锁读判定。
	default:
		return RouteRegisterResult{}, fmt.Errorf("insert player_route player_id=%d: 受影响行数 %d 不在 {1=插入, 0=撞键 no-op} 之内,"+
			"routeInsertNewSQL 的 ON DUPLICATE KEY UPDATE 必须保持 no-op、连接不得带 CLIENT_FOUND_ROWS", req.PlayerID, n)
	}

	// R5
	row := RouteRow{PlayerID: req.PlayerID}
	err = scanRouteRow(tx.QueryRowContext(ctx, routeLockRowSQL, rowShard, req.PlayerID), &row, false)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteRegisterResult{}, fmt.Errorf("%w (player_id=%d)", ErrRouteRowVanished, req.PlayerID)
	}
	if err != nil {
		return RouteRegisterResult{}, fmt.Errorf("lock player_route player_id=%d: %w", req.PlayerID, err)
	}
	switch {
	case row.HomeZone == req.HomeZone:
		return RouteRegisterResult{Outcome: RouteRegisterAlreadySame}, nil
	case row.HomeZone != 0:
		return RouteRegisterResult{Outcome: RouteRegisterConflict, ExistingHome: row.HomeZone}, nil
	}

	// home 为 NULL:补 home(以及请求带、行里没有的落点)。
	outcome := RouteRegisterFilledHome
	var upd sql.Result
	if req.StorageID != 0 && !row.HasPlacement() {
		upd, err = tx.ExecContext(ctx, routeFillHomePlacementSQL, uint64(req.HomeZone), storageArg, s.writer, rowShard, req.PlayerID, row.Rev)
	} else {
		upd, err = tx.ExecContext(ctx, routeFillHomeSQL, uint64(req.HomeZone), s.writer, rowShard, req.PlayerID, row.Rev)
		if req.StorageID != 0 {
			// 行里已有落点、请求也带了 storage:不覆盖(见 RouteRegisterFilledHomePlacementKept)。
			outcome = RouteRegisterFilledHomePlacementKept
		}
	}
	if err != nil {
		return RouteRegisterResult{}, fmt.Errorf("fill player_route home player_id=%d: %w", req.PlayerID, err)
	}
	if err := requireOneRowAffected(upd, "fill player_route home player_id="+strconv.FormatUint(req.PlayerID, 10)); err != nil {
		return RouteRegisterResult{}, err
	}
	if err := commit(); err != nil {
		return RouteRegisterResult{}, err
	}
	return RouteRegisterResult{Outcome: outcome}, nil
}

// requireOneRowAffected 是「行已被加锁读锁住」之后那条 CAS UPDATE 的判据:必须恰好 1 行
// (口径同 id_segment_store.go 的 CAS)。不是 1 就说明锁语义被破坏(有人绕过本 store 改了行),fail-closed。
func requireOneRowAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("%s: CAS affected %d rows (expected exactly 1 under the row lock)", what, n)
	}
	return nil
}

// ── 重试 ──────────────────────────────────────────────────────────────────────

// isRouteLockConflict 判断错误是不是可整事务重跑的锁冲突:1213(InnoDB 死锁)与 9007(TiDB 写冲突)。
// 1205 刻意不在其中(文件头不变量 4)。
func isRouteLockConflict(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlErrDeadlock || me.Number == tidbErrWriteConflict
}

// runRouteTx 跑一个「一次事务尝试」的函数,有界重来(规则见 retryRouteTx),1213 / 9007 的重来次数记进 s.lockRetries。
func (s *RouteStore) runRouteTx(ctx context.Context, op string, attempt func() error) error {
	return retryRouteTx(ctx, op, &s.lockRetries, attempt)
}

// retryRouteTx 是 runRouteTx 的循环体,有界重来:
//   - 1213 / 9007:计 lockRetries,抖动退避(可被 ctx 取消)后重来;
//   - 其它错误(含 1205):立刻返回。
//
// 总尝试次数不超过 routeTxAttempts;用尽后把最后一次的错误包着返回(errors.As 仍能取到 MySQLError)。
// 抽成自由函数是因为建 store 的探针 P4 + P5(newRouteStoreFromDB)也要同一套重来规则,而那时 store 还没建出来:
// lockRetries 为 nil 时不计数(日志照打)。
func retryRouteTx(ctx context.Context, op string, lockRetries *atomic.Uint64, attempt func() error) error {
	var err error
	for n := 1; n <= routeTxAttempts; n++ {
		if err = attempt(); err == nil {
			return nil
		}
		if !isRouteLockConflict(err) {
			return err
		}
		if n == routeTxAttempts {
			break
		}
		if lockRetries != nil {
			lockRetries.Add(1)
		}
		logx.Infof("[RouteStore] %s retry %d/%d after lock conflict: %v", op, n, routeTxAttempts, err)
		if waitErr := sleepCtx(ctx, jitteredBackoff(routeLockRetryBackoffMin, routeLockRetryBackoffMax)); waitErr != nil {
			return fmt.Errorf("%s: %w", op, errors.Join(err, waitErr))
		}
	}
	return fmt.Errorf("%s: gave up after %d attempts: %w", op, routeTxAttempts, err)
}

// ── 只读接口(都是一致读,不取锁)──────────────────────────────────────────────

// MergedIntoRow 是 zone_merged_into 的一行。
type MergedIntoRow struct {
	SrcZone    uint32
	DstZone    uint32
	RunID      string
	MergedAtMs uint64
}

// GetRoutes 按 id 批量点查。缺席的 id 不出现在结果里(不是错误);id 为 0 的项被跳过。
// 超过 RouteBatchLimit 直接拒绝。
//
// 按「正确的片 + id」点查:库里一条错片行(只有 CHECK 曾经不生效时才写得进来)在这里**表现为缺席**,
// 不会报错。防它靠 chk_route_shard 与启动时的只读核验(verifyRouteShardConstraint)。
func (s *RouteStore) GetRoutes(ctx context.Context, ids []uint64) (map[uint64]RouteRow, error) {
	if len(ids) > RouteBatchLimit {
		return nil, fmt.Errorf("%w: %d ids exceed RouteBatchLimit %d", ErrRouteInvalidArgument, len(ids), RouteBatchLimit)
	}
	nonZero := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id != 0 {
			nonZero = append(nonZero, id)
		}
	}
	if len(nonZero) == 0 {
		return map[uint64]RouteRow{}, nil
	}
	return loadRoutes(ctx, s.db, nonZero)
}

// loadRoutes 按 id 批量读(R13)。实参按聚簇键升序交错给出 (行片, player_id)(拷一份再排,不改调用方的切片);
// 结果仍按 player_id 索引。
func loadRoutes(ctx context.Context, q routeQueryer, ids []uint64) (map[uint64]RouteRow, error) {
	keyed := append([]uint64(nil), ids...)
	sortRouteIDsByKey(keyed)
	args := make([]any, 0, 2*len(keyed))
	for _, id := range keyed {
		args = append(args, routeShardArg(id), id)
	}
	rows, err := q.QueryContext(ctx, routeSelectRowsSQL(len(keyed)), args...)
	if err != nil {
		return nil, fmt.Errorf("read player_route (%d ids): %w", len(ids), err)
	}
	defer rows.Close()

	out := make(map[uint64]RouteRow, len(ids))
	for rows.Next() {
		var row RouteRow
		if err := scanRouteRow(rows, &row, true); err != nil {
			return nil, fmt.Errorf("scan player_route row: %w", err)
		}
		out[row.PlayerID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate player_route rows: %w", err)
	}
	return out, nil
}

// ListMergedInto 列出全部合走标记,按 src_zone 升序(行数 = 合过的区数,不分页)。
func (s *RouteStore) ListMergedInto(ctx context.Context) ([]MergedIntoRow, error) {
	rows, err := s.db.QueryContext(ctx, routeListMergedIntoSQL)
	if err != nil {
		return nil, fmt.Errorf("list zone_merged_into: %w", err)
	}
	defer rows.Close()

	var out []MergedIntoRow
	for rows.Next() {
		var r MergedIntoRow
		if err := rows.Scan(&r.SrcZone, &r.DstZone, &r.RunID, &r.MergedAtMs); err != nil {
			return nil, fmt.Errorf("scan zone_merged_into row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate zone_merged_into: %w", err)
	}
	return out, nil
}

// ── 迁移期的行管理(由 schema.go 的 migrateSchemaOn 在表就位、形状守卫通过之后调用)────────────

// bootstrapRouteMetaRow 种 route_meta(id=1):generation 是 16 字节 crypto/rand 的十六进制(32 字符),
// writable=1。ODKU no-op:已存在的行一个字节都不碰(尤其不换 generation:写者资格框架的每个事务都核它)。
// 日志区分「新种了一行」与「已存在,未改」。
func bootstrapRouteMetaRow(ctx context.Context, db *sql.DB) error {
	generation, err := newRouteGeneration()
	if err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, routeMetaSeedSQL, generation)
	if err != nil {
		return fmt.Errorf("bootstrap route_meta row id=%d: %w", routeMetaRowID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("bootstrap route_meta row id=%d rows affected: %w", routeMetaRowID, err)
	}
	switch n {
	case 1:
		logx.Infof("[schema] route_meta: seeded row id=%d generation=%s writable=1", routeMetaRowID, generation)
	case 0:
		logx.Infof("[schema] route_meta: row id=%d already present, left untouched", routeMetaRowID)
	default:
		return fmt.Errorf("bootstrap route_meta row id=%d: 受影响行数 %d 不在 {1=新种, 0=已存在} 之内,"+
			"routeMetaSeedSQL 的 ON DUPLICATE KEY UPDATE 必须保持 no-op", routeMetaRowID, n)
	}
	return nil
}

// newRouteGeneration 生成一个新的库世代标识(32 个十六进制字符)。
func newRouteGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate route_meta generation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
