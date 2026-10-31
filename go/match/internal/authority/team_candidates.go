package authority

import (
	"context"
	"database/sql"
	"errors"
	"match/internal/config"
	"strings"

	pb "proto/match"
	teampb "proto/team"
)

type TeamLaunchCursor struct{ EnqueuedMs, ActorId, RequestHigh, RequestLow uint64 }

func (s *Store) TeamLaunchRepairHighWater(ctx context.Context) (TeamLaunchCursor, error) {
	var key TeamLaunchCursor
	err := s.DB.QueryRowContext(ctx, "SELECT enqueued_ms,actor_id,request_high,request_low FROM team_launch WHERE state=2 ORDER BY enqueued_ms DESC,actor_id DESC,request_high DESC,request_low DESC LIMIT 1").Scan(&key.EnqueuedMs, &key.ActorId, &key.RequestHigh, &key.RequestLow)
	if errors.Is(err, sql.ErrNoRows) {
		return TeamLaunchCursor{}, nil
	}
	return key, err
}

func (s *Store) RepairTeamLaunchProjectionThrough(ctx context.Context, after, through TeamLaunchCursor) (TeamLaunchCursor, bool, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE state=2 AND (enqueued_ms,actor_id,request_high,request_low)>(?,?,?,?) AND (enqueued_ms,actor_id,request_high,request_low)<=(?,?,?,?) ORDER BY enqueued_ms,actor_id,request_high,request_low LIMIT 32", after.EnqueuedMs, after.ActorId, after.RequestHigh, after.RequestLow, through.EnqueuedMs, through.ActorId, through.RequestHigh, through.RequestLow)
	if err != nil {
		return after, false, err
	}
	var launches []*teampb.TeamLaunchRecord
	for rows.Next() {
		launch, err := scanTeamLaunch(rows)
		if err != nil {
			rows.Close()
			return after, false, err
		}
		launches = append(launches, launch)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return after, false, err
	}
	if len(launches) == 0 {
		return TeamLaunchCursor{}, true, nil
	}
	for _, launch := range launches {
		if _, err = s.MaintainQueuedTeam(ctx, launch); err != nil && !errors.Is(err, ErrOriginalPlayer) {
			return after, false, err
		}
		after = TeamLaunchCursor{EnqueuedMs: launch.EnqueuedMs, ActorId: launch.ActorId, RequestHigh: launch.RequestHigh, RequestLow: launch.RequestLow}
	}
	return after, false, nil
}

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

func (s *Store) TeamSoloAnchorsAfter(ctx context.Context, afterMs, afterPlayer uint64) ([]*pb.MatchAuthorityTicketRecord, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE state=1 AND mode IN(?,?) AND team_id=0 AND (enqueued_ms>? OR (enqueued_ms=? AND player_id>?)) ORDER BY enqueued_ms,player_id LIMIT 32", uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM), uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM), afterMs, afterMs, afterPlayer)
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

// 每个size桶资格过滤后至多5个完整unit；不按玩家LIMIT再拼回残队。
func (s *Store) TeamLaunchPeers(ctx context.Context, anchor *pb.MatchAuthorityTicketRecord, exclude *teampb.TeamLaunchRecord, region, size uint32, local bool, stamp uint64) ([]*teampb.TeamLaunchRecord, error) {
	if anchor == nil || !validTeamQueueMode(anchor.Mode) || region == 0 || size < 1 || size > config.TeamCapacity {
		return nil, ErrDirectConflict
	}
	columns := strings.Split(teamLaunchColumns, ",")
	for index := range columns {
		columns[index] = "l." + columns[index]
	}
	query := "SELECT " + strings.Join(columns, ",") + " FROM team_launch_region r JOIN team_launch l ON l.actor_id=r.actor_id AND l.request_high=r.request_high AND l.request_low=r.request_low AND l.enqueued_ms=r.enqueued_ms AND l.config_id=r.config_id AND l.target_size=r.target_size AND l.member_count=r.member_count AND r.mode=IF(l.mode=0,5,l.mode) WHERE r.physical_region=? AND r.mode=? AND r.config_id=? AND r.target_size=? AND r.member_count=? AND r.expires_ms>? AND r.max_rtt_ms<=? AND l.state=2 AND l.not_before_ms<=?"
	args := []any{region, anchor.Mode, anchor.ConfigId, anchor.TeamSize, size, stamp, s.Config.HardLatencyMs, stamp}
	if !s.TeamConfig.AllowCrossZone {
		if anchor.ZoneId == 0 {
			return nil, ErrTeamRoute
		}
		query += " AND r.home_zone=?"
		args = append(args, anchor.ZoneId)
	}
	if local {
		query += " AND r.local_preferred=1"
	} else {
		wait := uint64(s.Config.CrossRegionWaitSeconds) * 1000
		if stamp < wait {
			return nil, nil
		}
		query += " AND r.enqueued_ms<=?"
		args = append(args, stamp-wait)
	}
	if exclude != nil {
		query += " AND NOT(l.actor_id=? AND l.request_high=? AND l.request_low=?)"
		args = append(args, exclude.ActorId, exclude.RequestHigh, exclude.RequestLow)
	}
	query += " ORDER BY r.enqueued_ms,r.actor_id,r.request_high,r.request_low LIMIT ?"
	args = append(args, config.TeamCapacity)
	rows, err := s.DB.QueryContext(ctx, query, args...)
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
