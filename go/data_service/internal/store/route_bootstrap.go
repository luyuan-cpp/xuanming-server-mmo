package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// HasAnyRoute 只判正规 SQL 是否为空,逐片问;不以进程记忆或本实例 zone 代替。
func (s *RouteStore) HasAnyRoute(ctx context.Context) (bool, error) {
	for _, db := range s.pools.pools {
		var present int
		err := db.QueryRowContext(ctx, "SELECT 1 FROM player_route LIMIT 1").Scan(&present)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

// HasHistoricalPlayers 给空路由启动判据用:路由全空时,目录或任一已配置落点库有角色历史都说明不是新空环境。
// GoDB 的正规迁移给每个落点库注册同一表清单,含 centre 与旧 user_accounts;不查询独立 PasswordAuth DSN。
// 每个 EXISTS 只读 LIMIT 1,密码账户的 NULL / 空角色目录不算玩家;非空原 blob 未知也不能证明为空。
// 每个落点走自己的连接池发一条查询;缺库 / 表 / 列与超时同样不能证明为空。
func (s *AccountDirectoryStore) HasHistoricalPlayers(ctx context.Context, storageIDs []uint32) (bool, error) {
	if len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return false, fmt.Errorf("玩家存储白名单缺失或越界")
	}
	for _, db := range s.shards.pools {
		found, err := anyRow(ctx, db, "SELECT 1 FROM account_role LIMIT 1")
		if found || err != nil {
			return found, err
		}
	}
	for _, id := range storageIDs {
		db, _, err := s.stores.DB(ctx, id)
		if err != nil {
			return false, err
		}
		found, err := anyRow(ctx, db, "SELECT 1 WHERE EXISTS(SELECT 1 FROM `player_store` LIMIT 1) OR "+
			"EXISTS(SELECT 1 FROM `player_centre_database` LIMIT 1) OR "+
			"EXISTS(SELECT 1 FROM `user_accounts` WHERE simple_players IS NOT NULL AND OCTET_LENGTH(simple_players)>0 LIMIT 1)")
		if found || err != nil {
			return found, err
		}
	}
	return false, nil
}

func anyRow(ctx context.Context, db *sql.DB, query string) (bool, error) {
	var present int
	err := db.QueryRowContext(ctx, query).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
