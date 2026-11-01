package data

import (
	"context"
	"database/sql"
	"errors"

	rfpb "proto/role_fence"
	"shared/rolefence"
)

// RoleFencePending 是 guild 的挂起全集,在 PrepareRoleFreeze 的事务里跑(docs/design/horizontal-scale-logical-db.md §3.4)。
// 语句和判据从目录原来的跨库守卫原样搬来:未知状态、NULL 一律阻塞,查询失败回错误,都不算通过。
// 此时已持有这名角色的副本行锁,新的业务写入排在后面。
//
// 锁序:副本 → player_state → member。这里不取 guild 主表锁,所以不会和原 G→state 的锁序成环。
// guild 全部成员新增入口先锁同一副本行,状态行缺失、成员不存在都不会形成插入窗口。
//
// 必须离帮只对过户成立,删角原来就不要求离帮。
// outbox 只放行 APPLIED=2、REJECTED=3、ABORTED=4、ABANDONED=6,其中 6 是已确认 scene 没见过这个 seq,不能和 APPLIED_PARTIAL=5 混同。
func RoleFencePending(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
	player := state.PlayerId
	if !rolefence.IsRemovalToken(state.FreezeToken) {
		var row uint64
		err := tx.QueryRowContext(ctx, "SELECT player_id FROM guild_player_state WHERE player_id=? FOR UPDATE", player).Scan(&row)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		err = tx.QueryRowContext(ctx, "SELECT guild_id FROM guild_member WHERE player_id=? FOR UPDATE", player).Scan(&row)
		if err == nil {
			return "guild_member", nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	for _, pending := range []struct{ blocker, query string }{
		{"guild_asset_op", "SELECT op_id FROM guild_asset_op WHERE player_id=? AND (status NOT IN (2,3,4,6) OR status IS NULL) LIMIT 1 FOR UPDATE"},
		{"guild_trial_reward_owed", "SELECT battle_id FROM guild_trial_reward_owed WHERE player_id=? LIMIT 1 FOR UPDATE"},
		{"guild_trial_launch_role", "SELECT r.request_id FROM guild_trial_launch_role r LEFT JOIN guild_trial_launch l ON l.request_id=r.request_id WHERE r.player_id=? AND (r.state IS NULL OR r.state<>2 OR l.request_id IS NULL OR l.state IS NULL OR l.state<>3) LIMIT 1 FOR UPDATE"},
		{"guild_trial_lobby_slot", "SELECT s.player_id FROM guild_trial_lobby_slot s LEFT JOIN guild_trial_lobby l ON l.lobby_id=s.lobby_id WHERE s.player_id=? AND (s.lobby_id IS NULL OR (s.lobby_id<>0 AND (l.lobby_id IS NULL OR l.state IS NULL OR l.state<>4))) LIMIT 1 FOR UPDATE"},
	} {
		var found uint64
		err := tx.QueryRowContext(ctx, pending.query, player).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		return pending.blocker, nil
	}
	return "", nil
}
