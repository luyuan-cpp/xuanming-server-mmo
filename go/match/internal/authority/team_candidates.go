package authority

import (
	"context"
	"database/sql"
	"errors"

	pb "proto/match"
	teampb "proto/team"
)

type TeamLaunchCursor struct{ EnqueuedMs, ActorId, RequestHigh, RequestLow uint64 }

func (s *Store) TeamLaunchesAfter(ctx context.Context, state uint32, after TeamLaunchCursor) ([]*teampb.TeamLaunchRecord, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE state=? AND (enqueued_ms,actor_id,request_high,request_low)>(?,?,?,?) ORDER BY enqueued_ms,actor_id,request_high,request_low LIMIT 32", state, after.EnqueuedMs, after.ActorId, after.RequestHigh, after.RequestLow)
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

func (s *Store) TeamLaunchTickets(ctx context.Context, launch *teampb.TeamLaunchRecord) ([]*pb.MatchAuthorityTicketRecord, error) {
	data, err := DecodeTeamLaunch(launch)
	if err != nil {
		return nil, err
	}
	// 不设 ReadOnly:写者资格门要在同一事务里锁 writer_slot,MySQL 的 READ ONLY 事务里 FOR UPDATE / UPDATE 都是 1792。
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var out []*pb.MatchAuthorityTicketRecord
	for _, member := range data.Members {
		ticket, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=?", member.Original.PlayerId))
		if err != nil {
			return nil, err
		}
		if ticket.State != 1 || ticket.Ticket != member.Ticket || ticket.TeamId != launch.TeamId || ticket.OwnerAccount != member.Original.OwnerAccount || ticket.OwnerRevision != member.Original.OwnerRevision || ticket.Mode != TeamLaunchMode(launch) || ticket.ConfigId != launch.ConfigId || ticket.TeamSize != launch.TargetSize {
			return nil, ErrDirectConflict
		}
		out = append(out, ticket)
	}
	return out, tx.Commit()
}

func (s *Store) TeamQueueState(ctx context.Context, player, teamID uint64) (uint32, uint32, uint32, uint64, error) {
	ticket, err := scanTicket(s.DB.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=?", player))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if ticket.TeamId != teamID || teamID == 0 {
		return 0, 0, 0, 0, nil
	}
	if ticket.State == 1 {
		return 1, ticket.TeamSize, ticket.ConfigId, ticket.EnqueuedMs, nil
	}
	if ticket.State == 2 {
		return 2, 0, 0, 0, nil
	}
	return 0, 0, 0, 0, nil
}
