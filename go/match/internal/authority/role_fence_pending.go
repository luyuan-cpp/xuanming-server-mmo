package authority

import (
	"context"
	"database/sql"
	"errors"

	rfpb "proto/role_fence"
	"shared/matchentry"
)

const fenceNowMsSQL = "CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED)"

// RoleFencePending 是 match 的挂起全集,在 PrepareRoleFreeze 的事务里跑(docs/design/horizontal-scale-logical-db.md §3.4)。
// 语句和判据从目录原来的跨库守卫原样搬来:未知状态、NULL 一律阻塞,查询失败回错误,都不算通过。
// 此时已持有这名角色的副本行锁,新的匹配参与者排在后面。
//
// 票 state=1/2/3 即使墙钟过期也阻塞,只有 Match 自己提交 state=4 才释放,本域不按 TTL 猜终局。
// 先 Cancel 后迟到 Join 的 state=0 空占位只有在全部业务字段为空时才放行。
// 成员和同意原件属于原账号,角色过户或删除前必须先解除,不能由新主补权。
// 切磋邀请:state=1 且没过期、未知状态、NULL 阻塞双方;过期的 PENDING 不能再接受,state=2 已交接到票和成局,state=3 已拒绝。
// 占用过期视为空闲,所以这里只看未过期的行。
func RoleFencePending(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
	player := state.PlayerId
	for _, pending := range []struct {
		blocker string
		query   string
		args    []any
	}{
		{"match_authority_ticket", "SELECT player_id FROM match_authority_ticket WHERE player_id=? AND " + matchentry.TicketOccupiedSQL + " LIMIT 1 FOR UPDATE", []any{player}},
		{"team_membership", "SELECT player_id FROM team_membership WHERE player_id=? AND (team_id IS NULL OR team_id<>0) LIMIT 1 FOR UPDATE", []any{player}},
		{"team_invite_index", "SELECT team_id FROM team_invite_index WHERE invitee_id=? AND (expires_ms IS NULL OR expires_ms>" + fenceNowMsSQL + ") LIMIT 1 FOR UPDATE", []any{player}},
		{"team_application_index", "SELECT team_id FROM team_application_index WHERE applicant_id=? AND (expires_ms IS NULL OR expires_ms>" + fenceNowMsSQL + ") LIMIT 1 FOR UPDATE", []any{player}},
		{"team_launch_role", "SELECT r.player_id FROM team_launch_role r LEFT JOIN team_launch l ON l.actor_id=r.actor_id AND l.request_high=r.request_high AND l.request_low=r.request_low WHERE r.player_id=? AND (r.state IS NULL OR r.state<>5 OR l.actor_id IS NULL OR l.state IS NULL OR l.state<>5) LIMIT 1 FOR UPDATE", []any{player}},
		{"match_spectator", "SELECT player_id FROM match_spectator WHERE player_id=? AND (state IS NULL OR state<>5) LIMIT 1 FOR UPDATE", []any{player}},
		{"match_challenge_invitation", "SELECT challenge_id FROM match_challenge_invitation WHERE (challenger_player_id=? OR target_player_id=?) AND (state IS NULL OR state NOT IN (1,2,3,5) OR (state=1 AND (expires_ms IS NULL OR expires_ms>" + fenceNowMsSQL + "))) LIMIT 1 FOR UPDATE", []any{player, player}},
		{"match_role_occupancy", "SELECT player_id FROM match_role_occupancy WHERE player_id=? AND (expires_ms IS NULL OR expires_ms>" + fenceNowMsSQL + ") LIMIT 1 FOR UPDATE", []any{player}},
	} {
		var found uint64
		err := tx.QueryRowContext(ctx, pending.query, pending.args...).Scan(&found)
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
