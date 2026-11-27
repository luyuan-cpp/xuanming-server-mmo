package store

import (
	"context"
	"database/sql"
	"errors"

	dspb "proto/data_service"
	"shared/accountdirectory"
)

// 第三方登录渠道身份表(docs/design/rename-and-content-safety.md §6.5):在账号分片存最近一次登录拿到的
// channel(如 wechat_minigame / douyin_minigame)与 platform open_id;content_guard 调微信 msgSecCheck v2 时按此查 openid。
const AccountChannelIdentityTableName = "account_channel_identity"

const accountChannelIdentityDDL = "CREATE TABLE IF NOT EXISTS account_channel_identity (" +
	"account VARBINARY(191) NOT NULL COMMENT 'pb:1'," +
	"channel VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'pb:2'," +
	"open_id VARCHAR(191) NOT NULL DEFAULT '' COMMENT 'pb:3'," +
	"updated_ms BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:4'," +
	"PRIMARY KEY(account)" +
	") ENGINE=InnoDB"

// UpsertChannelIdentity 在账号所在分片记录或刷新渠道与 open_id。
func (s *AccountDirectoryStore) UpsertChannelIdentity(ctx context.Context, account, channel, openID string) error {
	if !accountdirectory.ValidAccount(account) || len(channel) == 0 || len(channel) > 32 || len(openID) > 191 {
		return accountdirectory.ErrInvalid
	}
	return shardTx(ctx, s.shards.forAccount(account), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO "+AccountChannelIdentityTableName+" (account,channel,open_id,updated_ms) VALUES (?,?,?,UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000) "+
				"ON DUPLICATE KEY UPDATE channel=VALUES(channel),open_id=VALUES(open_id),updated_ms=VALUES(updated_ms)",
			[]byte(account), channel, openID)
		return err
	})
}

// ChannelIdentityOfPlayer 返回角色的账号、渠道身份、账号创建时刻(新号判断)与角色等级(世界频道门槛)。
// 目录里不存在或已删角色返回 Found=false(不报错)。
func (s *AccountDirectoryStore) ChannelIdentityOfPlayer(ctx context.Context, player uint64) (*dspb.GetPlayerChannelIdentityResponse, error) {
	if player == 0 {
		return nil, accountdirectory.ErrInvalid
	}
	owned, err := s.Lookup(ctx, player)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return &dspb.GetPlayerChannelIdentityResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}

	out, err := s.ChannelIdentityOfAccount(ctx, owned.Account)
	if err != nil {
		return nil, err
	}
	displays, err := s.ReadPlayerDisplays(ctx, []uint64{player})
	if err != nil {
		// 等级读失败整次按没读成回(服务端回 Retry):聊天按等级判世界频道门槛,拿一个 0 级回去会把人挡在外面。
		return nil, err
	}
	if d := displays[player]; d != nil {
		out.PlayerLevel = d.GetLevel()
	}
	return out, nil
}

// ChannelIdentityOfAccount 按账号返回渠道身份与账号创建时刻,不回角色等级。建角送检时角色还没发号,只能按账号查。
// 账号没有渠道身份(口令登录,或还没用第三方渠道登录过)时 channel、open_id 为空,不报错。
func (s *AccountDirectoryStore) ChannelIdentityOfAccount(ctx context.Context, account string) (*dspb.GetPlayerChannelIdentityResponse, error) {
	if !accountdirectory.ValidAccount(account) {
		return nil, accountdirectory.ErrInvalid
	}
	out := &dspb.GetPlayerChannelIdentityResponse{Found: true, Account: account}
	shard := s.shards.forAccount(account)
	err := shard.QueryRowContext(ctx,
		"SELECT created_ms FROM "+AccountDirectoryTableName+" WHERE account=?",
		[]byte(account)).Scan(&out.AccountCreatedMs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	err = shard.QueryRowContext(ctx,
		"SELECT channel,open_id FROM "+AccountChannelIdentityTableName+" WHERE account=?",
		[]byte(account)).Scan(&out.Channel, &out.OpenId)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
