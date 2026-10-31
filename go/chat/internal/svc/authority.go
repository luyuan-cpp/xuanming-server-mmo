package svc

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"chat/internal/authority"
	"chat/internal/config"
	"shared/authoritysql"
	"shared/playerredis"

	"github.com/redis/go-redis/v9"
)

func OpenAuthority(ctx context.Context, c config.Config, migrate bool) (*sql.DB, *authority.Store, error) {
	// 先于消费一次性 writer 资格:档位写错不能白白烧掉一份 boot grant。
	if _, err := authoritysql.ParseBackend(c.Backend); err != nil {
		return nil, nil, err
	}
	var permission authoritysql.Permission
	var loadErr error
	if migrate {
		permission, loadErr = authoritysql.LoadSchemaPermission(os.Getenv)
		if loadErr != nil {
			return nil, nil, loadErr
		}

	} else {
		writer, loadErr := authoritysql.LoadWriterConfig(os.Getenv)
		if loadErr != nil {
			return nil, nil, loadErr
		}
		permission = authoritysql.Writer(writer)
	}
	return OpenAuthorityWithPermission(ctx, c, migrate, permission)
}

// OpenAuthorityWithPermission 由进程显式传同一已消费 boot，避免 SQL 与排空身份分裂。
func OpenAuthorityWithPermission(ctx context.Context, c config.Config, migrate bool, permission authoritysql.Permission) (*sql.DB, *authority.Store, error) {
	backend, err := authoritysql.ParseBackend(c.Backend)
	if err != nil {
		return nil, nil, err
	}
	var db *sql.DB
	if migrate {
		db, err = authoritysql.OpenForMigration(ctx, os.Getenv(c.DSNEnv), authority.DatabaseName, backend, permission)
	} else {
		db, err = authoritysql.Open(ctx, os.Getenv(c.DSNEnv), authority.DatabaseName, backend, c.MaxOpenConns, permission)
	}
	if err != nil {
		return nil, nil, err
	}
	if migrate {
		if err = authority.Migrate(ctx, db); err != nil {
			db.Close()
			return nil, nil, err
		}
		return db, nil, nil
	}
	store, err := authority.New(ctx, db, c.AccountRoleSchema, backend, authority.Rules{MaxContentBytes: c.MaxContentBytes, RateLimitPerSecond: c.RateLimitPerSecond, HistoryMaxEntries: c.HistoryMaxEntries, HistoryTTLSeconds: c.HistoryTTLSeconds, HistoryDefaultLimit: c.HistoryDefaultLimit, HistoryMaxLimit: c.HistoryMaxLimit, SensitiveWords: c.SensitiveWords, ActorVerifyTTLSeconds: c.ActorVerifyTTLSeconds})
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, store, nil
}

func openLoginSessions(ctx context.Context, c config.Config) (redis.UniversalClient, error) {
	if !c.GlobalLoginSessions || !c.LoginRedis.HasAddr() || c.LoginRedis.DB < 0 || c.LoginRedis.PoolSizePerNode < 1 || c.LoginRedis.PoolSizePerNode > 128 {
		return nil, errors.New("聊天全局原登录态依赖缺失")
	}
	r, err := playerredis.New(c.LoginRedis.ClientConfig(playerredis.Config{DialTimeoutMs: 1000, ReadTimeoutMs: 1000, WriteTimeoutMs: 1000, PoolTimeoutMs: 1000}))
	if err != nil {
		return nil, errors.New("聊天全局原登录态配置非法")
	}
	if err := r.Ping(ctx).Err(); err != nil {
		r.Close()
		return nil, errors.New("聊天全局原登录态不可用")
	}
	return r, nil
}
