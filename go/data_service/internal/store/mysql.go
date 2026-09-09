package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// MySQLConfig holds MySQL connection settings for the global-DB stores.
// snapshot / transaction_log / id_segment 三个 store 都落在同一个全局库
// (config.SnapshotMySQL),这里是它们共用的连接参数。
type MySQLConfig struct {
	Host        string
	User        string
	Password    string
	DBName      string
	MaxOpenConn int
	MaxIdleConn int
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

// openMySQL 建池、设上限、ping。返回错误时池已关闭,调用方不必再 Close。
func openMySQL(cfg MySQLConfig) (*sql.DB, error) {
	db, err := sql.Open("mysql", buildDSN(cfg))
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

	ctx, cancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}
