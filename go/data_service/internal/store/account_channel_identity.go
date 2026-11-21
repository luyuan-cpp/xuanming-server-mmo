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

	out := &dspb.GetPlayerChannelIdentityResponse{
		Found:   true,
		Account: owned.Account,
	}
	shard := s.shards.forAccount(owned.Account)

	var createdMs uint64
	err = shard.QueryRowContext(ctx,
		"SELECT created_ms FROM "+AccountDirectoryTableName+" WHERE account=?",
		[]byte(owned.Account)).Scan(&createdMs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out.AccountCreatedMs = createdMs

	var channel, openID string
	err = shard.QueryRowContext(ctx,
		"SELECT channel,open_id FROM "+AccountChannelIdentityTableName+" WHERE account=?",
		[]byte(owned.Account)).Scan(&channel, &openID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out.Channel = channel
	out.OpenId = openID

	if displays, dispErr := s.ReadPlayerDisplays(ctx, []uint64{player}); dispErr == nil {
		if d := displays[player]; d != nil {
			out.PlayerLevel = d.GetLevel()
		}
	} else {
		// 等级读失败不应阻断 content_guard 拿 open_id;调用方按需判 retry 或重查。
		return nil, dispErr
	}
	return out, nil
}
