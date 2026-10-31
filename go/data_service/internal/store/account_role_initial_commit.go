package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	base "proto/common/base"
	"shared/accountdirectory"
	"shared/placement"
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

func assertInitialPlayerCommitShape(ctx context.Context, db *sql.DB, schema string) error {
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

// ConfigureInitialPlayerCommit 与现有跨库 SQL 证据共用全局权威池。
// 白名单只限制可访问库，实际落点必须来自同一事务锁住的 player_route，绝不由 caller 指定。
func (s *AccountDirectoryStore) ConfigureInitialPlayerCommit(storageIDs []uint32) error {
	if s == nil || len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return accountdirectory.ErrUnavailable
	}
	allowed := make(map[uint32]bool, len(storageIDs))
	for _, id := range storageIDs {
		if _, ok := placement.StoreDBName(id); !ok || allowed[id] {
			return accountdirectory.ErrInvalid
		}
		allowed[id] = true
	}
	s.initialStores = allowed
	return nil
}

// lockInitialPlayerRoute 必须在获取角色归属行锁之前调用。
// 复用正式角色路由前缀:只锁这名玩家自己的路由行并复核,再允许调用者获取角色 P 和玩家行。
//
// route_meta.writable 是维护开关:维护期间冻结路由写者。
// 这里只普通读,不加锁:每次建角都对这一行 FOR UPDATE 的话全服建角排成一列
// (docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条)。
//
// 读到旧值的窗口:读到 writable=1 之后维护才开始冻结,这笔建角仍可能提交。提交点的保证不靠这条读,靠写者闸门:
// 事务从 BEGIN 起持着本写者的 writer_slot 行锁,持锁后复核过 route_meta(authoritysql/writer.go),而 Catalog.Freeze 推进
// epoch 要先拿到所有在册写者的 slot 行锁,所以冻结只能排在这笔事务提交之后,之后的新事务在闸门上就被拒。route_tool 的冻结
// 流程走 Catalog。这条普通读只负责把「已冻结」翻译成 ErrRouteNotWritable,
// 并覆盖不走写者闸门的连接池;没有闸门时窗口是一笔事务的时长,与 RegisterHome(W1)的 requireRouteWritable 同口径。
func (s *AccountDirectoryStore) lockInitialPlayerRoute(ctx context.Context, tx *sql.Tx, role *base.AccountSimplePlayer) error {
	if len(s.initialStores) == 0 || role.GetZoneId() == 0 {
		return accountdirectory.ErrUnavailable
	}
	if err := requireRouteWritable(ctx, tx); err != nil {
		return err
	}
	return placement.LockRoleRoutes(ctx, tx, s.schema, []uint64{role.PlayerId})
}

// insertInitialPlayerRows 写新角色的 player_store 初始行,scene 进场时从这些行认领。
func (s *AccountDirectoryStore) insertInitialPlayerRows(ctx context.Context, tx *sql.Tx, role *base.AccountSimplePlayer) error {
	var row RouteRow
	if err := scanRouteRow(tx.QueryRowContext(ctx, routeLockRowSQL, placement.RouteRowShard(role.PlayerId), role.PlayerId), &row, false); err != nil {
		return err
	}
	if row.HomeZone != role.ZoneId || row.Rev == 0 || row.FrozenRunID != "" {
		return accountdirectory.ErrConflict
	}
	// 建角恒写落点,和 go/db 一样不回落 home_zone:猜一个库写初始行,比拒绝更糟。
	if !row.HasPlacement() {
		return accountdirectory.ErrConflict
	}
	storage := row.StorageID
	if !s.initialStores[storage] {
		return accountdirectory.ErrUnavailable
	}
	schema, ok := placement.StoreDBName(storage)
	if !ok {
		return accountdirectory.ErrInvalid
	}
	return insertInitialPlayerStoreRows(ctx, tx, schema, role)
}
