package team

import (
	"context"
	"database/sql"
	"errors"

	"match/internal/authority"

	matchpb "proto/match"
	teampb "proto/team"
	"shared/generated/pb/table"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type sqlTeamBattleStarter interface {
	ReserveSQLTeamMatch(context.Context, *teampb.TeamLaunchRecord) (*matchpb.MatchFormationRecord, error)
}

func (s *Service) startSQLTeamMatch(ctx context.Context, caller uint64, in *teampb.StartTeamMatchRequest) *teampb.TeamResponse {
	mode, validMode := normalizeTeamMatchMode(uint32(in.GetMatchMode()))
	if !validMode {
		return s.respond(ctx, caller, ErrDungeonNotOpen, 0)
	}
	if _, err := authority.DirectUUIDKey(1, caller, in.GetRequestId()); err != nil {
		return s.respond(ctx, caller, uint32(table.CommonError_kInvalidParameter), 0)
	}
	port, ok := s.starter.(sqlTeamBattleStarter)
	if !ok {
		return s.respond(ctx, caller, ErrInternal, 0)
	}
	launch, err := s.svcCtx.Authority.TeamLaunch(ctx, caller, in.RequestId)
	if err != nil {
		return s.respond(ctx, caller, ErrInternal, 0)
	}
	if launch != nil {
		data, err := authority.DecodeTeamLaunch(launch)
		if err != nil || !proto.Equal(data.Original.Request, in) {
			return s.respond(ctx, caller, uint32(table.CommonError_kInvalidParameter), 0)
		}
		return s.resumeSQLTeamLaunch(ctx, caller, launch, port)
	}
	for attempt := 0; attempt < matchStartRounds && ctx.Err() == nil; attempt++ {
		result, err := s.store.Mutate(ctx, BindCaller(caller, in.ExpectedTeamId), RefreshOp(caller))
		if err != nil {
			return s.respond(ctx, caller, ErrInternal, 0)
		}
		s.mutateEffects(caller, methodStartTeamMatch, result)
		if result.Outcome == OutcomeCommitted {
			continue
		}
		if result.Outcome == OutcomeNotBound || result.Outcome == OutcomeRecordMissing {
			return s.respond(ctx, caller, ErrNoTeam, 0)
		}
		if result.Outcome == OutcomeRejected {
			return s.respond(ctx, caller, result.Code, result.CodeParam)
		}
		snap := result.Snapshot
		if code := CheckMatchStart(snap.Record, caller, snap.NowMs); code != 0 {
			return s.matchReject(ctx, caller, code, 0, &snap)
		}
		limit := s.starter.TeamSizeFor(uint32(mode), in.BattleConfigId)
		if code := CheckMatchTeamSize(snap.Record, limit); code != 0 {
			return s.matchReject(ctx, caller, code, 0, &snap)
		}
		size := in.TargetTeamSize
		if size == 0 {
			size = uint32(len(snap.Record.Members))
		}
		if size < uint32(len(snap.Record.Members)) || size > limit {
			return s.matchReject(ctx, caller, ErrSizeExceeded, 0, &snap)
		}
		if mode != matchpb.MatchMode_MATCH_MODE_PVP_TEAM && size > uint32(len(snap.Record.Members)) && s.svcCtx.Config.Team.FillQueueDisabled {
			return s.matchReject(ctx, caller, ErrDungeonNotOpen, 0, &snap)
		}
		roster := MatchRoster(snap.Record)
		zones, code, culprit := s.preflightMatch(ctx, roster)
		if code != 0 {
			return s.matchReject(ctx, caller, code, culprit, &snap)
		}
		if hook := afterPreflightHook; hook != nil {
			hook(snap.TeamId, roster)
		}
		ttl := uint32(s.starter.MatchLockTTLSeconds(len(roster)))
		pinned, created, err := s.store.sql.createLaunch(ctx, &snap, caller, in, uuid.NewString(), roster, zones, ttl)
		if err != nil {
			return s.respond(ctx, caller, ErrInternal, 0)
		}
		if pinned.Retry {
			continue
		}
		if pinned.Code != 0 {
			return s.respond(ctx, caller, pinned.Code, pinned.CodeParam)
		}
		if pinned.Commit != nil {
			s.publish(caller, []*CommitResult{pinned.Commit}, nil)
		}
		return s.resumeSQLTeamLaunch(ctx, caller, created, port)
	}
	return s.respond(ctx, caller, ErrStateChanged, 0)
}

func (s *Service) resumeSQLTeamLaunch(ctx context.Context, caller uint64, launch *teampb.TeamLaunchRecord, port sqlTeamBattleStarter) *teampb.TeamResponse {
	if launch.State == 5 && launch.BattleId == 0 {
		return s.respond(ctx, caller, ErrStateChanged, 0)
	}
	if launch.State == 1 {
		formation, err := port.ReserveSQLTeamMatch(ctx, launch)
		if err != nil {
			return s.respond(ctx, caller, ErrInternal, 0)
		}
		if formation != nil {
			launch.BattleId, launch.State = formation.BattleId, 3
		}
	}
	response := s.respond(ctx, caller, 0, 0)
	if launch.BattleId != 0 {
		formation, err := s.svcCtx.Authority.Formation(ctx, launch.BattleId)
		if err != nil {
			return s.respond(ctx, caller, ErrInternal, 0)
		}
		data, err := authority.DecodeTeamLaunch(launch)
		if err != nil {
			return s.respond(ctx, caller, ErrInternal, 0)
		}
		response.MatchIntent = &teampb.TeamMatchIntent{RequestId: data.Original.Request.RequestId, BattleId: formation.BattleId, State: formation.State}
	}
	return response
}

func (s *sqlTeamStore) createLaunch(ctx context.Context, snap *Snapshot, caller uint64, request *teampb.StartTeamMatchRequest, token string, roster []uint64, zones map[uint64]uint32, ttl uint32) (*PinnedResult, *teampb.TeamLaunchRecord, error) {
	key, err := authority.DirectUUIDKey(1, caller, request.RequestId)
	if err != nil {
		return nil, nil, err
	}
	observed, err := loadSQLTeam(ctx, s.authority.DB, snap.TeamId, false)
	if err != nil {
		return nil, nil, err
	}
	tx, err := s.authority.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	locked, err := s.authority.CaptureTeamOriginals(ctx, tx, caller, observed.data.Members, 0)
	if err != nil {
		return nil, nil, err
	}
	var actors []*matchpb.MatchAuthorityTicketRecord
	for _, role := range locked {
		actors = append(actors, &matchpb.MatchAuthorityTicketRecord{PlayerId: role.PlayerId, OwnerAccount: role.OwnerAccount, OwnerRevision: role.OwnerRevision})
	}
	exits, err := s.authority.CaptureSpectatorExitsTx(ctx, tx, actors)
	if err != nil {
		return nil, nil, err
	}
	if cancelled, cancelErr := s.authority.TeamStartCancelledTx(ctx, tx, caller, key.RequestHigh, key.RequestLow); cancelErr != nil {
		return nil, nil, cancelErr
	} else if cancelled {
		return &PinnedResult{Code: ErrStateChanged}, nil, nil
	}
	current, err := loadSQLTeam(ctx, tx, snap.TeamId, true)
	if err != nil {
		return nil, nil, err
	}
	if !sameSQLTeam(current, observed) || current.version != snap.Version {
		return &PinnedResult{Retry: true}, nil, nil
	}
	previous, err := s.authority.TeamLaunchTx(ctx, tx, caller, key.RequestHigh, key.RequestLow)
	if err != nil {
		return nil, nil, err
	}
	if previous != nil {
		data, decodeErr := authority.DecodeTeamLaunch(previous)
		if decodeErr != nil || !proto.Equal(data.GetOriginal().GetRequest(), request) {
			return nil, nil, authority.ErrDirectConflict
		}
		// 本轮可能只在事务内标记了新看到的观战；重放已有意图必须回滚它，不能清退新watch。
		return &PinnedResult{}, previous, nil
	}
	if err = s.authority.RequireNewTeamLaunchTx(ctx, tx, locked); err != nil {
		return nil, nil, err
	}
	var active uint64
	err = tx.QueryRowContext(ctx, "SELECT actor_id FROM team_launch WHERE team_id=? AND state IN(1,2,3) ORDER BY state,enqueued_ms,actor_id,request_high,request_low LIMIT 1 FOR UPDATE", snap.TeamId).Scan(&active)
	if err == nil {
		return &PinnedResult{Code: ErrInMatch}, nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	size := request.TargetTeamSize
	mode, validMode := normalizeTeamMatchMode(uint32(request.GetMatchMode()))
	if !validMode {
		return nil, nil, authority.ErrDirectConflict
	}
	if size == 0 {
		size = uint32(len(roster))
	}
	decision := LockMatch(current.data.Record, caller, token, roster, stamp+uint64(ttl)*1000, stamp)
	if decision.Code != 0 {
		return &PinnedResult{Code: decision.Code, CodeParam: decision.CodeParam}, nil, nil
	}
	queued := mode == matchpb.MatchMode_MATCH_MODE_PVP_TEAM || size > uint32(len(roster))
	if queued && len(exits) == 0 {
		decision.Record.MatchLockToken = ""
		decision.Record.MatchLockExpireAtMs = 0
		decision.Record.MatchLockRoster = nil
		decision.Reason = teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_QUEUED
	} else {
		decision.Record.MatchLockExpireAtMs = 0
	}
	data, err := nextSQLTeamData(current.data, decision, RefreshOp(caller), locked)
	if err != nil {
		return nil, nil, err
	}
	commit, code, param, err := s.commitDecision(ctx, tx, snap.TeamId, current, data, decision, stamp)
	if err != nil {
		return nil, nil, err
	}
	if code != 0 {
		return &PinnedResult{Code: code, CodeParam: param}, nil, nil
	}
	original := &teampb.TeamLaunchOriginal{Actor: originalByPlayer(locked, caller), TeamId: snap.TeamId, TeamVersion: commit.Version, LockToken: token, Request: proto.Clone(request).(*teampb.StartTeamMatchRequest), Zones: zones, LockTtlSeconds: ttl, EnqueuedMs: stamp}
	for index, player := range roster {
		original.Members = append(original.Members, &teampb.TeamLaunchMember{Original: originalByPlayer(locked, player), RosterOrder: uint32(index + 1), Ticket: uuid.NewString()})
	}
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return nil, nil, err
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(&teampb.TeamLaunchData{Members: original.Members, RequestPayload: requestBytes, TeamRecord: decision.Record, Original: original, SpectatorExits: exits})
	if err != nil {
		return nil, nil, err
	}
	launch := &teampb.TeamLaunchRecord{ActorId: caller, RequestHigh: key.RequestHigh, RequestLow: key.RequestLow, TeamId: snap.TeamId, TeamVersion: commit.Version, LockToken: token, ConfigId: request.BattleConfigId, TargetSize: size, MemberCount: uint32(len(roster)), State: 1, EnqueuedMs: stamp, Payload: payload, OwnerAccount: original.Actor.OwnerAccount, OwnerRevision: original.Actor.OwnerRevision, Mode: uint32(mode)}
	if err = s.authority.InsertTeamLaunchTx(ctx, tx, launch); err != nil {
		return nil, nil, err
	}
	if queued && len(exits) == 0 {
		if err = s.authority.EnqueueTeamLaunchTx(ctx, tx, launch); err != nil {
			return nil, nil, err
		}
		launch.State = 2
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return &PinnedResult{Commit: commit}, launch, nil
}

// SQL开战必须有持久完整Start意图，不能让旧Redis钉版本入口制造无原件锁。
func (s *sqlTeamStore) commitMatchLock(context.Context, *Snapshot, uint64, string, []uint64, uint64) (*PinnedResult, error) {
	return nil, errors.New("team: SQL开战须提交完整原launch")
}
