package main

// 合服工具在全局库路由表上的 SQL 写入层(D1 第 2 批底座;规格 docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md
// §6.1、§6.2、§6.3、§13.3,提案 docs/design/player-route-mapping-durability-proposal.md §3.2、§4.1、§11.2 G1a)。
//
// 为什么在这里自带一份(规格 §6.1 选 (a)):merge_zone 是独立 module,import 不了 go/data_service/internal/store;
// 走 data_service 的管理 RPC 要新增 proto 与一套跨进程围栏协议;把 store 挪出 internal 会把 go-zero / Kafka 拖进运维工具。
// 库结构的一致性靠开写前的运行期探针(与 data_service NewRouteStore 的 P1–P5 同判据,SQL 文本照抄);Redis 一侧的脚本与常量
// 由 route_mirror_test.go 与 go/shared/placement 逐字节比对。
//
// 本文件是 merge_zone 里**唯一**写 player_route / zone_merge_fence / zone_merged_into 的文件(route_store_test.go 的
// TestNoRouteTableWritesOutsideMergeRouteStore 用 AST 守住;route_meta / route_reconcile 本工具一律不写)。
// 事务里绝不做 Redis 读写:发布在提交之后,由 route_publish.go 完成;每个批原语把「本批处于目标状态的全部行」交给它。
//
// 各写入点(fence.go / merged_into.go / merge_run.go / placement_ops.go / pin_placement.go / unmerge.go /
// backfill_home_zone.go / relocate*.go)在 -route-source=db 时调用这里的原语(第 2 批第二波接线;分派规则见 merge_run.go
// 「-route-source=db:装配、读方与步骤 5」)。TakeOverFence 是接线时补的(续跑接手,fence.go -take-over-fence)。
//
// 不变量(与 data_service store/route_store.go 文件头同口径):
//  1. 判定只认加锁读(FOR UPDATE)。新行一律 ODKU no-op、撞键 = 受影响 0 行,不用 INSERT IGNORE(它把 CHECK 违例降成 warning)。
//  2. 锁序:route_meta(普通读)→ zone_merge_fence(zone 升序,再 shard 升序)→ zone_merged_into → player_route
//     (按聚簇键 (route_shard, player_id) 升序)。批事务只锁每个相关 zone 的 63 号围栏片(规格 §13.3 第 1 条);
//     获取 / 续期 / 释放写满 0..63 全部片,必经 63 号片的行锁,所以持有它期间围栏不可能易主。
//  3. 会话级 READ-COMMITTED + innodb_lock_wait_timeout=1(routeDBDSN 钉住,开写前探针读回)。
//  4. 1213 / 9007 有界重试(routeTxAttempts 次,抖动退避),1205 不重试(已经等满封顶的 1s,再等结论不变)。
//  5. 时间一律取库时钟(routeDBNowMsSQL);围栏是否生效只看库时钟,不看本机时钟。
//  6. 行片 = player_id % 64(routeRowShards,镜像 shared/placement.RouteRowShards,route_mirror_test.go 钉住);
//     按 player_id 的每一条读写都同时带行片。
//
// 前提(与 data_service 同口径):连接不带 CLIENT_FOUND_ROWS(受影响行数 = 实际改变的行数,routeDBDSN 拒绝它);
// binlog_format = ROW(RC 在 STATEMENT 下拒写,第一条写就会响亮失败)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

// ── 常量 ──────────────────────────────────────────────────────────────────────

const (
	// routeRowShards 是 player_route 的行片模数(镜像 shared/placement.RouteRowShards;建表后不可改)。
	routeRowShards uint32 = 64
	// routeFenceShards 是围栏持有者必须锁住并写满的分片数(镜像 shared/placement.MaxRouteFenceShards):
	// 各 data_service 的 FenceShards 可以不同,持有者写满 0..63,W1 落到哪一片都能看见围栏。
	routeFenceShards uint32 = 64
	// routeFenceCheckShard 是批事务锁 / 读的那一片:W1 只锁 player_id % FenceShards 号片,FenceShards < 64 时永远碰不到
	// 63 号片,工具的批事务不拖累建角;获取 / 续期 / 释放写满全部片,必经这一片的行锁(规格 §13.3 第 1 条)。
	routeFenceCheckShard = routeFenceShards - 1
	// routeHomeZoneMax 是 zone id 的上限(= schema.go chk_route_home 的 999999,zone 库编号同一空间)。
	routeHomeZoneMax uint32 = 999999
	// routeWriteBatchLimit 是一个写事务最多处理的玩家数(W5 每 500 人一个事务,规格 §6.3)。
	routeWriteBatchLimit = 500
	// routePinZoneBatchLimit 是 W10(-mode pin-placement)每批的上限(规格 §6.3:≤ 200 人一个事务)。
	routePinZoneBatchLimit = 200
	// routeReadBatchLimit 是一次按 id 批量读 / 键集翻页的上限(= data_service store.RouteBatchLimit)。
	routeReadBatchLimit = 1000
	// routeTxAttempts 事务尝试上限:1 次 + 1213 / 9007 重试 3 次(规格 §6.2「有界重试 4 次」)。
	routeTxAttempts = 4
	// routeLockWaitTimeoutSec 是会话级 innodb_lock_wait_timeout,由 routeDBDSN 钉死、探针读回。
	routeLockWaitTimeoutSec = 1
	// routeLockRetryBackoffMin / Max 是 1213 / 9007 重来之前的抖动等待(可被 ctx 取消)。
	routeLockRetryBackoffMin = 10 * time.Millisecond
	routeLockRetryBackoffMax = 50 * time.Millisecond
	// routeProbeTimeout 是开写前探针的总时限(会话变量、TiDB 事务模式、route_meta 行、P4 + P5)。
	routeProbeTimeout = 5 * time.Second
	// routeWriterMaxLen = player_route.updated_by 的列宽 varchar(96)。
	routeWriterMaxLen = 96
	// routeWriterRunIDPrefix 是写者标识里 run_id 前缀的最大字节数。
	routeWriterRunIDPrefix = 40
	// routeHolderJSONMaxChars = zone_merge_fence.holder_json 的列宽 varchar(1024)(字符数)。
	routeHolderJSONMaxChars = 1024
	// routeFenceMinTTL 是围栏 TTL 的下限。调用方按 max(-timeout + 30m, 1h) 取值(fence.go 的口径),这里只挡明显的误用。
	routeFenceMinTTL = 10 * time.Minute
	// routeFenceLocalSafety 是本地失权截止时间比库里的过期时刻提前的量(规格 §13.3 第 4 条:lastRenewOK + ttl − 60s)。
	routeFenceLocalSafety = 60 * time.Second
	// routeFenceRetryWait 是续期遇到 1205 / 网络错误后的重试间隔。
	routeFenceRetryWait = 5 * time.Second
	// routeFenceRenewMaxInterval 是续期间隔的上限:interval = min(ttl/3, 它)。被 -take-over-fence 接手而其实还活着的旧进程,
	// 映射批次立刻被库按 fencing token 拒绝,但不碰映射的步骤(拷行、帮会、交易、榜单、热状态)只靠下一次续期失败才停 ——
	// 按 ttl/3(≥20 分钟)两边会交错写这么久,压到 30s 量级。每次续期是 128 行 UPDATE 加两次镜像 EVAL + WAITAOF,开销可忽略;
	// 本地截止时刻(最后一次成功续期 + ttl − 60s)不变。
	routeFenceRenewMaxInterval = 30 * time.Second
	// mysqlErrCheckConstraintViolated 3819:探针 P4 期望收到的那一个。
	mysqlErrCheckConstraintViolated = 3819
	// mysqlErrTableAccessDenied 1142:账号对表没有该权限(P4 / P5 要 player_route 的 INSERT 权限)。
	mysqlErrTableAccessDenied = 1142
)

// 围栏用途(zone_merge_fence.purpose,varchar(16) ascii)。
const (
	routeFencePurposeMerge   = "merge"
	routeFencePurposeUnmerge = "unmerge"
)

// DSN 里钉住的会话变量(另外两项 transaction_isolation / tx_isolation 与 merge_run.go 的 readCommittedDSN 共用常量)。
const (
	routeLockWaitParam = "innodb_lock_wait_timeout"
	routeSQLModeParam  = "sql_mode"
	routeSQLModeStrict = "'STRICT_TRANS_TABLES'"
)

// routeWriterHeadPart 是写者标识的固定前缀(newMergeRouteWriter)。
const routeWriterHeadPart = "merge_zone@"

// ── 错误 ──────────────────────────────────────────────────────────────────────

var (
	// errRouteInvalidArgument 入参在库层就被拒(zone 越界、批次为空 / 重复 / 超限、run_id 不合法等),一条语句都不发。
	errRouteInvalidArgument = errors.New("route store: invalid argument")
	// errRouteMetaMissing route_meta 没有 id=1 那一行(迁移没跑过)。
	errRouteMetaMissing = errors.New("route_meta: row id=1 missing; run `data_service -f <yaml> -migrate` against the database of -route-db-dsn")
	// errRouteNotWritable route_meta.writable != 1:映射写者被冻结(回档恢复流程),一律拒写。
	errRouteNotWritable = errors.New("route_meta: writable!=1, route writers are frozen")
	// errRouteSessionMisconfigured 探针读回的隔离级别 / 锁等待 / TiDB 事务模式不符合要求。
	errRouteSessionMisconfigured = errors.New("route store: session isolation / lock wait / txn mode not as required")
	// errRouteCheckNotEnforced 探针 P4 证不了 chk_route_shard 生效(player_id 的唯一性靠它)。
	errRouteCheckNotEnforced = errors.New("player_route: CHECK constraint chk_route_shard is not enforced " +
		"(needs MySQL >= 8.0.16, or TiDB >= 7.2 with `SET GLOBAL tidb_enable_check_constraint=ON` before the table is created)")
	// errRouteShardModulusMismatch 探针 P5 证明表的行片模数不是 64(按另一个模数建的表)。
	errRouteShardModulusMismatch = errors.New("player_route: the route_shard modulus in CHECK chk_route_shard is not 64; " +
		"the route tables were created with another modulus (proposal §11.2 HR4: DROP the five route tables by hand after review, " +
		"then rerun `data_service -f <yaml> -migrate`)")
	// errRouteShardMismatch 库里一行存的 route_shard 不等于 player_id % 64(只有 CHECK 曾经不生效时才写得进来)。
	errRouteShardMismatch = errors.New("player_route: stored route_shard does not match player_id % 64")
	// errRouteFenceLost 本次运行已不再持有围栏(库里 run_id 不是本次、已过期,或本地截止时间已过)。
	// 调用方必须停止**一切**写步骤(含不碰映射的 player_blob / guild / trade 步骤),退出非 0(规格 §6.3 W12 续期)。
	errRouteFenceLost = errors.New("merge fence lost: zone_merge_fence no longer carries this run (released, taken over or expired by the DB clock)")
	// errRouteFenceHeld 获取围栏时,某个 zone 的围栏正被另一次运行持有且未过期。
	errRouteFenceHeld = errors.New("merge fence is held by another run")
	// errRouteZoneFenced 要求「没有合服在进行」的写(W9 / W10 / 搬库 CAS 的围栏 zone)撞上了生效的围栏。
	errRouteZoneFenced = errors.New("zone is fenced by a merge in progress")
	// errRouteFenceRowMissing 刚确保过的围栏分片行,事务里的加锁读却读不到(有人绕过本工具删了行):一律当错误,不当「未生效」。
	errRouteFenceRowMissing = errors.New("zone_merge_fence: shard row missing right after it was ensured")
	// errRouteZoneMergedAway zone_merged_into 记着这个 zone 已被合走(回填 / 钉落点拒绝)。
	errRouteZoneMergedAway = errors.New("zone was merged away (zone_merged_into)")
	// errRouteMergedIntoConflict zone_merged_into 已记着这个源区合进了另一个目标区。
	errRouteMergedIntoConflict = errors.New("zone_merged_into already records another target zone for this source zone")
	// errRouteInvariant 锁语义被破坏、或调用方要写的值违反不变量(P-2 等):fail-closed,整批回滚。
	errRouteInvariant = errors.New("route store: invariant violated")
	// errRouteRowRaced 内部哨兵:批量插入的受影响行数少于待插行数(加锁读之后有并发写者插了同一个 id),整批回滚重来。
	errRouteRowRaced = errors.New("player_route: a concurrent writer inserted a row after the locking read")
)

// ── SQL ───────────────────────────────────────────────────────────────────────

// routeDBNowMsSQL 是库时钟的 epoch 毫秒(与 data_service dbNowMsSQL 同一表达式)。
const routeDBNowMsSQL = "CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED)"

// routeRowColumnsSQL 是 mergeRouteRow 除聚簇键外的列,顺序与 scanMergeRouteRow 一一对应。
const routeRowColumnsSQL = "home_zone, storage_id, placement_version, frozen_run_id, rev"

// 本文件的全部 SQL。抽成常量是为了让假驱动用例 / 真库用例执行**同一段文本**。
const (
	// CHECK、行片及围栏探针与DS同源契约；引擎与当前强读由current_tidb.go统一校验，全球工具不接受历史MySQL域。
	routeProbeSessionSQL = "SELECT @@SESSION.transaction_isolation, @@SESSION.innodb_lock_wait_timeout"
	// P4:一行只违反 chk_route_shard 的插入(player_id=0 的片应为 0,这里绑 1),包在必然回滚的事务里。
	routeProbeShardCheckSQL = "INSERT INTO player_route (route_shard, player_id, rev, updated_by, created_at_ms, updated_at_ms)" +
		" VALUES (1, 0, 1, 'route-probe', 0, 0)"
	// P5:按模数 64 合法的插入(实参:行片、player_id,取自 routeShardModulusProbeKeys),与 P4 同一个必然回滚的事务。
	routeProbeShardModulusSQL = "INSERT INTO player_route (route_shard, player_id, rev, updated_by, created_at_ms, updated_at_ms)" +
		" VALUES (?, ?, 1, 'route-probe', 0, 0)"
	// 普通一致读可写开关(每个写事务的第一句,提案 §4.1-2 第 1 步)。
	routeReadWritableSQL = "SELECT writable FROM route_meta WHERE id = 1"
	// 探针事务的守卫:同一行加锁读(多副本 / 多工具并发探针时在一行已提交的记录上排队)。
	routeLockProbeGuardSQL = routeReadWritableSQL + " FOR UPDATE"
	// route_meta 整行(库世代与可写开关):探针 P3 与「库与 Redis 成对」核对用。
	routeReadMetaSQL = "SELECT generation, writable FROM route_meta WHERE id = 1"
	// 读库时钟(围栏过期时刻按它算)。
	routeNowSQL = "SELECT " + routeDBNowMsSQL

	// 围栏:确保分片行存在(autocommit、事务外;ODKU no-op)。由 routeEnsureFenceSQL 按 (zone, shard) 升序拼 n 个元组。
	routeEnsureFenceHeadSQL = "INSERT INTO zone_merge_fence (zone_id, shard) VALUES "
	routeEnsureFenceTailSQL = " ON DUPLICATE KEY UPDATE zone_id = zone_id"
	// 围栏:一个分片行在不在(事务外一致读,只为少发一次 ODKU)。实参:zone、shard。
	routeFenceShardExistsSQL = "SELECT 1 FROM zone_merge_fence WHERE zone_id = ? AND shard = ?"
	// 围栏:按 shard 升序锁一个 zone 的全部分片(获取用)。active 为 NULL(run_id 非空而 expires_at_ms 为 NULL,
	// 只有 CHECK 不生效时可能)按生效处理。src_zone / dst_zone 给接手核对被接手分片的 zone 对(TakeOverFence)。实参:zone、分片数(64)。
	routeLockFenceAllSQL = "SELECT shard, run_id, (run_id IS NOT NULL AND expires_at_ms > " + routeDBNowMsSQL + ") AS active," +
		" fence_epoch, holder_json, src_zone, dst_zone FROM zone_merge_fence WHERE zone_id = ? AND shard < ? ORDER BY shard FOR UPDATE"
	// 围栏:获取 —— 把一个 zone 的全部分片写成本次运行。实参:run_id、purpose、src、dst、expires_at_ms、holder_json、
	// fence_epoch、zone、分片数(64)。fence_epoch 严格变大,所以受影响行数恒为 64(不依赖「值没变」的口径)。
	routeAcquireFenceSQL = "UPDATE zone_merge_fence SET run_id = ?, purpose = ?, src_zone = ?, dst_zone = ?, expires_at_ms = ?," +
		" holder_json = ?, fence_epoch = ? WHERE zone_id = ? AND shard < ?"
	// 围栏:续期 —— 只续「仍是本次且按库时钟未过期」的分片。GREATEST(expires_at_ms + 1, ?) 保证值必变(受影响行数 = 命中行数),
	// 也保证续期永不缩短过期时刻。实参:新过期时刻、zone、分片数(64)、run_id、库时钟。
	routeRenewFenceSQL = "UPDATE zone_merge_fence SET expires_at_ms = GREATEST(expires_at_ms + 1, ?)" +
		" WHERE zone_id = ? AND shard < ? AND run_id = ? AND expires_at_ms > ?"
	// 围栏:续期后读回过期时刻(给镜像的 PEXPIREAT 用;取最大值:镜像只许比库晚过期)。实参:zone、分片数、run_id。
	routeReadFenceExpirySQL = "SELECT MAX(expires_at_ms) FROM zone_merge_fence WHERE zone_id = ? AND shard < ? AND run_id = ?"
	// 围栏:释放 —— 只清本次运行的分片;fence_epoch 保留(单调,下一次获取在它之上 +1)。实参:zone、分片数、run_id。
	routeReleaseFenceSQL = "UPDATE zone_merge_fence SET run_id = NULL, purpose = NULL, src_zone = NULL, dst_zone = NULL," +
		" expires_at_ms = NULL, holder_json = NULL WHERE zone_id = ? AND shard < ? AND run_id = ?"
	// 围栏:批事务锁 63 号片并读回 run_id 与是否生效。实参:zone、63。
	routeLockFenceShardSQL = "SELECT run_id, (run_id IS NOT NULL AND expires_at_ms > " + routeDBNowMsSQL + ") AS active" +
		" FROM zone_merge_fence WHERE zone_id = ? AND shard = ? FOR UPDATE"
	// 围栏:事务外读 63 号片(dry-run / 排障)。实参:zone、63。
	routeReadFenceSQL = "SELECT run_id, (run_id IS NOT NULL AND expires_at_ms > " + routeDBNowMsSQL + ") AS active," +
		" holder_json, fence_epoch, expires_at_ms FROM zone_merge_fence WHERE zone_id = ? AND shard = ?"

	// merged_into:加锁读 / 一致读 / 插入(ODKU no-op)/ 同 dst 改 run_id / 按 (src, dst, run_id) 删。
	routeLockMergedIntoSQL   = "SELECT dst_zone, run_id FROM zone_merged_into WHERE src_zone = ? FOR UPDATE"
	routeReadMergedIntoSQL   = "SELECT dst_zone, run_id FROM zone_merged_into WHERE src_zone = ?"
	routeInsertMergedIntoSQL = "INSERT INTO zone_merged_into (src_zone, dst_zone, run_id, merged_at_ms)" +
		" VALUES (?, ?, ?, " + routeDBNowMsSQL + ") ON DUPLICATE KEY UPDATE src_zone = src_zone"
	routeUpdateMergedIntoRunSQL = "UPDATE zone_merged_into SET run_id = ? WHERE src_zone = ? AND dst_zone = ?"
	routeDeleteMergedIntoSQL    = "DELETE FROM zone_merged_into WHERE src_zone = ? AND dst_zone = ? AND run_id = ?"

	// player_route 按 id 批量读的头部(行构造器 (route_shard, player_id) IN (…),走聚簇主键),由 routeSelectRowsSQL 拼。
	routeSelectRowsHeadSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE (route_shard, player_id) IN ("
	// 按 home zone / 落点键集翻页(提案 §11.2 G1a「后续批次的读方模板」;游标不用行比较,MySQL 不对它做 range 优化)。
	// 实参:zone(或落点)、游标片、游标片、游标 player_id、limit。
	routeScanByHomeSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE home_zone = ? AND (route_shard > ? OR (route_shard = ? AND player_id > ?))" +
		" ORDER BY route_shard, player_id LIMIT ?"
	routeScanByStorageSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE storage_id = ? AND (route_shard > ? OR (route_shard = ? AND player_id > ?))" +
		" ORDER BY route_shard, player_id LIMIT ?"
	// W10:home = zone 且没有落点的玩家,按聚簇键键集游标加锁(走 idx_route_home)。实参同 routeScanByHomeSQL。
	routePinZoneSelectSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE home_zone = ? AND storage_id IS NULL AND (route_shard > ? OR (route_shard = ? AND player_id > ?))" +
		" ORDER BY route_shard, player_id LIMIT ? FOR UPDATE"
	// W5 / W8:改 home(按读到的 rev 与原 home 做 CAS;行已被加锁读锁住,不是恰好 1 行就是不可能状态)。
	// 实参:新 home、写者、行片、player_id、库 rev、原 home。
	routeMoveHomeSQL = "UPDATE player_route SET home_zone = ?, rev = rev + 1, updated_by = ?, updated_at_ms = " + routeDBNowMsSQL +
		" WHERE route_shard = ? AND player_id = ? AND rev = ? AND home_zone = ?"
	// W9:给 home 为 NULL 的行补 home。实参:home、写者、行片、player_id、库 rev。
	routeFillHomeSQL = "UPDATE player_route SET home_zone = ?, rev = rev + 1, updated_by = ?, updated_at_ms = " + routeDBNowMsSQL +
		" WHERE route_shard = ? AND player_id = ? AND rev = ? AND home_zone IS NULL"
	// W9:批量插只有 home 的新行(rev = 1),由 routeInsertHomeSQL 按聚簇键升序拼。每行实参:行片、player_id、home、写者。
	routeInsertHomeHeadSQL = "INSERT INTO player_route (route_shard, player_id, home_zone, storage_id, placement_version, frozen_run_id," +
		" rev, updated_by, created_at_ms, updated_at_ms) VALUES "
	routeInsertHomeTupleSQL = "(?, ?, ?, NULL, NULL, NULL, 1, ?, " + routeDBNowMsSQL + ", " + routeDBNowMsSQL + ")"
	routeInsertHomeTailSQL  = " ON DUPLICATE KEY UPDATE player_id = player_id"
	// W7 / W10:钉落点 "{zone}:1"(只钉没有落点且 home 就是该 zone 的行)。实参:storage、写者、行片、player_id、库 rev、home。
	routePinSQL = "UPDATE player_route SET storage_id = ?, placement_version = 1, rev = rev + 1, updated_by = ?, updated_at_ms = " +
		routeDBNowMsSQL + " WHERE route_shard = ? AND player_id = ? AND rev = ? AND storage_id IS NULL AND home_zone = ?"
	// W11:搬库 CAS 写落点三列。实参:storage、version、frozen_run_id(稳定态 NULL)、写者、行片、player_id、库 rev。
	routeSetPlacementSQL = "UPDATE player_route SET storage_id = ?, placement_version = ?, frozen_run_id = ?, rev = rev + 1," +
		" updated_by = ?, updated_at_ms = " + routeDBNowMsSQL + " WHERE route_shard = ? AND player_id = ? AND rev = ?"
)

// routeSelectRowsSQL 拼 n 个 id 的批量读:n 个行构造器 (?, ?),实参按聚簇键升序交错给出 (行片_i, player_id_i);
// ORDER BY 聚簇键让加锁形态按聚簇键升序取锁。
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

// routeEnsureFenceSQL 拼 n 个 (zone_id, shard) 元组的 ODKU。
func routeEnsureFenceSQL(n int) string {
	return routeEnsureFenceHeadSQL + strings.TrimSuffix(strings.Repeat("(?, ?), ", n), ", ") + routeEnsureFenceTailSQL
}

// routeInsertHomeSQL 拼 n 行的 W9 插入。
func routeInsertHomeSQL(n int) string {
	return routeInsertHomeHeadSQL + strings.TrimSuffix(strings.Repeat(routeInsertHomeTupleSQL+", ", n), ", ") + routeInsertHomeTailSQL
}

// ── 行与键 ────────────────────────────────────────────────────────────────────

// routeRowShard 返回 player_route.route_shard(镜像 shared/placement.RouteRowShard;写者与读者都只能经它算)。
func routeRowShard(playerID uint64) uint32 {
	return uint32(playerID % uint64(routeRowShards))
}

// routeShardArg 是绑进 SQL 的行片实参(交给驱动 uint64,与 data_service 同口径)。
func routeShardArg(playerID uint64) uint64 {
	return uint64(routeRowShard(playerID))
}

// routeScanCursor 是键集游标 = 上一页最后一行的聚簇键 (route_shard, player_id);零值 = 从头开始。
type routeScanCursor struct {
	Shard    uint32
	PlayerID uint64
}

// routeCursorOf 返回 player_id 的聚簇键。
func routeCursorOf(playerID uint64) routeScanCursor {
	return routeScanCursor{Shard: routeRowShard(playerID), PlayerID: playerID}
}

// less 按聚簇键序比较(先片、后 player_id)。
func (c routeScanCursor) less(o routeScanCursor) bool {
	if c.Shard != o.Shard {
		return c.Shard < o.Shard
	}
	return c.PlayerID < o.PlayerID
}

// mergeRouteRow 是 player_route 的一行(只取发布与判定用得到的列)。「0 / 空串 = NULL」:HomeZone、StorageID
// (与 PlacementVersion 同 NULL)、FrozenRunID。
type mergeRouteRow struct {
	PlayerID         uint64
	HomeZone         uint32
	StorageID        uint32
	PlacementVersion uint64
	FrozenRunID      string
	Rev              uint64
}

// homeValue 返回 player:zone 的值:没有 home 为空串(发布脚本按空串 DEL),否则十进制。
func (r mergeRouteRow) homeValue() string {
	if r.HomeZone == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(r.HomeZone), 10)
}

// placement 把落点三列还原成落点记录;没有落点回 (零值, false, nil)。三列不自洽一律 errPlacementMalformed:
// 坏行绝不发布、绝不交给判定(fail-closed)。
func (r mergeRouteRow) placement() (placementRecord, bool, error) {
	if r.StorageID == 0 {
		if r.PlacementVersion != 0 || r.FrozenRunID != "" {
			return placementRecord{}, false, fmt.Errorf("%w: player_route player_id=%d has no storage_id but placement_version=%d frozen_run_id=%q",
				errPlacementMalformed, r.PlayerID, r.PlacementVersion, r.FrozenRunID)
		}
		return placementRecord{}, false, nil
	}
	rec := placementRecord{StorageID: r.StorageID, Version: r.PlacementVersion, Frozen: r.FrozenRunID != "", RunID: r.FrozenRunID}
	if err := rec.Validate(); err != nil {
		return placementRecord{}, false, fmt.Errorf("player_route player_id=%d: %w", r.PlayerID, err)
	}
	return rec, true, nil
}

// placementValue 返回 player:placement 的值:没有落点为空串,否则与 Redis 里同一编码(placementRecord.Encode)。
func (r mergeRouteRow) placementValue() (string, error) {
	rec, ok, err := r.placement()
	if err != nil || !ok {
		return "", err
	}
	return rec.Encode(), nil
}

// withPlacement 把一条落点记录写进行的三列。
func (r mergeRouteRow) withPlacement(rec placementRecord) mergeRouteRow {
	r.StorageID, r.PlacementVersion, r.FrozenRunID = rec.StorageID, rec.Version, ""
	if rec.Frozen {
		r.FrozenRunID = rec.RunID
	}
	return r
}

// routePlacementRead 把库里的一行编码成与 readPlacements(Redis)同形状的读结果:值串与 Redis 里的编码逐字节相同,
// 所以 decideFreeze / decidePin / placementCAS 的期望值不必区分来源(规格 §6.3 W11「read 读库」)。
// present=false(库里没有这一行)回零值,即「两键都不存在」。落点三列不自洽时 RecordPresent=true、RecordErr 非空(调用方 fail-closed)。
func routePlacementRead(row mergeRouteRow, present bool) placementRead {
	if !present {
		return placementRead{}
	}
	var rec, home any
	if v := row.homeValue(); v != "" {
		home = v
	}
	plc, err := row.placementValue()
	if err != nil {
		rd, _ := decodePlacementRead(nil, home)
		rd.RecordPresent, rd.RecordErr = true, err
		return rd
	}
	if plc != "" {
		rec = plc
	}
	// 两个元素都是 nil 或 string,decodePlacementRead 不会报错。
	rd, _ := decodePlacementRead(rec, home)
	return rd
}

// routeScanner 是 *sql.Row 与 *sql.Rows 共有的 Scan。
type routeScanner interface {
	Scan(dest ...any) error
}

// scanMergeRouteRow 扫一行(聚簇键两列 + routeRowColumnsSQL),并校验库里存的行片等于 player_id 的行片(不等即
// errRouteShardMismatch,fail-closed)。可空列用 sql.Null[T] 扫:bigint unsigned 可能超过 2^63,NullInt64 装不下。
func scanMergeRouteRow(sc routeScanner) (mergeRouteRow, error) {
	var row mergeRouteRow
	var shard uint64
	var home, storage sql.Null[uint32]
	var version sql.Null[uint64]
	var frozen sql.NullString
	if err := sc.Scan(&shard, &row.PlayerID, &home, &storage, &version, &frozen, &row.Rev); err != nil {
		return mergeRouteRow{}, err
	}
	if want := routeShardArg(row.PlayerID); shard != want {
		return mergeRouteRow{}, fmt.Errorf("%w: player_id=%d route_shard=%d want %d", errRouteShardMismatch, row.PlayerID, shard, want)
	}
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
	return row, nil
}

// routeBatchResult 是一次批原语的结果。
//   - Verdicts:每个入参 id 的判定(提交后的口径);
//   - Target:本批**处于目标状态**的全部行(本批改的 + 早已是目标的),按 player_id 升序,值是提交后的库值 ——
//     调用方必须把它们全部交给发布层,全员确认之后依赖步骤才能推进(规格 §13.3 第 2 条);
//   - Changed:本批真正改了的行数。
type routeBatchResult[V any] struct {
	Verdicts map[uint64]V
	Target   []mergeRouteRow
	Changed  int
}

func newRouteBatchResult[V any](n int) routeBatchResult[V] {
	return routeBatchResult[V]{Verdicts: make(map[uint64]V, n)}
}

func (r *routeBatchResult[V]) sortTarget() {
	sort.Slice(r.Target, func(i, j int) bool { return r.Target[i].PlayerID < r.Target[j].PlayerID })
}

// ── 连接与探针 ────────────────────────────────────────────────────────────────

// routeDBDSN 把 -route-db-dsn 规范成路由表专用池的 DSN(与 data_service buildRouteDSN 同口径):
//   - transaction_isolation='READ-COMMITTED':锁推演只在 RC 下成立(ODKU 撞键只加记录锁、FOR UPDATE 读不存在的行不留间隙锁);
//   - innodb_lock_wait_timeout=1:锁等待封顶,等满即 1205、不重试;
//   - sql_mode='STRICT_TRANS_TABLES':超长的 holder_json 等不会被静默截断。
//   - 与业务/Team池共用pinCurrentTiDB，清除历史快照/旧副本/低精度TSO，逐连接固定当前强读。
//
// 运维写在 DSN 里的同名参数一律被覆盖;旧名 tx_isolation 删掉(两个名字同时出现时生效的是哪个不确定)。
// 必须带库名(全局库:dev 是 testdb,K8s 是 mmorpg_global);带 clientFoundRows=true 拒绝(ODKU 撞键 = 受影响 0 行的
// 判据会失效)。返回值含口令,只交给 sql.Open,不进日志、不进错误信息(ParseDSN 的错误不回显 DSN 原文)。
func routeDBDSN(raw string) (string, error) {
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return "", fmt.Errorf("parse -route-db-dsn: %w", err)
	}
	if cfg.DBName == "" {
		return "", errors.New("-route-db-dsn must name the global database that holds player_route (dev: testdb, K8s: mmorpg_global)")
	}
	if cfg.ClientFoundRows {
		return "", errors.New("-route-db-dsn must not set clientFoundRows=true: the route writes judge ON DUPLICATE KEY no-ops by 0 affected rows")
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	pinCurrentTiDB(cfg)
	cfg.Params[routeLockWaitParam] = strconv.Itoa(routeLockWaitTimeoutSec)
	cfg.Params[routeSQLModeParam] = routeSQLModeStrict
	return cfg.FormatDSN(), nil
}

// routeMeta 是 route_meta(id=1):库世代与可写开关。
type routeMeta struct {
	Generation string
	Writable   uint64
}

// mergeRouteStore 是路由表的读写。连接池独立于 -mysql-dsn 的池(会话变量不同)。并发安全(sql.DB 是)。
type mergeRouteStore struct {
	db        *sql.DB
	writer    string
	closeOnce sync.Once
	closeErr  error
	// sleep / backoff 是 1213 / 9007 重试的退避接缝:生产 sleepCtx 与均匀抖动,单测注入不睡的实现。
	sleep   func(ctx context.Context, d time.Duration) error
	backoff func() time.Duration
}

// openMergeRouteStore 在 -route-db-dsn 上开池、跑开写前探针(与 data_service NewRouteStore 的 P1–P5 同判据),
// 任一步失败就关池返回 error(fail-closed,调用方退出码非 0)。返回探针读到的 route_meta(成对核对要 generation)。
func openMergeRouteStore(ctx context.Context, rawDSN, writer string) (*mergeRouteStore, routeMeta, error) {
	if !validRouteWriter(writer) {
		return nil, routeMeta{}, fmt.Errorf("%w: writer %q must be 1..%d bytes of [A-Za-z0-9._@:-] (use newMergeRouteWriter)",
			errRouteInvalidArgument, writer, routeWriterMaxLen)
	}
	dsn, err := routeDBDSN(rawDSN)
	if err != nil {
		return nil, routeMeta{}, err
	}
	db, err := openToolSQLPool(ctx, dsn)
	if err != nil {
		return nil, routeMeta{}, fmt.Errorf("open -route-db-dsn: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	pctx, cancel := context.WithTimeout(ctx, routeProbeTimeout)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		return nil, routeMeta{}, fmt.Errorf("ping -route-db-dsn: %w", errors.Join(err, db.Close()))
	}
	return newMergeRouteStoreFromDB(pctx, db, writer)
}

// newMergeRouteStoreFromDB 在一个已开好的池上跑探针并装出 store;任一步失败都关掉 db。单测注入假驱动用。
func newMergeRouteStoreFromDB(ctx context.Context, db *sql.DB, writer string) (*mergeRouteStore, routeMeta, error) {
	st := &mergeRouteStore{db: db, writer: writer, sleep: sleepCtx, backoff: routeLockRetryBackoff}
	if err := registerMaintenanceResource(ctx, st); err != nil {
		return nil, routeMeta{}, errors.Join(err, st.Close())
	}
	meta, err := st.probe(ctx)
	if err != nil {
		return nil, routeMeta{}, fmt.Errorf("route db probe: %w", errors.Join(err, st.Close()))
	}
	if meta.Writable != 1 {
		if normalFreezeFromContext(ctx) == nil {
			log.Printf("WARN: route_meta(id=1).writable=%d: 缺少本轮NORMAL冻结收据，拒绝路由写入", meta.Writable)
		}
	}
	return st, meta, nil
}

// Close 关闭连接池。
func (s *mergeRouteStore) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

// probe 是开写前探针(判据与 SQL 照抄 data_service store.newRouteStoreFromDB):
//  1. 会话隔离级别必须是 READ-COMMITTED、innodb_lock_wait_timeout 必须是 1(DSN 的会话变量没生效时锁推演不成立);
//  2. 对端必须是真TiDB，事务/副本/一致性/快照/staleness/低精度TSO读回全部符合当前强读策略;
//  3. route_meta(id=1) 必须在;
//  4. chk_route_shard 必须生效(P4:必然回滚的事务里插一行只违反它的行,必须收到点名它的 3819);
//  5. 模数必须是 64(P5:同一事务再插两行按 64 合法的行,(0, 64) 与 (63, 63) 都被接受 ⇔ 模数 = 64)。
//
// P4 + P5 遇 1213 / 9007 整事务重跑(1205 不重试)。需要 player_route 的 INSERT 与 route_meta 的 UPDATE(FOR UPDATE)权限。
func (s *mergeRouteStore) probe(ctx context.Context) (routeMeta, error) {
	if err := probeRouteSession(ctx, s.db); err != nil {
		return routeMeta{}, err
	}
	meta, err := readRouteMeta(ctx, s.db)
	if err != nil {
		return routeMeta{}, err
	}
	if maintenanceFromContext(ctx) == nil {
		// audit/dry-run 装配只读句柄；会话与元数据仍须校验，不能为探针发送回滚 DML。
		// 这不授予写权限：所有真实维护写边界另外核同 boot TOOL 身份。
		return meta, nil
	}
	if err := retryRouteTx(ctx, "chk_route_shard probes", s.sleep, s.backoff, func() error {
		return probeRouteShardCheck(ctx, s.db)
	}); err != nil {
		return routeMeta{}, err
	}
	return meta, nil
}

// probeRouteSession 是探针第 1、2 步。
func probeRouteSession(ctx context.Context, db *sql.DB) error {
	var isolation string
	var lockWait int64
	if err := db.QueryRowContext(ctx, routeProbeSessionSQL).Scan(&isolation, &lockWait); err != nil {
		return fmt.Errorf("read session transaction_isolation / innodb_lock_wait_timeout: %w", err)
	}
	if !strings.EqualFold(isolation, "READ-COMMITTED") || lockWait != routeLockWaitTimeoutSec {
		return fmt.Errorf("%w: transaction_isolation=%q innodb_lock_wait_timeout=%d, want READ-COMMITTED and %d "+
			"(the DSN session variables pinned by routeDBDSN did not take effect)",
			errRouteSessionMisconfigured, isolation, lockWait, routeLockWaitTimeoutSec)
	}
	return requireCurrentTiDB(ctx, db)
}

// readRouteMeta 读 route_meta(id=1)(探针第 3 步;一致读)。
func readRouteMeta(ctx context.Context, q routeQueryer) (routeMeta, error) {
	var m routeMeta
	err := q.QueryRowContext(ctx, routeReadMetaSQL).Scan(&m.Generation, &m.Writable)
	if errors.Is(err, sql.ErrNoRows) {
		return routeMeta{}, errRouteMetaMissing
	}
	if err != nil {
		return routeMeta{}, fmt.Errorf("read route_meta(id=1): %w", err)
	}
	return m, nil
}

// probeRouteShardCheck 是探针第 4、5 步:一个必然回滚的事务,第一句锁 route_meta(id=1) 作守卫,再跑 P4,P4 通过后同一事务跑 P5。
func probeRouteShardCheck(ctx context.Context, db *sql.DB) (err error) {
	if err := checkMaintenanceTool(ctx); err != nil {
		return err
	}
	proof := normalFreezeFromContext(ctx)
	if proof == nil && !normalToolFromContext(ctx) {
		return errNormalFreezeProof
	}
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		return err
	}
	defer finish()
	defer func() {
		if problem := recover(); problem != nil {
			holdMaintenanceTool(ctx)
			panic(problem)
		}
		if err != nil {
			holdMaintenanceTool(ctx)
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin chk_route_shard probe: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()

	var writable uint64
	switch err := tx.QueryRowContext(ctx, routeLockProbeGuardSQL).Scan(&writable); {
	case errors.Is(err, sql.ErrNoRows):
		return errRouteMetaMissing
	case err != nil:
		return fmt.Errorf("lock route_meta(id=1) as the probe guard: %w", err)
	}
	if proof != nil {
		if err := proof.check(ctx, tx); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, routeProbeShardCheckSQL)
	if err == nil {
		return fmt.Errorf("%w: a row violating it was accepted (rolled back)", errRouteCheckNotEnforced)
	}
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return fmt.Errorf("chk_route_shard probe: %w", err)
	}
	switch {
	case me.Number == mysqlErrCheckConstraintViolated && namesRouteShardCheck(me):
		return probeRouteShardModulus(ctx, tx)
	case me.Number == mysqlErrCheckConstraintViolated:
		return fmt.Errorf("%w: the probe row was rejected by another constraint, so chk_route_shard is not proven: %v", errRouteCheckNotEnforced, err)
	case me.Number == mysqlErrDupEntry:
		return fmt.Errorf("chk_route_shard probe hit 1062: player_route already holds the mis-sharded row (route_shard=1, player_id=0) "+
			"written while CHECK was not enforced; remove it after review, then run `route_tool audit`: %w", err)
	case me.Number == mysqlErrTableAccessDenied:
		return routeProbeInsertDenied(err)
	default:
		return fmt.Errorf("chk_route_shard probe: %w", err)
	}
}

// probeRouteShardModulus 是探针第 5 步(P5):逐行判定,成功或 1062 = 该行被接受;3819 点名 chk_route_shard = 模数不对;
// 1142 = 缺权限;其余原样包装(证不了就不开写)。
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
			continue
		case isMySQL && me.Number == mysqlErrCheckConstraintViolated && namesRouteShardCheck(me):
			return fmt.Errorf("%w: probe row (route_shard=%d, player_id=%d) is legal under %% %d but chk_route_shard rejected it: %v",
				errRouteShardModulusMismatch, key.Shard, key.PlayerID, routeRowShards, err)
		case isMySQL && me.Number == mysqlErrTableAccessDenied:
			return routeProbeInsertDenied(err)
		default:
			return fmt.Errorf("route_shard modulus probe row (route_shard=%d, player_id=%d): %w", key.Shard, key.PlayerID, err)
		}
	}
	return nil
}

// routeShardModulusProbeKeys 返回 P5 的两行聚簇键(按聚簇键升序):(0, 64) 与 (63, 63)。
func routeShardModulusProbeKeys() []routeScanCursor {
	n := uint64(routeRowShards)
	return []routeScanCursor{routeCursorOf(n), routeCursorOf(n - 1)}
}

// namesRouteShardCheck 报告一条 3819 的报错是否点名 chk_route_shard(其余五条约束的名字都不含这个子串)。
func namesRouteShardCheck(me *mysql.MySQLError) bool {
	return strings.Contains(strings.ToLower(me.Message), "chk_route_shard")
}

func routeProbeInsertDenied(err error) error {
	return fmt.Errorf("the chk_route_shard probes need INSERT privilege on player_route (rolled-back inserts): %w", err)
}

// ── 写者标识 ──────────────────────────────────────────────────────────────────

// newMergeRouteWriter 生成写进 player_route.updated_by 的写者标识 "merge_zone@{host}:{run_id 前缀}"(规格 §6.2):
// 只留 [A-Za-z0-9._@:-](列是 ascii,与 data_service validRouteWriter 同一字符集),总长不超过 96 字节。host 由装配层传入。
func newMergeRouteWriter(host, runID string) string {
	run := sanitizeRouteWriterPart(runID)
	if len(run) > routeWriterRunIDPrefix {
		run = run[:routeWriterRunIDPrefix]
	}
	if run == "" {
		run = "norun"
	}
	h := sanitizeRouteWriterPart(host)
	if h == "" {
		h = "unknown"
	}
	if room := routeWriterMaxLen - len(routeWriterHeadPart) - 1 - len(run); len(h) > room {
		h = h[:room] // 洗过之后全是单字节 ASCII,按字节截不会截断字符
	}
	return routeWriterHeadPart + h + ":" + run
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
	}
	return false
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

// ── 事务与重试 ────────────────────────────────────────────────────────────────

// routeQueryer 是 *sql.DB 与 *sql.Tx 共有的读接口。
type routeQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// isRouteLockConflict 判断错误是不是可整事务重跑的锁冲突:1213(InnoDB 死锁)与 9007(TiDB 写冲突)。
// 1205 刻意不在其中(与 merge_run.go isRetryableLockConflict 的口径不同:那边是单行短事务、锁等待 50s 的池)。
func isRouteLockConflict(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlErrDeadlock || me.Number == tidbErrWriteConflict
}

// routeLockRetryBackoff 在 [Min, Max] 内均匀取一个等待时长(同时失败的事务错开重来)。
func routeLockRetryBackoff() time.Duration {
	return routeLockRetryBackoffMin + rand.N(routeLockRetryBackoffMax-routeLockRetryBackoffMin+1)
}

// retryRouteTx 有界重来:1213 / 9007 抖动退避后重来;errRouteRowRaced 不退避、直接重来;其它错误(含 1205)立刻返回。
// 总尝试次数不超过 routeTxAttempts;用尽后把最后一次的错误包着返回(errors.As 仍能取到 MySQLError)。
func retryRouteTx(ctx context.Context, op string, sleep func(context.Context, time.Duration) error,
	backoff func() time.Duration, attempt func() error) error {
	var err error
	for n := 1; n <= routeTxAttempts; n++ {
		if err = attempt(); err == nil {
			return nil
		}
		raced := errors.Is(err, errRouteRowRaced)
		if !raced && !isRouteLockConflict(err) {
			return err
		}
		if n == routeTxAttempts {
			break
		}
		log.Printf("WARN: %s: attempt %d/%d failed (%v), retrying", op, n, routeTxAttempts, err)
		if raced {
			continue
		}
		if werr := sleep(ctx, backoff()); werr != nil {
			return fmt.Errorf("%s: %w", op, errors.Join(err, werr))
		}
	}
	return fmt.Errorf("%s: gave up after %d attempts: %w", op, routeTxAttempts, err)
}

// runTx 跑一个事务(BEGIN → body → COMMIT,出错 ROLLBACK),按 retryRouteTx 有界重来。body 必须可整体重跑:
// 结果只在 body 成功返回时交给外层变量。
func (s *mergeRouteStore) runTx(ctx context.Context, op string, body func(tx *sql.Tx) error) (err error) {
	if err := checkMaintenanceTool(ctx); err != nil {
		return err
	}
	proof := normalFreezeFromContext(ctx)
	if proof == nil && !normalToolFromContext(ctx) {
		return errNormalFreezeProof
	}
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		return err
	}
	defer finish()
	defer func() {
		if problem := recover(); problem != nil {
			holdMaintenanceTool(ctx)
			panic(problem)
		}
		if err != nil {
			holdMaintenanceTool(ctx)
		}
	}()
	return retryRouteTx(ctx, op, s.sleep, s.backoff, func() (err error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("%s: begin: %w", op, err)
		}
		defer func() {
			rollbackErr := tx.Rollback()
			if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				holdMaintenanceTool(ctx)
				err = errors.Join(err, rollbackErr)
			}
		}()
		// 释放/续租等body原先没有requireRouteWritable，也必须在同一事务锁meta并强读原冻结。
		if proof != nil {
			if err := proof.lockTransaction(ctx, tx); err != nil {
				return err
			}
		}
		if err := body(tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			holdMaintenanceTool(ctx)
			return fmt.Errorf("%s: commit: %w", op, err)
		}
		return nil
	})
}

// requireRowsAffected 是「行已被加锁读锁住」之后那条写的判据:受影响行数必须恰好是 want,否则锁语义被破坏,fail-closed。
func requireRowsAffected(res sql.Result, want int64, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", what, err)
	}
	if n != want {
		return fmt.Errorf("%w: %s affected %d rows, want %d under the row locks", errRouteInvariant, what, n, want)
	}
	return nil
}

// readRouteDBNowMs 读库时钟(epoch 毫秒)。
func readRouteDBNowMs(ctx context.Context, q routeQueryer) (uint64, error) {
	var now uint64
	if err := q.QueryRowContext(ctx, routeNowSQL).Scan(&now); err != nil {
		return 0, fmt.Errorf("read DB clock: %w", err)
	}
	if now == 0 {
		return 0, fmt.Errorf("%w: DB clock read as 0", errRouteInvariant)
	}
	return now, nil
}

// requireRouteWritable 是每个写事务的第一句:route_meta.writable != 1 即拒写(普通一致读,提案 §4.1-2 第 1 步)。
func requireRouteWritable(ctx context.Context, q routeQueryer) error {
	if proof := normalFreezeFromContext(ctx); proof != nil {
		// 维护工具不改writable；每个实际路由写事务先锁meta并强读原NORMAL消费链。
		// 此锁与正式Release同一行，不能由事务外预检代替提交前缀。
		tx, ok := q.(*sql.Tx)
		if !ok {
			return errNormalFreezeProof
		}
		return proof.lockTransaction(ctx, tx)
	}
	var writable uint64
	err := q.QueryRowContext(ctx, routeReadWritableSQL).Scan(&writable)
	if errors.Is(err, sql.ErrNoRows) {
		return errRouteMetaMissing
	}
	if err != nil {
		return fmt.Errorf("read route_meta.writable: %w", err)
	}
	if writable != 1 {
		return fmt.Errorf("%w (writable=%d)", errRouteNotWritable, writable)
	}
	return nil
}

// ── 入参整理 ──────────────────────────────────────────────────────────────────

// routeZoneOK 报告 zone 是否在 [1, routeHomeZoneMax]。
func routeZoneOK(zone uint32) bool {
	return zone >= 1 && zone <= routeHomeZoneMax
}

// sortedDistinctRouteZones 校验 zone 列表(非空、都在 [1, 999999]、不重复)并返回升序副本(围栏按 zone 升序取锁)。
func sortedDistinctRouteZones(zones []uint32) ([]uint32, error) {
	if len(zones) == 0 {
		return nil, fmt.Errorf("%w: no zone", errRouteInvalidArgument)
	}
	out := append([]uint32(nil), zones...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	for i, z := range out {
		if !routeZoneOK(z) {
			return nil, fmt.Errorf("%w: zone %d not in [1, %d]", errRouteInvalidArgument, z, routeHomeZoneMax)
		}
		if i > 0 && out[i-1] == z {
			return nil, fmt.Errorf("%w: zone %d listed twice", errRouteInvalidArgument, z)
		}
	}
	return out, nil
}

// routeZonePair 校验一对不同的 zone,返回升序的两个(围栏取锁顺序)。
func routeZonePair(a, b uint32) ([]uint32, error) {
	if a == b {
		return nil, fmt.Errorf("%w: zones must differ (both %d)", errRouteInvalidArgument, a)
	}
	return sortedDistinctRouteZones([]uint32{a, b})
}

// prepareRouteBatch 校验一批 player_id(不含 0、不重复、不超过 limit),返回按聚簇键升序的副本(锁多行一律按这个顺序)。
// 空批返回空切片、不报错(调用方不发任何语句)。
func prepareRouteBatch(ids []uint64, limit int) ([]uint64, error) {
	if len(ids) > limit {
		return nil, fmt.Errorf("%w: %d players in one batch, limit %d", errRouteInvalidArgument, len(ids), limit)
	}
	out := append([]uint64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return routeCursorOf(out[i]).less(routeCursorOf(out[j])) })
	for i, id := range out {
		if id == 0 {
			return nil, fmt.Errorf("%w: player_id 0", errRouteInvalidArgument)
		}
		if i > 0 && out[i-1] == id {
			return nil, fmt.Errorf("%w: player %d listed twice", errRouteInvalidArgument, id)
		}
	}
	return out, nil
}

// ── player_route 读 ───────────────────────────────────────────────────────────

// loadRouteRows 按 id 读一批行(ids 已按聚簇键升序、不重复、不超过 routeReadBatchLimit)。forUpdate 时按聚簇键升序取锁。
// 返回的行必须都是请求过的、且不重复,否则 fail-closed。
func loadRouteRows(ctx context.Context, q routeQueryer, ids []uint64, forUpdate bool) (map[uint64]mergeRouteRow, error) {
	out := make(map[uint64]mergeRouteRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	want := make(map[uint64]bool, len(ids))
	args := make([]any, 0, 2*len(ids))
	for _, id := range ids {
		want[id] = true
		args = append(args, routeShardArg(id), id)
	}
	rows, err := q.QueryContext(ctx, routeSelectRowsSQL(len(ids), forUpdate), args...)
	if err != nil {
		return nil, fmt.Errorf("read player_route (%d players): %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanMergeRouteRow(rows)
		if err != nil {
			return nil, err
		}
		if !want[row.PlayerID] {
			return nil, fmt.Errorf("%w: player_route returned player %d that was not asked for", errRouteInvariant, row.PlayerID)
		}
		if _, dup := out[row.PlayerID]; dup {
			return nil, fmt.Errorf("%w: player_route returned player %d twice", errRouteInvariant, row.PlayerID)
		}
		out[row.PlayerID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read player_route (%d players): %w", len(ids), err)
	}
	return out, nil
}

// ReadRoutes 按 id 一致读(不加锁),每 routeReadBatchLimit 个一条语句;重复 id 去重,0 拒绝。
// 返回 map 里没有的 id = 库里没有这一行(权威缺席)。R8 各读方在 -route-source=db 时改用它(下一波接线)。
func (s *mergeRouteStore) ReadRoutes(ctx context.Context, ids []uint64) (map[uint64]mergeRouteRow, error) {
	seen := make(map[uint64]bool, len(ids))
	var uniq []uint64
	for _, id := range ids {
		if id == 0 {
			return nil, fmt.Errorf("%w: player_id 0", errRouteInvalidArgument)
		}
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	sort.Slice(uniq, func(i, j int) bool { return routeCursorOf(uniq[i]).less(routeCursorOf(uniq[j])) })
	out := make(map[uint64]mergeRouteRow, len(uniq))
	for _, chunk := range chunkUint64(uniq, routeReadBatchLimit) {
		rows, err := loadRouteRows(ctx, s.db, chunk, false)
		if err != nil {
			return nil, err
		}
		for id, row := range rows {
			out[id] = row
		}
	}
	return out, nil
}

// ScanByHome 按 home zone 键集翻页(一致读,走 idx_route_home):返回 home_zone = zone 且聚簇键 > after 的至多 limit 行,
// 按 (route_shard, player_id) 升序;下一页从 routeCursorOf(最后一行.PlayerID) 接着翻,一页不足 limit 即翻完。
// 每行都校验(行片一致、home 等于 zone、键严格递增且大于 after),任一不符整页作废。
func (s *mergeRouteStore) ScanByHome(ctx context.Context, zone uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error) {
	if !routeZoneOK(zone) {
		return nil, fmt.Errorf("%w: scan by home_zone %d not in [1, %d]", errRouteInvalidArgument, zone, routeHomeZoneMax)
	}
	return s.scanByIndex(ctx, "home_zone", routeScanByHomeSQL, uint64(zone), after, limit,
		func(r mergeRouteRow) bool { return r.HomeZone == zone })
}

// ScanByStorage 按落点键集翻页(走 idx_route_storage),其余同 ScanByHome。storage 必须非 0。
func (s *mergeRouteStore) ScanByStorage(ctx context.Context, storage uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error) {
	if storage == 0 {
		return nil, fmt.Errorf("%w: scan by storage_id 0", errRouteInvalidArgument)
	}
	return s.scanByIndex(ctx, "storage_id", routeScanByStorageSQL, uint64(storage), after, limit,
		func(r mergeRouteRow) bool { return r.StorageID == storage })
}

func (s *mergeRouteStore) scanByIndex(ctx context.Context, what, query string, key uint64, after routeScanCursor, limit int,
	match func(mergeRouteRow) bool) ([]mergeRouteRow, error) {
	if limit < 1 || limit > routeReadBatchLimit || after.Shard >= routeRowShards {
		return nil, fmt.Errorf("%w: scan by %s limit=%d after=%+v", errRouteInvalidArgument, what, limit, after)
	}
	rows, err := s.db.QueryContext(ctx, query, key, uint64(after.Shard), uint64(after.Shard), after.PlayerID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan player_route by %s=%d: %w", what, key, err)
	}
	defer rows.Close()
	return collectRouteRowsAfter(rows, what, key, after, limit, match)
}

// collectRouteRowsAfter 收一页键集翻页的结果并逐行校验(见 ScanByHome)。
func collectRouteRowsAfter(rows *sql.Rows, what string, key uint64, after routeScanCursor, limit int,
	match func(mergeRouteRow) bool) ([]mergeRouteRow, error) {
	var out []mergeRouteRow
	prev := after
	for rows.Next() {
		row, err := scanMergeRouteRow(rows)
		if err != nil {
			return nil, err
		}
		cur := routeCursorOf(row.PlayerID)
		switch {
		case !match(row):
			return nil, fmt.Errorf("%w: scan by %s=%d returned player %d whose row does not match", errRouteInvariant, what, key, row.PlayerID)
		case !prev.less(cur):
			return nil, fmt.Errorf("%w: scan by %s=%d returned key %+v not after %+v", errRouteInvariant, what, key, cur, prev)
		case len(out) >= limit:
			return nil, fmt.Errorf("%w: scan by %s=%d returned more than %d rows", errRouteInvariant, what, key, limit)
		}
		prev = cur
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan player_route by %s=%d: %w", what, key, err)
	}
	return out, nil
}

// ── 围栏 ──────────────────────────────────────────────────────────────────────

// ensureAllFenceShards 确保每个 zone 的 0..63 号分片行都在(autocommit、事务外,一条 ODKU,按 (zone, shard) 升序)。
// 不在持锁事务里插:两个事务同时对不存在的行 FOR UPDATE 再各自 INSERT 是经典的成环形状(data_service R3 同理)。
func (s *mergeRouteStore) ensureAllFenceShards(ctx context.Context, zones []uint32) error {
	if err := checkMaintenanceTool(ctx); err != nil {
		return err
	}
	if normalFreezeFromContext(ctx) == nil && !normalToolFromContext(ctx) {
		return errNormalFreezeProof
	}
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		return err
	}
	defer finish()
	defer func() {
		if problem := recover(); problem != nil {
			holdMaintenanceTool(ctx)
			panic(problem)
		}
	}()
	args := make([]any, 0, 2*len(zones)*int(routeFenceShards))
	for _, zone := range zones {
		for shard := uint32(0); shard < routeFenceShards; shard++ {
			args = append(args, uint64(zone), uint64(shard))
		}
	}
	query := routeEnsureFenceSQL(len(args) / 2)
	if normalFreezeFromContext(ctx) != nil {
		return s.runTx(ctx, "冻结维护初始化围栏片", func(tx *sql.Tx) error {
			if err := requireRouteWritable(ctx, tx); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		})
	}
	return retryRouteTx(ctx, "ensure merge fence shards", s.sleep, s.backoff, func() error {
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			holdMaintenanceTool(ctx)
			return fmt.Errorf("ensure zone_merge_fence shards for zones %v: %w", zones, err)
		}
		return nil
	})
}

// ensureFenceCheckShard 确保每个 zone 的 63 号片行在(先一致读,缺了才发 ODKU:不在线上建角的围栏片上白白加一次 X 锁)。
func (s *mergeRouteStore) ensureFenceCheckShard(ctx context.Context, zones []uint32) error {
	if err := checkMaintenanceTool(ctx); err != nil {
		return err
	}
	if normalFreezeFromContext(ctx) == nil && !normalToolFromContext(ctx) {
		return errNormalFreezeProof
	}
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		return err
	}
	defer finish()
	defer func() {
		if problem := recover(); problem != nil {
			holdMaintenanceTool(ctx)
			panic(problem)
		}
	}()
	for _, zone := range zones {
		var one int64
		err := s.db.QueryRowContext(ctx, routeFenceShardExistsSQL, uint64(zone), uint64(routeFenceCheckShard)).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read zone_merge_fence(%d, %d): %w", zone, routeFenceCheckShard, err)
		}
		args := []any{uint64(zone), uint64(routeFenceCheckShard)}
		if normalFreezeFromContext(ctx) != nil {
			if err := s.runTx(ctx, "冻结维护初始化检查片", func(tx *sql.Tx) error {
				if err := requireRouteWritable(ctx, tx); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, routeEnsureFenceSQL(1), args...)
				return err
			}); err != nil {
				return err
			}
			continue
		}
		if err := retryRouteTx(ctx, "ensure merge fence check shard", s.sleep, s.backoff, func() error {
			if _, err := s.db.ExecContext(ctx, routeEnsureFenceSQL(1), args...); err != nil {
				holdMaintenanceTool(ctx)
				return fmt.Errorf("ensure zone_merge_fence(%d, %d): %w", zone, routeFenceCheckShard, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// routeFenceShardState 是获取围栏时锁到的一个分片行。
type routeFenceShardState struct {
	shard      uint32
	runID      string
	active     bool
	epoch      uint64
	holderJSON string
	// srcZone / dstZone 是分片上记着的 zone 对(0 = NULL,即未封锁)。
	srcZone, dstZone uint32
}

// lockAllFenceShards 按 shard 升序锁一个 zone 的全部 64 片。少一行(或多出 / 乱序)一律 errRouteFenceRowMissing / errRouteInvariant。
func lockAllFenceShards(ctx context.Context, tx *sql.Tx, zone uint32) ([]routeFenceShardState, error) {
	rows, err := tx.QueryContext(ctx, routeLockFenceAllSQL, uint64(zone), uint64(routeFenceShards))
	if err != nil {
		return nil, fmt.Errorf("lock zone_merge_fence zone %d: %w", zone, err)
	}
	defer rows.Close()
	out := make([]routeFenceShardState, 0, routeFenceShards)
	for rows.Next() {
		var shard uint64
		var run, holder sql.NullString
		var active sql.Null[int64]
		var epoch uint64
		var srcZone, dstZone sql.Null[uint64]
		if err := rows.Scan(&shard, &run, &active, &epoch, &holder, &srcZone, &dstZone); err != nil {
			return nil, fmt.Errorf("lock zone_merge_fence zone %d: %w", zone, err)
		}
		if shard != uint64(len(out)) {
			return nil, fmt.Errorf("%w: zone_merge_fence zone %d returned shard %d at position %d", errRouteInvariant, zone, shard, len(out))
		}
		if srcZone.V > math.MaxUint32 || dstZone.V > math.MaxUint32 {
			return nil, fmt.Errorf("%w: zone_merge_fence zone %d shard %d has src_zone=%d dst_zone=%d", errRouteInvariant, zone, shard,
				srcZone.V, dstZone.V)
		}
		out = append(out, routeFenceShardState{
			shard:      uint32(shard),
			runID:      run.String,
			active:     !active.Valid || active.V != 0,
			epoch:      epoch,
			holderJSON: holder.String,
			srcZone:    uint32(srcZone.V),
			dstZone:    uint32(dstZone.V),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock zone_merge_fence zone %d: %w", zone, err)
	}
	if len(out) != int(routeFenceShards) {
		return nil, fmt.Errorf("%w: zone %d has %d of %d shard rows", errRouteFenceRowMissing, zone, len(out), routeFenceShards)
	}
	return out, nil
}

// routeFenceCheck 是批事务锁 63 号片读到的状态。
type routeFenceCheck struct {
	// runID 为空串 = run_id 为 NULL。
	runID string
	// active:按库时钟生效;run_id 非空而 expires_at_ms 为 NULL(只有 CHECK 不生效时可能)也算生效(fail-closed)。
	active bool
	// proven:「run_id 非空且 expires_at_ms 大于库时钟」被证得(上面那种 NULL 情形为 false)。
	proven bool
}

// lockFenceCheckShard 锁 zone 的 63 号片。读不到行一律 errRouteFenceRowMissing(不当「未生效」,规格 §13.3 第 1 条)。
func lockFenceCheckShard(ctx context.Context, tx *sql.Tx, zone uint32) (routeFenceCheck, error) {
	var run sql.NullString
	var active sql.Null[int64]
	err := tx.QueryRowContext(ctx, routeLockFenceShardSQL, uint64(zone), uint64(routeFenceCheckShard)).Scan(&run, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return routeFenceCheck{}, fmt.Errorf("%w: zone %d shard %d", errRouteFenceRowMissing, zone, routeFenceCheckShard)
	}
	if err != nil {
		return routeFenceCheck{}, fmt.Errorf("lock zone_merge_fence(%d, %d): %w", zone, routeFenceCheckShard, err)
	}
	return routeFenceCheck{
		runID:  run.String,
		active: !active.Valid || active.V != 0,
		proven: active.Valid && active.V != 0,
	}, nil
}

// requireFenceHeld 是批事务内的 fencing token 复核:每个 zone(升序)的 63 号片 run_id 必须是本次、且按库时钟未过期,
// 否则 errRouteFenceLost。持有这把行锁期间围栏不可能易主(获取 / 续期 / 释放都要改它)。
func requireFenceHeld(ctx context.Context, tx *sql.Tx, zones []uint32, runID string) error {
	for _, zone := range zones {
		c, err := lockFenceCheckShard(ctx, tx, zone)
		if err != nil {
			return err
		}
		if c.runID != runID || !c.proven {
			return fmt.Errorf("%w: zone %d holds run_id=%q (active=%v), this run is %s", errRouteFenceLost, zone, c.runID, c.active, runID)
		}
	}
	return nil
}

// requireFenceInactive 要求 zone 没有生效的围栏(W9 / W10 / 搬库 CAS),生效即 errRouteZoneFenced。
func requireFenceInactive(ctx context.Context, tx *sql.Tx, zone uint32) error {
	c, err := lockFenceCheckShard(ctx, tx, zone)
	if err != nil {
		return err
	}
	if c.active {
		return fmt.Errorf("%w: zone %d (run_id=%q)", errRouteZoneFenced, zone, c.runID)
	}
	return nil
}

// routeFenceAcquire 是一次获取围栏的入参。
type routeFenceAcquire struct {
	Zones   []uint32
	RunID   string
	Purpose string
	Src     uint32
	Dst     uint32
	Holder  mergeInProgressValue
	TTL     time.Duration
}

// routeFenceGrant 是获取成功的结果:本次的 fence_epoch、库时钟的过期时刻、写进库里(也要写进镜像)的持有者 JSON。
type routeFenceGrant struct {
	Epoch       uint64
	ExpiresAtMs uint64
	HolderJSON  string
}

// routeFenceHolder 是 holder_json 列(与镜像 merge:in_progress:{zone})的值:fence.go 的 mergeInProgressValue 加 fence_epoch
// (规格 §13.1 第 3 条:读方只看 EXISTS,值格式不是契约;镜像脚本按 fence_epoch 做 set-if-newer)。
type routeFenceHolder struct {
	mergeInProgressValue
	FenceEpoch uint64 `json:"fence_epoch"`
}

func (a routeFenceAcquire) validate() ([]uint32, error) {
	zones, err := sortedDistinctRouteZones(a.Zones)
	if err != nil {
		return nil, err
	}
	switch {
	case !validRunID(a.RunID):
		return nil, fmt.Errorf("%w: run_id %q", errRouteInvalidArgument, a.RunID)
	case a.Purpose != routeFencePurposeMerge && a.Purpose != routeFencePurposeUnmerge:
		return nil, fmt.Errorf("%w: purpose %q (want %s or %s)", errRouteInvalidArgument, a.Purpose, routeFencePurposeMerge, routeFencePurposeUnmerge)
	case !routeZoneOK(a.Src) || !routeZoneOK(a.Dst) || a.Src == a.Dst:
		return nil, fmt.Errorf("%w: src=%d dst=%d", errRouteInvalidArgument, a.Src, a.Dst)
	case a.TTL < routeFenceMinTTL:
		return nil, fmt.Errorf("%w: fence ttl %s below %s", errRouteInvalidArgument, a.TTL, routeFenceMinTTL)
	}
	return zones, nil
}

// encodeRouteFenceHolder 编码 holder_json:run_id / src / dst / 过期时刻以库里的值为准(覆盖调用方给的),带上 fence_epoch。
func encodeRouteFenceHolder(req routeFenceAcquire, epoch, expiresAtMs uint64) (string, error) {
	h := req.Holder
	if h.Tool == "" {
		h.Tool = "tools/merge_zone"
	}
	h.RunID = req.RunID
	h.SourceZone, h.TargetZone = req.Src, req.Dst
	h.ExpiresAtUnixMs = int64(expiresAtMs)
	b, err := json.Marshal(routeFenceHolder{mergeInProgressValue: h, FenceEpoch: epoch})
	if err != nil {
		return "", fmt.Errorf("encode merge fence holder: %w", err)
	}
	if n := utf8.RuneCount(b); n > routeHolderJSONMaxChars {
		return "", fmt.Errorf("%w: holder_json is %d characters, zone_merge_fence.holder_json holds %d (shorten -manifest-path)",
			errRouteInvalidArgument, n, routeHolderJSONMaxChars)
	}
	return string(b), nil
}

// AcquireFence 获取合服围栏(W12 获取,规格 §6.3):
//
//	每个 zone 的 64 片行 ODKU 确保存在(事务外)→ BEGIN → 普通读 writable → 按 (zone, shard) 升序全部 FOR UPDATE →
//	任一片生效且 run_id ≠ 本次 → errRouteFenceHeld → newEpoch = 全部片 fence_epoch 的最大值 + 1 →
//	全部 UPDATE 为本次(expires_at_ms = 库时钟 + TTL,holder_json 带 fence_epoch = newEpoch)→ COMMIT。
//
// 同 run_id 重入(续跑)允许,epoch 照样 +1。返回之后调用方写镜像(setFenceMirror)+ WAITAOF,确认了才能收集清单。
// 冻结(writable=0)时拒绝获取;续期与释放不看 writable(保住围栏 / 解除封锁都是安全方向)。
func (s *mergeRouteStore) AcquireFence(ctx context.Context, req routeFenceAcquire) (routeFenceGrant, error) {
	return s.acquireFence(ctx, "acquire merge fence", req, "")
}

// TakeOverFence 是续跑接手(W12 获取的变体,fence.go 的 -take-over-fence):与 AcquireFence 同一个事务形状,只多放行一种
// 「生效且 run_id ≠ 本次」—— run_id == previousRunID(运维已确认那次运行的进程不在了)。接手与获取在同一个事务里完成,
// 两区在库里一刻都不解封(对照 redis 源「DEL 镜像 → 重跑」之间那几秒无围栏的窗口)。旧进程若其实还活着,它的下一个批事务
// 按 run_id 复核失败(errRouteFenceLost)、下一次续期确定失权,映射写入被拦在库这一层;它不碰映射的写只靠本地判据,
// 要到下一次续期失败才停(续期间隔不超过 routeFenceRenewMaxInterval),所以接手的前提仍是运维确认那次运行的进程不在了。
// 生效且属于第三个 run_id 的分片照样 errRouteFenceHeld;被接手的分片还要过 checkTakeOverShard(zone 对、清单路径),且每个 zone
// 接手 0 片或全部 64 片,不满足即 errRouteInvalidArgument。previousRunID 必须合法且不同于本次。
// (前一波原语里没有它:续跑接手是本波接线 db 源的中止 / 续跑流程时补的;AcquireFence 的语义不变,只是与它共用事务体。)
func (s *mergeRouteStore) TakeOverFence(ctx context.Context, req routeFenceAcquire, previousRunID string) (routeFenceGrant, error) {
	if !validRunID(previousRunID) || previousRunID == req.RunID {
		return routeFenceGrant{}, fmt.Errorf("%w: take over the merge fence from run_id %q (this run is %q)",
			errRouteInvalidArgument, previousRunID, req.RunID)
	}
	return s.acquireFence(ctx, "take over merge fence", req, previousRunID)
}

// checkTakeOverShard 是接手时对一片「生效且 run_id == previousRunID」分片的核对(纯函数):
//   - zone 对(src_zone / dst_zone)必须与本次相同。不比 purpose:合服中止后改跑撤销是 runbook 认可的续跑路径(zone 对相同、
//     purpose 不同)。zone 对写错时若照样接手,点对的那一区被接走,另一区仍挂在已死的那次运行上、过期后在半截状态下解封。
//   - holder_json 里记着 manifest_path 时,必须与本次的清单路径(filepath.Clean 后)相同:续跑落到别的清单(合服没给 -manifest-path
//     时是带时间戳的新路径)会被当成首跑 —— 收集出漏人的清单,或在拒绝出口释放刚接手的围栏。
//
// 不满足一律 errRouteInvalidArgument:事务回滚,旧围栏原样留在 previousRunID 上。
func checkTakeOverShard(zone uint32, st routeFenceShardState, req routeFenceAcquire) error {
	if st.srcZone != req.Src || st.dstZone != req.Dst {
		return fmt.Errorf("%w: take over zone %d shard %d from run_id=%q: that fence is for zone pair %d → %d, this run is %d → %d "+
			"(wrong -source-zone / -target-zone, or the wrong -take-over-fence run_id)", errRouteInvalidArgument, zone, st.shard, st.runID,
			st.srcZone, st.dstZone, req.Src, req.Dst)
	}
	if st.holderJSON == "" {
		return nil
	}
	var h mergeInProgressValue
	if err := json.Unmarshal([]byte(st.holderJSON), &h); err != nil {
		return fmt.Errorf("%w: take over zone %d shard %d from run_id=%q: holder_json is not readable (%v), so its manifest_path cannot "+
			"be compared with this run's: %s", errRouteInvalidArgument, zone, st.shard, st.runID, err, st.holderJSON)
	}
	if h.ManifestPath != "" && filepath.Clean(h.ManifestPath) != filepath.Clean(req.Holder.ManifestPath) {
		return fmt.Errorf("%w: take over zone %d shard %d from run_id=%q: that run used -manifest-path %q, this run uses %q — re-run "+
			"with -manifest-path %q (a resume on another manifest starts over as a first run)", errRouteInvalidArgument, zone, st.shard,
			st.runID, h.ManifestPath, req.Holder.ManifestPath, h.ManifestPath)
	}
	return nil
}

// acquireFence 是 AcquireFence / TakeOverFence 的事务体。previousRunID 为空 = 不接手任何人(AcquireFence 的原语义)。
// 接手时被接手的分片逐片过 checkTakeOverShard,且每个 zone 接手 0 片或全部 64 片。
func (s *mergeRouteStore) acquireFence(ctx context.Context, op string, req routeFenceAcquire, previousRunID string) (routeFenceGrant, error) {
	zones, err := req.validate()
	if err != nil {
		return routeFenceGrant{}, err
	}
	if err := s.ensureAllFenceShards(ctx, zones); err != nil {
		return routeFenceGrant{}, err
	}
	var grant routeFenceGrant
	var takenOver int
	err = s.runTx(ctx, op, func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		var maxEpoch uint64
		takenOver = 0
		for _, zone := range zones {
			states, err := lockAllFenceShards(ctx, tx, zone)
			if err != nil {
				return err
			}
			zoneTaken := 0
			for _, st := range states {
				if st.active && st.runID != req.RunID {
					if previousRunID == "" || st.runID != previousRunID {
						return fmt.Errorf("%w: zone %d shard %d run_id=%q holder=%s (if run %s is no longer running, re-run the same "+
							"command — same -manifest-path — with -take-over-fence %s; otherwise wait for it to finish or for its "+
							"expires_at_ms)", errRouteFenceHeld, zone, st.shard, st.runID, st.holderJSON, st.runID, st.runID)
					}
					if err := checkTakeOverShard(zone, st, req); err != nil {
						return err
					}
					zoneTaken++
				}
				maxEpoch = max(maxEpoch, st.epoch)
			}
			// 接手按 zone 要么全接、要么一片不接(那次运行的围栏已整体过期 = 普通获取):只接到一部分说明分片状态不一致,不在它上面续跑。
			if previousRunID != "" && zoneTaken != 0 && zoneTaken != int(routeFenceShards) {
				return fmt.Errorf("%w: take over zone %d from run_id=%q: only %d of %d shards are live under that run_id — the shard rows "+
					"are inconsistent; check zone_merge_fence by hand", errRouteInvalidArgument, zone, previousRunID, zoneTaken, routeFenceShards)
			}
			takenOver += zoneTaken
		}
		if maxEpoch == math.MaxUint64 {
			return fmt.Errorf("%w: fence_epoch exhausted on zones %v", errRouteInvariant, zones)
		}
		nowMs, err := readRouteDBNowMs(ctx, tx)
		if err != nil {
			return err
		}
		epoch := maxEpoch + 1
		expires := nowMs + uint64(req.TTL.Milliseconds())
		holder, err := encodeRouteFenceHolder(req, epoch, expires)
		if err != nil {
			return err
		}
		for _, zone := range zones {
			res, err := tx.ExecContext(ctx, routeAcquireFenceSQL, req.RunID, req.Purpose, uint64(req.Src), uint64(req.Dst),
				expires, holder, epoch, uint64(zone), uint64(routeFenceShards))
			if err != nil {
				return fmt.Errorf("acquire merge fence zone %d: %w", zone, err)
			}
			if err := requireRowsAffected(res, int64(routeFenceShards), fmt.Sprintf("acquire merge fence zone %d", zone)); err != nil {
				return err
			}
		}
		grant = routeFenceGrant{Epoch: epoch, ExpiresAtMs: expires, HolderJSON: holder}
		return nil
	})
	if err != nil {
		return routeFenceGrant{}, err
	}
	if previousRunID != "" {
		log.Printf("merge fence taken over in zone_merge_fence from run_id=%s (%d live shards): zones=%v run_id=%s purpose=%s fence_epoch=%d expires_at_ms=%d",
			previousRunID, takenOver, zones, req.RunID, req.Purpose, grant.Epoch, grant.ExpiresAtMs)
		return grant, nil
	}
	log.Printf("merge fence acquired in zone_merge_fence: zones=%v run_id=%s purpose=%s fence_epoch=%d expires_at_ms=%d",
		zones, req.RunID, req.Purpose, grant.Epoch, grant.ExpiresAtMs)
	return grant, nil
}

// RenewFence 续期(W12 续期):一个事务,读库时钟,对每个 zone(升序)`UPDATE … WHERE run_id = ? AND expires_at_ms > 库时钟`
// (全部 64 片)。任一 zone 受影响行数 ≠ 64 → errRouteFenceLost(整笔回滚;这是**确定**失权)。其它错误(1205、网络)原样返回,
// 由调用方退避重试并 WARN(不当失权,规格 §13.3 第 4 条)。成功时返回每个 zone 续期后的过期时刻(给镜像的 PEXPIREAT)。
func (s *mergeRouteStore) RenewFence(ctx context.Context, zones []uint32, runID string, ttl time.Duration) (map[uint32]uint64, error) {
	zs, err := sortedDistinctRouteZones(zones)
	if err != nil {
		return nil, err
	}
	if !validRunID(runID) || ttl < routeFenceMinTTL {
		return nil, fmt.Errorf("%w: renew run_id=%q ttl=%s", errRouteInvalidArgument, runID, ttl)
	}
	var expiries map[uint32]uint64
	err = s.runTx(ctx, "renew merge fence", func(tx *sql.Tx) error {
		nowMs, err := readRouteDBNowMs(ctx, tx)
		if err != nil {
			return err
		}
		want := nowMs + uint64(ttl.Milliseconds())
		var lost []string
		for _, zone := range zs {
			res, err := tx.ExecContext(ctx, routeRenewFenceSQL, want, uint64(zone), uint64(routeFenceShards), runID, nowMs)
			if err != nil {
				return fmt.Errorf("renew merge fence zone %d: %w", zone, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("renew merge fence zone %d rows affected: %w", zone, err)
			}
			if n != int64(routeFenceShards) {
				lost = append(lost, fmt.Sprintf("zone %d renewed %d/%d shards", zone, n, routeFenceShards))
			}
		}
		if len(lost) > 0 {
			return fmt.Errorf("%w: run_id=%s: %s", errRouteFenceLost, runID, strings.Join(lost, ", "))
		}
		out := make(map[uint32]uint64, len(zs))
		for _, zone := range zs {
			var exp sql.Null[uint64]
			if err := tx.QueryRowContext(ctx, routeReadFenceExpirySQL, uint64(zone), uint64(routeFenceShards), runID).Scan(&exp); err != nil {
				return fmt.Errorf("read merge fence expiry zone %d: %w", zone, err)
			}
			if !exp.Valid {
				return fmt.Errorf("%w: zone %d renewed but no expiry read back", errRouteInvariant, zone)
			}
			out[zone] = exp.V
		}
		expiries = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return expiries, nil
}

// ReleaseFence 释放(W12 释放):一个事务,对每个 zone(升序)只清本次运行的分片(fence_epoch 保留)。返回每个 zone 清掉的分片数:
// 64 = 正常释放;0 = 不是本次的(已过期被人接手,或早已释放)—— 调用方打 WARN,不能悄悄过去;其它值说明分片状态不一致,
// 同样交给调用方报告。之后调用方按完整值 compare-and-delete 镜像(deleteFenceMirror)。
func (s *mergeRouteStore) ReleaseFence(ctx context.Context, zones []uint32, runID string) (map[uint32]int64, error) {
	zs, err := sortedDistinctRouteZones(zones)
	if err != nil {
		return nil, err
	}
	if !validRunID(runID) {
		return nil, fmt.Errorf("%w: release run_id=%q", errRouteInvalidArgument, runID)
	}
	var released map[uint32]int64
	err = s.runTx(ctx, "release merge fence", func(tx *sql.Tx) error {
		out := make(map[uint32]int64, len(zs))
		for _, zone := range zs {
			res, err := tx.ExecContext(ctx, routeReleaseFenceSQL, uint64(zone), uint64(routeFenceShards), runID)
			if err != nil {
				return fmt.Errorf("release merge fence zone %d: %w", zone, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("release merge fence zone %d rows affected: %w", zone, err)
			}
			out[zone] = n
		}
		released = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}

// routeFenceState 是事务外读到的一个 zone 的 63 号片(dry-run 预判、排障)。
type routeFenceState struct {
	Present     bool
	RunID       string
	Active      bool
	HolderJSON  string
	Epoch       uint64
	ExpiresAtMs uint64
}

// ReadFence 事务外一致读 zone 的 63 号片(不加锁)。行不在 = Present=false(从未封锁过)。
func (s *mergeRouteStore) ReadFence(ctx context.Context, zone uint32) (routeFenceState, error) {
	if !routeZoneOK(zone) {
		return routeFenceState{}, fmt.Errorf("%w: zone %d", errRouteInvalidArgument, zone)
	}
	var run, holder sql.NullString
	var active sql.Null[int64]
	var epoch uint64
	var expires sql.Null[uint64]
	err := s.db.QueryRowContext(ctx, routeReadFenceSQL, uint64(zone), uint64(routeFenceCheckShard)).Scan(&run, &active, &holder, &epoch, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return routeFenceState{}, nil
	}
	if err != nil {
		return routeFenceState{}, fmt.Errorf("read zone_merge_fence(%d, %d): %w", zone, routeFenceCheckShard, err)
	}
	return routeFenceState{
		Present:     true,
		RunID:       run.String,
		Active:      !active.Valid || active.V != 0,
		HolderJSON:  holder.String,
		Epoch:       epoch,
		ExpiresAtMs: expires.V,
	}, nil
}

// ── 围栏失权判据(fenceGuard)────────────────────────────────────────────────

// routeFenceRenewer 是续期的接缝(生产 *mergeRouteStore;单测注入假实现)。
type routeFenceRenewer interface {
	RenewFence(ctx context.Context, zones []uint32, runID string, ttl time.Duration) (map[uint32]uint64, error)
}

// routeFenceGuard 维护「本次运行仍持有围栏」的本地判据(规格 §6.3 W12 续期、§13.3 第 4 条):
//   - 续期 goroutine 每 min(ttl/3, routeFenceRenewMaxInterval) 续一次;成功把 lastOK 推到这次续期**开始**的本地单调时刻(保守:库里的新过期时刻
//     不早于它 + ttl);
//   - 续期回 errRouteFenceLost(受影响行数不足)= 确定失权:置 lost,goroutine 退出;
//   - 1205 / 网络错误:WARN,routeFenceRetryWait 后重试,不当失权;
//   - Check() 在「已失权」或「本地单调时钟已过 lastOK + ttl − 60s」时返回 errRouteFenceLost。
//
// 每个写步骤(含不碰映射的 player_blob / guild / trade 步骤)每批之前调 Check(),非 nil 即中止、退出非 0;
// 批事务内的 requireFenceHeld 仍是权威判据,本地判据只负责「库连不上时也别再往别的存储写」。
type routeFenceGuard struct {
	renewer   routeFenceRenewer
	zones     []uint32
	runID     string
	ttl       time.Duration
	interval  time.Duration
	retryWait time.Duration
	// now 必须带单调读数(time.Now 的返回值带):截止时间比较走单调时钟,不受墙钟回拨影响。
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	// onRenewed 在每次续期成功后调用(下一波接线:按新的库 expires_at_ms 刷新围栏镜像);返回错误只打 WARN ——
	// 镜像在获取时已按 TTL(≥ 整个运行预算)设过过期,leader 也会按库补镜像。
	onRenewed func(ctx context.Context, expiries map[uint32]uint64) error

	mu     sync.Mutex
	lastOK time.Time
	lost   error

	cancel      context.CancelFunc
	done        chan struct{}
	started     bool
	stopping    bool
	runErr      error
	workContext context.Context
}

// newRouteFenceGuard 装配守卫。acquiredAt 是调用 AcquireFence **之前**取的本地时刻(保守起点)。
func newRouteFenceGuard(renewer routeFenceRenewer, zones []uint32, runID string, ttl time.Duration, acquiredAt time.Time) (*routeFenceGuard, error) {
	zs, err := sortedDistinctRouteZones(zones)
	if err != nil {
		return nil, err
	}
	if ttl <= 2*routeFenceLocalSafety || !validRunID(runID) || renewer == nil {
		return nil, fmt.Errorf("%w: fence guard ttl=%s run_id=%q", errRouteInvalidArgument, ttl, runID)
	}
	return &routeFenceGuard{
		renewer:   renewer,
		zones:     zs,
		runID:     runID,
		ttl:       ttl,
		interval:  min(ttl/3, routeFenceRenewMaxInterval),
		retryWait: routeFenceRetryWait,
		now:       time.Now,
		sleep:     sleepCtx,
		lastOK:    acquiredAt,
	}, nil
}

// routeFenceLocalDeadline 是本地失权截止时刻:最后一次成功续期(或获取)开始的时刻 + ttl − 60s。
func routeFenceLocalDeadline(lastOK time.Time, ttl time.Duration) time.Time {
	return lastOK.Add(ttl - routeFenceLocalSafety)
}

// Check 返回 nil 表示本地判据下仍持有围栏;否则返回包着 errRouteFenceLost 的错误。
func (g *routeFenceGuard) Check() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != nil {
		return g.lost
	}
	deadline := routeFenceLocalDeadline(g.lastOK, g.ttl)
	if !g.now().Before(deadline) {
		return fmt.Errorf("%w: run_id=%s has no successful renewal for %s (local deadline = last renewal + ttl - %s)",
			errRouteFenceLost, g.runID, g.ttl-routeFenceLocalSafety, routeFenceLocalSafety)
	}
	return nil
}

// renewOnce 续期一次:成功推进 lastOK 并调 onRenewed;确定失权置 lost;其它错误原样返回(调用方重试)。
func (g *routeFenceGuard) renewOnce(ctx context.Context) error {
	start := g.now()
	expiries, err := g.renewer.RenewFence(ctx, g.zones, g.runID, g.ttl)
	if err != nil {
		g.keepMaintenanceFailure(ctx, err)
		if errors.Is(err, errRouteFenceLost) {
			g.mu.Lock()
			if g.lost == nil {
				g.lost = err
			}
			g.mu.Unlock()
		}
		return err
	}
	g.mu.Lock()
	if start.After(g.lastOK) {
		g.lastOK = start
	}
	g.mu.Unlock()
	if g.onRenewed != nil {
		if herr := g.onRenewed(ctx, expiries); herr != nil {
			g.keepMaintenanceFailure(ctx, herr)
			log.Printf("WARN: merge fence renewed in zone_merge_fence but the mirror refresh failed (the reconcile leader re-syncs it): %v", herr)
		}
	}
	return nil
}

// Start 起续期 goroutine(只能调一次);Stop 取消并等它退出。
func (g *routeFenceGuard) Start(ctx context.Context) {
	g.mu.Lock()
	if g.started || g.stopping {
		g.mu.Unlock()
		holdMaintenanceTool(ctx)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	g.done = make(chan struct{})
	g.started = true
	g.workContext = ctx
	done := g.done
	g.mu.Unlock()
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		g.keepMaintenanceFailure(ctx, err)
		cancel()
		close(done)
		return
	}
	if err := registerMaintenanceStop(ctx, func(stopCtx context.Context) error {
		err := g.StopAndReport(stopCtx)
		select {
		case <-done:
			// 历史写错误已经永久 HOLD；本回调只证明本机 worker 已 join。
			return nil
		default:
			return err
		}
	}); err != nil {
		g.keepMaintenanceFailure(ctx, err)
		cancel()
		finish()
		close(done)
		return
	}
	go func() {
		returned := false
		defer close(done)
		defer finish()
		defer func() {
			if !returned {
				holdMaintenanceTool(ctx)
			}
		}()
		g.loop(ctx)
		returned = true
	}()
}

// Stop 取消续期并等 goroutine 退出;没 Start 过时是空操作。
func (g *routeFenceGuard) Stop() {
	_ = g.StopAndReport(context.Background())
}

// StopAndReport 区分真实 worker 退出与 Redis/SQL 未知；取消本身不产生成功原件。
func (g *routeFenceGuard) StopAndReport(ctx context.Context) error {
	g.mu.Lock()
	g.stopping = true
	cancel, done, runErr := g.cancel, g.done, g.runErr
	g.mu.Unlock()
	if cancel == nil {
		return runErr
	}
	cancel()
	select {
	case <-done:
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.runErr
	case <-ctx.Done():
		g.keepMaintenanceFailure(ctx, ctx.Err())
		return errors.Join(runErr, ctx.Err())
	}
}

func (g *routeFenceGuard) keepMaintenanceFailure(ctx context.Context, err error) {
	if err == nil {
		return
	}
	g.mu.Lock()
	if g.runErr == nil {
		g.runErr = err
	}
	workCtx := g.workContext
	g.mu.Unlock()
	if workCtx == nil {
		workCtx = ctx
	}
	holdMaintenanceTool(workCtx)
}

func (g *routeFenceGuard) loop(ctx context.Context) {
	wait := g.interval
	for {
		if err := g.sleep(ctx, wait); err != nil {
			return
		}
		err := g.renewOnce(ctx)
		switch {
		case err == nil:
			wait = g.interval
		case errors.Is(err, errRouteFenceLost):
			log.Printf("ERROR: merge fence renewal: %v — every further write step must stop", err)
			return
		case ctx.Err() != nil:
			return
		default:
			log.Printf("WARN: merge fence renewal failed, retrying in %s (the run stops writing at its local deadline if this keeps failing): %v",
				g.retryWait, err)
			wait = g.retryWait
		}
	}
}

// ── merged_into(W6 / 5'a)───────────────────────────────────────────────────

// encodeRouteMergedIntoValue 编码合走标记的镜像值,与 merged_into.go / shared/placement.EncodeMergedIntoValue 逐字节相同。
func encodeRouteMergedIntoValue(dst uint32, runID string) (string, error) {
	b, err := json.Marshal(mergedIntoValue{Dst: dst, RunID: runID})
	if err != nil {
		return "", fmt.Errorf("encode merged_into: %w", err)
	}
	return string(b), nil
}

// MarkMergedInto 写合走标记(W6,规格 §6.3):围栏属于本次(src、dst 两个 zone,fenceRunID)→ 加锁读 zone_merged_into(src):
// 无行插入;同 dst 把 run_id 改成 markRunID(清单 run_id);异 dst → errRouteMergedIntoConflict。返回镜像值(调用方 SET + WAITAOF)。
// 必须先于 W5(现有顺序)。
func (s *mergeRouteStore) MarkMergedInto(ctx context.Context, src, dst uint32, fenceRunID, markRunID string) (string, error) {
	zones, err := routeZonePair(src, dst)
	if err != nil {
		return "", err
	}
	if !validRunID(fenceRunID) || !validRunID(markRunID) {
		return "", fmt.Errorf("%w: run ids %q / %q", errRouteInvalidArgument, fenceRunID, markRunID)
	}
	value, err := encodeRouteMergedIntoValue(dst, markRunID)
	if err != nil {
		return "", err
	}
	if err := s.ensureFenceCheckShard(ctx, zones); err != nil {
		return "", err
	}
	err = s.runTx(ctx, "mark merged_into", func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		if err := requireFenceHeld(ctx, tx, zones, fenceRunID); err != nil {
			return err
		}
		curDst, curRun, found, err := readRouteMergedInto(ctx, tx, src, true)
		if err != nil {
			return err
		}
		switch {
		case !found:
			res, err := tx.ExecContext(ctx, routeInsertMergedIntoSQL, uint64(src), uint64(dst), markRunID)
			if err != nil {
				return fmt.Errorf("insert zone_merged_into(%d): %w", src, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("insert zone_merged_into(%d) rows affected: %w", src, err)
			}
			if n != 1 {
				return fmt.Errorf("%w: zone_merged_into(%d)", errRouteRowRaced, src)
			}
		case curDst != dst:
			return fmt.Errorf("%w: zone_merged_into(%d) = dst %d run_id %s, refusing to record dst %d",
				errRouteMergedIntoConflict, src, curDst, curRun, dst)
		case curRun != markRunID:
			res, err := tx.ExecContext(ctx, routeUpdateMergedIntoRunSQL, markRunID, uint64(src), uint64(dst))
			if err != nil {
				return fmt.Errorf("update zone_merged_into(%d): %w", src, err)
			}
			if err := requireRowsAffected(res, 1, fmt.Sprintf("update zone_merged_into(%d)", src)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return value, nil
}

// ClearMergedInto 删合走标记(撤销 5'a):围栏属于本次撤销(fenceRunID)→ DELETE … WHERE src_zone = ? AND dst_zone = ? AND run_id = ?
// (markRunID = 当初合服的清单 run_id)。返回是否删了一行;没删 = 标记本来不在,或是另一次合服写的(调用方 WARN、留给人判断)。
// 之后调用方按完整值 compare-and-delete 镜像。
func (s *mergeRouteStore) ClearMergedInto(ctx context.Context, src, dst uint32, fenceRunID, markRunID string) (bool, error) {
	zones, err := routeZonePair(src, dst)
	if err != nil {
		return false, err
	}
	if !validRunID(fenceRunID) || !validRunID(markRunID) {
		return false, fmt.Errorf("%w: run ids %q / %q", errRouteInvalidArgument, fenceRunID, markRunID)
	}
	if err := s.ensureFenceCheckShard(ctx, zones); err != nil {
		return false, err
	}
	var deleted bool
	err = s.runTx(ctx, "clear merged_into", func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		if err := requireFenceHeld(ctx, tx, zones, fenceRunID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, routeDeleteMergedIntoSQL, uint64(src), uint64(dst), markRunID)
		if err != nil {
			return fmt.Errorf("delete zone_merged_into(%d): %w", src, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete zone_merged_into(%d) rows affected: %w", src, err)
		}
		deleted = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// ReadMergedInto 一致读 zone_merged_into(src)(不加锁):合服前置核对在 -route-source=db 时读库(下一波接线)。
func (s *mergeRouteStore) ReadMergedInto(ctx context.Context, src uint32) (dst uint32, runID string, found bool, err error) {
	if !routeZoneOK(src) {
		return 0, "", false, fmt.Errorf("%w: zone %d", errRouteInvalidArgument, src)
	}
	return readRouteMergedInto(ctx, s.db, src, false)
}

func readRouteMergedInto(ctx context.Context, q routeQueryer, src uint32, forUpdate bool) (uint32, string, bool, error) {
	query := routeReadMergedIntoSQL
	if forUpdate {
		query = routeLockMergedIntoSQL
	}
	var dst uint32
	var run string
	err := q.QueryRowContext(ctx, query, uint64(src)).Scan(&dst, &run)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("read zone_merged_into(%d): %w", src, err)
	}
	return dst, run, true, nil
}

// requireNotMergedAway 是 W9 / W10 的前置(事务内,围栏之后):库里记着 zone 已被合走就拒绝(Redis 丢了也拦得住)。
func requireNotMergedAway(ctx context.Context, tx *sql.Tx, zone uint32) error {
	dst, run, found, err := readRouteMergedInto(ctx, tx, zone, true)
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("%w: zone %d was merged into zone %d by run %s; restore lost mappings by replaying that merge's manifest, not by backfill / pin",
			errRouteZoneMergedAway, zone, dst, run)
	}
	return nil
}

// ── 批原语:改 home(W5 / W8)──────────────────────────────────────────────

// classifyRouteHome 是 classifyMappingValue 的库版:没有行或 home 为 NULL = 映射缺失;== from 要改;== to 已是目标;其它 = 第三个 zone。
func classifyRouteHome(row mergeRouteRow, present bool, from, to uint32) mappingOutcome {
	switch {
	case !present || row.HomeZone == 0:
		return mappingMissing
	case row.HomeZone == from:
		return mappingChanged
	case row.HomeZone == to:
		return mappingAlready
	default:
		return mappingForeign
	}
}

// RemapHomeBatch 合服改 home(W5,规格 §6.3):src、dst 围栏属于本次(runID)→ 玩家行按聚簇键 FOR UPDATE →
// 按 classifyMappingValue 同口径分类 → home == src 的逐行 CAS 改成 dst、rev + 1 → COMMIT。
// 缺失 / 第三个 zone 的行不动,判定交给调用方(与 Redis 版 remapPlayerMapping 的异常口径一致)。
// 返回的 Target(本批改的 + 早已是 dst 的)全部发布并确认之后,步骤 5 才能标记完成。
func (s *mergeRouteStore) RemapHomeBatch(ctx context.Context, src, dst uint32, runID string, ids []uint64) (routeBatchResult[mappingOutcome], error) {
	return s.moveHomeBatch(ctx, "remap home_zone (merge step 5)", src, dst, runID, ids)
}

// RestoreHomeBatch 撤销合服改回 home(W8 5',规格 §6.3):两区围栏属于本次撤销(runID)→ home == dst 的 CAS 改回 src
// (原来是无条件 SET,现在是 CAS)。其余同 RemapHomeBatch。
func (s *mergeRouteStore) RestoreHomeBatch(ctx context.Context, src, dst uint32, runID string, ids []uint64) (routeBatchResult[mappingOutcome], error) {
	return s.moveHomeBatch(ctx, "restore home_zone (unmerge step 5')", dst, src, runID, ids)
}

func (s *mergeRouteStore) moveHomeBatch(ctx context.Context, op string, from, to uint32, runID string, ids []uint64) (routeBatchResult[mappingOutcome], error) {
	zones, err := routeZonePair(from, to)
	if err != nil {
		return routeBatchResult[mappingOutcome]{}, err
	}
	if !validRunID(runID) {
		return routeBatchResult[mappingOutcome]{}, fmt.Errorf("%w: run_id %q", errRouteInvalidArgument, runID)
	}
	keys, err := prepareRouteBatch(ids, routeWriteBatchLimit)
	if err != nil || len(keys) == 0 {
		return newRouteBatchResult[mappingOutcome](0), err
	}
	if err := s.ensureFenceCheckShard(ctx, zones); err != nil {
		return routeBatchResult[mappingOutcome]{}, err
	}
	var result routeBatchResult[mappingOutcome]
	err = s.runTx(ctx, op, func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		if err := requireFenceHeld(ctx, tx, zones, runID); err != nil {
			return err
		}
		rows, err := loadRouteRows(ctx, tx, keys, true)
		if err != nil {
			return err
		}
		out := newRouteBatchResult[mappingOutcome](len(keys))
		for _, id := range keys {
			row, ok := rows[id]
			verdict := classifyRouteHome(row, ok, from, to)
			out.Verdicts[id] = verdict
			switch verdict {
			case mappingChanged:
				res, err := tx.ExecContext(ctx, routeMoveHomeSQL, uint64(to), s.writer, routeShardArg(id), id, row.Rev, uint64(from))
				if err != nil {
					return fmt.Errorf("%s: player %d: %w", op, id, err)
				}
				if err := requireRowsAffected(res, 1, fmt.Sprintf("%s: player %d", op, id)); err != nil {
					return err
				}
				row.HomeZone, row.Rev = to, row.Rev+1
				out.Changed++
				out.Target = append(out.Target, row)
			case mappingAlready:
				out.Target = append(out.Target, row)
			}
		}
		out.sortTarget()
		result = out
		return nil
	})
	if err != nil {
		return routeBatchResult[mappingOutcome]{}, err
	}
	return result, nil
}

// ── 批原语:钉落点(W7 / W10)────────────────────────────────────────────

// PinPlacementBatch 合服 pin 模式钉落点(W7,规格 §6.3):src、dst 围栏属于本次 → 行锁 → decidePin(按库值编码,与 Redis 版
// 同一判定)→ 该钉的 `UPDATE storage_id = src, placement_version = 1 … WHERE storage_id IS NULL AND home_zone = src` → COMMIT。
// Target = 本批钉的 + 早已是 "{src}:1" 的 + 已有别的记录的(都已有落点:改 home 不再牵动有效落点);
// HomeMoved(无记录且 home 不是 src)不在 Target,由调用方按异常处置。已有记录是否等于清单记下的原值也由调用方判。
func (s *mergeRouteStore) PinPlacementBatch(ctx context.Context, src, dst uint32, runID string, ids []uint64) (routeBatchResult[pinVerdict], error) {
	zones, err := routeZonePair(src, dst)
	if err != nil {
		return routeBatchResult[pinVerdict]{}, err
	}
	if !validRunID(runID) {
		return routeBatchResult[pinVerdict]{}, fmt.Errorf("%w: run_id %q", errRouteInvalidArgument, runID)
	}
	keys, err := prepareRouteBatch(ids, routeWriteBatchLimit)
	if err != nil || len(keys) == 0 {
		return newRouteBatchResult[pinVerdict](0), err
	}
	if err := s.ensureFenceCheckShard(ctx, zones); err != nil {
		return routeBatchResult[pinVerdict]{}, err
	}
	var result routeBatchResult[pinVerdict]
	err = s.runTx(ctx, "pin placement (merge pin step)", func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		if err := requireFenceHeld(ctx, tx, zones, runID); err != nil {
			return err
		}
		rows, err := loadRouteRows(ctx, tx, keys, true)
		if err != nil {
			return err
		}
		out := newRouteBatchResult[pinVerdict](len(keys))
		for _, id := range keys {
			row, ok := rows[id]
			recRaw, err := row.placementValue()
			if err != nil {
				return err
			}
			verdict := decidePin(recRaw, row.homeValue(), src)
			if !ok {
				verdict = pinVerdictHomeMoved
			}
			out.Verdicts[id] = verdict
			switch verdict {
			case pinVerdictPin:
				pinned, err := pinRouteRow(ctx, tx, s.writer, row, src)
				if err != nil {
					return err
				}
				out.Changed++
				out.Target = append(out.Target, pinned)
			case pinVerdictAlreadyPinned, pinVerdictKept:
				out.Target = append(out.Target, row)
			}
		}
		out.sortTarget()
		result = out
		return nil
	})
	if err != nil {
		return routeBatchResult[pinVerdict]{}, err
	}
	return result, nil
}

// pinRouteRow 把一行钉成 "{zone}:1"(行已被锁住;按读到的 rev CAS,且要求仍无落点、home 仍是 zone)。返回提交后的行。
func pinRouteRow(ctx context.Context, tx *sql.Tx, writer string, row mergeRouteRow, zone uint32) (mergeRouteRow, error) {
	res, err := tx.ExecContext(ctx, routePinSQL, uint64(zone), writer, routeShardArg(row.PlayerID), row.PlayerID, row.Rev, uint64(zone))
	if err != nil {
		return mergeRouteRow{}, fmt.Errorf("pin placement player %d: %w", row.PlayerID, err)
	}
	if err := requireRowsAffected(res, 1, fmt.Sprintf("pin placement player %d", row.PlayerID)); err != nil {
		return mergeRouteRow{}, err
	}
	pinned := row.withPlacement(placementRecord{StorageID: zone, Version: 1})
	pinned.Rev++
	return pinned, nil
}

// PinZonePlacementBatch 是 -mode pin-placement 的一批(W10,规格 §6.3 / §13.3 第 5 条):zone 的 63 号片围栏未生效 →
// zone_merged_into 没有该 zone → `SELECT … WHERE home_zone = ? AND storage_id IS NULL AND <键集游标> … LIMIT ? FOR UPDATE`
// → 逐行钉 "{zone}:1" → COMMIT。返回本批结果(Target 全是本批钉的)、下一批的游标,以及是否已翻完(本批不足 limit 行)。
func (s *mergeRouteStore) PinZonePlacementBatch(ctx context.Context, zone uint32, after routeScanCursor, limit int) (routeBatchResult[pinVerdict], routeScanCursor, bool, error) {
	if !routeZoneOK(zone) || limit < 1 || limit > routePinZoneBatchLimit || after.Shard >= routeRowShards {
		return routeBatchResult[pinVerdict]{}, after, false, fmt.Errorf("%w: pin zone %d limit=%d after=%+v", errRouteInvalidArgument, zone, limit, after)
	}
	if err := s.ensureFenceCheckShard(ctx, []uint32{zone}); err != nil {
		return routeBatchResult[pinVerdict]{}, after, false, err
	}
	var result routeBatchResult[pinVerdict]
	next := after
	var done bool
	err := s.runTx(ctx, "pin placement (-mode pin-placement)", func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		if err := requireFenceInactive(ctx, tx, zone); err != nil {
			return err
		}
		if err := requireNotMergedAway(ctx, tx, zone); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, routePinZoneSelectSQL, uint64(zone), uint64(after.Shard), uint64(after.Shard), after.PlayerID, limit)
		if err != nil {
			return fmt.Errorf("select unpinned players of zone %d: %w", zone, err)
		}
		page, err := collectRouteRowsAfter(rows, "home_zone (unpinned)", uint64(zone), after, limit,
			func(r mergeRouteRow) bool { return r.HomeZone == zone && r.StorageID == 0 })
		_ = rows.Close()
		if err != nil {
			return err
		}
		out := newRouteBatchResult[pinVerdict](len(page))
		cursor := after
		for _, row := range page {
			pinned, err := pinRouteRow(ctx, tx, s.writer, row, zone)
			if err != nil {
				return err
			}
			out.Verdicts[row.PlayerID] = pinVerdictPin
			out.Changed++
			out.Target = append(out.Target, pinned)
			cursor = routeCursorOf(row.PlayerID)
		}
		out.sortTarget()
		result, next, done = out, cursor, len(page) < limit
		return nil
	})
	if err != nil {
		return routeBatchResult[pinVerdict]{}, after, false, err
	}
	return result, next, done, nil
}

// ── 批原语:回填 home(W9)───────────────────────────────────────────────

// routeBackfillVerdict 是回填对一名玩家的判定。
type routeBackfillVerdict int

const (
	// routeBackfillInserted 库里没有这一行:插入只有 home 的新行(rev = 1)。
	routeBackfillInserted routeBackfillVerdict = iota + 1
	// routeBackfillFilled 行在、home 为 NULL(只有落点):补上 home。
	routeBackfillFilled
	// routeBackfillAlready home 已是这个 zone。
	routeBackfillAlready
	// routeBackfillConflict home 是另一个 zone:不动,交给人(与 Redis 版「SET NX 绝不覆盖」同口径)。
	routeBackfillConflict
)

// BackfillHomeBatch 给从未登记过的存量号补 home(W9,规格 §6.3):zone 的 63 号片围栏未生效(新增)→ zone_merged_into
// 有该 zone 作 src 就拒绝(库里的标记,Redis 丢了也拦得住)→ 行锁 → 无行批量 ODKU 插入 / home 为 NULL 补 home → COMMIT。
// Target = 插入的 + 补上的 + 早已是该 zone 的。
func (s *mergeRouteStore) BackfillHomeBatch(ctx context.Context, zone uint32, ids []uint64) (routeBatchResult[routeBackfillVerdict], error) {
	if !routeZoneOK(zone) {
		return routeBatchResult[routeBackfillVerdict]{}, fmt.Errorf("%w: backfill zone %d", errRouteInvalidArgument, zone)
	}
	keys, err := prepareRouteBatch(ids, routeWriteBatchLimit)
	if err != nil || len(keys) == 0 {
		return newRouteBatchResult[routeBackfillVerdict](0), err
	}
	if err := s.ensureFenceCheckShard(ctx, []uint32{zone}); err != nil {
		return routeBatchResult[routeBackfillVerdict]{}, err
	}
	var result routeBatchResult[routeBackfillVerdict]
	err = s.runTx(ctx, "backfill home_zone", func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		if err := requireFenceInactive(ctx, tx, zone); err != nil {
			return err
		}
		if err := requireNotMergedAway(ctx, tx, zone); err != nil {
			return err
		}
		rows, err := loadRouteRows(ctx, tx, keys, true)
		if err != nil {
			return err
		}
		out := newRouteBatchResult[routeBackfillVerdict](len(keys))
		var inserts []uint64
		for _, id := range keys {
			row, ok := rows[id]
			switch {
			case !ok:
				inserts = append(inserts, id)
				out.Verdicts[id] = routeBackfillInserted
			case row.HomeZone == 0:
				res, err := tx.ExecContext(ctx, routeFillHomeSQL, uint64(zone), s.writer, routeShardArg(id), id, row.Rev)
				if err != nil {
					return fmt.Errorf("backfill home_zone player %d: %w", id, err)
				}
				if err := requireRowsAffected(res, 1, fmt.Sprintf("backfill home_zone player %d", id)); err != nil {
					return err
				}
				row.HomeZone, row.Rev = zone, row.Rev+1
				out.Verdicts[id] = routeBackfillFilled
				out.Changed++
				out.Target = append(out.Target, row)
			case row.HomeZone == zone:
				out.Verdicts[id] = routeBackfillAlready
				out.Target = append(out.Target, row)
			default:
				out.Verdicts[id] = routeBackfillConflict
			}
		}
		if len(inserts) > 0 {
			args := make([]any, 0, 4*len(inserts))
			for _, id := range inserts {
				args = append(args, routeShardArg(id), id, uint64(zone), s.writer)
			}
			res, err := tx.ExecContext(ctx, routeInsertHomeSQL(len(inserts)), args...)
			if err != nil {
				return fmt.Errorf("backfill insert %d players: %w", len(inserts), err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("backfill insert rows affected: %w", err)
			}
			if n != int64(len(inserts)) {
				return fmt.Errorf("%w: inserted %d of %d", errRouteRowRaced, n, len(inserts))
			}
			for _, id := range inserts {
				out.Changed++
				out.Target = append(out.Target, mergeRouteRow{PlayerID: id, HomeZone: zone, Rev: 1})
			}
		}
		out.sortTarget()
		result = out
		return nil
	})
	if err != nil {
		return routeBatchResult[routeBackfillVerdict]{}, err
	}
	return result, nil
}

// ── 批原语:搬库 CAS(W11)───────────────────────────────────────────────

// routeCASResult 是一次搬库 CAS 的结局:placementCASResult(与 Redis 版同一组结局码,CurrentRecord / CurrentHome 是执行时的
// 库值编码)加上行。
type routeCASResult struct {
	placementCASResult
	// Row 是提交后的行(Written)或执行时的库行(其余结局);RowPresent=false 表示库里没有这一行。
	Row        mergeRouteRow
	RowPresent bool
}

// RelocateCAS 是 relocatePlacements.cas 的库版(W11,规格 §6.3):一个事务 —— 普通读 writable → 各 op 的 FenceZone(去重、升序)
// 锁 63 号片读是否生效 → 玩家行按聚簇键 FOR UPDATE → 逐 op 按 placementCASScript 同一顺序判:记录(编码后的当前值 vs
// ExpectAbsent / ExpectRecord)→ home(ExpectHome 非空时)→ 围栏(生效回 Fenced)→ 写落点三列、rev + 1 → COMMIT。
//
// 新增断言(P-2):稳定态的新 version 不得小于当前 version,违反即整批回滚、返回错误(调用方的状态机有错,不是业务结局)。
// 库里没有这一行而判定要写(ExpectAbsent 且不核对 home)同样整批报错:搬库对象必须是已登记的玩家。
// 结果与 ops 同序;出错时一条都没提交、返回 nil 结果。Written 的行要发布并确认之后,调用方才能回 Written(下一波)。
func (s *mergeRouteStore) RelocateCAS(ctx context.Context, ops []placementCAS) ([]routeCASResult, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	ids := make([]uint64, len(ops))
	fenceSet := map[uint32]bool{}
	newRecs := make([]placementRecord, len(ops))
	for i, op := range ops {
		if err := op.validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", errRouteInvalidArgument, err)
		}
		rec, err := parsePlacement(op.NewValue)
		if err != nil {
			return nil, fmt.Errorf("%w: player %d: %w", errRouteInvalidArgument, op.PlayerID, err)
		}
		newRecs[i] = rec
		ids[i] = op.PlayerID
		if op.FenceZone != 0 {
			if !routeZoneOK(op.FenceZone) {
				return nil, fmt.Errorf("%w: player %d fence zone %d", errRouteInvalidArgument, op.PlayerID, op.FenceZone)
			}
			fenceSet[op.FenceZone] = true
		}
	}
	keys, err := prepareRouteBatch(ids, routeWriteBatchLimit)
	if err != nil {
		return nil, err
	}
	fenceZones := sortedZones(fenceSet)
	if err := s.ensureFenceCheckShard(ctx, fenceZones); err != nil {
		return nil, err
	}
	var results []routeCASResult
	err = s.runTx(ctx, "relocate placement CAS", func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		fenced := make(map[uint32]bool, len(fenceZones))
		for _, zone := range fenceZones {
			c, err := lockFenceCheckShard(ctx, tx, zone)
			if err != nil {
				return err
			}
			fenced[zone] = c.active
		}
		rows, err := loadRouteRows(ctx, tx, keys, true)
		if err != nil {
			return err
		}
		out := make([]routeCASResult, len(ops))
		for i, op := range ops {
			row, ok := rows[op.PlayerID]
			r, err := relocateCASOne(ctx, tx, s.writer, op, newRecs[i], row, ok, fenced)
			if err != nil {
				return err
			}
			out[i] = r
		}
		results = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// relocateCASOne 判定并(需要时)写一个 op。判定顺序与 placementCASScript 相同:记录 → home → 围栏。
func relocateCASOne(ctx context.Context, tx *sql.Tx, writer string, op placementCAS, newRec placementRecord,
	row mergeRouteRow, present bool, fenced map[uint32]bool) (routeCASResult, error) {
	cur, curPresent, err := row.placement()
	if err != nil {
		return routeCASResult{}, err
	}
	curRaw := ""
	if curPresent {
		curRaw = cur.Encode()
	}
	curHome := row.homeValue()
	res := routeCASResult{Row: row, RowPresent: present}
	res.CurrentRecord, res.CurrentHome = curRaw, curHome
	switch {
	case op.ExpectAbsent && curPresent, !op.ExpectAbsent && curRaw != op.ExpectRecord:
		res.Outcome = placementCASRecordChanged
		return res, nil
	case op.ExpectHome != "" && curHome != op.ExpectHome:
		res.Outcome = placementCASHomeChanged
		return res, nil
	case op.FenceZone != 0 && fenced[op.FenceZone]:
		res.Outcome = placementCASFenced
		return res, nil
	}
	if !present {
		return routeCASResult{}, fmt.Errorf("%w: player %d has no player_route row to write placement %q into", errRouteInvariant, op.PlayerID, op.NewValue)
	}
	if !newRec.Frozen && curPresent && newRec.Version < cur.Version {
		return routeCASResult{}, fmt.Errorf("%w: player %d: stable placement %q would lower the version below the current %q (P-2)",
			errRouteInvariant, op.PlayerID, op.NewValue, curRaw)
	}
	var frozen any
	if newRec.Frozen {
		frozen = newRec.RunID
	}
	sqlRes, err := tx.ExecContext(ctx, routeSetPlacementSQL, uint64(newRec.StorageID), newRec.Version, frozen, writer,
		routeShardArg(op.PlayerID), op.PlayerID, row.Rev)
	if err != nil {
		return routeCASResult{}, fmt.Errorf("relocate CAS player %d: %w", op.PlayerID, err)
	}
	if err := requireRowsAffected(sqlRes, 1, fmt.Sprintf("relocate CAS player %d", op.PlayerID)); err != nil {
		return routeCASResult{}, err
	}
	written := row.withPlacement(newRec)
	written.Rev++
	res.Outcome, res.Row = placementCASWritten, written
	return res, nil
}

// ── 开写前的统一入口 ──────────────────────────────────────────────────────────

// routeWatermarkReader 是读 mapping:route:watermark 的接缝(生产 *routePublisher)。
type routeWatermarkReader interface {
	readWatermark(ctx context.Context) (raw string, present bool, err error)
}

// openRouteBackend 是各模式开写前的统一入口(下一波接线时调用):
//   - -route-source=redis:不开库;mapping Redis 上有水位键就拒绝(这个 Redis 已归 db_first 管);
//   - -route-source=db:开库跑探针,再要求水位键存在、可解码、世代等于该库 route_meta.generation(规格 §13.3 第 3 条)。
//
// 返回的 store 在 redis 模式下为 nil。
func openRouteBackend(ctx context.Context, source, rawDSN, writer string, wm routeWatermarkReader) (*mergeRouteStore, error) {
	switch source {
	case routeSourceRedis:
		raw, present, err := wm.readWatermark(ctx)
		if err != nil {
			return nil, err
		}
		return nil, decideRoutePairing(source, raw, present, "")
	case routeSourceDB:
		st, meta, err := openMergeRouteStore(ctx, rawDSN, writer)
		if err != nil {
			return nil, err
		}
		raw, present, err := wm.readWatermark(ctx)
		if err == nil {
			err = decideRoutePairing(source, raw, present, meta.Generation)
		}
		if err != nil {
			return nil, errors.Join(err, st.Close())
		}
		return st, nil
	default:
		return nil, fmt.Errorf("%w: -route-source %q", errRouteInvalidArgument, source)
	}
}
