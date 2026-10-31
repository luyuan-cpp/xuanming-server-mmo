package store

import (
	"bytes"
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

	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/placement"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// 玩家归属与落点映射的持久化真源(docs/design/player-route-mapping-durability-proposal.md,第 1a 批)。
//
// 路由表(建表语句在 schema.go 的 bootstrapTables):
//   - player_route:每个玩家一行,home_zone 与落点同行;rev 本行任何列变化 +1,只供缓存排序。
//     聚簇主键 (route_shard, player_id),route_shard = placement.RouteRowShard(player_id)(G1a,TiDB 写热点修复):
//     **按 player_id 的每一条读写都必须同时带 route_shard**,片一律经 placement.RouteRowShard 算;
//   - zone_merged_into:合服映射(源区 → 目标区)的永久记录,由 route_tool 从 Redis 导入,由 merge_zone 的合服 / 撤销写入与删除;
//     别名合服之后它就是合服的全部写入:玩家行和路由行都不动(docs/design/player-data-tidb-scaling-plan.md §7.1);
//   - route_meta:开关表,单行(id=1),writable=0 = 全体映射写者拒写;很少写;
//   - route_reconcile:对账表,单行(id=1),对账 leader(route_reconcile.go)的租约、水位与 cache_verified_ms。
//
// 写入方:data_service 进程内只有本文件与 route_reconcile.go(route_store_test.go 的 TestNoPlayerRouteWritesOutsideRouteStore /
// TestRouteMetaAndReconcileWritesOnlyInStoreFiles 用 AST 机械守住);进程外只有 tools/merge_zone/route_store.go
// (它自己的 TestNoRouteTableWritesOutsideMergeRouteStore 与源码比对守卫)。Redis 一侧的发布(set-if-newer)在 routing 包,不在这里:
// 事务里绝不做 Redis 读写 —— 预检在开事务之前,发布在提交之后(规格 c.2 第 3 条)。
//
// 2026-10-18(第 1b-1 批)补:route_meta / route_reconcile 的写语句同样只在本文件(TestRouteMetaAndReconcileWritesOnlyInStoreFiles
// 用同样的 AST 守卫钉住;第 1b-2 批的对账 leader 新增 route_reconcile.go 时一并放进允许集合)。本批新增的写入方:
// 冻结 / 解冻(FreezeRouteWriters / UnfreezeRouteWriters,route_tool freeze / unfreeze)与冻结态下的反向修库
// (RepairRoutesFromCache / RepairMergedIntoFromCache,route_tool reconcile-from-cache)。锁序见下面不变量 2 的补充。
//
// 行片 rowShard = player_id % placement.RouteRowShards(聚簇主键首列,常量 64,建表后不可改)。
//
// 不变量,按重要性排序:
//  1. **判定只认加锁读**(R5 / R8 / R11 的 FOR UPDATE)。新行一律 ODKU no-op 插入、撞键 = 受影响 0 行,
//     **不用 INSERT IGNORE**:它会把 CHECK 违例降成 warning,坏值就静默进库了(提案 §4.1-3)。
//  2. **锁顺序**:route_meta(普通读)→ player_route(按聚簇键 (route_shard, player_id);
//     ODKU 或 FOR UPDATE)。W1 只等最多一个玩家行;导入只按聚簇键升序锁玩家行;
//     任何一次锁多行的语句(R8 FOR UPDATE、R7、R9 循环)都按聚簇键升序,两个并发导入之间也不成环(规格 c.2)。
//     2026-10-18(第 1b-1 批)补:反向修库 route_meta(R18x FOR UPDATE)→ player_route(按聚簇键升序);
//     解冻 route_reconcile(R20 FOR UPDATE)→ route_meta(R22);冻结只写 route_meta(R19,单条自动提交)。
//     W1、导入与建角的初始提交对 route_meta 只做普通读、从不持它的锁(每笔业务事务都锁这一行就是全服排队,
//     提交点的保证靠写者闸门,见 lockInitialPlayerRoute);只有低频的修库、解冻与建池探针才锁它。
//     建池探针守卫是 route_meta → player_route(与修库同向),没有任何一方持 route_meta 再去等 route_reconcile,所以不成环。
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
	RouteReconcileTableName = "route_reconcile"

	// RouteHomeZoneMax 是 home_zone 的上限,= playerRouteBootstrapDDL 里 chk_route_home 的 999999
	// (与 placement.MaxZoneStorageID 同值:zone id 与 zone 库编号是同一个空间)。
	// 应用层在写之前就拒绝,CHECK 只兜底(TiDB < 7.2 或没开 tidb_enable_check_constraint 时 CHECK 不生效)。
	RouteHomeZoneMax uint32 = 999999

	// RouteBatchLimit 是 GetRoutes / ImportRoutes / ScanRoutes / ImportMergedInto 单批上限:
	// 一条无上限的 IN 会把单次查询变成事实上的全表读,导入一批的事务也会持锁过久。
	// 占位符:R8 / R13 每批 2×N 个(行构造器 (route_shard, player_id) 成对),R7 每批 8×N 个;
	// N = 1000 时最多 8000 个,远低于 MySQL 预处理语句 65535 的上限。
	RouteBatchLimit = 1000

	// routeMetaRowID 是 route_meta / route_reconcile 唯一那一行的 id(R1 / R15 / R16 里写死的 1)。
	routeMetaRowID = 1

	// routeTxAttempts 事务尝试上限:1 次 + 1213/9007 重试 3 次(导入里 R7 行数对不上的竞态重来共用这一预算)。
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
	// ErrRouteInvalidArgument 入参在库层就被拒:player_id=0、home_zone 越界、导入批次乱序 / 重复 / 超限等。
	// 纵深防御:路由层也会先挡一遍,但写进库的垃圾行没有人会再去清。
	ErrRouteInvalidArgument = errors.New("player_route: invalid argument")
	// ErrRouteMetaMissing route_meta 没有 id=1 那一行(迁移没跑过,或库被重置后没重跑迁移)。
	ErrRouteMetaMissing = errors.New("route_meta: row id=1 missing; run `data_service -f <yaml> -migrate`")
	// ErrRouteNotWritable route_meta.writable != 1:映射写者被冻结(第 1b 批的回档恢复流程会置 0)。
	ErrRouteNotWritable = errors.New("route_meta: writable!=1, route writers are frozen")
	// ErrRouteRowVanished ODKU 撞了键(受影响 0 行),紧接着的加锁读却读不到这一行。
	// 本批没有删 player_route 的写者,出现即说明有人绕过本 store 删行,fail-closed。
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
		"tables hold no data worth keeping, DROP TABLE player_route, zone_merged_into, route_meta, " +
		"route_reconcile by hand (proposal §11.2 HR4), then rerun `data_service -f <yaml> -migrate`; " +
		"do not change the constant to fit the old table")
	// ErrRouteShardMismatch 库里某一行存的 route_shard 不等于 placement.RouteRowShard(player_id):只有 CHECK 曾经
	// 不生效时被绕过 store 的写者写进来。fail-closed,整页 / 整批作废。只有全表键集扫描(ScanRoutes)在真库上
	// 看得见这种行;按 id 的点读(R5 / R8 / R13)按正确片查,错片行对它们表现为缺席。
	ErrRouteShardMismatch = errors.New("player_route: stored route_shard does not match placement.RouteRowShard(player_id)")
	// ErrRouteTiDBNotClustered tidb-prepare 的前置核对失败:player_route 在 TiDB 上不是聚簇主键。
	// 非聚簇的表按 (route_shard, player_id) 切毫无意义,而且建表后改不了,只能趁表空 DROP 后用 -migrate 重建。
	ErrRouteTiDBNotClustered = errors.New("route tables are not CLUSTERED on TiDB; while they are still empty, " +
		"DROP them and rebuild with `data_service -f <yaml> -migrate`")
	// ErrRouteNotFrozen route_meta.writable == 1:反向修库(缓存 → 库)只许在映射写者冻结时做(第 1b-1 批),
	// 否则修库写进去的旧缓存值会与并发的 W1 互相覆盖。先 `route-tool freeze`。
	ErrRouteNotFrozen = errors.New("route_meta: writable=1; repairing the DB from the cache needs the route writers frozen first " +
		"(`data_service -f <yaml> -route-tool freeze`)")
	// ErrRouteReconcileMissing route_reconcile 没有 id=1 那一行(迁移没跑过,或库被重置后没重跑迁移)。
	ErrRouteReconcileMissing = errors.New("route_reconcile: row id=1 missing; run `data_service -f <yaml> -migrate`")
)

// errRouteImportRaced 内部哨兵:导入的 R7 受影响行数小于待插行数 —— R8 之后有 W1 并发插入了同一个 id。
// 整批回滚后重来(重来时那一行会被 R8 读到);不是锁冲突,不计 lockRetries、不退避。
var errRouteImportRaced = errors.New("player_route import: a concurrent writer inserted a row after the locking read")

// dbNowMsSQL 是库时钟的 epoch 毫秒。所有时间戳都取它,不取任何一台应用机的本地时钟。
const dbNowMsSQL = "CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED)"

// routeRowColumnsSQL 是 RouteRow 除聚簇键 (route_shard, player_id) 外的列,顺序与 scanRouteRow 的 Scan 目标一一对应。
const routeRowColumnsSQL = "home_zone, storage_id, placement_version, frozen_run_id, rev, updated_by, created_at_ms, updated_at_ms"

// 本 store 的全部 SQL(编号同规格 b.4;G1a 起按 player_id 的读写都带 route_shard)。
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

	// R1 读可写开关(普通一致读,提案 §4.1-2 第 1 步)。建 store 的探针也用它确认 meta 行在。
	routeReadWritableSQL = "SELECT writable FROM route_meta WHERE id = 1"
	// 建池探针的守卫(lockRouteProbeGuard):同一行,加锁读。
	routeLockProbeGuardSQL = routeReadWritableSQL + " FOR UPDATE"

	// R4 新行。storage / version 用 nullU32 / nullU64 绑定(0 → NULL)。受影响 1 = 插入,0 = 撞键 no-op。
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

	// R7 导入批量插的三段(VALUES 按聚簇键 (route_shard, player_id) 升序;rev 逐行绑定 = maprev 或 1),
	// 由 routeImportInsertSQL 拼。每行实参:行片、player_id、home、storage、version、frozen、rev、writer。
	routeImportInsertHeadSQL = "INSERT INTO player_route (route_shard, player_id, home_zone, storage_id, placement_version, frozen_run_id," +
		" rev, updated_by, created_at_ms, updated_at_ms) VALUES "
	routeImportInsertTupleSQL = "(?, ?, ?, ?, ?, ?, ?, ?, " + dbNowMsSQL + ", " + dbNowMsSQL + ")"
	routeImportInsertTailSQL  = " ON DUPLICATE KEY UPDATE player_id = player_id"

	// R8 / R13 按 id 批量读的头部,由 routeSelectRowsSQL 拼(导入加 FOR UPDATE;dry-run 与 GetRoutes 不加)。
	// 用行构造器 (route_shard, player_id) IN ((?, ?), …):作为聚簇主键的前缀,MySQL 8.0 走 PRIMARY 的 range
	// (由真库 EXPLAIN 用例取证),TiDB 期望 Batch_Point_Get(未核实,只记录计划)。
	routeSelectRowsHeadSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE (route_shard, player_id) IN ("

	// R9 导入补列(按 R8 读到的 rev 做 CAS;所有可空列按 null* 绑定,dbAhead 的列原样写回库值)。
	// 实参:home、storage、version、frozen、writer、行片、player_id、库 rev。
	routeImportFillSQL = "UPDATE player_route SET home_zone = ?, storage_id = ?, placement_version = ?, frozen_run_id = ?," +
		" rev = rev + 1, updated_by = ?, updated_at_ms = " + dbNowMsSQL +
		" WHERE route_shard = ? AND player_id = ? AND rev = ?"

	// R10 / R11 merged_into(每行一个事务)。R11 的只读形态给 dry-run 用。
	routeInsertMergedIntoSQL = "INSERT INTO zone_merged_into (src_zone, dst_zone, run_id, merged_at_ms)" +
		" VALUES (?, ?, ?, " + dbNowMsSQL + ") ON DUPLICATE KEY UPDATE src_zone = src_zone"
	routeLockMergedIntoSQL = "SELECT dst_zone, run_id, merged_at_ms FROM zone_merged_into WHERE src_zone = ? FOR UPDATE"
	routeReadMergedIntoSQL = "SELECT dst_zone, run_id, merged_at_ms FROM zone_merged_into WHERE src_zone = ?"

	// R12 键集翻页 / R14 列 merged_into(都是一致读,不取锁)。
	// R12 沿聚簇键 (route_shard, player_id) 翻页,实参:游标片、游标片、游标 player_id、limit。
	// 写成 `a > x OR (a = x AND b > y)` 而不是行比较 `(a, b) > (x, y)`:前者两种引擎都确定能走主键 range,
	// 后者 MySQL 不做 range 优化。
	routeScanSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE route_shard > ? OR (route_shard = ? AND player_id > ?)" +
		" ORDER BY route_shard, player_id LIMIT ?"
	routeListMergedIntoSQL = "SELECT src_zone, dst_zone, run_id, merged_at_ms FROM zone_merged_into ORDER BY src_zone"

	// R15 迁移种 meta 行(ODKU no-op:已存在时 generation / writable 一个字节都不改)。
	routeMetaSeedSQL = "INSERT INTO route_meta (id, generation, writable)" +
		" VALUES (1, ?, 1) ON DUPLICATE KEY UPDATE id = id"

	// R16 迁移种对账行(ODKU no-op:已存在时 leader / 租约 / 水位一个字节都不改 —— 第 1b 批的 leader 可能正在跑)。
	routeReconcileSeedSQL = "INSERT INTO route_reconcile (id, watermark_ms, cache_verified_ms)" +
		" VALUES (1, 0, 0) ON DUPLICATE KEY UPDATE id = id"

	// ── 第 1b-1 批(2026-10-18):冻结、解冻与冻结态下的反向修库 ──

	// R17 反向修库的整行覆盖:缓存 maprev > 库 rev(库被回档)或同 rev 平局且显式 -resolve-ties=cache 时,把缓存那一版
	// 连同 rev 写回库。rev 置为 maprev 而不是 +1:两边 rev 相同,之后的发布与 audit 才认得出是同一版。
	// 按读到的库 rev 做 CAS(行已被 R8 锁住,不是恰好 1 行就是不可能状态)。
	// 实参:home、storage、version、frozen、rev(= maprev)、writer、行片、player_id、库 rev。
	routeRepairOverwriteSQL = "UPDATE player_route SET home_zone = ?, storage_id = ?, placement_version = ?, frozen_run_id = ?," +
		" rev = ?, updated_by = ?, updated_at_ms = " + dbNowMsSQL +
		" WHERE route_shard = ? AND player_id = ? AND rev = ?"

	// R18 读 route_meta 整行(普通一致读):库世代与可写开关。
	routeReadMetaSQL = "SELECT generation, writable FROM route_meta WHERE id = 1"
	// R18x R18 的加锁形态:反向修库的写事务第一句,在提交点用同一把行锁复核冻结态。修库持着它直到提交,
	// 并发的解冻(R22 要这一行的 X 锁)只能排在修库提交之后 —— 普通读做不到这一点:解冻一提交,
	// 已经过了检查的修库事务照样提交,而 W1 已经恢复写入(AGENTS.md §11.3「提交点用同一并发控制机制原子复核」)。
	// 与建池探针的守卫(routeLockProbeGuardSQL)锁同一行、文本不同:多取一列 generation 进错误信息,契约用例也靠文本区分两者。
	routeLockMetaSQL = routeReadMetaSQL + " FOR UPDATE"

	// writable / epoch / catalog 仅由 shared authoritysql.Catalog 的正式维护事务修改。

)

// RouteFreezeTimeoutMin 是含冻结写(R19 / R22)的调用至少要给的时限。TiDB 把 route_meta 设成缓存表之后
// (提案 §11.2「TiDB 运维步骤」第 6 条,S2 档起必选),对它的写要等当前读租约到期(tidb_table_cache_lease,
// 缺省 3s【未核实】),这段等待不是锁等待、不受会话 innodb_lock_wait_timeout=1 约束,只受 ctx 约束。
// 所以时限取 max(5s, 租约 + 2s);租约调大时调用方(route_tool -freeze-timeout)要同步放大。
// 本批 tidb-prepare 不发 `ALTER TABLE route_meta CACHE`,那一步留给 TiDB Phase 1 的部署步骤。
const RouteFreezeTimeoutMin = 5 * time.Second

// routeImportInsertSQL 拼 n 行的 R7。
func routeImportInsertSQL(n int) string {
	var b strings.Builder
	b.WriteString(routeImportInsertHeadSQL)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(routeImportInsertTupleSQL)
	}
	b.WriteString(routeImportInsertTailSQL)
	return b.String()
}

// routeSelectRowsSQL 拼 n 个 id 的批量读(R8 / R13):n 个行构造器 (?, ?),实参按聚簇键升序交错给出
// (行片_i, player_id_i)。ORDER BY route_shard, player_id 让加锁形态按聚簇键升序取锁
// (与 W1「同一时刻只等一个玩家行」配合,不成环)。
func routeSelectRowsSQL(n int, forUpdate bool) string {
	var b strings.Builder
	b.WriteString(routeSelectRowsHeadSQL)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?)")
	}
	b.WriteString(") ORDER BY route_shard, player_id")
	if forUpdate {
		b.WriteString(" FOR UPDATE")
	}
	return b.String()
}

// RouteScanCursor 是 ScanRoutes 的键集游标 = 上一页最后一行的聚簇键 (route_shard, player_id)。
// 零值 = 从头开始(player_id 为 0 的行不合法,所以 {0, 0} 之后就是第一行)。
type RouteScanCursor struct {
	Shard    uint32
	PlayerID uint64
}

// RouteScanCursorAt 返回 row 的聚簇键(片由 player_id 算出;ScanRoutes 已校验库里存的片与之一致)。
func RouteScanCursorAt(row RouteRow) RouteScanCursor {
	return routeKeyOf(row.PlayerID)
}

// Less 按聚簇键序比较(先片、后 player_id)。假 routeDB、导入排序与 loadRoutes 共用这一个判据。
func (c RouteScanCursor) Less(o RouteScanCursor) bool {
	if c.Shard != o.Shard {
		return c.Shard < o.Shard
	}
	return c.PlayerID < o.PlayerID
}

// routeKeyOf 返回 player_id 的聚簇键 (route_shard, player_id)。片只经 placement.RouteRowShard 算。
func routeKeyOf(playerID uint64) RouteScanCursor {
	return RouteScanCursor{Shard: placement.RouteRowShard(playerID), PlayerID: playerID}
}

// routeShardArg 是绑进 SQL 的行片实参。与 uint64(req.HomeZone) 同口径交给驱动 uint64(见 nullU32 的注释)。
func routeShardArg(playerID uint64) uint64 {
	return uint64(placement.RouteRowShard(playerID))
}

// sortRouteIDsByKey 把 ids 原地按聚簇键升序排(任何一次锁多行的语句都按这个顺序取锁)。
func sortRouteIDsByKey(ids []uint64) {
	sort.Slice(ids, func(i, j int) bool { return routeKeyOf(ids[i]).Less(routeKeyOf(ids[j])) })
}

// nullU32 / nullU64 / nullString 把「0 / 空串 = NULL」的领域约定翻成 SQL 参数。
//
// 绑定统一走这套「0 / 空串 = NULL」的约定,刻意不用 sql.Null[T] 当参数:它的 Value() 随 Go 版本变化 ——
// 1.24 之前原样回 T(uint32 会被 go-sql-driver 以 "non-Value type" 拒绝),1.24 起先过
// driver.DefaultParameterConverter(uint64 高位为 1 时报错)。两种行为都不是我们想要的绑定形状;这里直接给驱动
// uint64 / nil,与 Go 版本无关(契约用例 TestRegisterHomeBindsNullForAbsentPlacement 钉住实参形状)。
// 也不用 NullInt64 扫描 / 绑定:bigint unsigned 可能超过 2^63。
func nullU32(v uint32) any {
	if v == 0 {
		return nil
	}
	return uint64(v)
}

func nullU64(v uint64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── 建 store 与探针 ─────────────────────────────────────────────────────────────

// RouteStoreOptions 是 RouteStore 的行为参数,来自 config.RouteStoreConfig。
type RouteStoreOptions struct {
	// MaintenanceFreeze 仅给恢复工具；必须是该库永久正常冻结收据，不能使用紧急冻结。
	MaintenanceFreeze *pb.SqlWriterMaintenanceReceipt
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
	db                *sql.DB
	writer            string
	maintenanceFreeze *pb.SqlWriterMaintenanceReceipt

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
	st := &RouteStore{db: db, writer: opts.Writer}
	if opts.MaintenanceFreeze != nil {
		st.maintenanceFreeze = proto.Clone(opts.MaintenanceFreeze).(*pb.SqlWriterMaintenanceReceipt)
	}
	return st, nil
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
// 三个入口共用:db_first 的 data_service 装配、debugutil、route_tool。configPath 是 data_service 的 yaml 路径,
// 不知道时传 "<yaml>"。表按另一个行片模数建的(ErrRouteShardModulusMismatch)不能照常提示「先跑迁移」:
// CREATE TABLE IF NOT EXISTS 不改已有的表,只重跑 -migrate 修不好,要先按报错里的步骤 DROP 四张路由表
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
//     这一步是一次必然回滚的写尝试,所以只读用途(route_tool audit)也要 player_route 的 INSERT 权限;
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
		logx.Errorf("[RouteStore] route_meta(id=%d).writable=%d: route writers are frozen, every db_first RegisterPlayerZone "+
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
// CHECK 不生效时一个算错片的写者就能给同一个 player_id 写出两行 —— 而 R5 / R8 / R13 都按「正确的片 + id」点查,
// 错片行对它们不可见,读路径兜不住(只有全表键集扫描 ScanRoutes 看得见)。所以在一个必然回滚的事务里插一行
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
			"remove it by hand after review, then run `route_tool audit`: %w", err)
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
// 作用范围:只在建 route store 时跑(db_first 的 data_service、debugutil、route_tool)。redis 模式的 data_service
// 不建这个 store,它启动期的迁移照样会建出(或留着)旧模数的表而这里挡不住,所以提案 §11.2「行片 16 → 64」与
// runbook §9.3 前置的人工核对(SHOW CREATE TABLE player_route)仍要做。
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
func routeShardModulusProbeKeys() []RouteScanCursor {
	n := uint64(placement.RouteRowShards)
	return []RouteScanCursor{routeKeyOf(n), routeKeyOf(n - 1)}
}

// namesRouteShardCheck 报告一条 3819 的报错是否点名 chk_route_shard(P4 与 P5 共用这一个判据)。只按名字的子串认:
// MySQL 8.0 的原文是 "Check constraint 'chk_route_shard' is violated.",TiDB 的引号格式未核实;
// 其余五条约束(chk_route_home 等)的名字都不含这个子串。
func namesRouteShardCheck(me *mysql.MySQLError) bool {
	return strings.Contains(strings.ToLower(me.Message), "chk_route_shard")
}

// routeProbeInsertDenied 是 P4 / P5 收到 1142 时的报错:两步都是必然回滚的 INSERT,只读用途也要这条权限。
func routeProbeInsertDenied(err error) error {
	return fmt.Errorf("route store: the chk_route_shard probes need INSERT privilege on player_route "+
		"(P4 and the modulus probe P5 are rolled-back inserts, so read-only uses such as route_tool audit need it too): %w", err)
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
// W1 对 route_meta 只做一致读(requireRouteWritable),不受它影响;冻结写者(1b 把 writable 置 0)最多等一个探针事务。
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
			"(UPDATE privilege), including for read-only uses such as route_tool audit: %w", err)
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
// 普通读而非加锁读:第 1b 批把它置 0 时,已经过了这一步的事务仍会提交,1b 的流程要先等它们结束(规格 g.3 第 3 条)。
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

// RouteRow 是 player_route 的一行,**领域类型**:带 HasHome / Placement / HomeValue 等判定,是 SQL 扫描与发布的载体。
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
	// CreatedAtMs / UpdatedAtMs 取库时钟。写入分支(Inserted / Filled*)不回读:Inserted 两者为 0,
	// Filled* 的 CreatedAtMs 是加锁读到的值、UpdatedAtMs 为 0。发布不用这两列。
	CreatedAtMs uint64
	UpdatedAtMs uint64
}

// HasHome 报告这一行有没有 home_zone。
func (r RouteRow) HasHome() bool { return r.HomeZone != 0 }

// HasPlacement 报告这一行有没有落点记录。
func (r RouteRow) HasPlacement() bool { return r.StorageID != 0 }

// IsTombstone 两列都是 NULL = 「真的没有记录」(提案 §3.2 末),与没有这一行同义。
func (r RouteRow) IsTombstone() bool { return !r.HasHome() && !r.HasPlacement() }

// HomeValue 返回 player:zone 的值:没有 home 为空串(发布脚本按空串 DEL),否则十进制。
func (r RouteRow) HomeValue() string {
	if r.HomeZone == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(r.HomeZone), 10)
}

// Placement 把落点三列还原成 placement.Record。没有落点时回 (零值, false, nil)。
// 有落点就 Validate,三列不自洽(version 为 0、冻结 run_id 不合法、没有落点却带着 version / run_id)
// 一律回 ErrMalformed —— 读方必须 fail-closed,绝不能把一条坏行发布进缓存。
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

// PlacementValue 返回 player:placement 的值:没有落点为空串,否则 Record.Encode()。
func (r RouteRow) PlacementValue() (string, error) {
	rec, ok, err := r.Placement()
	if err != nil || !ok {
		return "", err
	}
	return rec.Encode(), nil
}

// withPlacement 把一条落点记录写进行的三列(nil = 清空)。
func (r RouteRow) withPlacement(rec *placement.Record) RouteRow {
	r.StorageID, r.PlacementVersion, r.FrozenRunID = 0, 0, ""
	if rec != nil {
		r.StorageID = rec.StorageID
		r.PlacementVersion = rec.Version
		if rec.Frozen {
			r.FrozenRunID = rec.RunID
		}
	}
	return r
}

// routeScanner 是 *sql.Row 与 *sql.Rows 共有的 Scan。
type routeScanner interface {
	Scan(dest ...any) error
}

// scanRouteRow 按 routeRowColumnsSQL 的顺序扫一行(withKey 时最前面多两列聚簇键 route_shard, player_id)。
// 可空列用 sql.Null[uint32] / sql.Null[uint64] / sql.NullString 扫:bigint unsigned 可能超过 2^63,NullInt64 装不下。
//
// withKey 时校验库里存的 route_shard == placement.RouteRowShard(player_id),不等即 ErrRouteShardMismatch(fail-closed)。
// 真库上只有全表键集扫描(R12)看得见错片行;R8 / R13 按正确片点查,这里对它们只是纵深防御
// (见 ErrRouteShardMismatch)。RouteRow 不带片:片由 player_id 唯一决定,校验一致后丢弃。
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

// RouteRegisterRequest 是一次 W1(RegisterPlayerZone,db_first)的入参。
type RouteRegisterRequest struct {
	PlayerID  uint64
	HomeZone  uint32 // 1..RouteHomeZoneMax
	StorageID uint32 // 0 = 不钉落点
	// ExistingRowOnly = 调用方在缓存里已经见过这名玩家(player:zone / placement / maprev 任一在):
	// 库里就必须已有这一行,只认已有行、绝不新插。缺行说明 P1 导入没做或库被回档,回 RowMissing 由人处理,
	// 而不是悄悄插一行 rev=1 —— 那会让之后的发布被缓存里更大的 maprev 判成 Superseded,永远发布不上去。
	ExistingRowOnly bool
}

// RouteRegisterOutcome 是一次 W1 在库里的终局。业务结果(冲突、缺行)走这里,只有故障才走 error。
type RouteRegisterOutcome uint8

const (
	// RouteRegisterInserted 新插入了一行(rev=1)。
	RouteRegisterInserted RouteRegisterOutcome = iota + 1
	// RouteRegisterFilledHome 行已在、home 为 NULL,本次补上 home(以及请求带的落点,如果行里没有落点)。
	RouteRegisterFilledHome
	// RouteRegisterFilledHomePlacementKept 补上了 home,但行里已有落点、请求也带了 storage:落点不覆盖
	// (它指向玩家数据**真正所在**的库),与 redis 模式的 zoneRegisteredPlacementKept 同义,调用方打 ERROR。
	RouteRegisterFilledHomePlacementKept
	// RouteRegisterAlreadySame 行里 home 已等于请求值:幂等,没有写入。调用方仍要重新发布(补齐上次失败的发布)。
	RouteRegisterAlreadySame
	// RouteRegisterConflict 行里 home 是另一个值,ExistingHome 给出。不写。
	RouteRegisterConflict
	// RouteRegisterRowMissing 只在 ExistingRowOnly 时出现:缓存认得这名玩家,库里却没有行。不写。
	RouteRegisterRowMissing
)

// String 仅供日志与指标 label(低基数,六个固定值)。
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
	case RouteRegisterRowMissing:
		return "row_missing"
	default:
		return "unknown"
	}
}

// RouteRegisterResult 是 RegisterHome 的结果。
type RouteRegisterResult struct {
	Outcome RouteRegisterOutcome
	// Row 在 Inserted / Filled* / AlreadySame 时 = 提交后(或读到)的行,用于发布;其余结局为零值。
	Row RouteRow
	// ExistingHome 只在 Conflict 时有值。
	ExistingHome uint32
}

// RegisterHome 是 W1 的库内提交点(规格 c.2):
//
//	BEGIN(RC,DSN 已设)
//	R1 普通读 route_meta.writable            缺行 → ErrRouteMetaMissing;!=1 → ErrRouteNotWritable
//	R4 ODKU 插新行(仅 !ExistingRowOnly)     1 行 → COMMIT,Inserted;0 行 → 往下;其它 → fail-closed
//	R5 加锁读玩家行 (player_id % 64, id)      缺行 → RowMissing(ExistingRowOnly)/ ErrRouteRowVanished
//	   home == 请求 → 回滚,AlreadySame;home 是别的值 → 回滚,Conflict
//	   home 为 NULL → R6b(请求带 storage 且行里没落点)或 R6a → COMMIT,Filled*
//
// 不看合服状态:合服过程中在源区建号是允许的,home_zone 就是请求里的源区,归属解析之后落到目标区
// (docs/design/player-data-tidb-scaling-plan.md §7.1)。
//
// 错误语义:冲突、缺行通过 Outcome 返回,err 为 nil;只有故障才返回 err —— 参数非法
// (ErrRouteInvalidArgument)、meta 缺失 / 不可写、行消失、SQL 错误(含 1205)、1213 / 9007 重试用尽。
// COMMIT 出错时结果不确定,按 err 返回;调用方重试会落到幂等分支(AlreadySame),安全。
//
// 事务里绝不做 Redis 读写:预检在调用方开事务之前,发布在提交之后。
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

	// R4(调用方在缓存里见过这名玩家时跳过:只认已有行)
	if !req.ExistingRowOnly {
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
			row := RouteRow{PlayerID: req.PlayerID, HomeZone: req.HomeZone, Rev: 1, UpdatedBy: s.writer}
			if req.StorageID != 0 {
				row.StorageID, row.PlacementVersion = req.StorageID, 1
			}
			return RouteRegisterResult{Outcome: RouteRegisterInserted, Row: row}, nil
		case 0:
			// 撞键:行已在,落到 R5 用加锁读判定。
		default:
			return RouteRegisterResult{}, fmt.Errorf("insert player_route player_id=%d: 受影响行数 %d 不在 {1=插入, 0=撞键 no-op} 之内,"+
				"routeInsertNewSQL 的 ON DUPLICATE KEY UPDATE 必须保持 no-op、连接不得带 CLIENT_FOUND_ROWS", req.PlayerID, n)
		}
	}

	// R5
	row := RouteRow{PlayerID: req.PlayerID}
	err = scanRouteRow(tx.QueryRowContext(ctx, routeLockRowSQL, rowShard, req.PlayerID), &row, false)
	if errors.Is(err, sql.ErrNoRows) {
		if req.ExistingRowOnly {
			return RouteRegisterResult{Outcome: RouteRegisterRowMissing}, nil
		}
		return RouteRegisterResult{}, fmt.Errorf("%w (player_id=%d)", ErrRouteRowVanished, req.PlayerID)
	}
	if err != nil {
		return RouteRegisterResult{}, fmt.Errorf("lock player_route player_id=%d: %w", req.PlayerID, err)
	}
	switch {
	case row.HomeZone == req.HomeZone:
		return RouteRegisterResult{Outcome: RouteRegisterAlreadySame, Row: row}, nil
	case row.HomeZone != 0:
		return RouteRegisterResult{Outcome: RouteRegisterConflict, ExistingHome: row.HomeZone}, nil
	}

	// home 为 NULL:补 home(以及请求带、行里没有的落点)。
	outcome := RouteRegisterFilledHome
	next := row
	next.HomeZone = req.HomeZone
	var upd sql.Result
	if req.StorageID != 0 && !row.HasPlacement() {
		upd, err = tx.ExecContext(ctx, routeFillHomePlacementSQL, uint64(req.HomeZone), storageArg, s.writer, rowShard, req.PlayerID, row.Rev)
		next.StorageID, next.PlacementVersion, next.FrozenRunID = req.StorageID, 1, ""
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
	next.Rev = row.Rev + 1
	next.UpdatedBy = s.writer
	next.UpdatedAtMs = 0 // 库时钟写入,不回读(发布不用)
	return RouteRegisterResult{Outcome: outcome, Row: next}, nil
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
//   - errRouteImportRaced:不是锁冲突,不计数、不退避,直接重来;
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
		raced := errors.Is(err, errRouteImportRaced)
		if !raced && !isRouteLockConflict(err) {
			return err
		}
		if n == routeTxAttempts {
			break
		}
		if raced {
			logx.Infof("[RouteStore] %s retry %d/%d: %v", op, n, routeTxAttempts, err)
			continue
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

// ── 导入(route_tool import-from-redis,提案 P1)────────────────────────────────

// RouteImportRow 是从 mapping Redis 读出的一名玩家(已按规范形式校验过)。
type RouteImportRow struct {
	PlayerID  uint64
	HomeZone  uint32            // 0 = Redis 无 player:zone
	Placement *placement.Record // nil = Redis 无 player:placement
	MapRev    uint64            // 0 = Redis 无 player:maprev
}

// RouteImportConflict 是一条不写入、要报给人看的分歧(冲突或 rev 倒退)。
type RouteImportConflict struct {
	PlayerID uint64
	DB       RouteRow
	Redis    RouteImportRow
}

// RouteImportStats 是一批导入的判定统计(dry-run 时是「如果写会怎样」)。
type RouteImportStats struct {
	Inserted, Matched, Filled, DBAhead int
	// RevRegressions 库 rev < maprev:疑似库被回档,不写(把它洗成 rev 倒退,之后的发布会被静默判成 Superseded)。
	RevRegressions []RouteImportConflict
	// Conflicts 库与 Redis 同一列都有值且不同:不覆盖,由人处理。
	Conflicts []RouteImportConflict
}

// ImportRoutes 把一批 Redis 映射导入 player_route(规格 b.2 / e 节)。
//
// rows 必须按 player_id 严格升序、无重复、不超过 RouteBatchLimit,每行至少有 home 或落点之一,
// 否则 ErrRouteInvalidArgument(一条语句都不发)。
//
// 非 dry-run 时一批一个事务:R1 → R8(按聚簇键升序加锁读已有行)→ decideImport 逐行判定 → R7(批量插新行)
// → R9(逐行补 NULL 列,按读到的 rev 做 CAS)→ COMMIT。R7 的受影响行数必须等于待插行数:少了说明 R8 之后
// 有 W1 并发插入了同一个 id,整批回滚后重来(那一行会被 R8 读到),总尝试次数不超过 routeTxAttempts。
// 导入只按聚簇键 (route_shard, player_id) 升序锁玩家行(R8 / R7 / R9 同序),与 W1、与另一个导入都不成环。
// 入参仍按 player_id 升序给(契约不变),排成聚簇键序是本函数内部的事。
//
// R1 是本实现在规格 b.4 之外补的一步(提案 §4.1-2 第 1 步「全体映射写者」都要先读 writable):
// 冻结期间导入同样拒写。
//
// dry-run(route_tool 的默认)只做 R8 的不加锁形态,不开事务、不写任何东西,同样给出统计。
// 1205 不重试,整批失败;导入幂等,重跑即可。
func (s *RouteStore) ImportRoutes(ctx context.Context, rows []RouteImportRow, dryRun bool) (RouteImportStats, error) {
	if err := validateRouteImportRows(rows); err != nil {
		return RouteImportStats{}, err
	}
	if len(rows) == 0 {
		return RouteImportStats{}, nil
	}
	ids := make([]uint64, len(rows))
	for i, r := range rows {
		ids[i] = r.PlayerID
	}

	if dryRun {
		existing, err := loadRoutes(ctx, s.db, ids, false)
		if err != nil {
			return RouteImportStats{}, err
		}
		return planRouteImport(rows, existing).stats, nil
	}

	var stats RouteImportStats
	op := fmt.Sprintf("player_route import (%d rows, player_id %d..%d)", len(rows), rows[0].PlayerID, rows[len(rows)-1].PlayerID)
	err := s.runRouteTx(ctx, op, func() error {
		var attemptErr error
		stats, attemptErr = s.importRoutesTx(ctx, ids, rows)
		return attemptErr
	})
	if err != nil {
		return RouteImportStats{}, err
	}
	return stats, nil
}

// importRoutesTx 一次导入事务尝试。
func (s *RouteStore) importRoutesTx(ctx context.Context, ids []uint64, rows []RouteImportRow) (RouteImportStats, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RouteImportStats{}, fmt.Errorf("begin player_route import tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := requireRouteWritable(ctx, tx); err != nil {
		return RouteImportStats{}, err
	}
	existing, err := loadRoutes(ctx, tx, ids, true)
	if err != nil {
		return RouteImportStats{}, err
	}
	plan := planRouteImport(rows, existing)
	// planRouteImport 按入参的 player_id 序产出;写之前改成聚簇键序,让 R7 的插入与 R9 的逐行补列都按聚簇键升序
	// 取锁(与 R8 的取锁顺序一致)。统计与冲突清单不受影响。
	sort.Slice(plan.inserts, func(i, j int) bool {
		return RouteScanCursorAt(plan.inserts[i]).Less(RouteScanCursorAt(plan.inserts[j]))
	})
	sort.Slice(plan.fills, func(i, j int) bool {
		return RouteScanCursorAt(plan.fills[i].row).Less(RouteScanCursorAt(plan.fills[j].row))
	})

	if err := s.insertRouteRows(ctx, tx, "import", plan.inserts); err != nil {
		return RouteImportStats{}, err
	}
	if err := s.fillRouteRows(ctx, tx, "import", plan.fills); err != nil {
		return RouteImportStats{}, err
	}

	if err := tx.Commit(); err != nil {
		return RouteImportStats{}, fmt.Errorf("commit player_route import (outcome unknown; the import is idempotent, rerun it): %w", err)
	}
	committed = true
	return plan.stats, nil
}

// insertRouteRows 执行 R7(rows 已按聚簇键升序,rev 已定):受影响行数必须等于行数。少了 = 加锁读之后有 W1 并发插入了
// 同一个 id(errRouteImportRaced,整批回滚后重来,重来时那一行会被 R8 读到);多了 = 语句被改过(fail-closed、不重试)。
// 导入与反向修库共用;what 只进报错。
func (s *RouteStore) insertRouteRows(ctx context.Context, tx *sql.Tx, what string, rows []RouteRow) error {
	if len(rows) == 0 {
		return nil
	}
	args := make([]any, 0, len(rows)*8)
	for _, r := range rows {
		args = append(args, routeShardArg(r.PlayerID), r.PlayerID, nullU32(r.HomeZone), nullU32(r.StorageID), nullU64(r.PlacementVersion),
			nullString(r.FrozenRunID), r.Rev, s.writer)
	}
	res, err := tx.ExecContext(ctx, routeImportInsertSQL(len(rows)), args...)
	if err != nil {
		return fmt.Errorf("%s insert %d player_route rows: %w", what, len(rows), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s insert %d player_route rows affected: %w", what, len(rows), err)
	}
	want := int64(len(rows))
	switch {
	case n == want:
		return nil
	case n >= 0 && n < want:
		return fmt.Errorf("%w: inserted %d of %d rows", errRouteImportRaced, n, want)
	default:
		// ODKU no-op 不可能让受影响行数超过待插行数;出现即说明语句被改过,fail-closed、不重试。
		return fmt.Errorf("%s insert player_route: 受影响行数 %d 超过待插行数 %d,"+
			"ON DUPLICATE KEY UPDATE 必须保持 no-op、连接不得带 CLIENT_FOUND_ROWS", what, n, want)
	}
}

// fillRouteRows 逐行执行 R9(fills 已按聚簇键升序):只补库里为 NULL 的列、rev + 1,按读到的 rev 做 CAS,必须恰好 1 行。
// 导入与反向修库(无 maprev 时退回导入语义)共用;what 只进报错。
func (s *RouteStore) fillRouteRows(ctx context.Context, tx *sql.Tx, what string, fills []routeImportFill) error {
	for _, f := range fills {
		r := f.row
		res, err := tx.ExecContext(ctx, routeImportFillSQL, nullU32(r.HomeZone), nullU32(r.StorageID), nullU64(r.PlacementVersion),
			nullString(r.FrozenRunID), s.writer, routeShardArg(r.PlayerID), r.PlayerID, f.dbRev)
		if err != nil {
			return fmt.Errorf("%s fill player_route player_id=%d: %w", what, r.PlayerID, err)
		}
		if err := requireOneRowAffected(res, what+" fill player_route player_id="+strconv.FormatUint(r.PlayerID, 10)); err != nil {
			return err
		}
	}
	return nil
}

// routeImportFill 是一行要补列的写:row 是补完之后的整行,dbRev 是 R8 读到的 rev(R9 的 CAS 条件)。
type routeImportFill struct {
	row   RouteRow
	dbRev uint64
}

// routeImportPlan 是一批导入的判定结果。inserts / fills 保持 rows 的 player_id 升序;
// importRoutesTx 写之前再按聚簇键排序(R7 / R9 按聚簇键升序取锁)。
type routeImportPlan struct {
	stats   RouteImportStats
	inserts []RouteRow
	fills   []routeImportFill
}

// planRouteImport 对每一行跑 decideImport 并汇总。existing 是库里已有的行(按 player_id)。
func planRouteImport(rows []RouteImportRow, existing map[uint64]RouteRow) routeImportPlan {
	var plan routeImportPlan
	for _, r := range rows {
		var dbRow *RouteRow
		if cur, ok := existing[r.PlayerID]; ok {
			dbRow = &cur
		}
		d := decideImport(dbRow, r)
		switch d.action {
		case importInsert:
			plan.stats.Inserted++
			plan.inserts = append(plan.inserts, d.row)
		case importMatch:
			plan.stats.Matched++
		case importFill:
			plan.stats.Filled++
			plan.fills = append(plan.fills, routeImportFill{row: d.row, dbRev: dbRow.Rev})
		case importDBAhead:
			plan.stats.DBAhead++
		case importRevRegression:
			plan.stats.RevRegressions = append(plan.stats.RevRegressions, RouteImportConflict{PlayerID: r.PlayerID, DB: *dbRow, Redis: r})
		default: // importConflict
			plan.stats.Conflicts = append(plan.stats.Conflicts, RouteImportConflict{PlayerID: r.PlayerID, DB: *dbRow, Redis: r})
		}
	}
	return plan
}

// importAction 是 decideImport 对一行的结论。
type importAction uint8

const (
	importInsert importAction = iota + 1
	importMatch
	importFill
	importDBAhead
	importRevRegression
	importConflict
)

// String 只给用例的失败信息用。
func (a importAction) String() string {
	switch a {
	case importInsert:
		return "insert"
	case importMatch:
		return "match"
	case importFill:
		return "fill"
	case importDBAhead:
		return "db_ahead"
	case importRevRegression:
		return "rev_regression"
	case importConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// importDecision:insert 时 row 是要插入的行(rev 已定);fill 时 row 是补完 NULL 列之后的整行
// (Rev 仍是库里读到的那一版,写时由 R9 的 CAS 加一);其余结论 row 为零值。
type importDecision struct {
	action importAction
	row    RouteRow
}

// importColumnVerdict 是一列(home 或落点)的比对结论。
type importColumnVerdict uint8

const (
	importColumnMatch importColumnVerdict = iota
	importColumnFill
	importColumnDBAhead
	importColumnConflict
)

// compareImportColumn 比对一列的库值与 Redis 值(都已规范化成缓存里的字符串形态,"" = NULL / 无键)。
func compareImportColumn(dbVal, redisVal string) importColumnVerdict {
	switch {
	case dbVal == redisVal:
		return importColumnMatch
	case dbVal == "":
		return importColumnFill
	case redisVal == "":
		// 库领先(例如 db_first 提交后发布失败):不写,不算冲突。
		return importColumnDBAhead
	default:
		return importColumnConflict
	}
}

// decideImport 判定一名玩家该怎么导入(规格 b.2):
//  1. 库里没有行:插入,rev = MapRev(为 0 时取 1)—— 插入的值取自 Redis,代表的就是 maprev 那一版的状态;
//  2. MapRev > 库 rev:revRegression,不写,不再看列(疑似库被回档);
//  3. home 与落点两列分别比(落点按 Encode() 后的字符串比,冻结态与稳定态视为不同):
//     相等 → match;库 NULL、Redis 有 → fill;库有、Redis 无 → dbAhead;两边都有且不同 → conflict。
//     合并:任一列 conflict → conflict;否则任一列 fill → fill(只补 NULL 的列,dbAhead 的列保持库值);
//     否则任一列 dbAhead → dbAhead;否则 match。
//
// 库里这一行本身过不了 Placement() 的校验时按 conflict 处理:不写,报给人(导入不负责修库)。
func decideImport(db *RouteRow, r RouteImportRow) importDecision {
	if db == nil {
		rev := r.MapRev
		if rev == 0 {
			rev = 1
		}
		row := RouteRow{PlayerID: r.PlayerID, HomeZone: r.HomeZone, Rev: rev}.withPlacement(r.Placement)
		return importDecision{action: importInsert, row: row}
	}
	if r.MapRev > db.Rev {
		return importDecision{action: importRevRegression}
	}

	dbPlc, err := db.PlacementValue()
	if err != nil {
		return importDecision{action: importConflict}
	}
	redisPlc := ""
	if r.Placement != nil {
		redisPlc = r.Placement.Encode()
	}
	home := compareImportColumn(db.HomeValue(), RouteRow{HomeZone: r.HomeZone}.HomeValue())
	plc := compareImportColumn(dbPlc, redisPlc)

	switch {
	case home == importColumnConflict || plc == importColumnConflict:
		return importDecision{action: importConflict}
	case home == importColumnFill || plc == importColumnFill:
		row := *db
		if home == importColumnFill {
			row.HomeZone = r.HomeZone
		}
		if plc == importColumnFill {
			row = row.withPlacement(r.Placement)
		}
		return importDecision{action: importFill, row: row}
	case home == importColumnDBAhead || plc == importColumnDBAhead:
		return importDecision{action: importDBAhead}
	default:
		return importDecision{action: importMatch}
	}
}

// validateRouteImportRows 校验一批导入的入参(见 ImportRoutes)。
func validateRouteImportRows(rows []RouteImportRow) error {
	if len(rows) > RouteBatchLimit {
		return fmt.Errorf("%w: %d import rows exceed RouteBatchLimit %d", ErrRouteInvalidArgument, len(rows), RouteBatchLimit)
	}
	for i, r := range rows {
		if r.PlayerID == 0 {
			return fmt.Errorf("%w: import row %d has player_id 0", ErrRouteInvalidArgument, i)
		}
		if i > 0 && r.PlayerID <= rows[i-1].PlayerID {
			return fmt.Errorf("%w: import rows must be strictly ascending by player_id (row %d: %d after %d)",
				ErrRouteInvalidArgument, i, r.PlayerID, rows[i-1].PlayerID)
		}
		if r.HomeZone > RouteHomeZoneMax {
			return fmt.Errorf("%w: import player_id=%d home_zone=%d exceeds %d", ErrRouteInvalidArgument, r.PlayerID, r.HomeZone, RouteHomeZoneMax)
		}
		if r.HomeZone == 0 && r.Placement == nil {
			// 只有 maprev、没有 home 也没有落点:插进库就是一行墓碑,导入没有理由造它。
			return fmt.Errorf("%w: import player_id=%d has neither home_zone nor placement", ErrRouteInvalidArgument, r.PlayerID)
		}
		if r.Placement != nil {
			if err := r.Placement.Validate(); err != nil {
				return fmt.Errorf("%w: import player_id=%d: %w", ErrRouteInvalidArgument, r.PlayerID, err)
			}
		}
	}
	return nil
}

// ── merged_into 导入 ───────────────────────────────────────────────────────────

// MergedIntoRow 是 zone_merged_into 的一行。导入时 MergedAtMs 被忽略(取导入时刻的库时钟,原始时刻已无从得知)。
type MergedIntoRow struct {
	SrcZone    uint32
	DstZone    uint32
	RunID      string
	MergedAtMs uint64
}

// MergedIntoConflict 是同 src 而 dst 或 run_id 不同的一条分歧:不覆盖,报给人。
type MergedIntoConflict struct {
	DB, Redis MergedIntoRow
}

// MergedIntoImportStats 是一次 merged_into 导入的统计。
type MergedIntoImportStats struct {
	Inserted, Matched int
	Conflicts         []MergedIntoConflict
}

// mergedIntoVerdict 是一行 merged_into 导入的结论。
type mergedIntoVerdict uint8

const (
	mergedIntoInserted mergedIntoVerdict = iota + 1
	mergedIntoMatched
	mergedIntoConflicted
)

// ImportMergedInto 导入合走标记,每行一个事务:R1 → R10(ODKU no-op)→ 受影响 0 行时 R11 加锁读比对。
// 同 src 而 dst 或 run_id 不同:记冲突,不覆盖。dry-run 只做 R11 的不加锁形态。
// 每行 SrcZone / DstZone 必须非 0 且不相等、RunID 必须合法(placement.ValidRunID),否则 ErrRouteInvalidArgument。
func (s *RouteStore) ImportMergedInto(ctx context.Context, rows []MergedIntoRow, dryRun bool) (MergedIntoImportStats, error) {
	return s.addMergedInto(ctx, rows, dryRun, "import", requireRouteWritable)
}

// routeWriteGate 是一个映射写事务的第一步(拿到 tx 之后、任何写之前):导入要 writable=1(requireRouteWritable),
// 反向修库要 writable≠1 且加锁复核(requireRouteFrozenLocked)。
type routeWriteGate func(ctx context.Context, q routeQueryer) error

// addMergedInto 是 ImportMergedInto 与 RepairMergedIntoFromCache 共用的循环:每行一个事务,gate → R10 → 受影响 0 行时
// R11 加锁读比对;只增不删、冲突不覆盖。what 只进操作名与报错(import / repair)。dry-run 只做 R11 的不加锁形态,
// 不经 gate(调用方自己决定 dry-run 要不要先查开关)。
func (s *RouteStore) addMergedInto(ctx context.Context, rows []MergedIntoRow, dryRun bool, what string, gate routeWriteGate) (MergedIntoImportStats, error) {
	if err := validateMergedIntoRows(rows); err != nil {
		return MergedIntoImportStats{}, err
	}
	var stats MergedIntoImportStats
	for _, r := range rows {
		var verdict mergedIntoVerdict
		var dbRow MergedIntoRow
		if dryRun {
			cur, found, err := readMergedInto(ctx, s.db, r.SrcZone, false)
			if err != nil {
				return MergedIntoImportStats{}, err
			}
			verdict, dbRow = classifyMergedInto(cur, found, r)
		} else {
			op := fmt.Sprintf("zone_merged_into %s src_zone=%d", what, r.SrcZone)
			err := s.runRouteTx(ctx, op, func() error {
				var attemptErr error
				verdict, dbRow, attemptErr = s.importMergedIntoTx(ctx, r, what, gate)
				return attemptErr
			})
			if err != nil {
				return MergedIntoImportStats{}, err
			}
		}
		switch verdict {
		case mergedIntoInserted:
			stats.Inserted++
		case mergedIntoMatched:
			stats.Matched++
		default:
			stats.Conflicts = append(stats.Conflicts, MergedIntoConflict{DB: dbRow, Redis: r})
		}
	}
	return stats, nil
}

// importMergedIntoTx 一行 merged_into 的一次事务尝试(导入与反向修库共用,gate 见 routeWriteGate)。
func (s *RouteStore) importMergedIntoTx(ctx context.Context, r MergedIntoRow, what string, gate routeWriteGate) (mergedIntoVerdict, MergedIntoRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, MergedIntoRow{}, fmt.Errorf("begin zone_merged_into %s tx src_zone=%d: %w", what, r.SrcZone, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := gate(ctx, tx); err != nil {
		return 0, MergedIntoRow{}, err
	}
	res, err := tx.ExecContext(ctx, routeInsertMergedIntoSQL, uint64(r.SrcZone), uint64(r.DstZone), r.RunID)
	if err != nil {
		return 0, MergedIntoRow{}, fmt.Errorf("%s zone_merged_into src_zone=%d: %w", what, r.SrcZone, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, MergedIntoRow{}, fmt.Errorf("%s zone_merged_into src_zone=%d rows affected: %w", what, r.SrcZone, err)
	}
	switch n {
	case 1:
		if err := tx.Commit(); err != nil {
			return 0, MergedIntoRow{}, fmt.Errorf("commit zone_merged_into %s src_zone=%d (outcome unknown; rerun the %s): %w", what, r.SrcZone, what, err)
		}
		committed = true
		return mergedIntoInserted, MergedIntoRow{}, nil
	case 0:
		// 已存在:加锁读比对,零写入(由 defer 回滚)。
		cur, found, err := readMergedInto(ctx, tx, r.SrcZone, true)
		if err != nil {
			return 0, MergedIntoRow{}, err
		}
		if !found {
			return 0, MergedIntoRow{}, fmt.Errorf("zone_merged_into src_zone=%d: row vanished between ON DUPLICATE KEY and the locking read", r.SrcZone)
		}
		verdict, dbRow := classifyMergedInto(cur, true, r)
		return verdict, dbRow, nil
	default:
		return 0, MergedIntoRow{}, fmt.Errorf("%s zone_merged_into src_zone=%d: 受影响行数 %d 不在 {1=插入, 0=撞键 no-op} 之内", what, r.SrcZone, n)
	}
}

// classifyMergedInto 比对库里的一行与 Redis 的一行(found=false = 库里没有,dry-run 下即「会插入」)。
func classifyMergedInto(cur MergedIntoRow, found bool, r MergedIntoRow) (mergedIntoVerdict, MergedIntoRow) {
	switch {
	case !found:
		return mergedIntoInserted, MergedIntoRow{}
	case cur.DstZone == r.DstZone && cur.RunID == r.RunID:
		return mergedIntoMatched, cur
	default:
		return mergedIntoConflicted, cur
	}
}

// readMergedInto 按 src 读一行(R11;forUpdate=false 是 dry-run 的不加锁形态)。
func readMergedInto(ctx context.Context, q routeQueryer, src uint32, forUpdate bool) (MergedIntoRow, bool, error) {
	query := routeReadMergedIntoSQL
	if forUpdate {
		query = routeLockMergedIntoSQL
	}
	row := MergedIntoRow{SrcZone: src}
	err := q.QueryRowContext(ctx, query, uint64(src)).Scan(&row.DstZone, &row.RunID, &row.MergedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return MergedIntoRow{}, false, nil
	}
	if err != nil {
		return MergedIntoRow{}, false, fmt.Errorf("read zone_merged_into src_zone=%d: %w", src, err)
	}
	return row, true, nil
}

// validateMergedIntoRows 校验 merged_into 导入的入参。
func validateMergedIntoRows(rows []MergedIntoRow) error {
	if len(rows) > RouteBatchLimit {
		return fmt.Errorf("%w: %d merged_into rows exceed RouteBatchLimit %d", ErrRouteInvalidArgument, len(rows), RouteBatchLimit)
	}
	for _, r := range rows {
		if r.SrcZone == 0 || r.DstZone == 0 || r.SrcZone == r.DstZone || !placement.ValidRunID(r.RunID) {
			return fmt.Errorf("%w: merged_into src_zone=%d dst_zone=%d run_id=%q (want non-zero distinct zones and a valid run_id)",
				ErrRouteInvalidArgument, r.SrcZone, r.DstZone, r.RunID)
		}
	}
	return nil
}

// ── 冻结、解冻与冻结态下的反向修库(第 1b-1 批,2026-10-18)──────────────────────────
//
// 用途:db_first 下全局库被回档(PITR;K8s 下全局库与 zone 库同一个 mysql 实例,zone 回档会连带回档它)之后,
// 缓存才是较新的一边。恢复流程(提案 §11.2「第 1b 批」、runbook §9.4):freeze → reconcile-from-cache(缓存 → 库)
// → audit -frozen-recovery → unfreeze。这四步的库内原语都在这里;Redis 一侧的读与判定在 routetool 包。

// RouteMeta 是 route_meta(id=1) 那一行。
type RouteMeta struct {
	// Generation 是库世代(迁移种行时的 32 个十六进制字符;已存在的行迁移不改它)。
	Generation string
	// Writable 1 = 映射写者可写;其它 = 冻结(W1 与导入拒写,反向修库才可做)。
	Writable uint64
}

// Frozen 报告映射写者是否被冻结(writable != 1,与 requireRouteWritable 的判据互为反面)。
func (m RouteMeta) Frozen() bool { return m.Writable != 1 }

// ReadRouteMeta 读 route_meta(id=1)(R18,普通一致读)。没有这一行回 ErrRouteMetaMissing。
func (s *RouteStore) ReadRouteMeta(ctx context.Context) (RouteMeta, error) {
	return readRouteMeta(ctx, s.db, routeReadMetaSQL)
}

// readRouteMeta 用 query(R18 或其加锁形态 R18x)读 route_meta(id=1)。
func readRouteMeta(ctx context.Context, q routeQueryer, query string) (RouteMeta, error) {
	var m RouteMeta
	err := q.QueryRowContext(ctx, query).Scan(&m.Generation, &m.Writable)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteMeta{}, ErrRouteMetaMissing
	}
	if err != nil {
		return RouteMeta{}, fmt.Errorf("read route_meta: %w", err)
	}
	return m, nil
}

// requireRouteFrozen 是反向修库 dry-run 的前置(R18 普通读):writable == 1 即 ErrRouteNotFrozen。
func requireRouteFrozen(ctx context.Context, q routeQueryer) error {
	return requireRouteFrozenWith(ctx, q, routeReadMetaSQL)
}

// requireRouteFrozenLocked 是反向修库写事务的第一步(R18x 加锁读),q 必须是事务:行锁持到提交,
// 并发的解冻(R22)排在本事务之后,保证「冻结期内修库」在提交点仍然成立(见 routeLockMetaSQL)。
func requireRouteFrozenLocked(ctx context.Context, q routeQueryer) error {
	return requireRouteFrozenWith(ctx, q, routeLockMetaSQL)
}

func requireRouteFrozenWith(ctx context.Context, q routeQueryer, query string) error {
	m, err := readRouteMeta(ctx, q, query)
	if err != nil {
		return err
	}
	if !m.Frozen() {
		return fmt.Errorf("%w (generation=%s)", ErrRouteNotFrozen, m.Generation)
	}
	return nil
}

// FreezeRouteWriters 保留旧调用的显式拒绝；维护必须提交固定请求、正式票据与完整排空收据。
func (s *RouteStore) FreezeRouteWriters(ctx context.Context) (bool, error) {
	return false, errors.New("冻结必须使用 authoritysql.Catalog.Freeze 与正式维护收据")
}

// UnfreezeRouteWriters 不再接受没有 P8 原件的水位参数；水位下限由 Catalog.Release 在提交事务内落实。
func (s *RouteStore) UnfreezeRouteWriters(ctx context.Context, minWatermarkMs uint64) (bool, error) {
	return false, errors.New("解冻必须使用 authoritysql.Catalog.Release 与固定 ReleaseReadiness 原件")
}

// RouteRepairOptions 是反向修库(RepairRoutesFromCache)的行为参数。
type RouteRepairOptions struct {
	// DryRun 只判定不写:普通读开关与已有行,不开事务。
	DryRun bool
	// ResolveTies:缓存 maprev 与库 rev 相同而值不同(平局)时用缓存值覆盖(route_tool -resolve-ties=cache);
	// 默认只报告。同一 rev 出现两份值只可能是有写者绕过了发布脚本,或回档之后库复用了 rev,哪边对要人判断。
	ResolveTies bool
}

// RouteRepairStats 是一批反向修库的判定统计(dry-run 时是「如果写会怎样」)。
type RouteRepairStats struct {
	// Inserted 库里没有这一行:插入,rev = maprev(无 maprev 取 1)。
	Inserted int
	// Restored 缓存 maprev > 库 rev:用缓存整行覆盖,rev 置为 maprev(库被回档丢掉的写由此补回)。
	Restored int
	// Matched 两边一致(同 rev 同值;无 maprev 时按导入口径一致)。
	Matched int
	// DBAhead 库 rev > maprev,或无 maprev 时库有值而缓存无键:不动(解冻之后由增量推送补到缓存)。
	DBAhead int
	// Filled 无 maprev 时退回导入语义:只补库里为 NULL 的列(rev + 1)。
	Filled int
	// Ties 同 rev 而值不同、没有覆盖(默认):要人看。TiesResolved 同上、已按 ResolveTies 用缓存覆盖(rev 不变)。
	Ties, TiesResolved []RouteImportConflict
	// Conflicts 库行本身过不了校验,或无 maprev 时两边同一列都有值且不同:不写,要人看。
	Conflicts []RouteImportConflict
}

// RepairRoutesFromCache 把一批缓存里的映射修回 player_route(反向修库,第 1b-1 批)。只许在冻结时做:
// writable == 1 → ErrRouteNotFrozen(一条写都不发)。rows 的约束同 ImportRoutes(按 player_id 严格升序、无重复、
// 不超过 RouteBatchLimit、每行至少有 home 或落点之一),否则 ErrRouteInvalidArgument。逐行判定见 decideRepair。
//
// 非 dry-run 时一批一个事务:R18x(加锁复核冻结态)→ R8(按聚簇键升序加锁读已有行)→ decideRepair → R7(插新行)
// → R9(无 maprev 的补列)→ R17(整行覆盖,rev = maprev,按读到的 rev 做 CAS)→ COMMIT。三类写都按聚簇键升序。
// R7 行数对不上(冻结前已过开关检查的 W1 在 R8 之后插了同一个 id)整批回滚重来,共用 routeTxAttempts 的预算。
// 重跑幂等:已修过的行 rev 与值都等于缓存,再判就是 Matched。
//
// dry-run 只做 R18 与 R8 的不加锁形态,不开事务、不写任何东西,同样给出统计。
func (s *RouteStore) RepairRoutesFromCache(ctx context.Context, rows []RouteImportRow, opts RouteRepairOptions) (RouteRepairStats, error) {
	if err := validateRouteImportRows(rows); err != nil {
		return RouteRepairStats{}, err
	}
	if len(rows) == 0 {
		return RouteRepairStats{}, nil
	}
	ids := make([]uint64, len(rows))
	for i, r := range rows {
		ids[i] = r.PlayerID
	}

	if opts.DryRun {
		if err := s.requireMaintenanceFrozen(ctx, s.db, false); err != nil {
			return RouteRepairStats{}, err
		}
		existing, err := loadRoutes(ctx, s.db, ids, false)
		if err != nil {
			return RouteRepairStats{}, err
		}
		return planRouteRepair(rows, existing, opts.ResolveTies).stats, nil
	}

	var stats RouteRepairStats
	op := fmt.Sprintf("player_route repair from cache (%d rows, player_id %d..%d)", len(rows), rows[0].PlayerID, rows[len(rows)-1].PlayerID)
	err := s.runRouteTx(ctx, op, func() error {
		var attemptErr error
		stats, attemptErr = s.repairRoutesTx(ctx, ids, rows, opts.ResolveTies)
		return attemptErr
	})
	if err != nil {
		return RouteRepairStats{}, err
	}
	return stats, nil
}

// repairRoutesTx 一次反向修库事务尝试。
func (s *RouteStore) repairRoutesTx(ctx context.Context, ids []uint64, rows []RouteImportRow, resolveTies bool) (RouteRepairStats, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RouteRepairStats{}, fmt.Errorf("begin player_route repair tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := s.requireMaintenanceFrozen(ctx, tx, true); err != nil {
		return RouteRepairStats{}, err
	}
	existing, err := loadRoutes(ctx, tx, ids, true)
	if err != nil {
		return RouteRepairStats{}, err
	}
	plan := planRouteRepair(rows, existing, resolveTies)
	// 三类写都改成聚簇键序,与 R8 的取锁顺序一致(同 importRoutesTx)。统计与清单不受影响。
	sort.Slice(plan.inserts, func(i, j int) bool {
		return RouteScanCursorAt(plan.inserts[i]).Less(RouteScanCursorAt(plan.inserts[j]))
	})
	sort.Slice(plan.fills, func(i, j int) bool {
		return RouteScanCursorAt(plan.fills[i].row).Less(RouteScanCursorAt(plan.fills[j].row))
	})
	sort.Slice(plan.overwrites, func(i, j int) bool {
		return RouteScanCursorAt(plan.overwrites[i].row).Less(RouteScanCursorAt(plan.overwrites[j].row))
	})

	if err := s.insertRouteRows(ctx, tx, "repair", plan.inserts); err != nil {
		return RouteRepairStats{}, err
	}
	if err := s.fillRouteRows(ctx, tx, "repair", plan.fills); err != nil {
		return RouteRepairStats{}, err
	}
	for _, o := range plan.overwrites {
		r := o.row
		res, err := tx.ExecContext(ctx, routeRepairOverwriteSQL, nullU32(r.HomeZone), nullU32(r.StorageID), nullU64(r.PlacementVersion),
			nullString(r.FrozenRunID), r.Rev, s.writer, routeShardArg(r.PlayerID), r.PlayerID, o.dbRev)
		if err != nil {
			return RouteRepairStats{}, fmt.Errorf("repair overwrite player_route player_id=%d rev %d -> %d: %w", r.PlayerID, o.dbRev, r.Rev, err)
		}
		if err := requireOneRowAffected(res, "repair overwrite player_route player_id="+strconv.FormatUint(r.PlayerID, 10)); err != nil {
			return RouteRepairStats{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return RouteRepairStats{}, fmt.Errorf("commit player_route repair (outcome unknown; the repair is idempotent, rerun it): %w", err)
	}
	committed = true
	return plan.stats, nil
}

// routeRepairOverwrite 是一行 R17:row 是缓存那一版的整行(Rev = maprev),dbRev 是 R8 读到的 rev(CAS 条件)。
type routeRepairOverwrite struct {
	row   RouteRow
	dbRev uint64
}

// routeRepairPlan 是一批反向修库的判定结果(inserts / fills / overwrites 保持 rows 的 player_id 升序,写之前再按聚簇键排)。
type routeRepairPlan struct {
	stats      RouteRepairStats
	inserts    []RouteRow
	fills      []routeImportFill
	overwrites []routeRepairOverwrite
}

// planRouteRepair 对每一行跑 decideRepair 并汇总。existing 是库里已有的行(按 player_id)。
func planRouteRepair(rows []RouteImportRow, existing map[uint64]RouteRow, resolveTies bool) routeRepairPlan {
	var plan routeRepairPlan
	for _, r := range rows {
		var dbRow *RouteRow
		if cur, ok := existing[r.PlayerID]; ok {
			dbRow = &cur
		}
		d := decideRepair(dbRow, r, resolveTies)
		switch d.action {
		case repairInsert:
			plan.stats.Inserted++
			plan.inserts = append(plan.inserts, d.row)
		case repairRestore:
			plan.stats.Restored++
			plan.overwrites = append(plan.overwrites, routeRepairOverwrite{row: d.row, dbRev: dbRow.Rev})
		case repairMatch:
			plan.stats.Matched++
		case repairDBAhead:
			plan.stats.DBAhead++
		case repairFill:
			plan.stats.Filled++
			plan.fills = append(plan.fills, routeImportFill{row: d.row, dbRev: dbRow.Rev})
		case repairTie:
			plan.stats.Ties = append(plan.stats.Ties, RouteImportConflict{PlayerID: r.PlayerID, DB: *dbRow, Redis: r})
		case repairTieResolved:
			plan.stats.TiesResolved = append(plan.stats.TiesResolved, RouteImportConflict{PlayerID: r.PlayerID, DB: *dbRow, Redis: r})
			plan.overwrites = append(plan.overwrites, routeRepairOverwrite{row: d.row, dbRev: dbRow.Rev})
		default: // repairConflict:dbRow 一定非 nil(库里没有行只会是 insert)
			plan.stats.Conflicts = append(plan.stats.Conflicts, RouteImportConflict{PlayerID: r.PlayerID, DB: *dbRow, Redis: r})
		}
	}
	return plan
}

// repairAction 是 decideRepair 对一行的结论。
type repairAction uint8

const (
	repairInsert repairAction = iota + 1
	repairRestore
	repairMatch
	repairDBAhead
	repairFill
	repairTie
	repairTieResolved
	repairConflict
)

// String 只给用例的失败信息用。
func (a repairAction) String() string {
	switch a {
	case repairInsert:
		return "insert"
	case repairRestore:
		return "restore"
	case repairMatch:
		return "match"
	case repairDBAhead:
		return "db_ahead"
	case repairFill:
		return "fill"
	case repairTie:
		return "tie"
	case repairTieResolved:
		return "tie_resolved"
	case repairConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// repairDecision:insert 时 row 是要插入的行(rev 已定);restore / tie_resolved 时 row 是缓存那一版的整行(Rev = maprev);
// fill 时 row 是补完 NULL 列之后的整行(Rev 仍是库里读到的那一版,写时由 R9 加一);其余结论 row 为零值。
type repairDecision struct {
	action repairAction
	row    RouteRow
}

// decideRepair 判定一名玩家在冻结态下该怎么从缓存修库(提案 §11.2「第 1b 批」D5):
//  1. 库里没有行,或缓存没有 maprev:退回导入语义(decideImport;库无行 → 插入,rev = maprev / 1;无 maprev 时只补 NULL 列,
//     两边都有值且不同 → conflict)。无 maprev 说明这一版从没经过发布脚本,缓存不比库更可信,不覆盖;
//  2. 库行本身过不了校验 → conflict(不在修库里顺手改坏行,交给人);
//  3. maprev > 库 rev → restore:库被回档丢了这几版,用缓存整行覆盖,rev 置为 maprev;
//     但库里某列有值而缓存同列缺席(home 或落点)→ conflict(2026-10-18 修复评审):db_first 下库的这两列只会从 NULL 变成有值
//     (W1 / 导入只补 NULL 列,W3 删号停用),发布脚本按 rev 原子写整行,所以「rev 更高却少一列」只可能是脚本之外的 DEL;
//     照覆盖会把库里正确的值清成 NULL,之后两边都没有这一列,audit 判 match、静默放过。将来启用删号墓碑时要同步调整这条判据;
//  4. maprev < 库 rev → db_ahead:库更新(回档之后到冻结之前又提交过),不动;
//  5. maprev == 库 rev:值相同 → match;值不同 → tie(默认只报告),resolveTies 时 tie_resolved(用缓存覆盖,rev 不变)。
//
// 值按缓存形态比较(home 十进制、落点 Encode,冻结态与稳定态视为不同),与 audit / decideImport 同口径。
func decideRepair(db *RouteRow, r RouteImportRow, resolveTies bool) repairDecision {
	if db == nil || r.MapRev == 0 {
		d := decideImport(db, r)
		switch d.action {
		case importInsert:
			return repairDecision{action: repairInsert, row: d.row}
		case importMatch:
			return repairDecision{action: repairMatch}
		case importFill:
			return repairDecision{action: repairFill, row: d.row}
		case importDBAhead:
			return repairDecision{action: repairDBAhead}
		default:
			// importConflict(库行无效或同列异值);MapRev 为 0 时 decideImport 不会给 rev_regression。
			return repairDecision{action: repairConflict}
		}
	}
	dbPlc, err := db.PlacementValue()
	if err != nil {
		return repairDecision{action: repairConflict}
	}
	cache := RouteRow{PlayerID: r.PlayerID, HomeZone: r.HomeZone, Rev: r.MapRev}.withPlacement(r.Placement)
	cachePlc, err := cache.PlacementValue()
	if err != nil {
		// 入参已过 validateRouteImportRows 的落点校验,走不到这里;防御性地按冲突处理,不写。
		return repairDecision{action: repairConflict}
	}
	switch {
	case r.MapRev > db.Rev && ((db.HomeValue() != "" && cache.HomeValue() == "") || (dbPlc != "" && cachePlc == "")):
		return repairDecision{action: repairConflict}
	case r.MapRev > db.Rev:
		return repairDecision{action: repairRestore, row: cache}
	case r.MapRev < db.Rev:
		return repairDecision{action: repairDBAhead}
	case db.HomeValue() == cache.HomeValue() && dbPlc == cachePlc:
		return repairDecision{action: repairMatch}
	case resolveTies:
		return repairDecision{action: repairTieResolved, row: cache}
	default:
		return repairDecision{action: repairTie}
	}
}

// RepairMergedIntoFromCache 把缓存里的合走标记补回 zone_merged_into(反向修库的一部分):库里没有就插入;同 src 而 dst /
// run_id 不同记冲突、不覆盖;**永不删除**(合走标记是挡住「回填把已合走的玩家钉回源区」的唯一东西)。
// 与 ImportMergedInto 同一个循环,区别只在开关:要求冻结 —— 写事务的第一句是 R18x 加锁复核,dry-run 先 R18 普通读一次。
func (s *RouteStore) RepairMergedIntoFromCache(ctx context.Context, rows []MergedIntoRow, dryRun bool) (MergedIntoImportStats, error) {
	if dryRun && len(rows) > 0 {
		if err := validateMergedIntoRows(rows); err != nil {
			return MergedIntoImportStats{}, err
		}
		if err := s.requireMaintenanceFrozen(ctx, s.db, false); err != nil {
			return MergedIntoImportStats{}, err
		}
	}
	return s.addMergedInto(ctx, rows, dryRun, "repair", func(ctx context.Context, q routeQueryer) error { return s.requireMaintenanceFrozen(ctx, q, true) })
}

// ── 只读接口(route_tool audit 用;都是一致读,不取锁)──────────────────────────

// ScanRoutes 按聚簇键键集翻页:返回键 > after 的至多 limit 行,按 (route_shard, player_id) 升序(**不是** player_id 序)。
// limit 必须在 [1, RouteBatchLimit];after.Shard 必须 < placement.RouteRowShards(否则查询静默回空页,
// 调用方会当作扫完)。下一页从 RouteScanCursorAt(最后一行) 接着翻,不漏不重。
//
// 任一行库里存的 route_shard ≠ player_id % 64 → ErrRouteShardMismatch(整页作废,fail-closed)。
// 这是真库上唯一看得见错片行的地方(audit 方向 A / 第 1b 批全量预热就是检测器),点读看不见。
func (s *RouteStore) ScanRoutes(ctx context.Context, after RouteScanCursor, limit int) ([]RouteRow, error) {
	if limit < 1 || limit > RouteBatchLimit {
		return nil, fmt.Errorf("%w: scan limit %d not in [1, %d]", ErrRouteInvalidArgument, limit, RouteBatchLimit)
	}
	if after.Shard >= placement.RouteRowShards {
		return nil, fmt.Errorf("%w: scan cursor (shard, player_id)=(%d, %d): shard not in [0, %d)",
			ErrRouteInvalidArgument, after.Shard, after.PlayerID, placement.RouteRowShards)
	}
	rows, err := s.db.QueryContext(ctx, routeScanSQL, uint64(after.Shard), uint64(after.Shard), after.PlayerID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan player_route after (shard, player_id)=(%d, %d): %w", after.Shard, after.PlayerID, err)
	}
	defer rows.Close()

	out := make([]RouteRow, 0, limit)
	for rows.Next() {
		var row RouteRow
		if err := scanRouteRow(rows, &row, true); err != nil {
			return nil, fmt.Errorf("scan player_route row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate player_route scan: %w", err)
	}
	return out, nil
}

// GetRoutes 按 id 批量点查。缺席的 id 不出现在结果里(不是错误);id 为 0 的项被跳过。
// 超过 RouteBatchLimit 直接拒绝。
//
// 按「正确的片 + id」点查:库里一条错片行(只有 CHECK 曾经不生效时才写得进来)在这里**表现为缺席**,
// 不会报错 —— 检测它靠 ScanRoutes(audit 方向 A),防它靠建池探针 P4(见 probeRouteShardCheck)。
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
	return loadRoutes(ctx, s.db, nonZero, false)
}

// loadRoutes 按 id 批量读(R8 / R13)。forUpdate 只在事务里用(导入)。
// 实参按聚簇键升序交错给出 (行片, player_id)(拷一份再排,不改调用方的切片);结果仍按 player_id 索引。
func loadRoutes(ctx context.Context, q routeQueryer, ids []uint64, forUpdate bool) (map[uint64]RouteRow, error) {
	keyed := append([]uint64(nil), ids...)
	sortRouteIDsByKey(keyed)
	args := make([]any, 0, 2*len(keyed))
	for _, id := range keyed {
		args = append(args, routeShardArg(id), id)
	}
	rows, err := q.QueryContext(ctx, routeSelectRowsSQL(len(keyed), forUpdate), args...)
	if err != nil {
		return nil, fmt.Errorf("read player_route (%d ids, for_update=%t): %w", len(ids), forUpdate, err)
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
// writable=1。ODKU no-op:已存在的行一个字节都不碰(尤其不换 generation —— 它是「库世代」,
// 只有回档恢复流程才换,第 1b 批据它发现库被重置)。日志区分「新种了一行」与「已存在,未改」。
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

// bootstrapRouteReconcileRow 种 route_reconcile(id=1)(R16):水位与校验时刻为 0、无 leader。ODKU no-op:
// 已存在的行一个字节都不碰(第 1b 批的对账 leader 可能正持有租约、推进水位)。日志口径同 bootstrapRouteMetaRow。
func bootstrapRouteReconcileRow(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx, routeReconcileSeedSQL)
	if err != nil {
		return fmt.Errorf("bootstrap route_reconcile row id=%d: %w", routeMetaRowID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("bootstrap route_reconcile row id=%d rows affected: %w", routeMetaRowID, err)
	}
	switch n {
	case 1:
		logx.Infof("[schema] route_reconcile: seeded row id=%d watermark_ms=0", routeMetaRowID)
	case 0:
		logx.Infof("[schema] route_reconcile: row id=%d already present, left untouched", routeMetaRowID)
	default:
		return fmt.Errorf("bootstrap route_reconcile row id=%d: 受影响行数 %d 不在 {1=新种, 0=已存在} 之内,"+
			"routeReconcileSeedSQL 的 ON DUPLICATE KEY UPDATE 必须保持 no-op", routeMetaRowID, n)
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

// requireMaintenanceFrozen 将恢复写与本次正常冻结绑定。锁持到提交，跨 run / epoch 的再次冻结不能放行旧修复任务。
func (s *RouteStore) requireMaintenanceFrozen(ctx context.Context, q routeQueryer, lock bool) error {
	r := s.maintenanceFreeze
	if r == nil || r.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || r.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN || r.RunPhase != pb.SqlWriterDrainRun_SQL_FROZEN || r.CommittedMs == 0 || len(r.RequestId) != 16 || len(r.ReceiptDigest) != 32 {
		return errors.New("恢复写缺少正式正常冻结原件")
	}
	query := "SELECT generation,writable,writer_epoch,catalog_revision,freeze_run_id FROM route_meta WHERE id=1"
	if lock {
		query += " FOR UPDATE"
	}
	var generation string
	var writable, epoch, revision uint64
	var runID []byte
	if err := q.QueryRowContext(ctx, query).Scan(&generation, &writable, &epoch, &revision, &runID); err != nil {
		return err
	}
	if writable != 0 {
		return fmt.Errorf("恢复写要求当前已冻结: %w", ErrRouteNotFrozen)
	}
	if generation != r.Generation || epoch != r.AfterEpoch || revision != r.CatalogRevision || !bytes.Equal(runID, r.RunId) {
		return errors.New("恢复写所绑定的冻结 epoch / run 已变化")
	}
	var digest, requestDigest []byte
	var kind, phase uint32
	if err := q.QueryRowContext(ctx, "SELECT request_digest,receipt_digest,kind,phase FROM sql_writer_maintenance_receipt WHERE request_id=?", r.RequestId).Scan(&requestDigest, &digest, &kind, &phase); err != nil {
		return err
	}
	if !bytes.Equal(requestDigest, r.RequestDigest) || !bytes.Equal(digest, r.ReceiptDigest) || kind != uint32(pb.SqlWriterMaintenanceReceipt_FREEZE) || phase != uint32(pb.SqlWriterMaintenanceReceipt_FROZEN) {
		return errors.New("恢复写的永久冻结收据不匹配")
	}
	var runPhase uint32
	if err := q.QueryRowContext(ctx, "SELECT phase FROM sql_writer_drain_run WHERE operation_id=?", r.RunId).Scan(&runPhase); err != nil {
		return err
	}
	if runPhase != uint32(pb.SqlWriterDrainRun_SQL_FROZEN) {
		return errors.New("恢复写只允许在正式 SQL_FROZEN 阶段提交")
	}
	return nil
}
