package team

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync/atomic"

	"match/internal/authority"
	"match/internal/metrics"
	"match/internal/playercontract"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"
	teampb "proto/team"
	"shared/generated/pb/table"
	"shared/placement"
	"shared/playersync"

	"github.com/zeromicro/go-zero/core/logx"
)

// 组队 RPC 编排(设计文档 docs/design/team-system.md §D.5)。
//
// 每个方法:pre 校验与外部查询(只做一次)→ store.Mutate(读 → 纯函数规则 → 事务提交 → 记录变化时重试)
// → 结果映射成 tip → 回包视图 → 异步副作用(notify.go)。
//
// 契约:
//   - 调用者身份由 server 层从 session 取得并保证非 0;请求体里没有 player_id(§D.2);
//   - 方法从不返回 error:故障以 ErrInternal 表达,业务拒绝以对应 tip 表达;
//   - ctx 是请求预算(server 层 3500ms),所有 SQL / Redis / data_service 调用都受它约束;
//     副作用用独立 ctx(§A.3 第 7 条);
//   - 回包视图与推送视图一律同源构建(view.go),失败回包的视图取调用者自己的一次自由读;
//   - 线程安全:Service 无可变状态。

// IdGenerator team_id 发号器。生产为 svcCtx.BattleIDGen(battle_id / challenge_id / team_id 共用,§C.3);
// 发不出号时返回错误,调用方整体失败,不许用 0 顶替。
type IdGenerator interface {
	Generate() (uint64, error)
}

// BattleStarter 整队开战的票据域端口(§E.1),由 logic.TeamBattleStarter 实现、match_service.go 装配时注入,必需。
// 依赖方向 team → 接口 ← logic:team 只定义接口、不 import logic。
//
// 编排在本包(Service.StartTeamMatch):绑定读、惰性转让、队长 / 人数校验、roster、会话 / 战斗锁 / 位置预检、
// 持久 launch 原件。实现方只管票据与成局。
type BattleStarter interface {
	// TeamSizeFor 模式 mode(PVE_TEAM / PVP_TEAM)在模板 battleConfigId 上的每队人数上限,来自 PlayMode 表
	// (与 JoinQueue 同口径,已按引擎每队上限收口);0 = 该模板未开放这个模式。PVP_TEAM 是每侧人数。
	TeamSizeFor(mode, battleConfigId uint32) uint32
	// MatchLockTTLSeconds memberCount 人的开战锁时长(秒):覆盖备战预算 + 补偿窗口 + 余量。
	MatchLockTTLSeconds(memberCount int) int
	// TicketBlocked 成员票据预检:持有未结束的票据 → true。
	// 前置条件:已确认该成员没有 battle:lock。err 非 nil 时调用方 fail-closed。
	TicketBlocked(ctx context.Context, playerId uint64) (bool, error)
	// PartyQueueState playerId 是否正随 teamId 在排队:返回 (PartyQueue*, 目标人数, 副本, 入队时刻毫秒, err),
	// 后三项只在 Queued 时有值(原因码 / 状态码见 constants.Party*)。
	PartyQueueState(ctx context.Context, playerId, teamId uint64) (uint32, uint32, uint32, uint64, error)
	// PartyQueueMode playerId 当前票据的模式;视图模式来自票据,不在队伍记录里制造第二份匹配状态。
	PartyQueueMode(ctx context.Context, playerId, teamId uint64) (uint32, error)
	// ReserveSQLTeamMatch 沿持久 launch 原件提交意图与成局;整队进队列的原件返回 (nil, nil)。
	ReserveSQLTeamMatch(ctx context.Context, launch *teampb.TeamLaunchRecord) (*matchpb.MatchFormationRecord, error)
}

func normalizeTeamMatchMode(raw uint32) (matchpb.MatchMode, bool) {
	switch matchpb.MatchMode(raw) {
	case matchpb.MatchMode_MATCH_MODE_UNSPECIFIED, matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		return matchpb.MatchMode_MATCH_MODE_PVE_TEAM, true
	case matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		return matchpb.MatchMode_MATCH_MODE_PVP_TEAM, true
	default:
		return matchpb.MatchMode_MATCH_MODE_UNSPECIFIED, false
	}
}

// matchStartRounds 整队开战整轮重来的上限(§E.1),同时受请求预算约束。
const matchStartRounds = 3

// Service 组队服务。
type Service struct {
	svcCtx    *svc.ServiceContext
	store     *Store
	presence  presenceReader
	homeZones HomeZoneLookup
	ids       IdGenerator
	starter   BattleStarter
	// syncDigest 名册提交后 bump 落地摘要(syncdigest.go);nil 时 bump 是无操作。
	syncDigest *playersync.Bumper

	// queueStateBreakerUntil 视图装饰读票的熔断截止时刻(UnixNano;0 = 未熔断),见 decorateQueueState。
	// Service 其余部分无可变状态;这一项是原子读写。
	queueStateBreakerUntil atomic.Int64
}

// NewService 装配组队服务。
//   - svcCtx.Authority:权威存储,也是合服映射的入口;svcCtx.PlayerRedis:在线态(§C.2);
//   - homeZones:为 nil 时需要 home zone 的请求一律回 ErrInternal(fail-closed);
//   - starter:logic.NewTeamBattleStarter(svcCtx),必需;
//   - team_id 发号器取 svcCtx.BattleIDGen,为 nil 时建队回 ErrInternal;
//   - 落地摘要取 svcCtx.SyncDigest(PlayerRedis 上的 Bumper),为 nil 时不 bump。
func NewService(svcCtx *svc.ServiceContext, homeZones HomeZoneLookup, starter BattleStarter) *Service {
	presence := presenceReader{rds: svcCtx.PlayerRedis, displays: svcCtx.DataServiceClient}
	return &Service{
		svcCtx:     svcCtx,
		store:      NewSQLStore(svcCtx.Authority, presence.loadSessions, RuleConfig{AllowCrossZone: svcCtx.Config.Team.AllowCrossZone}),
		presence:   presence,
		homeZones:  homeZones,
		starter:    starter,
		syncDigest: svcCtx.SyncDigest,
		ids:        svcCtx.BattleIDGen,
	}
}

// ---- RPC ----

// CreateTeam 建队:已在队回 4003 + 当前视图(§D.6 重放语义)。
func (s *Service) createTeam(ctx context.Context, caller uint64, _ *teampb.CreateTeamRequest) *teampb.TeamResponse {
	self, failed := s.readSelf(ctx, methodCreateTeam, caller)
	if failed != nil {
		return failed
	}
	if self.PlayerTeamId != 0 {
		return s.snapshotResponse(ctx, caller, ErrMemberInTeam, 0, self)
	}
	zone, code := s.homeZoneOf(ctx, caller)
	if code != 0 {
		return s.snapshotResponse(ctx, caller, code, 0, self)
	}
	teamId, err := s.newTeamId()
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] CreateTeam failed to allocate id player=%d: %v", caller, err)
		return s.snapshotResponse(ctx, caller, ErrInternal, 0, self)
	}
	resp, _ := s.runMutate(ctx, caller, BindCreate(caller, teamId), CreateOp(caller, teamId, zone),
		mutateSpec{method: methodCreateTeam})
	return resp
}

// GetMyTeam 自由读 + 过期清理 / 惰性转让;没变化但 TTL 不足时续期;notify_online 时推给其他在线队员;
// regroup 时请求归队(见 publishRegroup)。
func (s *Service) GetMyTeam(ctx context.Context, caller uint64, in *teampb.GetMyTeamRequest) *teampb.TeamResponse {
	resp := s.getMyTeam(ctx, caller, in)
	if in.MatchRequestId != "" {
		key, e := authority.DirectUUIDKey(1, caller, in.MatchRequestId)
		if e != nil {
			return s.respond(ctx, caller, uint32(table.CommonError_kInvalidParameter), 0)
		}
		f, _, e := s.svcCtx.Authority.FindDirect(ctx, key)
		if e != nil {
			return s.respond(ctx, caller, ErrInternal, 0)
		}
		if f != nil {
			resp.MatchIntent = &teampb.TeamMatchIntent{RequestId: in.MatchRequestId, BattleId: f.BattleId, State: f.State}
		}
	}
	return resp
}

func (s *Service) getMyTeam(ctx context.Context, caller uint64, in *teampb.GetMyTeamRequest) *teampb.TeamResponse {
	self, failed := s.readSelf(ctx, methodGetMyTeam, caller)
	if failed != nil {
		return failed
	}
	if self.Record == nil {
		return s.snapshotResponse(ctx, caller, 0, 0, self)
	}
	// 读操作:两次读之间索引变了(未绑定 / 记录缺失)不是错误,回调用者当时的视图。
	resp, res := s.runMutate(ctx, caller, BindCaller(caller, self.TeamId), RefreshOp(caller),
		mutateSpec{method: methodGetMyTeam, unboundIsSuccess: true})
	if res != nil && res.Outcome == OutcomeUnchanged && NeedsTouch(&res.Snapshot) {
		if _, err := s.store.Touch(ctx, &res.Snapshot); err != nil {
			logx.WithContext(ctx).Errorf("[team] Renewal failed team=%d: %v", res.Snapshot.TeamId, err)
		}
	}
	if in.GetNotifyOnline() {
		if team := resp.GetTeam(); team.GetTeamId() != 0 {
			s.publishOnline(caller, team.GetTeamId())
		}
	}
	if in.GetRegroup() {
		s.publishRegroup(caller, resp.GetTeam())
	}
	return resp
}

// ApplyJoinTeam 申请加入目标玩家所在的队伍。
func (s *Service) applyJoinTeam(ctx context.Context, caller uint64, in *teampb.ApplyJoinTeamRequest) *teampb.TeamResponse {
	target := in.GetTargetPlayerId()
	if target == 0 || target == caller {
		return s.respond(ctx, caller, ErrPlayerId, 0)
	}
	self, failed := s.readSelf(ctx, methodApplyJoinTeam, caller)
	if failed != nil {
		return failed
	}
	if self.PlayerTeamId != 0 {
		return s.snapshotResponse(ctx, caller, ErrMemberInTeam, 0, self)
	}
	other, err := s.readFree(ctx, methodApplyJoinTeam, target)
	if err != nil {
		return s.snapshotResponse(ctx, caller, ErrInternal, 0, self)
	}
	if other.PlayerTeamId == 0 {
		return s.snapshotResponse(ctx, caller, ErrNoTeam, 0, self)
	}
	zone, code := s.homeZoneOf(ctx, caller)
	if code != 0 {
		return s.snapshotResponse(ctx, caller, code, 0, self)
	}
	resp, _ := s.runMutate(ctx, caller, BindTarget(caller, other.PlayerTeamId), ApplyOp(caller, zone),
		mutateSpec{method: methodApplyJoinTeam})
	return resp
}

// HandleApplication 队长同意 / 拒绝申请。
func (s *Service) handleApplication(ctx context.Context, caller uint64, in *teampb.HandleApplicationRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()),
		HandleApplicationOp(caller, in.GetApplicantId(), in.GetApprove()),
		mutateSpec{method: methodHandleApplication})
	return resp
}

// InviteToTeam 队长邀请在线且无队的玩家。
func (s *Service) inviteToTeam(ctx context.Context, caller uint64, in *teampb.InviteToTeamRequest) *teampb.TeamResponse {
	target := in.GetTargetPlayerId()
	if target == 0 || target == caller {
		return s.respond(ctx, caller, ErrPlayerId, 0)
	}
	session, err := playercontract.LoadSession(ctx, s.svcCtx, target)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] InviteToTeam failed to read target session target=%d: %v", target, err)
		return s.respond(ctx, caller, ErrInternal, 0)
	}
	if !playercontract.IsSessionOnline(session) {
		return s.respond(ctx, caller, ErrTargetOffline, 0)
	}
	other, err := s.readFree(ctx, methodInviteToTeam, target)
	if err != nil {
		return s.respond(ctx, caller, ErrInternal, 0)
	}
	if other.PlayerTeamId != 0 {
		return s.respond(ctx, caller, ErrMemberInTeam, target)
	}
	zone, code := s.homeZoneOf(ctx, target)
	if code != 0 {
		return s.respond(ctx, caller, code, 0)
	}
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()), InviteOp(caller, target, zone),
		mutateSpec{method: methodInviteToTeam})
	return resp
}

// RespondInvite 被邀请人接受 / 拒绝;team_id 本身即绑定的队伍。
func (s *Service) respondInvite(ctx context.Context, caller uint64, in *teampb.RespondInviteRequest) *teampb.TeamResponse {
	teamId := in.GetTeamId()
	if teamId == 0 {
		return s.respond(ctx, caller, ErrNoTeam, 0)
	}
	if in.GetAccept() {
		self, failed := s.readSelf(ctx, methodRespondInvite, caller)
		if failed != nil {
			return failed
		}
		if self.PlayerTeamId != 0 && self.PlayerTeamId != teamId {
			return s.snapshotResponse(ctx, caller, ErrMemberInTeam, 0, self)
		}
	}
	resp, _ := s.runMutate(ctx, caller, BindTarget(caller, teamId), RespondInviteOp(caller, in.GetAccept()),
		mutateSpec{method: methodRespondInvite, pruneInviteOnMissing: true})
	return resp
}

// ListMyInvites 列出收到的未过期邀请;索引里记录已失效的项按期限比较后删除(§C.5)。
func (s *Service) ListMyInvites(ctx context.Context, caller uint64, _ *teampb.ListMyInvitesRequest) *teampb.ListMyInvitesResponse {
	log := logx.WithContext(ctx)
	nowMs, entries, err := s.store.ListInvites(ctx, caller)
	if err != nil {
		log.Errorf("[team] ListMyInvites failed to read reverse index player=%d: %v", caller, err)
		return &teampb.ListMyInvitesResponse{ErrorMessage: tipOf(ErrInternal, 0)}
	}
	type liveInvite struct {
		rec   *teampb.TeamRecord
		nowMs uint64
	}
	live := make([]liveInvite, 0, len(entries))
	var roster []uint64
	for _, entry := range entries {
		snap, err := s.store.read(ctx, caller, entry.TeamId)
		if err != nil {
			log.Errorf("[team] ListMyInvites failed to read team player=%d team=%d: %v", caller, entry.TeamId, err)
			return &teampb.ListMyInvitesResponse{ErrorMessage: tipOf(ErrInternal, 0)}
		}
		if inv := findInvite(PruneExpired(snap.Record, snap.NowMs), caller); inv != nil {
			live = append(live, liveInvite{rec: snap.Record, nowMs: snap.NowMs})
			roster = append(roster, inv.GetInviterId())
			continue
		}
		// 记录已不存在,或记录里已没有给我的未过期邀请:只在期限未变时删(不误删队长刚重邀写入的新项)。
		if _, err := s.store.PruneInvite(ctx, caller, entry.TeamId, entry.Score); err != nil {
			log.Errorf("[team] ListMyInvites failed to clean stale index entry player=%d team=%d: %v", caller, entry.TeamId, err)
		}
	}
	dc := s.presence.loadDisplay(ctx, roster)
	zones := s.mergedZones()
	resp := &teampb.ListMyInvitesResponse{ServerTimeMs: nowMs}
	for _, l := range live {
		if view := incomingInviteView(l.rec, caller, l.nowMs, dc, zones); view != nil {
			resp.Invites = append(resp.Invites, view)
		}
	}
	sort.SliceStable(resp.Invites, func(i, j int) bool {
		if resp.Invites[i].GetExpireAtMs() != resp.Invites[j].GetExpireAtMs() {
			return resp.Invites[i].GetExpireAtMs() < resp.Invites[j].GetExpireAtMs()
		}
		return resp.Invites[i].GetTeamId() < resp.Invites[j].GetTeamId()
	})
	return resp
}

// LeaveTeam 离队;expected 队伍里已经没有我 → 成功、不写,带当前视图(§D.6)。
func (s *Service) leaveTeam(ctx context.Context, caller uint64, in *teampb.LeaveTeamRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()), LeaveOp(caller),
		mutateSpec{method: methodLeaveTeam, unboundIsSuccess: true})
	return resp
}

// KickMember 队长踢人。
func (s *Service) kickMember(ctx context.Context, caller uint64, in *teampb.KickMemberRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()), KickOp(caller, in.GetTargetPlayerId()),
		mutateSpec{method: methodKickMember})
	return resp
}

// TransferLeader 队长转让。
func (s *Service) transferLeader(ctx context.Context, caller uint64, in *teampb.TransferLeaderRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()),
		TransferLeaderOp(caller, in.GetTargetPlayerId()), mutateSpec{method: methodTransferLeader})
	return resp
}

// DisbandTeam 队长解散;当前队伍 ≠ expected → 4013 + 当前视图。
func (s *Service) disbandTeam(ctx context.Context, caller uint64, in *teampb.DisbandTeamRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()), DisbandOp(caller),
		mutateSpec{method: methodDisbandTeam})
	return resp
}

// preflightMatch 逐成员只读预检(§E.1 第 5 步,按 roster 顺序,fail-closed)。
// 返回 (tip, parameters[0])。顺序固定为会话 → 战斗锁 → 位置 → 票据,与 JoinQueue 同序。
// 位置只判「人在不在场景」:它的 zone 是物理节点,不是归属,开战原件记的归属在 createLaunch 的路由锁内读。
func (s *Service) preflightMatch(ctx context.Context, roster []uint64) (uint32, uint64) {
	log := logx.WithContext(ctx)
	for _, pid := range roster {
		session, err := playercontract.LoadSession(ctx, s.svcCtx, pid)
		if err != nil {
			log.Errorf("[team] Battle start precheck failed to read session player=%d: %v", pid, err)
			return ErrInternal, 0
		}
		if !playercontract.IsSessionOnline(session) {
			return ErrMemberOffline, pid
		}
		locked, err := playercontract.IsBattleLocked(ctx, s.svcCtx, pid)
		if err != nil {
			log.Errorf("[team] Battle start precheck failed to check battle lock; treating player as in battle player=%d: %v", pid, err)
		}
		if locked {
			return ErrMemberInBattle, pid
		}
		loc, err := playercontract.LoadLocation(ctx, s.svcCtx, pid)
		if err != nil {
			log.Errorf("[team] Battle start precheck failed to read location player=%d: %v", pid, err)
			return ErrInternal, 0
		}
		if loc == nil {
			return ErrMemberNotReady, pid
		}
		// node_id 为空 = 跨 zone 交接已放行、目标 zone 还没落点(cross-zone-scene-travel.md CZ-4,
		// scene_manager placePlayerLocation 只在这一种情况下写空 node_id)。此刻没有任何 scene 节点
		// 持有该玩家,gather 的 preparePlayer 必定定位不到对端(EndpointOf 查不到空 node)。
		// 在这里按"没准备好"拒绝,别让整队走完提交原件再死在备战上。
		if loc.GetNodeId() == "" {
			log.Infof("[team] Battle start precheck: member undergoing cross-zone handoff (awaiting destination); rejecting as not ready player=%d zone=%d", pid, loc.GetZoneId())
			return ErrMemberNotReady, pid
		}
		blocked, err := s.starter.TicketBlocked(ctx, pid)
		if err != nil {
			log.Errorf("[team] Battle start precheck failed to read ticket player=%d: %v", pid, err)
			return ErrInternal, 0
		}
		if blocked {
			return ErrMemberNotReady, pid
		}
	}
	return 0, 0
}

// mergedZones 取当前合服映射快照,一次构建视图只取一次。没装配(单测)是 nil 快照,按没有合服回答。
func (s *Service) mergedZones() *placement.MergedZones {
	if s.svcCtx == nil {
		return nil
	}
	return s.svcCtx.Authority.MergedZones()
}

// matchReject StartTeamMatch 的同步失败出口:记 team_match_total(按 tip 码表定性,不手写故障集合)
// 并回 tip + 视图(snap 可同源构建时复用,否则自由读)。
func (s *Service) matchReject(ctx context.Context, caller uint64, code uint32, param uint64, snap *Snapshot) *teampb.TeamResponse {
	metrics.ObserveTeamMatch(rpcOutcome(code))
	return s.snapshotResponse(ctx, caller, code, param, snap)
}

// ---- 结果映射 ----

type mutateSpec struct {
	// method RPC 名,用作日志与 team_commit_retry_total{op}。
	method string
	// unboundIsSuccess 未绑定 / 记录缺失回成功(LeaveTeam 的幂等语义、GetMyTeam 的读语义,§D.6)。
	unboundIsSuccess bool
	// pruneInviteOnMissing 记录缺失时顺手删调用者自己的邀请反查项(RespondInvite,§D.5)。
	pruneInviteOnMissing bool
}

// runMutate 执行一次名册写并映射结果(store.go MutateResult 的 Outcome 契约)。
// 返回的 *MutateResult 仅在存储故障时为 nil。
func (s *Service) runMutate(ctx context.Context, caller uint64, bind Binding, op Op, spec mutateSpec) (*teampb.TeamResponse, *MutateResult) {
	res, err := s.store.Mutate(ctx, bind, op)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] %s storage failure player=%d team=%d: %v", spec.method, caller, bind.TeamId(), err)
		return s.respond(ctx, caller, ErrInternal, 0), nil
	}
	s.mutateEffects(caller, spec.method, res)

	switch res.Outcome {
	case OutcomeCommitted:
		return s.commitResponse(ctx, caller, res.Commit), res
	case OutcomeNotBound, OutcomeRecordMissing:
		if res.Outcome == OutcomeRecordMissing && spec.pruneInviteOnMissing {
			s.pruneOwnInvite(ctx, caller, bind.TeamId())
		}
		code := ErrNoTeam
		if spec.unboundIsSuccess {
			code = 0
		}
		return s.snapshotResponse(ctx, caller, code, 0, &res.Snapshot), res
	case OutcomeUnchanged:
		return s.snapshotResponse(ctx, caller, 0, 0, &res.Snapshot), res
	}
	// OutcomeRejected:规则拒绝(Decision.Code≠0)的判定就基于提交事务里那次读,视图可同源复用;
	// 提交阶段的拒绝、冲突耗尽时那次读已不代表现状,改用自由读。
	var snap *Snapshot
	if res.Decision.Code != 0 {
		snap = &res.Snapshot
	}
	return s.snapshotResponse(ctx, caller, res.Code, res.CodeParam, snap), res
}

// mutateEffects 记一次 Mutate 的冲突指标,并派发已落盘提交的副作用。
func (s *Service) mutateEffects(caller uint64, method string, res *MutateResult) {
	for i := 0; i < res.Conflicts; i++ {
		metrics.ObserveTeamCommitRetry(method)
	}
	if res.Outcome == OutcomeCommitted {
		s.publish(caller, []*CommitResult{res.Commit})
	}
}

// commitResponse 提交成功的回包:调用者在 J/K/L 里时用同一次提交构建视图,否则自由读。
func (s *Service) commitResponse(ctx context.Context, caller uint64, c *CommitResult) *teampb.TeamResponse {
	if !commitViewable(c, caller) {
		return s.respond(ctx, caller, 0, 0)
	}
	var dc displayCache
	if c.Indexes[caller].TeamId != 0 {
		dc = s.presence.loadDisplay(ctx, rosterIds(c.Decision.Record))
	}
	view, _ := viewFromCommit(c, caller, dc, s.mergedZones())
	if rosterChanged(c) {
		// 改了成员集合的提交已在同一事务里撤掉本队的排队(commitDecision):这类回包不做匹配态装饰,不必再读一次票;能力位照标。
		s.markFillSupported(view)
	} else {
		s.decorateQueueState(ctx, caller, view)
	}
	return &teampb.TeamResponse{Team: view}
}

// snapshotResponse tip + 由 snap 同源构建的调用者视图;snap 为 nil 或不可同源构建时改用自由读。
func (s *Service) snapshotResponse(ctx context.Context, caller uint64, code uint32, param uint64, snap *Snapshot) *teampb.TeamResponse {
	resp := &teampb.TeamResponse{ErrorMessage: tipOf(code, param)}
	if view, ok := s.snapshotView(ctx, snap); ok {
		resp.Team = view
		return resp
	}
	resp.Team = s.freeView(ctx, caller)
	return resp
}

// respond tip + 调用者自由读视图(§C.4:失败回包的视图只取调用者自己的一次自由读)。
func (s *Service) respond(ctx context.Context, caller uint64, code uint32, param uint64) *teampb.TeamResponse {
	return &teampb.TeamResponse{ErrorMessage: tipOf(code, param), Team: s.freeView(ctx, caller)}
}

// freeView 调用者自由读视图;读失败返回 nil(回包不带视图,客户端靠下一次 GetMyTeam 自愈)。
func (s *Service) freeView(ctx context.Context, caller uint64) *teampb.TeamView {
	snap, err := s.readFree(ctx, "view", caller)
	if err != nil {
		return nil
	}
	view, _ := s.snapshotView(ctx, snap)
	return view
}

func (s *Service) snapshotView(ctx context.Context, snap *Snapshot) (*teampb.TeamView, bool) {
	if !snapshotViewable(snap) {
		return nil, false
	}
	var dc displayCache
	if snap.PlayerTeamId != 0 {
		dc = s.presence.loadDisplay(ctx, rosterIds(snap.Record))
	}
	view, ok := viewFromSnapshot(snap, dc, s.mergedZones())
	if ok {
		s.decorateQueueState(ctx, snap.PlayerId, view)
	}
	return view, ok
}

// readSelf 调用者自由读;失败时返回现成的失败回包(不带视图)。
func (s *Service) readSelf(ctx context.Context, method string, caller uint64) (*Snapshot, *teampb.TeamResponse) {
	snap, err := s.readFree(ctx, method, caller)
	if err != nil {
		return nil, &teampb.TeamResponse{ErrorMessage: tipOf(ErrInternal, 0)}
	}
	return snap, nil
}

// readFree store.ReadFree 加上故障日志。
func (s *Service) readFree(ctx context.Context, method string, playerId uint64) (*Snapshot, error) {
	snap, err := s.store.ReadFree(ctx, playerId)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] %s unlocked read failed player=%d: %v", method, playerId, err)
	}
	return snap, err
}

// homeZoneOf 查单个玩家 home zone,返回 (zone, tip);tip≠0 时 zone 无意义(§D.3 fail-closed)。
func (s *Service) homeZoneOf(ctx context.Context, playerId uint64) (uint32, uint32) {
	if s.homeZones == nil {
		logx.WithContext(ctx).Errorf("[team] HomeZoneLookup not injected; cannot query player=%d", playerId)
		return 0, ErrInternal
	}
	zones, err := s.homeZones.HomeZones(ctx, []uint64{playerId})
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] Failed to query home zone player=%d: %v", playerId, err)
		return 0, ErrInternal
	}
	zone := zones[playerId]
	if zone == 0 {
		logx.WithContext(ctx).Infof("[team] Player has no home zone mapping player=%d", playerId)
		return 0, ErrHomeZoneUnknown
	}
	return zone, 0
}

func (s *Service) newTeamId() (uint64, error) {
	if s.ids == nil {
		return 0, errors.New("team: team_id 发号器未配置")
	}
	id, err := s.ids.Generate()
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, errors.New("team: 发号器返回 0")
	}
	return id, nil
}

// pruneOwnInvite 删调用者邀请索引里 teamId 的项(按 ListInvites 看到的期限比较)。失败只记日志。
func (s *Service) pruneOwnInvite(ctx context.Context, caller, teamId uint64) {
	_, entries, err := s.store.ListInvites(ctx, caller)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] Failed to read index while cleaning invitation reverse entry player=%d team=%d: %v", caller, teamId, err)
		return
	}
	for _, entry := range entries {
		if entry.TeamId != teamId {
			continue
		}
		if _, err := s.store.PruneInvite(ctx, caller, teamId, entry.Score); err != nil {
			logx.WithContext(ctx).Errorf("[team] Failed to clean invitation reverse entry player=%d team=%d: %v", caller, teamId, err)
		}
	}
}

// tipOf 组装 TeamResponse.error_message:成功为 nil;param≠0 时放进 parameters[0](如出问题的 player_id)。
func tipOf(code uint32, param uint64) *base.TipInfoMessage {
	if code == 0 {
		return nil
	}
	msg := &base.TipInfoMessage{Id: code}
	if param != 0 {
		msg.Parameters = []string{strconv.FormatUint(param, 10)}
	}
	return msg
}
