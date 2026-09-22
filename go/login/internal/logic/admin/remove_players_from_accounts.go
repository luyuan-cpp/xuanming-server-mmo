package admin

import (
	"context"
	"errors"

	"login/internal/constants"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	login_data_base "proto/common/database"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/zeromicro/go-zero/core/logx"
)

type RemovePlayersFromAccountsReq struct {
	PlayerIDs []uint64
}

type RemovePlayersFromAccountsResp struct {
	RemovedCount  uint32
	NotFoundCount uint32
	FailedCount   uint32
}

// RemovePlayersFromAccounts removes the given player IDs from their accounts'
// SimplePlayers lists. Uses the player_to_account:{player_id} reverse mapping
// written by CreatePlayer to find each player's owning account.
//
// Intended for post-rollback orphan cleanup.
func RemovePlayersFromAccounts(ctx context.Context, svcCtx *svc.ServiceContext, req *RemovePlayersFromAccountsReq) (*RemovePlayersFromAccountsResp, error) {
	logx.Infof("[RemovePlayersFromAccounts] starting: %d player IDs to process: %v", len(req.PlayerIDs), req.PlayerIDs)
	resp := &RemovePlayersFromAccountsResp{}

	// Group player IDs by account to batch account updates
	accountToOrphans := make(map[string][]uint64)

	for _, pid := range req.PlayerIDs {
		reverseKey := constants.PlayerToAccountKey(pid)
		account, err := svcCtx.RedisClient.Get(ctx, reverseKey).Result()
		if errors.Is(err, redis.Nil) {
			logx.Infof("[RemovePlayersFromAccounts] no reverse mapping for player %d (created before reverse index existed)", pid)
			resp.NotFoundCount++
			continue
		}
		if err != nil {
			logx.Errorf("[RemovePlayersFromAccounts] failed to get reverse mapping for player %d: %v", pid, err)
			resp.FailedCount++
			continue
		}
		accountToOrphans[account] = append(accountToOrphans[account], pid)
	}

	// Process each account
	for account, orphanIDs := range accountToOrphans {
		removed, err := removePlayersFromAccount(ctx, svcCtx, account, orphanIDs)
		if err != nil {
			logx.Errorf("[RemovePlayersFromAccounts] account %s: %v", account, err)
			resp.FailedCount += uint32(len(orphanIDs))
			continue
		}
		resp.RemovedCount += uint32(removed)

		// Clean up reverse mappings for successfully removed players
		for _, pid := range orphanIDs {
			reverseKey := constants.PlayerToAccountKey(pid)
			if err := svcCtx.RedisClient.Del(ctx, reverseKey).Err(); err != nil {
				logx.Errorf("[RemovePlayersFromAccounts] failed to delete reverse mapping for player %d: %v", pid, err)
			} else {
				logx.Infof("[RemovePlayersFromAccounts] deleted reverse mapping: player_to_account:%d -> account=%s", pid, account)
			}
		}
	}

	logx.Infof("[RemovePlayersFromAccounts] done: removed=%d not_found=%d failed=%d",
		resp.RemovedCount, resp.NotFoundCount, resp.FailedCount)
	return resp, nil
}

// removePlayersFromAccount loads an account's SimplePlayers, removes the given
// player IDs, and writes back. Returns the number of players actually removed.
func removePlayersFromAccount(ctx context.Context, svcCtx *svc.ServiceContext, account string, playerIDs []uint64) (int, error) {
	accountKey := constants.GetAccountDataKey(account)

	data, err := svcCtx.RedisClient.Get(ctx, accountKey).Bytes()
	if errors.Is(err, redis.Nil) {
		logx.Infof("[RemovePlayersFromAccounts] account %s not found in Redis (expired?)", account)
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	userAccount := &login_data_base.UserAccounts{}
	if err := proto.Unmarshal(data, userAccount); err != nil {
		return 0, err
	}

	if userAccount.SimplePlayers == nil {
		return 0, nil
	}

	// Build removal set
	removeSet := make(map[uint64]bool, len(playerIDs))
	for _, pid := range playerIDs {
		removeSet[pid] = true
	}

	// Filter out orphan players
	original := userAccount.SimplePlayers.Players
	filtered := make([]*login_proto_common.AccountSimplePlayer, 0, len(original))
	removed := 0
	for _, p := range original {
		if removeSet[p.PlayerId] {
			removed++
			logx.Infof("[RemovePlayersFromAccounts] account %s: removing orphan player %d", account, p.PlayerId)
			continue
		}
		filtered = append(filtered, p)
	}

	if removed == 0 {
		return 0, nil
	}

	logx.Infof("[RemovePlayersFromAccounts] account %s: before=%d players, after=%d players, removing %d",
		account, len(original), len(filtered), removed)
	userAccount.SimplePlayers.Players = filtered

	// Write back
	updatedData, err := proto.Marshal(userAccount)
	if err != nil {
		return 0, err
	}
	if err := svcCtx.RedisClient.Set(ctx, accountKey, updatedData, 0).Err(); err != nil {
		return 0, err
	}

	logx.Infof("[RemovePlayersFromAccounts] account %s: successfully updated, removed %d orphan characters", account, removed)
	return removed, nil
}
