package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"shared/placement"
)

// HasAnyRoute只判正规SQL是否为空；不以缓存、进程记忆或本实例zone替代历史导入证明。
func (s *RouteStore) HasAnyRoute(ctx context.Context) (bool, error) {
	var present int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM player_route LIMIT 1").Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// 初始SQL路由为空时，目录或任一已配置正规落点库有角色历史都说明不是新空环境。
// GoDB的正规迁移给每个落点库注册同一表清单，含centre与旧user_accounts；不查询独立PasswordAuth DSN。
// 每个EXISTS只读LIMIT1，密码账户的NULL/空角色目录不算玩家；非空原blob未知也不能证明为空。
// 每个落点只发一条查询，库名仅由已校验白名单派生；缺库/表/列与超时同样不能证明为空。
func (s *RouteStore) HasHistoricalPlayers(ctx context.Context, storageIDs []uint32) (bool, error) {
	if len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return false, fmt.Errorf("玩家存储白名单缺失或越界")
	}
	queries := []string{"SELECT 1 FROM account_role LIMIT 1"}
	for _, id := range storageIDs {
		name, ok := placement.StoreDBName(id)
		if !ok {
			return false, fmt.Errorf("玩家存储白名单非法")
		}
		prefix := "`" + name + "`."
		queries = append(queries, "SELECT 1 WHERE EXISTS(SELECT 1 FROM "+prefix+"`player_store` LIMIT 1) OR "+
			"EXISTS(SELECT 1 FROM "+prefix+"`player_centre_database` LIMIT 1) OR "+
			"EXISTS(SELECT 1 FROM "+prefix+"`user_accounts` WHERE simple_players IS NOT NULL AND OCTET_LENGTH(simple_players)>0 LIMIT 1)")
	}
	for _, query := range queries {
		var present int
		err := s.db.QueryRowContext(ctx, query).Scan(&present)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}
