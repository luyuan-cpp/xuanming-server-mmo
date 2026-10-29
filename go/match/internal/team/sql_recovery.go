package team

import (
	"context"
	"errors"
	"time"

	"match/internal/authority"
	"match/internal/constants"

	"github.com/zeromicro/go-zero/core/logx"
	teampb "proto/team"

	"google.golang.org/protobuf/proto"
)

func (s *sqlTeamStore) endMatch(ctx context.Context, teamID uint64, token string) (EndMatchStop, *PinnedResult, error) {
	launch, err := s.authority.TeamLaunchByToken(ctx, teamID, token)
	if err != nil {
		return 0, nil, err
	}
	if launch == nil {
		return EndMatchTokenMismatch, nil, nil
	}
	tx, err := s.authority.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	current, data, version, state, err := s.authority.LockTeamLaunchForCompletion(ctx, tx, launch)
	if errors.Is(err, authority.ErrTeamLaunchActive) {
		return EndMatchDeadline, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	record := proto.Clone(data.Record).(*teampb.TeamRecord)
	record.MatchLockToken = ""
	record.MatchLockExpireAtMs = 0
	record.MatchLockRoster = nil
	reason := teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED
	if state == 4 {
		reason = teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED
	}
	decision := Decision{Changed: true, Record: record, Kept: MemberIds(record), Reason: reason}
	updated := proto.Clone(data).(*teampb.TeamAuthorityData)
	updated.Record = record
	commit, code, _, err := s.commitDecision(ctx, tx, teamID, &sqlTeamRow{version: version, state: 1, data: data}, updated, decision, stamp)
	if err != nil {
		return 0, nil, err
	}
	if code != 0 {
		return 0, nil, errors.New("team: 清原launch锁提交冲突")
	}
	if err = s.authority.FinishTeamLaunchTx(ctx, tx, current, state); err != nil {
		return 0, nil, err
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, err
	}
	return 0, &PinnedResult{Commit: commit}, nil
}

// 可重建公平游标；某个成局UNKNOWN不占住后续条目。业务锁只依赖SQL确证，不依赖游标或TTL。
func (s *Service) StartSQLMaintenance(ctx context.Context) {
	if s.store.sql == nil {
		return
	}
	// 四种恢复独立预算，持续新prepared或前列UNKNOWN不能饿死结案/到期清理。
	start := func(state uint32, point string) {
		cursor := authority.TeamLaunchCursor{}
		var lastWarning time.Time
		warn := func(err error) {
			if err != nil && time.Since(lastWarning) >= 10*time.Second {
				lastWarning = time.Now()
				logx.Errorf("队伍SQL恢复失败 phase=%s: %v", point, err)
			}
		}
		s.svcCtx.StartRelayLoop(ctx, point, 500*time.Millisecond, func(parent context.Context) {
			call, cancel := context.WithTimeout(parent, 3*time.Second)
			defer cancel()
			launches, err := s.svcCtx.Authority.TeamLaunchesAfter(call, state, cursor)
			if err != nil {
				warn(err)
				return
			}
			if len(launches) == 0 {
				cursor = authority.TeamLaunchCursor{}
				return
			}
			for _, launch := range launches {
				if call.Err() != nil {
					return
				}
				cursor = authority.TeamLaunchCursor{EnqueuedMs: launch.EnqueuedMs, ActorId: launch.ActorId, RequestHigh: launch.RequestHigh, RequestLow: launch.RequestLow}
				switch state {
				case 1:
					if starter, ok := s.starter.(sqlTeamBattleStarter); ok {
						_, err = starter.ReserveSQLTeamMatch(call, launch)
						s.svcCtx.HoldRelay(err)
						warn(err)
					}
				case 2:
					closed, err := s.svcCtx.Authority.MaintainQueuedTeam(call, launch)
					s.svcCtx.HoldRelay(err)
					warn(err)
					if err == nil && closed {
						data, err := authority.DecodeTeamLaunch(launch)
						if err == nil {
							var members []uint64
							for _, member := range data.Members {
								members = append(members, member.Original.PlayerId)
							}
							s.PartyEnded(launch.TeamId, members, constants.PartyEndMemberNotReady, 0)
						}
					}
				case 3:
					_, pinned, err := s.store.sql.endMatch(call, launch.TeamId, launch.LockToken)
					s.svcCtx.HoldRelay(err)
					warn(err)
					if err == nil && pinned != nil && pinned.Commit != nil {
						s.publish(0, []*CommitResult{pinned.Commit}, nil)
					}
				case 4:
					err := s.svcCtx.Authority.CloseCompletedTeamLaunch(call, launch)
					s.svcCtx.HoldRelay(err)
					warn(err)
				}
			}
		})
	}
	start(1, "match.team.sql.prepared")
	start(2, "match.team.sql.queued")
	start(3, "match.team.sql.forming")
	start(4, "match.team.sql.completed")
	var idleExpiry, idleTeam uint64
	s.svcCtx.StartRelayLoop(ctx, "match.team.sql.idle", time.Second, func(parent context.Context) {
		call, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		idleExpiry, idleTeam = s.expireSQLTeams(call, idleExpiry, idleTeam)
	})
}

func (s *Service) expireSQLTeams(ctx context.Context, afterExpiry, afterTeam uint64) (uint64, uint64) {
	rows, err := s.svcCtx.Authority.DB.QueryContext(ctx, "SELECT team_id,idle_expires_ms FROM team_authority WHERE state=1 AND idle_expires_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) AND (idle_expires_ms>? OR (idle_expires_ms=? AND team_id>?)) ORDER BY idle_expires_ms,team_id LIMIT 16", afterExpiry, afterExpiry, afterTeam)
	if err != nil {
		return afterExpiry, afterTeam
	}
	type idleTeam struct{ id, expiry uint64 }
	var teams []idleTeam
	for rows.Next() {
		var team idleTeam
		if err = rows.Scan(&team.id, &team.expiry); err != nil {
			break
		}
		teams = append(teams, team)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil || readErr != nil {
		return afterExpiry, afterTeam
	}
	if len(teams) == 0 {
		return 0, 0
	}
	for _, team := range teams {
		if ctx.Err() != nil {
			break
		}
		afterExpiry, afterTeam = team.expiry, team.id
		commit, err := s.store.sql.expireIdle(ctx, team.id)
		s.svcCtx.HoldRelay(err)
		if err == nil && commit != nil {
			s.publish(0, []*CommitResult{commit}, nil)
		}
	}
	return afterExpiry, afterTeam
}

func (s *sqlTeamStore) expireIdle(ctx context.Context, teamID uint64) (*CommitResult, error) {
	observed, err := loadSQLTeam(ctx, s.authority.DB, teamID, false)
	if err != nil {
		return nil, err
	}
	if observed.state != 1 {
		return nil, nil
	}
	tx, err := s.authority.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.authority.LockTeamOriginals(ctx, tx, observed.data.Members); err != nil {
		return nil, err
	}
	current, err := loadSQLTeam(ctx, tx, teamID, true)
	if err != nil {
		return nil, err
	}
	if !sameSQLTeam(current, observed) {
		return nil, nil
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	if current.state != 1 || current.idleExpires > stamp || current.data.Record.MatchLockToken != "" {
		return nil, nil
	}
	decision := Decision{Changed: true, Left: MemberIds(current.data.Record), Disbanded: true, Reason: teampb.TeamChangeReason_TEAM_CHANGE_REASON_DISBANDED}
	for _, invite := range current.data.Record.Invites {
		decision.InvitesRemoved = append(decision.InvitesRemoved, invite.InviteeId)
		if invite.ExpireAtMs > stamp {
			decision.RevokedInvitees = append(decision.RevokedInvitees, invite.InviteeId)
		}
	}
	commit, code, _, err := s.commitDecision(ctx, tx, teamID, current, &teampb.TeamAuthorityData{}, decision, stamp)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errors.New("team: 过期解散提交冲突")
	}
	return commit, tx.Commit()
}
