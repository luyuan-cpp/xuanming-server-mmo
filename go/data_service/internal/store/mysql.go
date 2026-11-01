package store

import (
	"context"
	"database/sql"
	"fmt"
	"shared/authoritysql"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// MySQLConfig holds MySQL connection settings for the global-DB stores.
// snapshot / transaction_log 等 store 都落在同一个全局库
// (config.SnapshotMySQL),这里是它们共用的连接参数。
type MySQLConfig struct {
	// Permission 由进程装配入口显式传入；同一 boot 的所有业务池复用同一不可变写者。
	Permission authoritysql.Permission
	// Backend 零值按 tidb(authoritysql.ParseBackend);测试连 MySQL 必须显式写 BackendMySQL。
	Backend     authoritysql.Backend
	Host        string
	User        string
	Password    string
	DBName      string
	MaxOpenConn int
	MaxIdleConn int
	// PoolOptions 是 authoritysql.Open 的可选项:按映射拨号时由装配入口传入(dbplacement),没有时固定连 Host。
	PoolOptions []authoritysql.OpenOption

	// LogicalShards 是目录分片数 K,0 按 1。DBName 这时是基名,片库名由 dirshard.DirectoryDBName 派生。
	// 只有目录的 store 看它;流水库和存档库是单个逻辑库,经 ForSchema 拿到自己的参数。
	LogicalShards int
	// PermissionFor 按逻辑库给 SQL 权限:闸门跟着库走,每个库一份写资格。nil 时所有库都用 Permission(一次性工具的 Operator、Reader)。
	PermissionFor func(schema string) (authoritysql.Permission, error)
	// PoolOptionsFor 按逻辑库给拨号选项。一次性工具用它逐库读一次映射;常驻服务的 Watcher 本身按库解析,用 PoolOptions。
	PoolOptionsFor func(schema string) ([]authoritysql.OpenOption, error)
}

// ForSchema 给出连某个逻辑库的参数:库名、权限、拨号选项都换成这个库的。账号口令和连接池上限不变。
func (c MySQLConfig) ForSchema(schema string) (MySQLConfig, error) {
	out := c
	out.DBName = schema
	out.LogicalShards = 0
	out.PermissionFor = nil
	out.PoolOptionsFor = nil
	if c.PermissionFor != nil {
		permission, err := c.PermissionFor(schema)
		if err != nil {
			return MySQLConfig{}, err
		}
		out.Permission = permission
	}
	if c.PoolOptionsFor != nil {
		options, err := c.PoolOptionsFor(schema)
		if err != nil {
			return MySQLConfig{}, err
		}
		out.PoolOptions = options
	}
	return out, nil
}

const mysqlPingTimeout = 5 * time.Second

// buildDSN 统一三个 store 与迁移入口的 DSN。
//
// sql_mode=%27STRICT_TRANS_TABLES%27 强制会话级严格模式(%27 是被 URL 转义的单引号,
// 驱动会还原成 SET sql_mode = 'STRICT_TRANS_TABLES')。快照是 MEDIUMBLOB 里的序列化
// 数据、流水 extra 是自由文本,非严格模式下超长写入会**静默截断且无报错**;在连接层
// 兜底,与 go/db 同口径。以前 transaction_log_store 漏了这一项,现在收口成一处。
func buildDSN(cfg MySQLConfig) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&sql_mode=%%27STRICT_TRANS_TABLES%%27",
		cfg.User, cfg.Password, cfg.Host, cfg.DBName)
}

// buildRouteDSN 是 RouteStore 专用池的 DSN:在 buildDSN 之上再钉两个会话变量
// (go-sql-driver 把不认识的 DSN 参数当成建连后的会话级 SET,先例见
// go/guild/internal/data/guild_manage_repo.go 的 WithLockWaitTimeout、go/friend/internal/svc 的 BuildDSN):
//
//   - transaction_isolation='READ-COMMITTED':player_route 的锁推演(规格 c.2「为什么这套锁
//     不会成环」)只在 RC 下成立 —— RC 下 ODKU 撞主键只加记录锁、FOR UPDATE 读不存在的行不留间隙锁。
//   - innodb_lock_wait_timeout=routeLockWaitTimeoutSec(1s):默认 50s 的锁等待在 data_service 2s 的服务端预算里
//     永远等不到 1205;ctx 到期时驱动只关连接,服务端那个等锁的线程会继续占着已拿到的行锁,
//     把同一 (zone, shard) 上的建角串成最长 50s 的停顿。封顶 1s 之后最多再占 1s。
//
// 与 buildDSN 一样不设 clientFoundRows:R4 / R7 / R10 / R15 的 ODKU no-op 靠「撞键 = 受影响 0 行」判定,
// 带了它 no-op 也回 1,撞键会被当成插入成功。
//
// 返回值含口令,只能交给 sql.Open,不进日志、不进错误信息。
func buildRouteDSN(cfg MySQLConfig) string {
	return buildDSN(cfg) + "&transaction_isolation=%27READ-COMMITTED%27&innodb_lock_wait_timeout=" +
		strconv.Itoa(routeLockWaitTimeoutSec)
}

// openMySQL 建池、设上限、ping。返回错误时池已关闭,调用方不必再 Close。
func openMySQL(cfg MySQLConfig) (*sql.DB, error) {
	return openMySQLWithDSN(cfg, buildDSN(cfg))
}

// openMySQLWithDSN 同 openMySQL,只是 DSN 由调用方给(RouteStore 要在 buildDSN 之上加会话变量,见 buildRouteDSN)。
// 池上限仍取 cfg;dsn 含口令,出错时的错误信息里不带它。
func openMySQLWithDSN(cfg MySQLConfig, dsn string) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	defer cancel()
	maxOpen := cfg.MaxOpenConn
	if maxOpen <= 0 {
		maxOpen = 5
	}
	db, err := authoritysql.Open(ctx, dsn, cfg.DBName, cfg.Backend, maxOpen, cfg.Permission, cfg.PoolOptions...)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	if cfg.MaxOpenConn > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConn)
	} else {
		db.SetMaxOpenConns(5)
	}
	if cfg.MaxIdleConn > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConn)
	} else {
		db.SetMaxIdleConns(2)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}
