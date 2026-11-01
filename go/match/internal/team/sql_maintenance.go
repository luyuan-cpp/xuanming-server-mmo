package team

import (
	"context"
	"strconv"

	teampb "proto/team"
)

func (s *sqlTeamStore) touch(ctx context.Context, snap *Snapshot) (touched bool, err error) {
	err = s.authority.Fenced(ctx, func() (e error) { touched, e = s.touchOnce(ctx, snap); return e })
	return touched, err
}

func (s *sqlTeamStore) touchOnce(ctx context.Context, snap *Snapshot) (bool, error) {
	if snap == nil || snap.Record == nil {
		return false, nil
	}
	observed, err := loadSQLTeam(ctx, s.authority.DB, snap.TeamId, false)
	if err != nil {
		return false, err
	}
	tx, err := s.authority.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = s.authority.CaptureTeamOriginals(ctx, tx, snap.PlayerId, observed.data.Members, 0); err != nil {
		return false, err
	}
	current, err := loadSQLTeam(ctx, tx, snap.TeamId, true)
	if err != nil {
		return false, err
	}
	if current.state != 1 || current.version != snap.Version || !sameSQLTeam(observed, current) {
		return false, nil
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return false, err
	}
	if current.idleExpires <= stamp {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_authority SET idle_expires_ms=? WHERE team_id=? AND version=? AND state=1", stamp+TeamIdleTTLSeconds*1000, snap.TeamId, snap.Version); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *sqlTeamStore) pruneInvite(ctx context.Context, player, teamID uint64, score string) (changed bool, err error) {
	err = s.authority.Fenced(ctx, func() (e error) { changed, e = s.pruneInviteOnce(ctx, player, teamID, score); return e })
	return changed, err
}

func (s *sqlTeamStore) pruneInviteOnce(ctx context.Context, player, teamID uint64, score string) (bool, error) {
	expectedExpiry, err := strconv.ParseUint(score, 10, 64)
	if err != nil {
		return false, err
	}
	actor, err := teamActor(ctx, player)
	if err != nil {
		return false, err
	}
	tx, err := s.authority.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = s.authority.LockTeamOriginals(ctx, tx, []*teampb.TeamOriginalRole{actor}); err != nil {
		return false, err
	}
	row, err := loadSQLTeam(ctx, tx, teamID, true)
	if err != nil {
		return false, err
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return false, err
	}
	if invitation := findInvite(row.data.Record, player); row.state == 1 && invitation != nil && invitation.ExpireAtMs > stamp {
		return false, nil // 活原件仍在，不能因展示缓存缺失修剪权威索引。
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM team_invite_index WHERE invitee_id=? AND team_id=? AND expires_ms=?", player, teamID, expectedExpiry)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed != 0, tx.Commit()
}
