package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shared/authoritysql"
	"shared/dbplacement"
	"trade/internal/config"
)

// toolPingTimeout 是运维工具连库之后探活的上限。
const toolPingTimeout = 5 * time.Second

// OpenToolMySQL 给运维工具(go/trade/cmd/*)开 mmorpg_trade 的连接:与服务进程同一套 DSN、落点和写资格,
// 连上之后探活一次。回的错误不含 DSN 原文(DSN 带口令)。
func OpenToolMySQL(ctx context.Context, c config.MySQLConf, permission authoritysql.Permission) (*sql.DB, error) {
	placementOptions, err := dbplacement.ForOneShot(ctx, c.DbPlacement, c.DBName)
	if err != nil {
		return nil, err
	}
	db, err := authoritysql.Open(ctx, BuildDSN(c), c.DBName, authoritysql.Backend(c.Backend), 2, permission, placementOptions...)
	if err != nil {
		return nil, errors.New("打开 MySQL 失败(DSN 含口令,不打印原错误)")
	}
	pingCtx, cancel := context.WithTimeout(ctx, toolPingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 MySQL 库 %s 失败: %w", c.DBName, err)
	}
	// 工具与服务进程同一套锁等待上限(session_lock_wait.go):工具的写与在线请求争的是同一批行锁。
	if err := verifySessionLockWaits(pingCtx, db, authoritysql.Backend(c.Backend)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("MySQL database %s: %w", c.DBName, err)
	}
	return db, nil
}
