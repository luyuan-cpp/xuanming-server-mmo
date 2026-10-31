package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
)

// 这里只保存操作者明确指定的SQL原件选择键，不复制正式proto正文或构造第二份排空事实。
// 维护写者必须强读NORMAL票据已消费的SQL_FROZEN结局；过期、grace或当前lag不构成授权。
type normalFreezeProof struct {
	runID, ticketID, requestID, receiptDigest []byte
	epoch                                     uint64
	authoritySchema                           string
}

var errNormalFreezeProof = errors.New("合服必须绑定同run NORMAL已消费票据及当前SQL_FROZEN永久收据")

const normalFrozenProofSQL = `SELECT COUNT(*)
FROM route_meta m
JOIN sql_writer_drain_run r ON r.operation_id=m.freeze_run_id
JOIN sql_writer_freeze_ticket t ON t.operation_id=r.operation_id AND t.attempt_revision=r.attempt_revision
JOIN sql_writer_maintenance_receipt f ON f.request_id=t.freeze_request_id
JOIN sql_writer_maintenance_receipt n ON n.request_id=f.prior_receipt_id
JOIN sql_writer_maintenance_receipt g ON g.request_id=t.grant_receipt_id
WHERE m.id=1 AND m.writable=0 AND m.writer_epoch=? AND m.freeze_run_id=?
AND t.ticket_id=? AND f.request_id=? AND f.receipt_digest=?
AND r.phase=7 AND r.generation=m.generation AND r.frozen_request_id=f.request_id AND r.latest_receipt_id=f.request_id AND r.latest_receipt_digest=f.receipt_digest
AND t.state=2 AND t.authorization_kind=1 AND t.generation=m.generation AND t.manifest_digest=r.manifest_digest
AND f.kind=1 AND f.phase=1 AND f.run_phase=7 AND f.run_id=r.operation_id AND f.generation=m.generation
AND f.attempt_revision=r.attempt_revision AND f.catalog_revision=m.catalog_revision
AND f.before_epoch>0 AND f.before_epoch<18446744073709551615 AND f.after_epoch=f.before_epoch+1 AND f.after_epoch=m.writer_epoch
AND t.freeze_request_digest=f.request_digest AND t.consumed_receipt_digest=f.receipt_digest
AND n.kind=3 AND n.run_phase=5 AND n.phase=0 AND n.run_id=r.operation_id AND n.generation=m.generation
AND n.attempt_revision=r.attempt_revision AND n.receipt_digest=f.prior_receipt_digest AND n.receipt_digest=t.non_sql_receipt_digest
AND g.kind=6 AND g.run_phase=6 AND g.run_id=r.operation_id AND g.generation=m.generation
AND g.attempt_revision=r.attempt_revision AND g.receipt_digest=t.grant_receipt_digest
AND g.prior_receipt_id=n.request_id AND g.prior_receipt_digest=n.receipt_digest`

func parseNormalFreezeProof(o options) (*normalFreezeProof, error) {
	if o.routeDBDSN == "" || o.frozenWriterEpoch == 0 {
		return nil, errNormalFreezeProof
	}
	cfg, err := mysql.ParseDSN(o.routeDBDSN)
	if err != nil || len(cfg.DBName) == 0 || len(cfg.DBName) > 64 {
		return nil, errNormalFreezeProof
	}
	for _, c := range cfg.DBName {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return nil, errNormalFreezeProof
		}
	}
	values := []string{o.maintenanceRunID, o.freezeTicketID, o.freezeRequestID, o.freezeReceiptSHA256}
	decoded := make([][]byte, len(values))
	for i, value := range values {
		want := 32
		if i == 3 {
			want = 64
		}
		if len(value) != want || strings.ToLower(value) != value {
			return nil, errNormalFreezeProof
		}
		b, err := hex.DecodeString(value)
		if err != nil {
			return nil, errNormalFreezeProof
		}
		var nonzero byte
		for _, v := range b {
			nonzero |= v
		}
		if nonzero == 0 {
			return nil, errNormalFreezeProof
		}
		decoded[i] = b
	}
	return &normalFreezeProof{runID: decoded[0], ticketID: decoded[1], requestID: decoded[2], receiptDigest: decoded[3], epoch: o.frozenWriterEpoch, authoritySchema: cfg.DBName}, nil
}

func normalFrozenProofSQLForSchema(schema string) string {
	query := normalFrozenProofSQL
	for _, table := range []string{"route_meta", "sql_writer_drain_run", "sql_writer_freeze_ticket", "sql_writer_maintenance_receipt"} {
		query = strings.ReplaceAll(query, " "+table+" ", " `"+schema+"`.`"+table+"` ")
	}
	return query
}

func (p *normalFreezeProof) lockSQL() string {
	return "SELECT writable FROM `" + p.authoritySchema + "`.`route_meta` WHERE id = 1 FOR UPDATE"
}

func (p *normalFreezeProof) lockTransaction(ctx context.Context, tx *sql.Tx) error {
	if p == nil || tx == nil {
		return errNormalFreezeProof
	}
	var writable uint64
	if err := tx.QueryRowContext(ctx, p.lockSQL()).Scan(&writable); err != nil {
		return fmt.Errorf("冻结维护锁定meta失败: %w", err)
	}
	if writable != 0 {
		return errNormalFreezeProof
	}
	return p.check(ctx, tx)
}

func (p *normalFreezeProof) check(ctx context.Context, q routeQueryer) error {
	if p == nil || q == nil {
		return errNormalFreezeProof
	}
	var matches uint64
	if err := q.QueryRowContext(ctx, normalFrozenProofSQLForSchema(p.authoritySchema), p.epoch, p.runID, p.ticketID, p.requestID, p.receiptDigest).Scan(&matches); err != nil {
		return fmt.Errorf("%w: 当前强读失败: %w", errNormalFreezeProof, err)
	}
	if matches != 1 {
		return errNormalFreezeProof
	}
	return nil
}

// merge/unmerge入口在任何写之前读取实际SQL；调用方在每个既有writeGate再核同一选择键。
// 这不是跨SQL/Redis事务：缺少应用停写/join原件时仍不能由此提前发布ReleaseReady。
func openNormalFreezeProof(ctx context.Context, o options) (*normalFreezeProof, *sql.DB, error) {
	p, err := parseNormalFreezeProof(o)
	if err != nil {
		return nil, nil, err
	}
	db, err := openCurrentSession(ctx, o.routeDBDSN, o.sqlBackend)
	if err != nil {
		return nil, nil, err
	}
	if err = p.check(ctx, db); err != nil {
		return nil, nil, errors.Join(err, db.Close())
	}
	return p, db, nil
}

type normalFreezeContextKey struct{}

func withNormalFreezeProof(ctx context.Context, p *normalFreezeProof) context.Context {
	return context.WithValue(ctx, normalFreezeContextKey{}, p)
}

func normalFreezeFromContext(ctx context.Context) *normalFreezeProof {
	p, _ := ctx.Value(normalFreezeContextKey{}).(*normalFreezeProof)
	return p
}

const (
	currentTiDBVersionSQL = "SELECT VERSION(),@@version_comment"
	currentTiDBSessionSQL = "SELECT @@tidb_txn_mode,@@transaction_isolation,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot,@@tidb_read_staleness,@@tidb_low_resolution_tso"
)

var (
	errCurrentTiDB  = errors.New("全局SQL连接必须使用真实TiDB当前强读会话")
	errCurrentMySQL = errors.New("全局SQL连接必须是MySQL主库的RC会话")
)

// 业务原件、Team原件、路由读取与审计共用同一规则。按大小写无关删除旧参数，避免驱动SET时同名别名覆盖安全值。
// 每条新连接都绑定，不只修改探针碰到的那一条连接；调用者仍须在交付句柄前读回策略。
// mysql 档只固定 RC,并删掉 DSN 里所有 tidb_* 参数:驱动会把它们当会话变量 SET,在 MySQL 上是 1193,建连就失败。
func pinCurrentSession(cfg *mysql.Config, backend authoritysql.Backend) {
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	for key := range cfg.Params {
		lower := strings.ToLower(key)
		switch lower {
		case mysqlIsolationParam, mysqlLegacyIsolationParam, "tidb_txn_mode", "tidb_replica_read", "tidb_read_consistency", "tidb_snapshot", "tidb_read_staleness", "tidb_low_resolution_tso":
			delete(cfg.Params, key)
		default:
			if backend == authoritysql.BackendMySQL && strings.HasPrefix(lower, "tidb_") {
				delete(cfg.Params, key)
			}
		}
	}
	cfg.Params[mysqlIsolationParam] = mysqlReadCommitted
	if backend == authoritysql.BackendMySQL {
		return
	}
	for key, value := range map[string]string{
		"tidb_txn_mode": "'pessimistic'", "tidb_replica_read": "'leader'", "tidb_read_consistency": "'strict'", "tidb_snapshot": "''", "tidb_read_staleness": "0", "tidb_low_resolution_tso": "OFF",
	} {
		cfg.Params[key] = value
	}
}

// requireCurrentSession 在交付句柄前读回策略。mysql 档与服务同一口径(authoritysql):不是 TiDB、是主库、会话是 RC。
func requireCurrentSession(ctx context.Context, db *sql.DB, backend authoritysql.Backend) error {
	if backend == authoritysql.BackendMySQL {
		if err := authoritysql.AssertCurrentSession(ctx, db, backend); err != nil {
			return fmt.Errorf("%w: %w", errCurrentMySQL, err)
		}
		return nil
	}
	return requireCurrentTiDB(ctx, db)
}

func requireCurrentTiDB(ctx context.Context, db *sql.DB) error {
	var version, comment string
	if err := db.QueryRowContext(ctx, currentTiDBVersionSQL).Scan(&version, &comment); err != nil {
		return fmt.Errorf("%w: 无法读取引擎: %w", errCurrentTiDB, err)
	}
	if !strings.Contains(strings.ToLower(version+comment), "tidb") {
		return fmt.Errorf("%w: 拒绝MySQL历史写入域", errCurrentTiDB)
	}
	var mode, isolation, replica, consistency, snapshot string
	var stale, low int64
	if err := db.QueryRowContext(ctx, currentTiDBSessionSQL).Scan(&mode, &isolation, &replica, &consistency, &snapshot, &stale, &low); err != nil {
		return fmt.Errorf("%w: 无法读回强读策略: %w", errCurrentTiDB, err)
	}
	if mode != "pessimistic" || isolation != "READ-COMMITTED" || replica != "leader" || consistency != "strict" || snapshot != "" || stale != 0 || low != 0 {
		return fmt.Errorf("%w: 读回策略不符，拒绝历史快照/旧副本/低精度TSO", errCurrentTiDB)
	}
	return nil
}

// 不使用log.Fatal，审计入口必须将连接失败保留为INFRA/exit2，不能把未执行误报为无阻塞。
func openCurrentSession(ctx context.Context, raw string, backend authoritysql.Backend) (*sql.DB, error) {
	dsn, err := readCommittedDSN(raw, backend)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := registerMaintenanceResource(ctx, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err := requireCurrentSession(ctx, db, backend); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}
