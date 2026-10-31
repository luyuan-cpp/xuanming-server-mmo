package main

// 全局库上的映射读写。
//
// merge_zone 是独立 module,import 不了 go/data_service/internal/store;表结构归 data_service 的迁移。
// 本工具对路由表的全部写只有 zone_merged_into 一张表,且都在本文件(route_store_test.go 的
// TestNoRouteTableWritesOutsideMergeRouteStore 守住);route_meta 只读和加锁,不写。
//
// 不变量:
//  1. 所有映射写者串行在 route_meta(id=1) 的行锁上。判定和写在同一个事务里,事务外读到的东西不算数。
//  2. 锁序:route_meta → zone_merged_into(zone 升序)。
//  3. 会话由 authoritysql 固定:READ-COMMITTED、innodb_lock_wait_timeout=1、严格 sql_mode,tidb 档再加当前强读。
//     锁等待超时、死锁、写冲突不在这里重试:工具是人跑的,报错后重跑同一条命令,结论不会错。
//  4. 错误信息不回显 DSN:ParseDSN 的错误不往外传,authoritysql 的错误是固定文案。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
)

const (
	// routeZoneMax 是 zone 编号上限(data_service schema.go chk_route_home 的 999999)。
	routeZoneMax uint32 = 999999
	// routeMaxOpenConns:工具串行跑,一个事务一条连接;留第二条给事务外的读。
	routeMaxOpenConns = 2

	routeDBNowMsSQL = "CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED)"

	routeMetaLockSQL = "SELECT writable FROM route_meta WHERE id = 1 FOR UPDATE"
	routeMetaReadSQL = "SELECT writable FROM route_meta WHERE id = 1"

	mappingLockSQL = "SELECT dst_zone FROM zone_merged_into WHERE src_zone = ? FOR UPDATE"
	mappingListSQL = "SELECT src_zone, dst_zone, run_id, merged_at_ms FROM zone_merged_into ORDER BY src_zone"
	// 撞主键不吞:持着 route_meta 的锁还能撞,说明有绕开这把锁的写者,这次按失败报出来,重跑时走幂等分支或拒绝。
	mappingInsertSQL = "INSERT INTO zone_merged_into (src_zone, dst_zone, run_id, merged_at_ms) VALUES (?, ?, ?, " + routeDBNowMsSQL + ")"
	mappingDeleteSQL = "DELETE FROM zone_merged_into WHERE src_zone = ? AND dst_zone = ?"
)

// routeStore 是 mappingStore 的 SQL 实现。
type routeStore struct {
	db *sql.DB
}

var _ mappingStore = (*routeStore)(nil)

// openRouteStore 开全局库。库名取自 DSN,authoritysql 连上之后核对实际库名与档位。
func openRouteStore(ctx context.Context, rawDSN string, backend authoritysql.Backend, permission authoritysql.Permission) (*routeStore, error) {
	cfg, err := mysql.ParseDSN(rawDSN)
	if err != nil {
		return nil, errors.New("cannot parse -route-db-dsn (its value is not echoed)")
	}
	if cfg.DBName == "" {
		return nil, errors.New("-route-db-dsn must name the global database that holds zone_merged_into (dev: testdb, K8s: mmorpg_global)")
	}
	db, err := authoritysql.Open(ctx, rawDSN, cfg.DBName, backend, routeMaxOpenConns, permission)
	if err != nil {
		return nil, fmt.Errorf("open global database %s: %w", cfg.DBName, err)
	}
	return &routeStore{db: db}, nil
}

// Close 关连接池。
func (s *routeStore) Close() error {
	return s.db.Close()
}

// readWritable 把 route_meta 的读结果转成开关。没有这一行说明迁移没跑过,指错了库的可能性比什么都大。
func readWritable(row *sql.Row) (bool, error) {
	var writable uint64
	err := row.Scan(&writable)
	if errors.Is(err, sql.ErrNoRows) {
		return false, refusedf("route_meta has no row id=1: this is not a migrated global database (run `data_service -f <yaml> -migrate`, or check the DSN)")
	}
	if err != nil {
		return false, fmt.Errorf("read route_meta: %w", err)
	}
	return writable != 0, nil
}

// routeQueryer 是 *sql.DB 与 *sql.Tx 共有的多行读。
type routeQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listMappingRows(ctx context.Context, q routeQueryer) ([]mappingRow, error) {
	rows, err := q.QueryContext(ctx, mappingListSQL)
	if err != nil {
		return nil, fmt.Errorf("read zone_merged_into: %w", err)
	}
	defer rows.Close()
	var out []mappingRow
	for rows.Next() {
		var row mappingRow
		if err := rows.Scan(&row.Src, &row.Dst, &row.RunID, &row.MergedAtMs); err != nil {
			return nil, fmt.Errorf("read zone_merged_into: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read zone_merged_into: %w", err)
	}
	return out, nil
}

// Snapshot 见 mappingStore。
func (s *routeStore) Snapshot(ctx context.Context) (bool, []mappingRow, error) {
	writable, err := readWritable(s.db.QueryRowContext(ctx, routeMetaReadSQL))
	if err != nil {
		return false, nil, err
	}
	rows, err := listMappingRows(ctx, s.db)
	if err != nil {
		return false, nil, err
	}
	return writable, rows, nil
}

// Locked 见 mappingStore。提交失败的结局未知(可能已经落库),错误文案让人重跑:判定在锁内重做,写过了就走幂等分支。
func (s *routeStore) Locked(ctx context.Context, zones []uint32, body func(mappingTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// 走到这里的回滚失败不改变结论:事务没提交,进程退出时连接断开,服务端同样回滚。
	defer func() { _ = tx.Rollback() }()

	writable, err := readWritable(tx.QueryRowContext(ctx, routeMetaLockSQL))
	if err != nil {
		return err
	}
	if err := refuseFrozen(writable); err != nil {
		return err
	}
	locks := slices.Clone(zones)
	slices.Sort(locks)
	for _, zone := range slices.Compact(locks) {
		var dst uint32
		if err := tx.QueryRowContext(ctx, mappingLockSQL, zone).Scan(&dst); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock zone_merged_into(%d): %w", zone, err)
		}
	}
	if err := body(sqlMappingTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: the outcome is unknown, re-run the same command (it is idempotent) or check with -mode audit: %w", err)
	}
	return nil
}

// sqlMappingTx 是持锁事务里的读写。
type sqlMappingTx struct {
	tx *sql.Tx
}

func (t sqlMappingTx) Rows(ctx context.Context) ([]mappingRow, error) {
	return listMappingRows(ctx, t.tx)
}

func (t sqlMappingTx) Insert(ctx context.Context, src, dst uint32, runID string) error {
	if _, err := t.tx.ExecContext(ctx, mappingInsertSQL, src, dst, runID); err != nil {
		return fmt.Errorf("insert zone_merged_into(%d -> %d): %w", src, dst, err)
	}
	return nil
}

func (t sqlMappingTx) Delete(ctx context.Context, src, dst uint32) (bool, error) {
	res, err := t.tx.ExecContext(ctx, mappingDeleteSQL, src, dst)
	if err != nil {
		return false, fmt.Errorf("delete zone_merged_into(%d -> %d): %w", src, dst, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete zone_merged_into(%d -> %d): %w", src, dst, err)
	}
	return n == 1, nil
}
