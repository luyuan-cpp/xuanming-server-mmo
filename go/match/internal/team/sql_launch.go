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

// StartTeamMatch 整队开战(§E.1)。先按 (调用者, request_id) 查持久 launch 原件,有就沿原件续推;没有就走一轮:
//
//  1. Mutate(BindCaller, Refresh):未绑定 / 记录缺失 → 4013 + 当前视图;惰性转让 / 过期清理有提交 → 整轮重来;
//  2. 队长、开战锁有效性(SQL 时钟);3. 副本人数与目标人数;4. roster = 队长在前再按 join_seq;
//  5. 逐成员预检(会话 → 战斗锁 → 位置 → 票据),失败时 parameters[0] 带该成员 pid;
//  6. 持久 launch 原件与开战锁同一事务提交(版本钉死在第 1 步);版本已变 → 整轮重来,最多 matchStartRounds 轮且不超出请求预算;
//  7. 沿原件提交成局(满队直接开局),或进队列(补位与 PVP)。
//
// 提交结果未知时原 request_id 查询 / 重试,不按未知去删除 SQL 成局。
func (s *Service) StartTeamMatch(ctx context.Context, caller uint64, in *teampb.StartTeamMatchRequest) *teampb.TeamResponse {
	mode, validMode := normalizeTeamMatchMode(uint32(in.GetMatchMode()))
	if !validMode {
		return s.respond(ctx, caller, ErrDungeonNotOpen, 0)
	}
	if _, err := authority.DirectUUIDKey(1, caller, in.GetRequestId()); err != nil {
		return s.respond(ctx, caller, uint32(table.CommonError_kInvalidParameter), 0)
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
		return s.resumeSQLTeamLaunch(ctx, caller, launch)
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
		code, culprit := s.preflightMatch(ctx, roster)
		if code != 0 {
			return s.matchReject(ctx, caller, code, culprit, &snap)
		}
		ttl := uint32(s.starter.MatchLockTTLSeconds(len(roster)))
		pinned, created, err := s.store.sql.createLaunch(ctx, &snap, caller, in, uuid.NewString(), roster, ttl)
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
			s.publish(caller, []*CommitResult{pinned.Commit})
		}
		return s.resumeSQLTeamLaunch(ctx, caller, created)
	}
	return s.respond(ctx, caller, ErrStateChanged, 0)
}

func (s *Service) resumeSQLTeamLaunch(ctx context.Context, caller uint64, launch *teampb.TeamLaunchRecord) *teampb.TeamResponse {
	if launch.State == 5 && launch.BattleId == 0 {
		return s.respond(ctx, caller, ErrStateChanged, 0)
	}
	if launch.State == 1 {
		formation, err := s.starter.ReserveSQLTeamMatch(ctx, launch)
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

func (s *sqlTeamStore) createLaunch(ctx context.Context, snap *Snapshot, caller uint64, request *teampb.StartTeamMatchRequest, token string, roster []uint64, ttl uint32) (*PinnedResult, *teampb.TeamLaunchRecord, error) {
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
	// LockMatch 已确认 roster 就是当前成员,这些路由行都在 CaptureTeamOriginals 里锁住了。
	zones, err := s.authority.TeamHomeZonesTx(ctx, tx, roster)
	if err != nil {
		return nil, nil, err
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
		// 撤销合服留下的跨区队伍:是业务拒绝,不是故障。这个事务整体回滚,名册和开战锁都没动。
		if errors.Is(err, authority.ErrTeamCrossZone) {
			return &PinnedResult{Code: ErrCrossZoneDenied}, nil, nil
		}
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
