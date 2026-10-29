package authority

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"google.golang.org/protobuf/proto"
	pb "proto/match"
	teampb "proto/team"
)

// MaintainQueuedTeam 永不按成员缓存缺失拆队；只用SQL原票/原profile到期撤整单。
func (s *Store) MaintainQueuedTeam(ctx context.Context, observed *teampb.TeamLaunchRecord) (bool, error) {
	data, err := DecodeTeamLaunch(observed)
	if err != nil {
		return false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = s.LockTeamOriginals(ctx, tx, teamLaunchOriginals(data)); err != nil {
		return false, err
	}
	current, _, _, err := s.lockTeamLaunch(ctx, tx, observed)
	if err != nil {
		return false, err
	}
	if current.State != 2 {
		return false, nil
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return false, err
	}
	var group []*pb.MatchAuthorityTicketRecord
	expired := false
	for _, member := range data.Members {
		ticket, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", member.Original.PlayerId))
		if err != nil {
			return false, err
		}
		if ticket.Ticket != member.Ticket || ticket.State != 1 || ticket.TeamId != current.TeamId {
			return false, ErrDirectConflict
		}
		expired = expired || ticket.ExpiresMs <= stamp
		group = append(group, ticket)
	}
	if expired {
		if err = s.closeQueuedTeamLaunchTx(ctx, tx, current); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	if err = s.replaceTeamLaunchRegions(ctx, tx, current, group, stamp); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

func (s *Store) WithdrawObservedTeamQueue(ctx context.Context, observed *teampb.TeamLaunchRecord) (bool, error) {
	data, err := DecodeTeamLaunch(observed)
	if err != nil {
		return false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = s.LockTeamOriginals(ctx, tx, teamLaunchOriginals(data)); err != nil {
		return false, err
	}
	current, _, _, err := s.lockTeamLaunch(ctx, tx, observed)
	if err != nil {
		return false, err
	}
	if current.State != 2 {
		return false, nil
	}
	if err = s.closeQueuedTeamLaunchTx(ctx, tx, current); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) CancelTeamQueueForActor(ctx context.Context, actor uint64, observed *pb.MatchAuthorityTicketRecord) ([]uint64, error) {
	if observed == nil || observed.TeamId == 0 || observed.PlayerId != actor || observed.State != 1 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE team_id=? AND state=2 ORDER BY enqueued_ms,actor_id,request_high,request_low LIMIT 1", observed.TeamId)
	if err != nil {
		return nil, err
	}
	var launch *teampb.TeamLaunchRecord
	if rows.Next() {
		launch, err = scanTeamLaunch(rows)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	if launch == nil {
		return nil, nil
	}
	data, err := DecodeTeamLaunch(launch)
	if err != nil {
		return nil, err
	}
	original := originalPlayer(ctx, actor)
	authorized := false
	var members []uint64
	for _, member := range data.Members {
		members = append(members, member.Original.PlayerId)
		if member.Original.PlayerId == actor && member.Ticket == observed.Ticket && member.Original.OwnerAccount == original.OwnerAccount && member.Original.OwnerRevision == original.OwnerRevision {
			authorized = true
		}
	}
	if !authorized {
		return nil, ErrOriginalPlayer
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.LockTeamOriginals(ctx, tx, teamLaunchOriginals(data)); err != nil {
		return nil, err
	}
	current, _, _, err := s.lockTeamLaunch(ctx, tx, launch)
	if err != nil {
		return nil, err
	}
	if current.State != 2 {
		return nil, nil
	}
	actual, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", actor))
	if err != nil {
		return nil, err
	}
	if actual.Ticket != observed.Ticket || actual.State != 1 || actual.FormationId != 0 {
		return nil, nil
	}
	if err = s.closeQueuedTeamLaunchTx(ctx, tx, current); err != nil {
		return nil, err
	}
	return members, tx.Commit()
}

// TeamLaunchByToken 仅用于恢复调度；它本身不授权清锁或开战。
func (s *Store) TeamLaunchByToken(ctx context.Context, teamID uint64, token string) (*teampb.TeamLaunchRecord, error) {
	launch, err := scanTeamLaunch(s.DB.QueryRowContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE team_id=? AND state IN(1,2,3) AND lock_token=? ORDER BY state,enqueued_ms,actor_id,request_high,request_low LIMIT 1", teamID, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return launch, err
}

func (s *Store) LockTeamLaunchForCompletion(ctx context.Context, tx *sql.Tx, launch *teampb.TeamLaunchRecord) (*teampb.TeamLaunchRecord, *teampb.TeamAuthorityData, uint64, uint32, error) {
	data, err := DecodeTeamLaunch(launch)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if err = s.LockTeamOriginals(ctx, tx, teamLaunchOriginals(data)); err != nil {
		return nil, nil, 0, 0, err
	}
	current, team, version, err := s.lockTeamLaunch(ctx, tx, launch)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if current.State != 3 || current.BattleId == 0 {
		return nil, nil, 0, 0, ErrTeamLaunchActive
	}
	var state uint32
	if err = tx.QueryRowContext(ctx, "SELECT state FROM match_formation WHERE battle_id=? FOR UPDATE", current.BattleId).Scan(&state); err != nil {
		return nil, nil, 0, 0, err
	}
	if state != 4 && state != 5 {
		return nil, nil, 0, 0, ErrTeamLaunchActive
	}
	return current, team, version, state, nil
}

func (s *Store) FinishTeamLaunchTx(ctx context.Context, tx *sql.Tx, launch *teampb.TeamLaunchRecord, state uint32) error {
	if state != 4 && state != 5 {
		return ErrDirectConflict
	}
	if state == 5 && (launch.TargetSize > launch.MemberCount || TeamLaunchMode(launch) == uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)) {
		return s.requeueFailedTeamLaunchTx(ctx, tx, launch)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE team_launch SET state=? WHERE actor_id=? AND request_high=? AND request_low=? AND state=3", state, launch.ActorId, launch.RequestHigh, launch.RequestLow); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE team_launch_role SET state=? WHERE actor_id=? AND request_high=? AND request_low=?", state, launch.ActorId, launch.RequestHigh, launch.RequestLow)
	return err
}

// 调用方已锁原全员P、Team、launch与确证F5。保留原身份/名单/票/profile/enqueue，
// 仅恢复整个unit；未知F绝不能走到这里。队伍的候选索引最后同tx恢复，不能逐人拆队。
func (s *Store) requeueFailedTeamLaunchTx(ctx context.Context, tx *sql.Tx, launch *teampb.TeamLaunchRecord) error {
	data, err := DecodeTeamLaunch(launch)
	if err != nil {
		return err
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
	}
	members := append([]*teampb.TeamLaunchMember(nil), data.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Original.PlayerId < members[j].Original.PlayerId })
	group := make([]*pb.MatchAuthorityTicketRecord, 0, len(members))
	expired := false
	for _, member := range members {
		ticket, readErr := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", member.Original.PlayerId))
		if readErr != nil {
			return readErr
		}
		if ticket.Ticket != member.Ticket || ticket.TeamId != launch.TeamId || ticket.OwnerAccount != member.Original.OwnerAccount || ticket.OwnerRevision != member.Original.OwnerRevision || !((ticket.State == 1 && ticket.FormationId == 0) || (ticket.State == 4 && ticket.FormationId == launch.BattleId)) {
			return ErrDirectConflict
		}
		profile, decodeErr := DecodeProfile(ticket.NetworkProfile)
		if decodeErr != nil {
			return decodeErr
		}
		expired = expired || ticket.ExpiresMs <= stamp || len(Latencies(profile, stamp, s.Config.HardLatencyMs)) == 0
		group = append(group, ticket)
	}
	state := uint32(2)
	if expired {
		state = 5
	}
	for _, ticket := range group {
		ticket.State, ticket.FormationId, ticket.SelectedRegion, ticket.NotBeforeMs = 1, 0, 0, stamp+1000
		if expired {
			ticket.State = 4
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=?,formation_id=0,selected_region=0,not_before_ms=? WHERE player_id=? AND ticket=?", ticket.State, ticket.NotBeforeMs, ticket.PlayerId, ticket.Ticket); err != nil {
			return err
		}
		if err = s.replaceTicketRegions(ctx, tx, ticket, stamp); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_launch SET state=?,battle_id=0,not_before_ms=? WHERE actor_id=? AND request_high=? AND request_low=? AND state=3 AND battle_id=?", state, stamp+1000, launch.ActorId, launch.RequestHigh, launch.RequestLow, launch.BattleId); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_launch_role SET state=? WHERE actor_id=? AND request_high=? AND request_low=?", state, launch.ActorId, launch.RequestHigh, launch.RequestLow); err != nil {
		return err
	}
	if expired {
		_, err = tx.ExecContext(ctx, "DELETE FROM team_launch_region WHERE actor_id=? AND request_high=? AND request_low=?", launch.ActorId, launch.RequestHigh, launch.RequestLow)
		return err
	}
	return s.replaceTeamLaunchRegions(ctx, tx, launch, group, stamp)
}

// 建房成功只能解名册锁；原成员全部由ready终态CAS释放后，才关闭挂起域原件。
// 队伍可以已经解散，恢复仍沿原launch/P，不把当前名单当成旧对局的成员。
func (s *Store) CloseCompletedTeamLaunch(ctx context.Context, observed *teampb.TeamLaunchRecord) error {
	data, err := DecodeTeamLaunch(observed)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.LockTeamOriginals(ctx, tx, teamLaunchOriginals(data)); err != nil {
		return err
	}
	var version uint64
	if err = tx.QueryRowContext(ctx, "SELECT version FROM team_authority WHERE team_id=? FOR UPDATE", observed.TeamId).Scan(&version); err != nil {
		return err
	}
	current, err := s.TeamLaunchTx(ctx, tx, observed.ActorId, observed.RequestHigh, observed.RequestLow)
	if err != nil {
		return err
	}
	if current == nil || current.State != 4 || current.BattleId != observed.BattleId || !proto.Equal(&teampb.TeamLaunchRecord{Payload: current.Payload}, &teampb.TeamLaunchRecord{Payload: observed.Payload}) {
		return nil
	}
	members := append([]*teampb.TeamLaunchMember(nil), data.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Original.PlayerId < members[j].Original.PlayerId })
	for _, member := range members {
		ticket, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", member.Original.PlayerId))
		if err != nil {
			return err
		}
		if ticket.Ticket == member.Ticket && ticket.State != 4 {
			return nil
		}
		if ticket.Ticket == "" || ticket.State == 0 {
			return ErrDirectConflict
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_launch SET state=5 WHERE actor_id=? AND request_high=? AND request_low=? AND state=4", current.ActorId, current.RequestHigh, current.RequestLow); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_launch_role SET state=5 WHERE actor_id=? AND request_high=? AND request_low=?", current.ActorId, current.RequestHigh, current.RequestLow); err != nil {
		return err
	}
	return tx.Commit()
}
