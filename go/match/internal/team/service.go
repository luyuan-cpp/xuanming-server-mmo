package team

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"match/generated/pb/game"
	"match/internal/metrics"
	"match/internal/playercontract"
	"match/internal/svc"

	base "proto/common/base"
	teampb "proto/team"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// 组队 RPC 编排(设计文档 docs/design/team-system.md §D.5)。
//
// 每个方法:pre 校验与外部查询(只做一次)→ store.Mutate(读 → 纯函数规则 → CAS 提交 → 重试 / 修复)
// → 结果映射成 tip → 回包视图 → 异步副作用(notify.go)。
//
// 契约:
//   - 调用者身份由 server 层从 session 取得并保证非 0;请求体里没有 player_id(§D.2);
//   - 方法从不返回 error:故障以 ErrInternal 表达,业务拒绝以对应 tip 表达;
//   - ctx 是请求预算(server 层 3500ms),所有 Redis / data_service 调用都受它约束;
//     副作用用独立 ctx(§A.3 第 7 条);
//   - 回包视图与推送视图一律同源构建(view.go),失败回包的视图取调用者自己的 S_READ;
//   - 线程安全:Service 无可变状态。

// IdGenerator team_id 发号器。生产为 svcCtx.BattleIDGen(battle_id / challenge_id / team_id 共用,§C.3);
// 失租被 fence 后返回错误,调用方整体失败,不许用 0 顶替。
type IdGenerator interface {
	Generate() (uint64, error)
}

// BattleStarter 整队开战的票据域端口(§E.1 第 3、5、7、8 步),由 logic.TeamBattleStarter 实现、
// match_service.go 装配时注入。依赖方向 team → 接口 ← logic:team 只定义接口、不 import logic;
// 方法签名只用 Go 内建类型,logic 不需要(也不能在 regen 前)import team。
//
// 编排全部在本包(Service.StartTeamMatch):bindCaller 读、惰性转让、队长 / 锁 / 人数校验、roster、
// 会话 / 战斗锁 / 位置预检、开战锁钉版本提交与整轮重来、EndMatch、推送。实现方只管票据与 gather。
type BattleStarter interface {
	// TeamSizeFor 副本组队开战人数上限(与 PVE_TEAM 排队同口径,已按引擎每队上限收口);0 = 未开放组队。
	TeamSizeFor(battleConfigId uint32) uint32
	// MatchLockTTLSeconds memberCount 人的开战锁时长(秒):覆盖 matched 票 TTL + 补偿窗口 + 余量。
	MatchLockTTLSeconds(memberCount int) int
	// TicketBlocked 成员票据预检:没有票据或残留已自愈 → false;仍在途 → true。
	// 前置条件:已确认该成员没有 battle:lock。err 非 nil 时调用方 fail-closed。
	TicketBlocked(ctx context.Context, playerId uint64) (bool, error)
	// CreateMatchedTickets 为 roster(非空)逐人建 matched 票(带 team_id)。成功返回 (tickets, 0);
	// 失败时实现方已用独立 ctx 回滚本次写下的票,返回 (nil, 失败成员)。
	CreateMatchedTickets(ctx context.Context, battleConfigId uint32, teamId uint64,
		roster []uint64, zones map[uint64]uint32) (map[uint64]string, uint64)
	// RunGather 同步执行一次整队 gather(roster 原序;失败时实现方已全员删票),返回是否开局成功。
	RunGather(battleConfigId uint32, roster []uint64, tickets map[uint64]string) bool
}

// afterPreflightHook 测试缝(生产恒为 nil):StartTeamMatch 每轮预检通过之后、开战锁提交之前调用,
// 确定性地插入"预检期间名单变化"(§I.3 #28)与"并发两次开战"。
var afterPreflightHook func(teamId uint64, roster []uint64)

const (
	// matchStartRounds 整队开战整轮重来的上限(§E.1),同时受请求预算约束。
	matchStartRounds = 3

	// team_match_total{outcome} 的固定取值。同步拒绝按 rpcOutcome(tip 码表)定性:ok / rejected / internal / unknown_code。
	matchOutcomeSuccess      = "success"
	matchOutcomeGatherFailed = "gather_failed"
	matchOutcomeTicketFailed = "ticket_failed"
)

// beforeInvitePruneHook 测试缝(生产恒为 nil):ListMyInvites 在每次 S_INVITE_PRUNE 之前调用,
// 用来确定性地插入"队长刚好重邀"的并发时序(§I.3 #23)。
var beforeInvitePruneHook func(playerId, teamId uint64)

// Service 组队服务。
type Service struct {
	svcCtx    *svc.ServiceContext
	store     *Store
	presence  presenceReader
	homeZones HomeZoneLookup
	ids       IdGenerator
	starter   BattleStarter
}

// NewService 装配组队服务。
//   - svcCtx.SharedRedis:权威存储与在线态(§C.2);
//   - homeZones:为 nil 时需要 home zone 的请求一律回 ErrInternal(fail-closed);
//   - starter:logic.NewTeamBattleStarter(svcCtx);为 nil 时 StartTeamMatch 回 ErrInternal;
//   - team_id 发号器取 svcCtx.BattleIDGen,为 nil 时建队回 ErrInternal。
func NewService(svcCtx *svc.ServiceContext, homeZones HomeZoneLookup, starter BattleStarter) *Service {
	presence := presenceReader{rds: svcCtx.SharedRedis}
	cfg := RuleConfig{AllowCrossZone: svcCtx.Config.Team.AllowCrossZone}
	s := &Service{
		svcCtx:    svcCtx,
		store:     NewStore(svcCtx.SharedRedis, presence.loadSessions, cfg),
		presence:  presence,
		homeZones: homeZones,
		starter:   starter,
	}
	if svcCtx.BattleIDGen != nil { // 避免把 nil 指针包成非 nil 接口
		s.ids = svcCtx.BattleIDGen
	}
	return s
}

// ---- RPC ----

// CreateTeam 建队:已在队回 4003 + 当前视图(§D.6 重放语义)。
func (s *Service) CreateTeam(ctx context.Context, caller uint64, _ *teampb.CreateTeamRequest) *teampb.TeamResponse {
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
		logx.WithContext(ctx).Errorf("[team] CreateTeam 发号失败 player=%d: %v", caller, err)
		return s.snapshotResponse(ctx, caller, ErrInternal, 0, self)
	}
	resp, _ := s.runMutate(ctx, caller, BindCreate(caller, teamId), CreateOp(caller, teamId, zone),
		mutateSpec{method: methodCreateTeam})
	return resp
}

// GetMyTeam 自由读 + 过期清理 / 惰性转让;没变化但 TTL 不足时续期;notify_online 时推给其他在线队员。
func (s *Service) GetMyTeam(ctx context.Context, caller uint64, in *teampb.GetMyTeamRequest) *teampb.TeamResponse {
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
			logx.WithContext(ctx).Errorf("[team] 续期失败 team=%d: %v", res.Snapshot.TeamId, err)
		}
	}
	if in.GetNotifyOnline() {
		if team := resp.GetTeam(); team.GetTeamId() != 0 {
			members := make([]uint64, 0, len(team.GetMembers()))
			for _, m := range team.GetMembers() {
				members = append(members, m.GetPlayerId())
			}
			s.publishOnline(caller, team.GetTeamId(), members)
		}
	}
	return resp
}

// ApplyJoinTeam 申请加入目标玩家所在的队伍。
func (s *Service) ApplyJoinTeam(ctx context.Context, caller uint64, in *teampb.ApplyJoinTeamRequest) *teampb.TeamResponse {
	target := in.GetTargetPlayerId()
	if target == 0 || target == caller {
		return s.respond(ctx, caller, ErrPlayerId, 0)
	}
	// 先对调用者自由读:顺手自愈孤儿索引,否则规则会因 CallerTeamId≠0 误回 4003。
	self, failed := s.readSelf(ctx, methodApplyJoinTeam, caller)
	if failed != nil {
		return failed
	}
	if self.PlayerTeamId != 0 {
		return s.snapshotResponse(ctx, caller, ErrMemberInTeam, 0, self)
	}
	other, err := s.readFree(ctx, methodApplyJoinTeam, target)
	if err != nil {
		return s.snapshotResponse(ctx, caller, readFailureCode(err), 0, self)
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
func (s *Service) HandleApplication(ctx context.Context, caller uint64, in *teampb.HandleApplicationRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()),
		HandleApplicationOp(caller, in.GetApplicantId(), in.GetApprove()),
		mutateSpec{method: methodHandleApplication})
	return resp
}

// InviteToTeam 队长邀请在线且无队的玩家。
func (s *Service) InviteToTeam(ctx context.Context, caller uint64, in *teampb.InviteToTeamRequest) *teampb.TeamResponse {
	target := in.GetTargetPlayerId()
	if target == 0 || target == caller {
		return s.respond(ctx, caller, ErrPlayerId, 0)
	}
	session, err := playercontract.LoadSession(ctx, s.svcCtx, target)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] InviteToTeam 读目标会话失败 target=%d: %v", target, err)
		return s.respond(ctx, caller, ErrInternal, 0)
	}
	if !playercontract.IsSessionOnline(session) {
		return s.respond(ctx, caller, ErrTargetOffline, 0)
	}
	other, err := s.readFree(ctx, methodInviteToTeam, target)
	if err != nil {
		return s.respond(ctx, caller, readFailureCode(err), 0)
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
func (s *Service) RespondInvite(ctx context.Context, caller uint64, in *teampb.RespondInviteRequest) *teampb.TeamResponse {
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

// ListMyInvites 列出收到的未过期邀请;反查索引里已失效的项按 score CAS 删除(§C.5 S_INVITE_PRUNE)。
func (s *Service) ListMyInvites(ctx context.Context, caller uint64, _ *teampb.ListMyInvitesRequest) *teampb.ListMyInvitesResponse {
	log := logx.WithContext(ctx)
	nowMs, entries, err := s.store.ListInvites(ctx, caller)
	if err != nil {
		log.Errorf("[team] ListMyInvites 读反查索引失败 player=%d: %v", caller, err)
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
			log.Errorf("[team] ListMyInvites 读队伍失败 player=%d team=%d: %v", caller, entry.TeamId, err)
			return &teampb.ListMyInvitesResponse{ErrorMessage: tipOf(ErrInternal, 0)}
		}
		if inv := findInvite(PruneExpired(snap.Record, snap.NowMs), caller); inv != nil {
			live = append(live, liveInvite{rec: snap.Record, nowMs: snap.NowMs})
			roster = append(roster, inv.GetInviterId())
			continue
		}
		// 记录已不存在,或记录里已没有给我的未过期邀请:只在 score 未变时删(不误删队长刚重邀写入的新项)。
		if hook := beforeInvitePruneHook; hook != nil {
			hook(caller, entry.TeamId)
		}
		if _, err := s.store.PruneInvite(ctx, caller, entry.TeamId, entry.Score); err != nil {
			log.Errorf("[team] ListMyInvites 清理失效索引失败 player=%d team=%d: %v", caller, entry.TeamId, err)
		}
	}
	dc := s.presence.loadDisplay(ctx, roster)
	resp := &teampb.ListMyInvitesResponse{ServerTimeMs: nowMs}
	for _, l := range live {
		if view := incomingInviteView(l.rec, caller, l.nowMs, dc); view != nil {
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
func (s *Service) LeaveTeam(ctx context.Context, caller uint64, in *teampb.LeaveTeamRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()), LeaveOp(caller),
		mutateSpec{method: methodLeaveTeam, unboundIsSuccess: true})
	return resp
}

// KickMember 队长踢人。
func (s *Service) KickMember(ctx context.Context, caller uint64, in *teampb.KickMemberRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()), KickOp(caller, in.GetTargetPlayerId()),
		mutateSpec{method: methodKickMember})
	return resp
}

// TransferLeader 队长转让。
func (s *Service) TransferLeader(ctx context.Context, caller uint64, in *teampb.TransferLeaderRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()),
		TransferLeaderOp(caller, in.GetTargetPlayerId()), mutateSpec{method: methodTransferLeader})
	return resp
}

// DisbandTeam 队长解散;当前队伍 ≠ expected → 4013 + 当前视图。
func (s *Service) DisbandTeam(ctx context.Context, caller uint64, in *teampb.DisbandTeamRequest) *teampb.TeamResponse {
	resp, _ := s.runMutate(ctx, caller, BindCaller(caller, in.GetExpectedTeamId()), DisbandOp(caller),
		mutateSpec{method: methodDisbandTeam})
	return resp
}

// StartTeamMatch 整队开战(§E.1,v1 不补位、即时开战、不进队列)。第 1–6 步算一轮;开战锁提交返回
// 冲突 / 修复时整轮重来(重新读记录、重排 roster、重新预检),最多 matchStartRounds 轮且不超出请求预算。
//
//  1. Mutate(BindCaller, Refresh):未绑定 / 记录缺失 → 4013 + 当前视图;惰性转让 / 过期清理有提交 → 整轮重来;
//  2. 队长、开战锁有效性(S_READ 的 Redis 时钟);3. 副本人数;4. roster = 队长在前再按 join_seq;
//  5. 逐成员预检(会话 → 战斗锁 → 位置 → 票据),失败时 parameters[0] 带该成员 pid;
//  6. 开战锁提交(ver 钉死在第 1 步);报错或未提交都可能已落锁(回复丢失 / go-redis 重发同一条 EVAL),
//     报错 → 后台按 token 清锁;未提交 → 整轮重来前按 token 同步确认一轮(§E.3);
//  7. 按锁内名单逐人建 matched 票;失败 → 端口已回滚,后台 EndMatch(false),回 TeamMemberNotReady,
//     推给全员的 MATCH_FAILED 带同一个 tip;
//  8. 后台 gather → EndMatch(ok) → 推 MATCH_ENDED / MATCH_FAILED;
//  9. 回包 = 加锁提交构建的视图(match_state=STARTING)。
func (s *Service) StartTeamMatch(ctx context.Context, caller uint64, in *teampb.StartTeamMatchRequest) *teampb.TeamResponse {
	log := logx.WithContext(ctx)
	if s.starter == nil {
		log.Errorf("[team] StartTeamMatch 未接线(BattleStarter 为 nil) player=%d", caller)
		return s.matchReject(ctx, caller, ErrInternal, 0, nil)
	}
	configId := in.GetBattleConfigId()
	bind := BindCaller(caller, in.GetExpectedTeamId())
	for round := 0; round < matchStartRounds && ctx.Err() == nil; round++ {
		res, err := s.store.Mutate(ctx, bind, RefreshOp(caller))
		if err != nil {
			log.Errorf("[team] StartTeamMatch 存储故障 player=%d team=%d: %v", caller, bind.TeamId(), err)
			return s.matchReject(ctx, caller, ErrInternal, 0, nil)
		}
		s.mutateEffects(caller, methodStartTeamMatch, res)
		switch res.Outcome {
		case OutcomeCommitted:
			continue // 惰性转让 / 过期清理已落盘:整轮重来
		case OutcomeNotBound, OutcomeRecordMissing:
			return s.matchReject(ctx, caller, ErrNoTeam, 0, freshSnapshot(res))
		case OutcomeRejected:
			return s.matchReject(ctx, caller, res.Code, res.CodeParam, nil)
		}
		// OutcomeUnchanged:记录、ver、nowMs 与调用者索引出自这一次 S_READ(记录必然存在)。
		snap := res.Snapshot
		rec := snap.Record
		if code := CheckMatchStart(rec, caller, snap.NowMs); code != 0 {
			return s.matchReject(ctx, caller, code, 0, &snap)
		}
		if code := CheckMatchTeamSize(rec, s.starter.TeamSizeFor(configId)); code != 0 {
			return s.matchReject(ctx, caller, code, 0, &snap)
		}
		roster := MatchRoster(rec)
		zones, code, param := s.preflightMatch(ctx, roster)
		if code != 0 {
			return s.matchReject(ctx, caller, code, param, &snap)
		}
		if hook := afterPreflightHook; hook != nil {
			hook(snap.TeamId, roster)
		}

		token := uuid.NewString()
		expireAtMs := snap.NowMs + uint64(s.starter.MatchLockTTLSeconds(len(roster)))*1000
		lock, err := s.store.CommitMatchLock(ctx, &snap, caller, token, roster, expireAtMs)
		if err != nil {
			log.Errorf("[team] StartTeamMatch 开战锁提交故障 player=%d team=%d: %v", caller, snap.TeamId, err)
			// 结果未知:EVAL 可能已在 Redis 执行、只是回复超时 / 断连。按 token 后台清锁(锁没写入时只读不写不推),
			// 否则锁白挂到自然过期,期间全队的名单操作都回 TeamInMatch(§E.3)。
			s.releaseLockInBackground(snap.TeamId, token)
			return s.matchReject(ctx, caller, ErrInternal, 0, nil)
		}
		commits := repairCommits(lock.Repairs)
		if lock.Commit != nil {
			commits = append(commits, lock.Commit)
		}
		s.publish(caller, commits, nil)
		if lock.Code != 0 {
			return s.matchReject(ctx, caller, lock.Code, lock.CodeParam, nil)
		}
		if lock.Commit == nil {
			// 预检期间名单或记录变了:整轮重来。绝不在新名单上加锁、却按旧 roster 建票。
			metrics.ObserveTeamCommitRetry(methodStartTeamMatch)
			// {0} 不等于没写入:go-redis 对断连 / 读超时会重发同一条 EVAL,第一次已落锁时第二次回 {0}。
			// 重来之前按 token 确认一次,否则下一轮会被自己的锁挡成 TeamInMatch。
			s.settleUnconfirmedLock(ctx, snap.TeamId, token)
			continue
		}
		return s.launchMatch(ctx, caller, configId, lock.Commit, token, zones)
	}
	return s.matchReject(ctx, caller, ErrStateChanged, 0, nil)
}

// preflightMatch 逐成员只读预检(§E.1 第 5 步,按 roster 顺序,fail-closed)。
// 返回 (各成员位置的 zone, tip, parameters[0])。顺序固定为会话 → 战斗锁 → 位置 → 票据:
// 票据自愈的 ready 分支依赖"已确认没有战斗锁",与 JoinQueue 同序。
func (s *Service) preflightMatch(ctx context.Context, roster []uint64) (map[uint64]uint32, uint32, uint64) {
	log := logx.WithContext(ctx)
	zones := make(map[uint64]uint32, len(roster))
	for _, pid := range roster {
		session, err := playercontract.LoadSession(ctx, s.svcCtx, pid)
		if err != nil {
			log.Errorf("[team] 开战预检读会话失败 player=%d: %v", pid, err)
			return nil, ErrInternal, 0
		}
		if !playercontract.IsSessionOnline(session) {
			return nil, ErrMemberOffline, pid
		}
		locked, err := playercontract.IsBattleLocked(ctx, s.svcCtx, pid)
		if err != nil {
			log.Errorf("[team] 开战预检查战斗锁失败,按战斗中处理 player=%d: %v", pid, err)
		}
		if locked {
			return nil, ErrMemberInBattle, pid
		}
		loc, err := playercontract.LoadLocation(ctx, s.svcCtx, pid)
		if err != nil {
			log.Errorf("[team] 开战预检读位置失败 player=%d: %v", pid, err)
			return nil, ErrInternal, 0
		}
		if loc == nil {
			return nil, ErrMemberNotReady, pid
		}
		blocked, err := s.starter.TicketBlocked(ctx, pid)
		if err != nil {
			log.Errorf("[team] 开战预检读票据失败 player=%d: %v", pid, err)
			return nil, ErrInternal, 0
		}
		if blocked {
			return nil, ErrMemberNotReady, pid
		}
		zones[pid] = loc.GetZoneId()
	}
	return zones, 0, 0
}

// launchMatch 开战锁已落盘之后(§E.1 第 7–9 步)。建票与 gather 只用锁内名单(与锁同一次提交落盘)。
func (s *Service) launchMatch(ctx context.Context, caller uint64, configId uint32, lock *CommitResult,
	token string, zones map[uint64]uint32,
) *teampb.TeamResponse {
	log := logx.WithContext(ctx)
	teamId := lock.TeamId
	roster := append([]uint64(nil), lock.Decision.Record.GetMatchLockRoster()...)
	tickets, failed := s.starter.CreateMatchedTickets(ctx, configId, teamId, roster, zones)
	if failed != 0 {
		metrics.ObserveTeamMatch(matchOutcomeTicketFailed)
		log.Infof("[team] 整队开战建票失败,释放开战锁 team=%d player=%d", teamId, failed)
		// 补偿不继承请求 ctx(§A.3 第 7 条):后台清锁,结果推全员(含发起人)。回包里的视图可能仍是 STARTING,
		// 客户端按 (epoch, version) 排序,随后到达的 MATCH_FAILED 视图版本更新。
		tip := tipOf(ErrMemberNotReady, failed) // 推给队员的 MATCH_FAILED 带同一个原因(§E.1 第 7 步、§H.2)
		asyncFn("match.team.end_match", func() { s.finishMatch(teamId, token, false, roster, tip) })
		return s.respond(ctx, caller, ErrMemberNotReady, failed)
	}
	resp := s.commitResponse(ctx, caller, lock)
	log.Infof("[team] 整队开战已受理 team=%d config=%d roster=%v", teamId, configId, roster)
	asyncFn("match.gather.pve_team", func() {
		ok := s.starter.RunGather(configId, roster, tickets)
		outcome := matchOutcomeGatherFailed
		if ok {
			outcome = matchOutcomeSuccess
		}
		metrics.ObserveTeamMatch(outcome)
		// gather 失败拿不到具体原因:tip 留空(§E.1 第 8 步)。
		s.finishMatch(teamId, token, ok, roster, nil)
	})
	return resp
}

// finishMatch 清开战锁并推结果(§E.1 EndMatch),在后台执行,不受任何请求 ctx 约束。
// 清锁提交成功 → 按提交推全员(含发起人)MATCH_ENDED / MATCH_FAILED;没有提交(锁已被清 / 重新加锁 /
// 自然过期 / 截止)→ 仍尽力给队员推一次当前视图与结果原因,保证客户端不停在 STARTING。
// tip 是结果推送的原因(TeamSnapshotS2C.tip,可为 nil),只随结果推送下发,不进存储。
func (s *Service) finishMatch(teamId uint64, token string, ok bool, roster []uint64, tip *base.TipInfoMessage) {
	res := s.store.EndMatch(teamId, token, ok)
	commits := repairCommits(res.Repairs)
	if res.Commit != nil {
		res.Commit.PushTip = tip
		commits = append(commits, res.Commit)
	}
	s.publish(0, commits, nil)
	if res.Stop == EndMatchReleased {
		return
	}
	if res.Stop == EndMatchDeadline {
		logx.Errorf("[team] EndMatch 截止仍未清锁(靠锁自然过期) team=%d conflicts=%d last_err=%v",
			teamId, res.Conflicts, res.LastErr)
	} else {
		logx.Infof("[team] EndMatch 未写入 team=%d stop=%d conflicts=%d", teamId, res.Stop, res.Conflicts)
	}
	reason := teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED
	if ok {
		reason = teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED
	}
	s.pushMatchView(teamId, roster, reason, tip)
}

// releaseLockInBackground 开战锁提交结果未知时的后台补偿:按 token 清锁(EndMatch,ok=false)。
// 与 finishMatch 不同,没有清锁提交就不推送 —— 锁可能根本没写入,给全队推 MATCH_FAILED 等于凭空多出
// 一场从没开始过的开战。锁没写入时 EndMatch 读到 token 不符即停止,不写。
func (s *Service) releaseLockInBackground(teamId uint64, token string) {
	asyncFn("match.team.end_match", func() {
		res := s.store.EndMatch(teamId, token, false)
		commits := repairCommits(res.Repairs)
		if res.Commit != nil {
			logx.Infof("[team] 结果未知的开战锁已按 token 清除 team=%d", teamId)
			commits = append(commits, res.Commit)
		}
		s.publish(0, commits, nil)
		if res.Stop == EndMatchDeadline {
			logx.Errorf("[team] 开战锁补偿截止仍未清锁(靠锁自然过期) team=%d conflicts=%d last_err=%v",
				teamId, res.Conflicts, res.LastErr)
		}
	})
}

// settleUnconfirmedLock 开战锁提交没拿到"已提交"(Retry)时,用请求 ctx 按 token 同步确认一轮:
// 锁不在 / token 不符 / 已过期 → 只读不写(常见情形:预检期间真有别的提交);锁在(同一条 EVAL 被重发、
// 第一次已落锁)→ 钉版本清掉并推送。同步确认不了(故障或清锁冲突)就交给后台补偿。
func (s *Service) settleUnconfirmedLock(ctx context.Context, teamId uint64, token string) {
	stop, pinned, err := s.store.ReleaseMatchLockOnce(ctx, teamId, token)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] 确认未提交的开战锁失败,转后台清锁 team=%d: %v", teamId, err)
		s.releaseLockInBackground(teamId, token)
		return
	}
	if stop != 0 {
		return
	}
	commits := repairCommits(pinned.Repairs)
	if pinned.Commit != nil {
		logx.WithContext(ctx).Infof("[team] 开战锁 EVAL 被重发且已落锁,已按 token 清除 team=%d", teamId)
		commits = append(commits, pinned.Commit)
	}
	s.publish(0, commits, nil)
	if pinned.Commit == nil {
		s.releaseLockInBackground(teamId, token)
	}
}

// pushMatchView 不经提交的结果推送:用 S_READ_MEMBERS 给记录里索引 tid == 本队的成员推当前视图
// (epoch 同源,§C.4;match_state 按锁是否有效算)。记录已不在或成员表持续变化时放弃,交给客户端拉取自愈。
func (s *Service) pushMatchView(teamId uint64, roster []uint64, reason teampb.TeamChangeReason, tip *base.TipInfoMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
	defer cancel()
	snap, err := s.store.ReadMembers(ctx, teamId, roster)
	if errors.Is(err, ErrMembersChanged) {
		metrics.ObserveTeamPush(pushKindMembersChanged, pushOutcomeSkipped)
		return
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] 开战结果推送读成员失败 team=%d: %v", teamId, err)
		metrics.ObserveTeamPush(pushKindSnapshot, pushOutcomeError)
		return
	}
	if snap.Record == nil {
		return
	}
	dc := s.presence.loadDisplay(ctx, rosterIds(snap.Record))
	for _, pid := range MemberIds(snap.Record) {
		view, ok := viewFromMembers(snap, pid, dc)
		if !ok {
			continue
		}
		s.push(ctx, pid, uint32(game.ClientPlayerTeamNotifyTeamSnapshotMessageId), pushKindSnapshot,
			&teampb.TeamSnapshotS2C{Team: view, Reason: reason, Tip: tip})
	}
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
		logx.WithContext(ctx).Errorf("[team] %s 存储故障 player=%d team=%d: %v", spec.method, caller, bind.TeamId(), err)
		return s.respond(ctx, caller, ErrInternal, 0), nil
	}
	// 副作用先于回包派发:修复提交无论最终结局如何都已落盘,必须照常推送、通知 scene。
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
		return s.snapshotResponse(ctx, caller, code, 0, freshSnapshot(res)), res
	case OutcomeUnchanged:
		return s.snapshotResponse(ctx, caller, 0, 0, freshSnapshot(res)), res
	}
	// OutcomeRejected:规则拒绝(Decision.Code≠0)的判定就基于最后一次 S_READ,视图可同源复用;
	// Lua 拒绝({-1}/{-3})、冲突耗尽、ctx 过期时那次读已不代表现状,改用自由读。
	var snap *Snapshot
	if res.Decision.Code != 0 {
		snap = freshSnapshot(res)
	}
	return s.snapshotResponse(ctx, caller, res.Code, res.CodeParam, snap), res
}

// mutateEffects 记一次 Mutate 的冲突 / 自愈指标,并派发已落盘提交的副作用(修复提交在前、主提交在后)。
func (s *Service) mutateEffects(caller uint64, method string, res *MutateResult) {
	for i := 0; i < res.Conflicts; i++ {
		metrics.ObserveTeamCommitRetry(method)
	}
	commits := repairCommits(res.Repairs)
	var healed []uint64
	if res.HealedOrphan {
		metrics.ObserveTeamHeal(healKindOrphanIndex)
		healed = append(healed, caller)
	}
	if res.Outcome == OutcomeCommitted {
		commits = append(commits, res.Commit)
	}
	s.publish(caller, commits, healed)
}

// repairCommits 把 {-2} 修复提交转成待推送列表(按落盘顺序),并记 team_heal_total{index_mismatch}。
func repairCommits(repairs []CommitResult) []*CommitResult {
	commits := make([]*CommitResult, 0, len(repairs)+1)
	for i := range repairs {
		metrics.ObserveTeamHeal(healKindIndexMismatch)
		commits = append(commits, &repairs[i])
	}
	return commits
}

// freshSnapshot 最后一轮 S_READ 仍能代表调用者当前状态时返回它;本次 Mutate 自己落过修复提交
// 或自愈过孤儿索引时返回 nil(那次读已过时,调用方改用自由读)。
func freshSnapshot(res *MutateResult) *Snapshot {
	if len(res.Repairs) > 0 || res.HealedOrphan {
		return nil
	}
	return &res.Snapshot
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
	view, _ := viewFromCommit(c, caller, dc)
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

// respond tip + 调用者自由读视图(§C.4:失败回包的视图只取调用者自己的一次 S_READ)。
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
	return viewFromSnapshot(snap, dc)
}

// readSelf 调用者自由读;失败时返回现成的失败回包(不带视图)。
func (s *Service) readSelf(ctx context.Context, method string, caller uint64) (*Snapshot, *teampb.TeamResponse) {
	snap, err := s.readFree(ctx, method, caller)
	if err != nil {
		return nil, &teampb.TeamResponse{ErrorMessage: tipOf(readFailureCode(err), 0)}
	}
	return snap, nil
}

// readFree store.ReadFree 加上自愈副作用(指标 + scene 信号)与故障日志。
func (s *Service) readFree(ctx context.Context, method string, playerId uint64) (*Snapshot, error) {
	snap, healed, err := s.store.ReadFree(ctx, playerId)
	if healed {
		metrics.ObserveTeamHeal(healKindOrphanIndex)
		s.publish(0, nil, []uint64{playerId})
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] %s 自由读失败 player=%d: %v", method, playerId, err)
	}
	return snap, err
}

// readFailureCode 自由读失败的 tip:索引持续变化是"状态已变化",其余是内部故障。
func readFailureCode(err error) uint32 {
	if errors.Is(err, ErrUnstableRead) {
		return ErrStateChanged
	}
	return ErrInternal
}

// homeZoneOf 查单个玩家 home zone,返回 (zone, tip);tip≠0 时 zone 无意义(§D.3 fail-closed)。
func (s *Service) homeZoneOf(ctx context.Context, playerId uint64) (uint32, uint32) {
	if s.homeZones == nil {
		logx.WithContext(ctx).Errorf("[team] HomeZoneLookup 未注入,无法查询 player=%d", playerId)
		return 0, ErrInternal
	}
	zones, err := s.homeZones.HomeZones(ctx, []uint64{playerId})
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] 查询 home zone 失败 player=%d: %v", playerId, err)
		return 0, ErrInternal
	}
	zone := zones[playerId]
	if zone == 0 {
		logx.WithContext(ctx).Infof("[team] 玩家没有 home zone 映射 player=%d", playerId)
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

// pruneOwnInvite 删调用者反查索引里 teamId 的项(按 ListInvites 看到的 score CAS)。失败只记日志。
func (s *Service) pruneOwnInvite(ctx context.Context, caller, teamId uint64) {
	_, entries, err := s.store.ListInvites(ctx, caller)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] 清理邀请反查项时读索引失败 player=%d team=%d: %v", caller, teamId, err)
		return
	}
	for _, entry := range entries {
		if entry.TeamId != teamId {
			continue
		}
		if _, err := s.store.PruneInvite(ctx, caller, teamId, entry.Score); err != nil {
			logx.WithContext(ctx).Errorf("[team] 清理邀请反查项失败 player=%d team=%d: %v", caller, teamId, err)
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
