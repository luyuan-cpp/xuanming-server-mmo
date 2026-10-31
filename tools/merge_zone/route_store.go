package main

// 合服工具在全局库上的路由表读写层。
//
// 为什么在这里自带一份:merge_zone 是独立 module,import 不了 go/data_service/internal/store;走 data_service 的管理 RPC 要新增 proto;
// 把 store 挪出 internal 会把 go-zero / Kafka 拖进运维工具。表结构归 data_service 的迁移;Redis 一侧的脚本与常量由
// route_mirror_test.go 与 go/shared/placement 逐字节比对。
//
// 本工具对路由表的全部写只有 zone_merged_into 一张表,且都在本文件(route_store_test.go 的
// TestNoRouteTableWritesOutsideMergeRouteStore 用 AST 守住);player_route / route_meta / route_reconcile 一律不写,player_route 只有
// 按 home_zone 键集翻页这一条只读查询(门禁与统计用)。事务里绝不做 Redis 读写:镜像在提交之后,由 route_publish.go 完成。
//
// 不变量(与 data_service store/route_store.go 文件头同口径):
//  1. 判定只认加锁读(FOR UPDATE)。新行一律 ODKU no-op、撞键 = 受影响 0 行,不用 INSERT IGNORE。
//  2. 锁序:route_meta → zone_merged_into(zone 升序)。
//  3. 会话级 READ-COMMITTED + innodb_lock_wait_timeout=1(routeDBDSN 钉住,开写前探针读回)。
//  4. 1213 / 9007 有界重试(routeTxAttempts 次,抖动退避),1205 不重试(已经等满封顶的 1s,再等结论不变)。
//  5. 行片 = player_id % 64(routeRowShards,镜像 shared/placement.RouteRowShards,route_mirror_test.go 钉住)。
//
// 前提(与 data_service 同口径):连接不带 CLIENT_FOUND_ROWS(受影响行数 = 实际改变的行数,routeDBDSN 拒绝它)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
)

// ── 常量 ──────────────────────────────────────────────────────────────────────

const (
	// routeRowShards 是 player_route 的行片模数(镜像 shared/placement.RouteRowShards;建表后不可改)。
	routeRowShards uint32 = 64
	// routeHomeZoneMax 是 zone id 的上限(= schema.go chk_route_home 的 999999,zone 库编号同一空间)。
	routeHomeZoneMax uint32 = 999999
	// routeReadBatchLimit 是键集翻页一页的上限(= data_service store.RouteBatchLimit)。
	routeReadBatchLimit = 1000
	// routeTxAttempts 事务尝试上限:1 次 + 1213 / 9007 重试 3 次。
	routeTxAttempts = 4
	// routeLockWaitTimeoutSec 是会话级 innodb_lock_wait_timeout,由 routeDBDSN 钉死、探针读回。
	routeLockWaitTimeoutSec = 1
	// routeLockRetryBackoffMin / Max 是 1213 / 9007 重来之前的抖动等待(可被 ctx 取消)。
	routeLockRetryBackoffMin = 10 * time.Millisecond
	routeLockRetryBackoffMax = 50 * time.Millisecond
	// routeProbeTimeout 是开写前探针的总时限。
	routeProbeTimeout = 5 * time.Second
)

// DSN 里钉住的会话变量(另外两项 transaction_isolation / tx_isolation 与 merge_run.go 的 readCommittedDSN 共用常量)。
const (
	routeLockWaitParam = "innodb_lock_wait_timeout"
	routeSQLModeParam  = "sql_mode"
	routeSQLModeStrict = "'STRICT_TRANS_TABLES'"
)

// ── 错误 ──────────────────────────────────────────────────────────────────────

var (
	// errRouteInvalidArgument 入参在库层就被拒(zone 越界、run_id 不合法等),一条语句都不发。
	errRouteInvalidArgument = errors.New("route store: invalid argument")
	// errRouteMetaMissing route_meta 没有 id=1 那一行(迁移没跑过)。
	errRouteMetaMissing = errors.New("route_meta: row id=1 missing; run `data_service -f <yaml> -migrate` against the database of -route-db-dsn")
	// errRouteSessionMisconfigured 探针读回的隔离级别 / 锁等待不符合要求。
	errRouteSessionMisconfigured = errors.New("route store: session isolation / lock wait not as required")
	// errRouteShardMismatch 库里一行存的 route_shard 不等于 player_id % 64(只有 CHECK 曾经不生效时才写得进来)。
	errRouteShardMismatch = errors.New("player_route: stored route_shard does not match player_id % 64")
	// errRouteMergedIntoConflict zone_merged_into 已记着这个源区合进了另一个目标区,或目标区自己已被合走。
	errRouteMergedIntoConflict = errors.New("zone_merged_into conflicts with this merge")
	// errRouteInvariant 锁语义被破坏:fail-closed,整批回滚。
	errRouteInvariant = errors.New("route store: invariant violated")
	// errRouteRowRaced 内部哨兵:插入的受影响行数不对(加锁读之后有并发写者插了同一行),整事务回滚重来。
	errRouteRowRaced = errors.New("zone_merged_into: a concurrent writer inserted the row after the locking read")
)

// ── SQL ───────────────────────────────────────────────────────────────────────

// routeDBNowMsSQL 是库时钟的 epoch 毫秒(与 data_service dbNowMsSQL 同一表达式)。
const routeDBNowMsSQL = "CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED)"

// 本文件的全部 SQL。抽成常量是为了让假驱动用例 / 真库用例执行**同一段文本**。
const (
	// 引擎与当前读按档位由 current_tidb.go 统一校验(requireCurrentSession)。
	routeProbeSessionSQL = "SELECT @@SESSION.transaction_isolation, @@SESSION.innodb_lock_wait_timeout"
	// route_meta 整行(库世代与可写开关):探针与「库与 Redis 成对」核对用。
	routeReadMetaSQL = "SELECT generation, writable FROM route_meta WHERE id = 1"

	// merged_into:加锁读 / 一致读 / 插入(ODKU no-op)/ 同 dst 改 run_id / 按 (src, dst, run_id) 删。
	routeLockMergedIntoSQL   = "SELECT dst_zone, run_id FROM zone_merged_into WHERE src_zone = ? FOR UPDATE"
	routeReadMergedIntoSQL   = "SELECT dst_zone, run_id FROM zone_merged_into WHERE src_zone = ?"
	routeInsertMergedIntoSQL = "INSERT INTO zone_merged_into (src_zone, dst_zone, run_id, merged_at_ms)" +
		" VALUES (?, ?, ?, " + routeDBNowMsSQL + ") ON DUPLICATE KEY UPDATE src_zone = src_zone"
	routeUpdateMergedIntoRunSQL = "UPDATE zone_merged_into SET run_id = ? WHERE src_zone = ? AND dst_zone = ?"
	routeDeleteMergedIntoSQL    = "DELETE FROM zone_merged_into WHERE src_zone = ? AND dst_zone = ? AND run_id = ?"
	routeListMergedIntoSQL      = "SELECT src_zone, dst_zone FROM zone_merged_into"

	// 按 home zone 键集翻页(游标不用行比较,MySQL 不对它做 range 优化)。实参:zone、游标片、游标片、游标 player_id、limit。
	routeScanByHomeSQL = "SELECT route_shard, player_id, home_zone" +
		" FROM player_route WHERE home_zone = ? AND (route_shard > ? OR (route_shard = ? AND player_id > ?))" +
		" ORDER BY route_shard, player_id LIMIT ?"
)

// ── 行与键 ────────────────────────────────────────────────────────────────────

// routeRowShard 返回 player_route.route_shard(镜像 shared/placement.RouteRowShard)。
func routeRowShard(playerID uint64) uint32 {
	return uint32(playerID % uint64(routeRowShards))
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

// mergeRouteRow 是 player_route 的一行(只取门禁与统计用得到的列)。
type mergeRouteRow struct {
	PlayerID uint64
	HomeZone uint32
}

// routeScanner 是 *sql.Row 与 *sql.Rows 共有的 Scan。
type routeScanner interface {
	Scan(dest ...any) error
}

// scanMergeRouteRow 扫一行(聚簇键两列 + home_zone),并校验库里存的行片等于 player_id 的行片(不等即 errRouteShardMismatch,fail-closed)。
func scanMergeRouteRow(sc routeScanner) (mergeRouteRow, error) {
	var row mergeRouteRow
	var shard uint64
	var home sql.Null[uint32]
	if err := sc.Scan(&shard, &row.PlayerID, &home); err != nil {
		return mergeRouteRow{}, err
	}
	if want := uint64(routeRowShard(row.PlayerID)); shard != want {
		return mergeRouteRow{}, fmt.Errorf("%w: player_id=%d route_shard=%d want %d", errRouteShardMismatch, row.PlayerID, shard, want)
	}
	if home.Valid {
		row.HomeZone = home.V
	}
	return row, nil
}

// ── 连接与探针 ────────────────────────────────────────────────────────────────

// routeDBDSN 把 -route-db-dsn 规范成路由表专用池的 DSN(与 data_service buildRouteDSN 同口径):
//   - transaction_isolation='READ-COMMITTED':锁推演只在 RC 下成立(ODKU 撞键只加记录锁、FOR UPDATE 读不存在的行不留间隙锁);
//   - innodb_lock_wait_timeout=1:锁等待封顶,等满即 1205、不重试;
//   - sql_mode='STRICT_TRANS_TABLES':超长的值不会被静默截断。
//   - 与业务/Team池共用pinCurrentSession：tidb 档清除历史快照/旧副本/低精度TSO，逐连接固定当前强读；mysql 档不带任何 tidb_* 参数。
//
// 运维写在 DSN 里的同名参数一律被覆盖;旧名 tx_isolation 删掉(两个名字同时出现时生效的是哪个不确定)。
// 必须带库名(全局库:dev 是 testdb,K8s 是 mmorpg_global);带 clientFoundRows=true 拒绝(ODKU 撞键 = 受影响 0 行的
// 判据会失效)。返回值含口令,只交给 sql.Open,不进日志、不进错误信息(ParseDSN 的错误不回显 DSN 原文)。
func routeDBDSN(raw string, backend authoritysql.Backend) (string, error) {
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return "", fmt.Errorf("parse -route-db-dsn: %w", err)
	}
	if cfg.DBName == "" {
		return "", errors.New("-route-db-dsn must name the global database that holds zone_merged_into (dev: testdb, K8s: mmorpg_global)")
	}
	if cfg.ClientFoundRows {
		return "", errors.New("-route-db-dsn must not set clientFoundRows=true: the route writes judge ON DUPLICATE KEY no-ops by 0 affected rows")
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	pinCurrentSession(cfg, backend)
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
	backend   authoritysql.Backend // 探针按它读回会话;零值按 tidb
	closeOnce sync.Once
	closeErr  error
	// sleep / backoff 是 1213 / 9007 重试的退避接缝:生产 sleepCtx 与均匀抖动,单测注入不睡的实现。
	sleep   func(ctx context.Context, d time.Duration) error
	backoff func() time.Duration
}

// openMergeRouteStore 在 -route-db-dsn 上开池、跑探针,任一步失败就关池返回 error(fail-closed,调用方退出码非 0)。
// 返回探针读到的 route_meta(成对核对要 generation)。
func openMergeRouteStore(ctx context.Context, rawDSN string, backend authoritysql.Backend) (*mergeRouteStore, routeMeta, error) {
	dsn, err := routeDBDSN(rawDSN, backend)
	if err != nil {
		return nil, routeMeta{}, err
	}
	db, err := sql.Open("mysql", dsn)
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
	return newMergeRouteStoreFromDB(pctx, db, backend)
}

// newMergeRouteStoreFromDB 在一个已开好的池上跑探针并装出 store;任一步失败都关掉 db。单测注入假驱动用。
func newMergeRouteStoreFromDB(ctx context.Context, db *sql.DB, backend authoritysql.Backend) (*mergeRouteStore, routeMeta, error) {
	st := &mergeRouteStore{db: db, backend: backend, sleep: sleepCtx, backoff: routeLockRetryBackoff}
	if err := registerMaintenanceResource(ctx, st); err != nil {
		return nil, routeMeta{}, errors.Join(err, st.Close())
	}
	meta, err := st.probe(ctx)
	if err != nil {
		return nil, routeMeta{}, fmt.Errorf("route db probe: %w", errors.Join(err, st.Close()))
	}
	return st, meta, nil
}

// Close 关闭连接池。
func (s *mergeRouteStore) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

// probe 是开写前探针:会话隔离级别必须是 READ-COMMITTED、innodb_lock_wait_timeout 必须是 1(DSN 的会话变量没生效时锁推演不成立);
// 对端必须符合档位(tidb 档:真TiDB，事务/副本/一致性/快照/staleness/低精度TSO读回全部符合当前强读策略;mysql 档:MySQL 主库);route_meta(id=1) 必须在。
// 不发任何 DML。这不授予写权限:所有真实维护写边界另外核同 boot TOOL 身份与冻结证明(runTx)。
func (s *mergeRouteStore) probe(ctx context.Context) (routeMeta, error) {
	var isolation string
	var lockWait int64
	if err := s.db.QueryRowContext(ctx, routeProbeSessionSQL).Scan(&isolation, &lockWait); err != nil {
		return routeMeta{}, fmt.Errorf("read session transaction_isolation / innodb_lock_wait_timeout: %w", err)
	}
	if !strings.EqualFold(isolation, "READ-COMMITTED") || lockWait != routeLockWaitTimeoutSec {
		return routeMeta{}, fmt.Errorf("%w: transaction_isolation=%q innodb_lock_wait_timeout=%d, want READ-COMMITTED and %d "+
			"(the DSN session variables pinned by routeDBDSN did not take effect)",
			errRouteSessionMisconfigured, isolation, lockWait, routeLockWaitTimeoutSec)
	}
	if err := requireCurrentSession(ctx, s.db, s.backend); err != nil {
		return routeMeta{}, err
	}
	return readRouteMeta(ctx, s.db)
}

// readRouteMeta 读 route_meta(id=1)(一致读)。
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

// runTx 跑一个写事务(BEGIN → 锁 meta 并强读原冻结 → body → COMMIT,出错 ROLLBACK),按 retryRouteTx 有界重来。
// 只有持 TOOL 身份与冻结证明才能进来;body 必须可整体重跑:结果只在 body 成功返回时交给外层变量。
func (s *mergeRouteStore) runTx(ctx context.Context, op string, body func(tx *sql.Tx) error) (err error) {
	if err := checkMaintenanceTool(ctx); err != nil {
		return err
	}
	proof := normalFreezeFromContext(ctx)
	if proof == nil {
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
		// 维护工具不改writable；每个实际写事务先锁meta并强读原NORMAL消费链。此锁与正式Release同一行，不能由事务外预检代替提交前缀。
		if err := proof.lockTransaction(ctx, tx); err != nil {
			return err
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

// ── 入参整理 ──────────────────────────────────────────────────────────────────

// routeZoneOK 报告 zone 是否在 [1, routeHomeZoneMax]。
func routeZoneOK(zone uint32) bool {
	return zone >= 1 && zone <= routeHomeZoneMax
}

// routeZonePair 校验一对不同的 zone,返回升序的两个(加锁顺序)。
func routeZonePair(a, b uint32) ([]uint32, error) {
	if a == b {
		return nil, fmt.Errorf("%w: zones must differ (both %d)", errRouteInvalidArgument, a)
	}
	if !routeZoneOK(a) || !routeZoneOK(b) {
		return nil, fmt.Errorf("%w: zones %d, %d not in [1, %d]", errRouteInvalidArgument, a, b, routeHomeZoneMax)
	}
	if a > b {
		a, b = b, a
	}
	return []uint32{a, b}, nil
}

// ── player_route 只读 ─────────────────────────────────────────────────────────

// ScanByHome 按 home zone 键集翻页(一致读,走 idx_route_home):返回 home_zone = zone 且聚簇键 > after 的至多 limit 行,
// 按 (route_shard, player_id) 升序;下一页从 routeCursorOf(最后一行.PlayerID) 接着翻,一页不足 limit 即翻完。
// 每行都校验(行片一致、home 等于 zone、键严格递增且大于 after),任一不符整页作废。
func (s *mergeRouteStore) ScanByHome(ctx context.Context, zone uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error) {
	if !routeZoneOK(zone) {
		return nil, fmt.Errorf("%w: scan by home_zone %d not in [1, %d]", errRouteInvalidArgument, zone, routeHomeZoneMax)
	}
	if limit < 1 || limit > routeReadBatchLimit || after.Shard >= routeRowShards {
		return nil, fmt.Errorf("%w: scan by home_zone limit=%d after=%+v", errRouteInvalidArgument, limit, after)
	}
	rows, err := s.db.QueryContext(ctx, routeScanByHomeSQL, uint64(zone), uint64(after.Shard), uint64(after.Shard), after.PlayerID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan player_route by home_zone=%d: %w", zone, err)
	}
	defer rows.Close()
	var out []mergeRouteRow
	prev := after
	for rows.Next() {
		row, err := scanMergeRouteRow(rows)
		if err != nil {
			return nil, err
		}
		cur := routeCursorOf(row.PlayerID)
		switch {
		case row.HomeZone != zone:
			return nil, fmt.Errorf("%w: scan by home_zone=%d returned player %d whose row does not match", errRouteInvariant, zone, row.PlayerID)
		case !prev.less(cur):
			return nil, fmt.Errorf("%w: scan by home_zone=%d returned key %+v not after %+v", errRouteInvariant, zone, cur, prev)
		case len(out) >= limit:
			return nil, fmt.Errorf("%w: scan by home_zone=%d returned more than %d rows", errRouteInvariant, zone, limit)
		}
		prev = cur
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan player_route by home_zone=%d: %w", zone, err)
	}
	return out, nil
}

// ── zone_merged_into ──────────────────────────────────────────────────────────

// encodeRouteMergedIntoValue 编码映射的镜像值,与 shared/placement.EncodeMergedIntoValue 逐字节相同。
func encodeRouteMergedIntoValue(dst uint32, runID string) (string, error) {
	b, err := json.Marshal(mergedIntoValue{Dst: dst, RunID: runID})
	if err != nil {
		return "", fmt.Errorf("encode merged_into: %w", err)
	}
	return string(b), nil
}

// MarkMergedInto 写映射(合服步骤 1)。事务里按 zone 升序给两行加锁读,在提交点复核事务外的冲突检查:
//   - 目标区有行 = 目标区已被合走,errRouteMergedIntoConflict;
//   - 源区无行插入;同 dst 把 run_id 改成 markRunID(续跑 / 撤销后重合同一对 zone);异 dst → errRouteMergedIntoConflict。
//
// 返回镜像值(调用方 SET + WAITAOF)。
func (s *mergeRouteStore) MarkMergedInto(ctx context.Context, src, dst uint32, markRunID string) (string, error) {
	zones, err := routeZonePair(src, dst)
	if err != nil {
		return "", err
	}
	if !validRunID(markRunID) {
		return "", fmt.Errorf("%w: run id %q", errRouteInvalidArgument, markRunID)
	}
	value, err := encodeRouteMergedIntoValue(dst, markRunID)
	if err != nil {
		return "", err
	}
	err = s.runTx(ctx, "mark merged_into", func(tx *sql.Tx) error {
		var curDst uint32
		var curRun string
		var found bool
		for _, zone := range zones {
			d, run, ok, err := readRouteMergedInto(ctx, tx, zone, true)
			if err != nil {
				return err
			}
			if zone == dst && ok {
				return fmt.Errorf("%w: target zone %d was itself merged into zone %d (run_id %s)", errRouteMergedIntoConflict, dst, d, run)
			}
			if zone == src {
				curDst, curRun, found = d, run, ok
			}
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

// ClearMergedInto 删映射(撤销的最后一步):DELETE … WHERE src_zone = ? AND dst_zone = ? AND run_id = ?(markRunID = 当初合服的清单 run_id)。
// 返回是否删了一行;没删 = 本来不在,或是另一次合服写的(撤销前置已拒绝后者)。之后调用方按完整值 compare-and-delete 镜像。
func (s *mergeRouteStore) ClearMergedInto(ctx context.Context, src, dst uint32, markRunID string) (bool, error) {
	if _, err := routeZonePair(src, dst); err != nil {
		return false, err
	}
	if !validRunID(markRunID) {
		return false, fmt.Errorf("%w: run id %q", errRouteInvalidArgument, markRunID)
	}
	var deleted bool
	err := s.runTx(ctx, "clear merged_into", func(tx *sql.Tx) error {
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

// ReadMergedInto 一致读 zone_merged_into(src)(不加锁)。
// ListMergedInto 读全部映射(src → dst)。表很小:每次合服一行。
func (s *mergeRouteStore) ListMergedInto(ctx context.Context) (map[uint32]uint32, error) {
	rows, err := s.db.QueryContext(ctx, routeListMergedIntoSQL)
	if err != nil {
		return nil, fmt.Errorf("list zone_merged_into: %w", err)
	}
	defer rows.Close()
	out := map[uint32]uint32{}
	for rows.Next() {
		var src, dst uint32
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, fmt.Errorf("list zone_merged_into: %w", err)
		}
		out[src] = dst
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list zone_merged_into: %w", err)
	}
	return out, nil
}

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

// ── 开库的统一入口 ────────────────────────────────────────────────────────────

// routeWatermarkReader 是读 mapping:route:watermark 的接缝(生产 *routePublisher)。
type routeWatermarkReader interface {
	readWatermark(ctx context.Context) (raw string, present bool, err error)
}

// openPairedRouteStore 是各模式开路由库的统一入口:开库跑探针,再要求水位键存在、可解码、世代等于该库 route_meta.generation。
func openPairedRouteStore(ctx context.Context, rawDSN string, backend authoritysql.Backend, wm routeWatermarkReader) (*mergeRouteStore, error) {
	st, meta, err := openMergeRouteStore(ctx, rawDSN, backend)
	if err != nil {
		return nil, err
	}
	raw, present, err := wm.readWatermark(ctx)
	if err == nil {
		err = decideRoutePairing(raw, present, meta.Generation)
	}
	if err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}
