// Package authoritysql 为全局领域的权威SQL物理连接固定当前强读会话。
package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var tidbSettings = []string{
	"SET SESSION tidb_snapshot=''",
	"SET SESSION tidb_read_consistency='strict'",
	"SET SESSION tidb_txn_mode='pessimistic'",
	"SET SESSION tidb_read_staleness=0",
	"SET SESSION tidb_low_resolution_tso=OFF",
	"SET SESSION tidb_replica_read='leader'",
}

type connector struct {
	inner        driver.Connector
	tidb         bool
	keepLockWait bool
	permission   Permission
}

func (c connector) Driver() driver.Driver { return opaqueDriver{} }
func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	var gate writerGate = passGate{}
	if c.permission.kind == permissionWriter {
		gate = runtimeGate{config: c.permission.writer}
	}
	guarded, err := newGuardedConnector(c.inner, gate, c, mysqlStatementPolicy{}, c.permission)
	if err != nil {
		return nil, err
	}
	return guarded.Connect(ctx)
}
func (c connector) pin(ctx context.Context, conn driver.Conn) error {
	exec, ok := conn.(driver.ExecerContext)
	if !ok {
		return errors.New("SQL驱动不支持会话初始化")
	}
	statements := []string{"SET SESSION autocommit=1", "SET SESSION transaction_isolation='READ-COMMITTED'", "SET SESSION sql_mode='STRICT_TRANS_TABLES'"}
	if !c.keepLockWait {
		statements = append(statements, "SET SESSION innodb_lock_wait_timeout=1")
	}
	if c.tidb {
		statements = append(statements, tidbSettings...)
	}
	for _, statement := range statements {
		if _, err := exec.ExecContext(ctx, statement, nil); err != nil {
			return errors.New("权威SQL会话初始化失败")
		}
	}
	values, err := rawOne(ctx, conn, "SELECT CURRENT_USER(),DATABASE()", nil, 2)
	if err != nil {
		return errors.New("权威SQL主体或实际库观测失败")
	}
	principal, database := stringValue(values[0]), stringValue(values[1])
	if c.permission.kind == permissionWriter {
		grant := c.permission.writer.boot.grant
		found := false
		for _, schema := range grant.ProtectedSchemas {
			if schema == database {
				found = true
			}
		}
		if principal != string(grant.SqlPrincipal) || !found {
			return ErrWriterDenied
		}
		meta, err := readRawMeta(ctx, conn, grant.AuthoritySchema)
		if err != nil || meta.Generation != grant.Generation || meta.WriterEpoch == 0 {
			return ErrWriterDenied
		}
		if err := verifyBootRegistration(ctx, conn, grant); err != nil {
			return err
		}
	} else if c.permission.kind == permissionSchema || c.permission.kind == permissionOperator {
		if principal != c.permission.principal {
			return errors.New("权威SQL受限主体不匹配")
		}
	}
	return nil
}

// PinCurrentSession 复用既有权威 SQL 会话初始化；调用方保留自己的连接池与锁等待预算。
// requireTiDB=true 时 TiDB 专用 SET 在每条物理连接建立时执行，接到 MySQL 必须失败。
func PinCurrentSession(inner driver.Connector, requireTiDB bool, permission Permission) (driver.Connector, error) {
	if inner == nil {
		return nil, errors.New("缺少SQL连接器")
	}
	if err := permission.validate(); err != nil {
		return nil, err
	}
	return connector{inner: inner, tidb: requireTiDB, keepLockWait: true, permission: permission}, nil
}

// AssertCurrentSession 在业务池投入使用前确认强读口径；MySQL 模式只承诺本地 RC。
func AssertCurrentSession(ctx context.Context, db *sql.DB, requireTiDB bool) error {
	if !requireTiDB {
		var isolation string
		if err := db.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&isolation); err != nil || isolation != "READ-COMMITTED" {
			return errors.New("玩家SQL隔离级别不是READ-COMMITTED")
		}
		return nil
	}
	var version string
	if err := db.QueryRowContext(ctx, "SELECT tidb_version()").Scan(&version); err != nil {
		return errors.New("权威SQL必须连接TiDB")
	}
	var mode, isolation, replica, consistency, snapshot string
	var stale, low int64
	if err := db.QueryRowContext(ctx, "SELECT @@tidb_txn_mode,@@transaction_isolation,@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot").Scan(&mode, &isolation, &stale, &low, &replica, &consistency, &snapshot); err != nil || mode != "pessimistic" || isolation != "READ-COMMITTED" || stale != 0 || low != 0 || replica != "leader" || consistency != "strict" || snapshot != "" {
		return errors.New("权威SQL强读会话不符")
	}
	return nil
}

// Open 不记录DSN；新物理连接逐条清历史快照后再关闭staleness，禁止URL绕过。
// 生产期望库名由领域代码常量给出；测试只能为本次随机库传入对应名字。
func Open(ctx context.Context, dsn, expectedDatabase string, requireTiDB bool, maxOpen int, permission Permission) (*sql.DB, error) {
	return open(ctx, dsn, expectedDatabase, requireTiDB, maxOpen, 5*time.Second, permission)
}

// OpenForMigration 与运行池复用相同的强会话约束，但DDL由迁移器的
// statement context限制。独立池的socket预算容纳TiDB预切Region，
// 不改变任何业务请求、锁等待或资产投递预算。
func OpenForMigration(ctx context.Context, dsn, expectedDatabase string, requireTiDB bool, permission Permission) (*sql.DB, error) {
	if permission.kind != permissionSchema {
		return nil, errors.New("迁移池必须显式Schema权限")
	}
	// 迁移器占住一条连接持有GET_LOCK；取消DDL时需第二条连接发KILL QUERY。
	return open(ctx, dsn, expectedDatabase, requireTiDB, 2, 2*time.Minute, permission)
}

func open(ctx context.Context, dsn, expectedDatabase string, requireTiDB bool, maxOpen int, ioTimeout time.Duration, permission Permission) (*sql.DB, error) {
	if err := permission.validate(); err != nil {
		return nil, err
	}
	if expectedDatabase == "" || maxOpen < 1 || maxOpen > 128 {
		return nil, errors.New("权威SQL配置无效")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("权威SQL连接串无效")
	}
	if parsed.DBName != expectedDatabase {
		return nil, errors.New("权威SQL目标库不匹配")
	}
	// 不让驱动在有序初始化之前设置这些会话变量。
	for key := range parsed.Params {
		if strings.HasPrefix(strings.ToLower(key), "tidb_") || strings.EqualFold(key, "transaction_isolation") || strings.EqualFold(key, "sql_mode") {
			delete(parsed.Params, key)
		}
	}
	parsed.ParseTime = true
	parsed.MultiStatements = false
	parsed.Timeout = time.Second
	parsed.ReadTimeout = ioTimeout
	parsed.WriteTimeout = ioTimeout
	inner, err := mysql.NewConnector(parsed)
	if err != nil {
		return nil, errors.New("权威SQL连接器无效")
	}
	db := sql.OpenDB(connector{inner: inner, tidb: requireTiDB, permission: permission})
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxLifetime(15 * time.Minute)
	ready := false
	defer func() {
		if !ready {
			_ = db.Close()
		}
	}()
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(check); err != nil {
		return nil, fmt.Errorf("权威SQL启动失败: %w", err)
	}
	var actual string
	if err := db.QueryRowContext(check, "SELECT DATABASE()").Scan(&actual); err != nil || actual != expectedDatabase {
		return nil, errors.New("权威SQL实际库不匹配")
	}
	if requireTiDB {
		var version string
		if err := db.QueryRowContext(check, "SELECT tidb_version()").Scan(&version); err != nil {
			return nil, errors.New("权威SQL必须连接TiDB")
		}
		var mode, isolation, replica, consistency, snapshot string
		var stale, low int64
		if err := db.QueryRowContext(check, "SELECT @@tidb_txn_mode,@@transaction_isolation,@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot").Scan(&mode, &isolation, &stale, &low, &replica, &consistency, &snapshot); err != nil || mode != "pessimistic" || isolation != "READ-COMMITTED" || stale != 0 || low != 0 || replica != "leader" || consistency != "strict" || snapshot != "" {
			return nil, errors.New("权威SQL强读会话不符")
		}
	}
	if permission.kind == permissionWriter {
		if err := VerifyCatalogSchema(check, db, permission.writer.AuthoritySchema()); err != nil {
			return nil, err
		}
	}
	ready = true
	return db, nil
}
