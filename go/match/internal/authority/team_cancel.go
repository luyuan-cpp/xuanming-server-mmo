package authority

import (
	"context"
	"database/sql"
	"errors"
	"math"

	teampb "proto/team"

	"google.golang.org/protobuf/proto"
)

func (s *Store) TeamStartCancelledTx(ctx context.Context, tx *sql.Tx, actor, high, low uint64) (bool, error) {
	var cancelled bool
	err := tx.QueryRowContext(ctx, "SELECT cancelled FROM match_join_intent WHERE player_id=? AND request_high=? AND request_low=? FOR UPDATE", actor, high, low).Scan(&cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return cancelled, err
}

// 只允许原actor撤销尚无formation的持久Start或queued整单；forming以后不能按意愿清占用。
func (s *Store) CancelTeamLaunchForActor(ctx context.Context, actor uint64, observed *teampb.TeamLaunchRecord) (members []uint64, err error) {
	err = s.Fenced(ctx, func() (e error) { members, e = s.cancelTeamLaunchForActorOnce(ctx, actor, observed); return e })
	return members, err
}

func (s *Store) cancelTeamLaunchForActorOnce(ctx context.Context, actor uint64, observed *teampb.TeamLaunchRecord) ([]uint64, error) {
	data, err := DecodeTeamLaunch(observed)
	if err != nil {
		return nil, err
	}
	caller := originalPlayer(ctx, actor)
	if observed.ActorId != actor || observed.OwnerAccount != caller.OwnerAccount || observed.OwnerRevision != caller.OwnerRevision {
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
	current, team, version, err := s.lockTeamLaunch(ctx, tx, observed)
	if err != nil {
		return nil, err
	}
	if current.State != 1 && current.State != 2 {
		return nil, nil
	}
	if current.BattleId != 0 {
		return nil, ErrTeamLaunchActive
	}
	var members []uint64
	for _, member := range data.Members {
		members = append(members, member.Original.PlayerId)
	}
	if current.State == 2 {
		if err = s.closeQueuedTeamLaunchTx(ctx, tx, current); err != nil {
			return nil, err
		}
	} else {
		if version == math.MaxUint64 {
			return nil, errors.New("team: 版本耗尽")
		}
		team.Record.MatchLockToken = ""
		team.Record.MatchLockExpireAtMs = 0
		team.Record.MatchLockRoster = nil
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(team)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE team_authority SET version=?,payload=? WHERE team_id=? AND version=?", version+1, payload, current.TeamId, version); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE team_launch SET state=5 WHERE actor_id=? AND request_high=? AND request_low=? AND state=1", actor, current.RequestHigh, current.RequestLow); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE team_launch_role SET state=5 WHERE actor_id=? AND request_high=? AND request_low=?", actor, current.RequestHigh, current.RequestLow); err != nil {
			return nil, err
		}
	}
	return members, tx.Commit()
}
