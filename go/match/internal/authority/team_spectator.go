package authority

import (
	"context"
	"math"

	"google.golang.org/protobuf/proto"
	pb "proto/match"
	teampb "proto/team"
)

// 旧观战已获得原Remove正证明后，prepared整队在同事务清名册锁并进入完整queue unit。
// 后来新watch不会被这个旧Start清退；只允许原退出列表均关闭且此刻确实没有观战占用。
func (s *Store) EnqueuePreparedTeamLaunch(ctx context.Context, observed *teampb.TeamLaunchRecord) error {
	return s.Fenced(ctx, func() error { return s.enqueuePreparedTeamLaunchOnce(ctx, observed) })
}

func (s *Store) enqueuePreparedTeamLaunchOnce(ctx context.Context, observed *teampb.TeamLaunchRecord) error {
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
	players := make([]uint64, 0, len(data.Members))
	for _, member := range data.Members {
		players = append(players, member.Original.PlayerId)
	}
	if err = s.RequireNoSpectators(ctx, tx, players); err != nil {
		return err
	}
	current, team, version, err := s.lockTeamLaunch(ctx, tx, observed)
	if err != nil {
		return err
	}
	if current.State == 2 {
		return tx.Commit()
	}
	// 进队列的是补位单与真人 PVP(满员一侧也要排对手),与 ReserveSQLTeamMatch 的分流、EnqueueTeamLaunchTx 的准入同一口径。
	// 原先只认补位(目标人数 > 成员数):满员 PVP 带观战退出原件时被拒成 ErrDirectConflict,开战原件永远停在 state 1。
	queued := TeamLaunchMode(current) == uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) || current.TargetSize > current.MemberCount
	if current.State != 1 || current.BattleId != 0 || !queued || version == math.MaxUint64 {
		return ErrDirectConflict
	}
	if err = s.RequireSpectatorExitsClosedTx(ctx, tx, data.SpectatorExits); err != nil {
		return err
	}
	team.Record.MatchLockToken = ""
	team.Record.MatchLockExpireAtMs = 0
	team.Record.MatchLockRoster = nil
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(team)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_authority SET version=?,payload=? WHERE team_id=? AND version=?", version+1, payload, current.TeamId, version); err != nil {
		return err
	}
	if err = s.EnqueueTeamLaunchTx(ctx, tx, current); err != nil {
		return err
	}
	return tx.Commit()
}
