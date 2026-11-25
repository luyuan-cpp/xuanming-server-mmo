package store

import (
	"context"
	"database/sql"
	"errors"

	"shared/accountdirectory"
)

const AccountRoleTradableQuotaTableName = "account_role_tradable_quota"

// Stage1TradableMaterialLifetimeLimit 是单账号跨角色终身阶段一可交易装备材料硬上限（删角不重置）。
const Stage1TradableMaterialLifetimeLimit uint32 = 60

const accountRoleTradableQuotaDDL = "CREATE TABLE IF NOT EXISTS account_role_tradable_quota (" +
	"account VARBINARY(191) NOT NULL COMMENT 'pb:1'," +
	"player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:2'," +
	"confirmed_units INT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:3'," +
	"updated_ms BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:4'," +
	"PRIMARY KEY(account,player_id)) ENGINE=InnoDB"

// SyncAccountStage1Tradable 在账号分片单片事务内幂等确认单角色的阶段一可交易装备材料份数，
// 并累加到 account_directory.stage1_tradable_material_used（跨角色/删角共享终身 60 份硬上限）。
// 角色行与账号行在不同分片：先在玩家分片点读归属账号（同 RealNameOfPlayer），再在账号分片单事务加行锁更新。
func (s *AccountDirectoryStore) SyncAccountStage1Tradable(ctx context.Context, playerID uint64, desiredCharTotal uint32) (found bool, confirmedChar uint32, accountUsed uint32, err error) {
	if playerID == 0 || desiredCharTotal > Stage1TradableMaterialLifetimeLimit {
		return false, 0, 0, accountdirectory.ErrInvalid
	}
	owned, err := s.Lookup(ctx, playerID)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return false, 0, 0, nil
	}
	if err != nil {
		return false, 0, 0, err
	}
	accountBytes := []byte(owned.Account)
	err = shardTx(ctx, s.shards.forAccount(owned.Account), func(tx *sql.Tx) error {
		var used uint32
		if scanErr := tx.QueryRowContext(ctx,
			"SELECT stage1_tradable_material_used FROM account_directory WHERE account=? FOR UPDATE",
			accountBytes).Scan(&used); scanErr != nil {
			if errors.Is(scanErr, sql.ErrNoRows) {
				return accountdirectory.ErrMissing
			}
			return scanErr
		}
		if used > Stage1TradableMaterialLifetimeLimit {
			used = Stage1TradableMaterialLifetimeLimit
		}
		var prevConfirmed uint32
		if scanErr := tx.QueryRowContext(ctx,
			"SELECT confirmed_units FROM "+AccountRoleTradableQuotaTableName+" WHERE account=? AND player_id=? FOR UPDATE",
			accountBytes, playerID).Scan(&prevConfirmed); scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}
		confirmedChar = prevConfirmed
		accountUsed = used
		if desiredCharTotal > prevConfirmed && used < Stage1TradableMaterialLifetimeLimit {
			want := desiredCharTotal - prevConfirmed
			remaining := Stage1TradableMaterialLifetimeLimit - used
			grant := min(want, remaining)
			if grant > 0 {
				confirmedChar = prevConfirmed + grant
				accountUsed = used + grant
				if _, execErr := tx.ExecContext(ctx,
					"INSERT INTO "+AccountRoleTradableQuotaTableName+
						"(account,player_id,confirmed_units,updated_ms) VALUES (?,?,?,"+dbNowMsSQL+") "+
						"ON DUPLICATE KEY UPDATE confirmed_units=VALUES(confirmed_units),updated_ms=VALUES(updated_ms)",
					accountBytes, playerID, confirmedChar); execErr != nil {
					return execErr
				}
				if _, execErr := tx.ExecContext(ctx,
					"UPDATE account_directory SET stage1_tradable_material_used=? WHERE account=?",
					accountUsed, accountBytes); execErr != nil {
					return execErr
				}
			}
		}
		found = true
		return nil
	})
	if errors.Is(err, accountdirectory.ErrMissing) {
		return false, 0, 0, nil
	}
	if err != nil {
		return false, 0, 0, err
	}
	return found, confirmedChar, accountUsed, nil
}
