package authority

import (
	"context"

	"match/internal/catalog"

	pb "proto/match"
	teampb "proto/team"
)

// 按池读候选（docs/design/global-match-hierarchical-pools.md §5.3）。只读、不加锁：
// 读到的是撮合的输入，成局仍由 claimTx / ClaimTeamUnits 在锁内复核原票。

// PoolCursor 是池内的排队序位置（原始入队时刻, player_id），只用于翻页调度，不参与任何提交决策。
type PoolCursor struct{ EnqueuedMs, PlayerId uint64 }

// PoolTickets 按排队序读一页单排票（team_id=0，整队成员由 PoolTeamLaunches 带出）。
// 不过滤过期票：过期票集中在队首，由调用方收集后交给 ExpireTickets 终止。
// 使用索引 (state,mode,config_id,team_size,team_id,enqueued_ms,player_id)。
func (s *Store) PoolTickets(ctx context.Context, pool catalog.Pool, after PoolCursor, limit int) ([]*pb.MatchAuthorityTicketRecord, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE state=1 AND mode=? AND config_id=? AND team_size=? AND team_id=0 AND (enqueued_ms>? OR (enqueued_ms=? AND player_id>?)) ORDER BY enqueued_ms,player_id LIMIT ?",
		uint32(pool.Mode), pool.ConfigId, pool.TeamSize, after.EnqueuedMs, after.EnqueuedMs, after.PlayerId, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchAuthorityTicketRecord
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ticket)
	}
	return out, rows.Err()
}

// PoolTeamLaunches 按排队序读排队中（state=2）的整队。只有 PVE_TEAM / PVP_TEAM 池有整队；
// mode=0 是旧 PVE 原件的写法，按 PVE_TEAM 解释（与 TeamLaunchMode 同口径）。
// 使用索引 (state,mode,config_id,target_size,enqueued_ms,actor_id,request_high,request_low)。
func (s *Store) PoolTeamLaunches(ctx context.Context, pool catalog.Pool, limit int) ([]*teampb.TeamLaunchRecord, error) {
	modes := []any{uint32(pool.Mode), uint32(pool.Mode)}
	switch pool.Mode {
	case pb.MatchMode_MATCH_MODE_PVE_TEAM:
		modes[1] = uint32(0)
	case pb.MatchMode_MATCH_MODE_PVP_TEAM:
	default:
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE state=2 AND mode IN(?,?) AND config_id=? AND target_size=? ORDER BY enqueued_ms,actor_id,request_high,request_low LIMIT ?",
		modes[0], modes[1], pool.ConfigId, pool.TeamSize, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*teampb.TeamLaunchRecord
	for rows.Next() {
		launch, err := scanTeamLaunch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, launch)
	}
	return out, rows.Err()
}

// ExpireTickets 终止已过期的单排票（state 1 → 4），按票号 CAS，只命中仍过期、仍在排队的那一张。
// 与 Ticket() 里本人读票时的惰性过期同一语义，不需要玩家锁：过期票不会再被续命（续命要先持锁并
// 校验原票），也不会被提交（claimTx 在锁内复核 expires_ms）。返回实际终止的张数。
func (s *Store) ExpireTickets(ctx context.Context, tickets []*pb.MatchAuthorityTicketRecord) (int, error) {
	expired := 0
	for _, ticket := range tickets {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return expired, err
		}
		r, err := tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND state=1 AND team_id=0 AND expires_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)", ticket.PlayerId, ticket.Ticket)
		if err == nil {
			_, err = tx.ExecContext(ctx, "DELETE FROM match_ticket_region WHERE player_id=? AND ticket=?", ticket.PlayerId, ticket.Ticket)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			_ = tx.Rollback()
			return expired, err
		}
		if n, _ := r.RowsAffected(); n == 1 {
			expired++
		}
	}
	return expired, nil
}
