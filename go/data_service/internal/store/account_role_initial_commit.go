package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// 只由正式迁移调用。旧角色只取得默认 0，绝不从现有 1:1 数字推断出生资格。
func ensureInitialPlayerCommitSchema(ctx context.Context, db *sql.DB, schema string) error {
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='account_role' AND COLUMN_NAME='initial_player_commit'", schema).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := db.ExecContext(ctx, "ALTER TABLE account_role ADD COLUMN initial_player_commit BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:7'"); err != nil {
			return err
		}
	}
	return assertInitialPlayerCommitShape(ctx, db, schema)
}

func assertInitialPlayerCommitShape(ctx context.Context, db schemaReader, schema string) error {
	var dataType, columnType, nullable string
	var defaultValue sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT DATA_TYPE,COLUMN_TYPE,IS_NULLABLE,COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='account_role' AND COLUMN_NAME='initial_player_commit'", schema).Scan(&dataType, &columnType, &nullable, &defaultValue); err != nil {
		return err
	}
	if dataType != "bigint" || !strings.Contains(columnType, "unsigned") || nullable != "NO" || !defaultValue.Valid || defaultValue.String != "0" {
		return errors.New("角色初始持久提交标记列形状不符")
	}
	return nil
}

// ConfigurePlayerStores 接上存档库连接池。白名单只限制可访问的库,实际落点必须来自权威路由,绝不由 caller 指定。
// 不配的话建角、证据、判死、账本和展示读都拒绝。
func (s *AccountDirectoryStore) ConfigurePlayerStores(storageIDs []uint32, options PlayerStorePoolOptions) error {
	if s == nil {
		return errors.New("账号目录未装配")
	}
	stores, err := newPlayerStorePools(s.cfg, s.backend, storageIDs, options)
	if err != nil {
		return err
	}
	s.stores = stores
	// 读名额按存档库的池算:目录的池是每片的上限,K 大的时候收得很小,按它算会把整个进程的账本和展示读压成一条。
	s.playerStoreReads = make(chan struct{}, playerStoreReadLimit(stores.options.MaxOpenConn))
	return nil
}
