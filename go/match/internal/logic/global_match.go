package logic

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"match/internal/authority"
	"match/internal/catalog"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/poolowner"
	"match/internal/svc"
	battlepb "proto/battle"
	pb "proto/match"
	"shared/generated/pb/table"
	"shared/serviceauth"
)

// globalJoin 是入队的提交路径（docs/design/global-match-hierarchical-pools.md §3、§5.4）：
//   - 票据记 home zone（角色归属），范围树按它逐级放宽；
//   - 计分模式（1V1 / 5V5）读不到评分就拒绝入队，不按默认分进池（计分池不能混进错误的分数）；
//   - 单人档（PVE_SOLO、PVE_TEAM 1 人档）入队提交后立即尝试成局，失败的票留给池写者兜底。
func globalJoin(l *JoinQueueLogic, in *pb.JoinQueueRequest, required uint32) (*pb.JoinQueueResponse, error) {
	if parsed, e := uuid.Parse(in.RequestId); e != nil || parsed == uuid.Nil || parsed.String() != in.RequestId {
		return nil, status.Error(codes.InvalidArgument, "匹配请求编号无效")
	}
	if old, e := l.svcCtx.Authority.Ticket(l.ctx, authoritativePlayerID(l.ctx, in.PlayerId)); e != nil {
		return nil, e
	} else if old != nil && old.State == 3 {
		if _, e = releaseGlobalFinished(l.ctx, l.svcCtx, old); e != nil {
			return nil, e
		}
	}
	d, _ := ctxkeys.GetSessionDetails(l.ctx)
	binding := authority.SessionBinding(l.ctx, d)
	if binding == "" || in.RequestId == "" || len(in.RequestId) > 64 || in.NetworkPlanId == "" {
		return &pb.JoinQueueResponse{ErrorCode: constants.ErrProbeRequired, ErrorMessage: tipErr(constants.ErrProbeRequired, "请完成地域延迟探测后重试")}, nil
	}
	home, e := l.svcCtx.Authority.PlayerHomeZone(l.ctx, d.PlayerId)
	if e != nil {
		return &pb.JoinQueueResponse{ErrorCode: constants.ErrInternal, ErrorMessage: tipErr(constants.ErrInternal, "角色归属暂不可用")}, nil
	}
	var rating float64
	if isRatedMode(in.Mode) {
		if rating, e = loadRating(l.svcCtx, d.PlayerId); e != nil {
			return &pb.JoinQueueResponse{ErrorCode: constants.ErrInternal, ErrorMessage: tipErr(constants.ErrInternal, "评分暂不可用")}, nil
		}
	}

	t := &pb.MatchAuthorityTicketRecord{PlayerId: d.PlayerId, Ticket: uuid.NewString(), RequestId: in.RequestId, Mode: uint32(in.Mode), ConfigId: in.BattleConfigId, TeamSize: required, ZoneId: home, RatingCenti: int64(math.Round(rating * 100))}
	if in.Mode == pb.MatchMode_MATCH_MODE_PVP_TEAM {
		t.TeamSize = required / 2
	}
	prepared, exit, pending, e := l.svcCtx.Authority.PrepareJoin(l.ctx, t, in, binding)
	if e != nil {
		return joinRejection(e, "匹配原意图暂不可用"), nil
	}
	if prepared.RequestId != in.RequestId {
		return &pb.JoinQueueResponse{QueueTicket: prepared.Ticket, ErrorCode: constants.ErrAlreadyQueued, ErrorMessage: tipErr(constants.ErrAlreadyQueued, "已在匹配队列中")}, nil
	}
	if pending && exit != nil {
		if e = closeCapturedSpectatorExit(l.ctx, l.svcCtx, exit); e != nil {
			return &pb.JoinQueueResponse{ErrorCode: constants.ErrInternal, ErrorMessage: tipErr(constants.ErrInternal, "原观战退出尚待确认，请复用原请求重试")}, nil
		}
	}
	t = prepared
	old, created, e := l.svcCtx.Authority.Join(l.ctx, t, in.NetworkPlanId, binding)
	if e != nil {
		return joinRejection(e, "匹配权威暂不可用"), nil
	}
	metrics.ObserveGlobalFlow("join", "accepted")
	if !created && old.RequestId != in.RequestId {
		return &pb.JoinQueueResponse{QueueTicket: old.Ticket, ErrorCode: constants.ErrAlreadyQueued, ErrorMessage: tipErr(constants.ErrAlreadyQueued, "已在匹配队列中")}, nil
	}
	if created && old.State == 1 && old.TeamSize == 1 && old.Mode != uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) {
		claimImmediately(l.svcCtx, old)
	}
	return &pb.JoinQueueResponse{QueueTicket: old.Ticket}, nil
}

// joinRejection 把入队失败映射成客户端可区分的提示：探测问题让客户端重新探测，其余按服务端故障处理。
func joinRejection(err error, fallback string) *pb.JoinQueueResponse {
	switch {
	case errors.Is(err, authority.ErrProbeRequired):
		return &pb.JoinQueueResponse{ErrorCode: constants.ErrProbeRequired, ErrorMessage: tipErr(constants.ErrProbeRequired, "请完成地域延迟探测后重试")}
	case errors.Is(err, authority.ErrNoRegionWithinLatency):
		return &pb.JoinQueueResponse{ErrorCode: constants.ErrNoRegionWithinLatency, ErrorMessage: tipErr(constants.ErrNoRegionWithinLatency, "附近没有延迟合适的服务器")}
	default:
		return &pb.JoinQueueResponse{ErrorCode: constants.ErrInternal, ErrorMessage: tipErr(constants.ErrInternal, fallback)}
	}
}

// directOptions 点名成局(切磋、活动、整队直接开战)的附加参数。
type directOptions struct {
	// teamID 整队直接开战的队伍 id,写进每名成员的票据。
	teamID uint64
	// activityContext 非空:写进成局原件,建房时原样放进 CreateBattleRequest.activity_context。
	activityContext *battlepb.BattleActivityContext
}

func reserveGlobalDirect(ctx context.Context, s *svc.ServiceContext, key *pb.MatchDirectIntentRecord, payload []byte, mode pb.MatchMode, config uint32, members []uint64, opts directOptions) (*pb.MatchFormationRecord, error) {
	if old, original, e := s.Authority.FindDirect(ctx, key); e != nil {
		return nil, e
	} else if old != nil {
		if !bytes.Equal(original, payload) {
			return nil, errors.New("相同业务意图的开局内容不同")
		}
		return old, nil
	}
	prepared, err := s.Authority.DirectAdmission(ctx, key)
	if err != nil {
		return nil, err
	}
	var f *pb.MatchFormationRecord
	var group []*pb.MatchAuthorityTicketRecord
	if prepared == nil || !prepared.ExitPrepared {
		f, group, err = buildGlobalDirect(ctx, s, mode, config, members, nil, opts)
		if err != nil {
			return nil, err
		}
	}
	prepared, err = s.Authority.PrepareDirectAdmission(ctx, key, payload, f, group)
	if err != nil {
		return nil, err
	}
	return resumeGlobalDirectAdmission(ctx, s, prepared)
}

func buildGlobalDirect(ctx context.Context, s *svc.ServiceContext, mode pb.MatchMode, config uint32, members []uint64, tickets map[uint64]string, opts directOptions) (*pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord, error) {
	n, e := s.Authority.Now(ctx)
	if e != nil {
		return nil, nil, e
	}
	lat := map[uint64]map[uint32]uint32{}
	profiles := map[uint64]*pb.MatchNetworkProfile{}
	for _, pid := range members {
		p, expires, e := s.Authority.Profile(ctx, pid)
		if e != nil || expires <= n {
			return nil, nil, errors.New("成员地域探测失效")
		}
		profiles[pid] = p
		lat[pid] = authority.Latencies(p, n, s.Config.GlobalMatch.HardLatencyMs)
	}
	// 点名成局（切磋、活动、整队直接开战）名单固定：在全员硬延迟内、且有可分配节点的机房里，
	// 选全员最大 RTT 最小者，其次 RTT 之和，再次机房号；机房内按负载选节点（battleplace）。
	nodes := s.BattleNodes.Snapshot()
	var region uint32
	bestMax, bestSum := uint32(math.MaxUint32), uint64(math.MaxUint64)
	for candidate := range s.Placement.Regions(nodes) {
		maxRTT := uint32(0)
		sum := uint64(0)
		eligible := true
		for _, pid := range members {
			rtt, ok := lat[pid][candidate]
			if !ok {
				eligible = false
				break
			}
			maxRTT = max(maxRTT, rtt)
			sum += uint64(rtt)
		}
		if eligible && (maxRTT < bestMax || (maxRTT == bestMax && (sum < bestSum || (sum == bestSum && candidate < region)))) {
			region, bestMax, bestSum = candidate, maxRTT, sum
		}
	}
	target, found := s.Placement.Pick(nodes, region)
	if region == 0 || !found {
		return nil, nil, errors.New("没有满足全员硬延迟的共同机房")
	}
	id, e := s.BattleIDGen.Generate()
	if e != nil {
		return nil, nil, e
	}
	f := &pb.MatchFormationRecord{BattleId: id, OperationId: uuid.NewString(), PhysicalRegion: target.PhysicalRegion, TargetNodeId: target.NodeId, TargetInstanceId: target.NodeUuid, TargetEndpoint: target.Endpoint, Mode: uint32(mode), ConfigId: config}
	if opts.activityContext != nil {
		f.ActivityContext, e = proto.Marshal(opts.activityContext)
		if e != nil {
			return nil, nil, e
		}
	}
	var group []*pb.MatchAuthorityTicketRecord
	for _, pid := range members {
		ticket := tickets[pid]
		if ticket == "" {
			ticket = uuid.NewString()
		}
		group = append(group, &pb.MatchAuthorityTicketRecord{PlayerId: pid, Ticket: ticket, RequestId: f.OperationId, Mode: uint32(mode), ConfigId: config, TeamSize: uint32(len(members)), TeamId: opts.teamID, OwnerAccount: profiles[pid].OwnerAccount, OwnerRevision: profiles[pid].OwnerRevision, RatingCenti: int64(math.Round(loadRatingOrDefault(s, pid) * 100))})
	}
	return f, group, nil
}

func releaseGlobalFinished(ctx context.Context, s *svc.ServiceContext, t *pb.MatchAuthorityTicketRecord) (bool, error) {
	// 原实例消失/内存墓碑都不能证明个人到账；旧协议保持UNKNOWN。
	if s.RefreshBattleSettlement != nil {
		if err := s.RefreshBattleSettlement(ctx, t); err != nil {
			return false, err
		}
	}
	return s.Authority.ReleaseCompletedReady(ctx, t)
}

func runGlobalRound(ctx context.Context, s *svc.ServiceContext) {
	ctx, cancelRound := context.WithTimeout(ctx, 30*time.Second)
	defer cancelRound()
	// 到期终局恢复(成功开房的 state4 等不在 Pending 里的成局)在独立循环 match.global.outcome.recover 里跑
	// (global_outcome_recover.go,2026-10-30):撮合轮不再调用它,也不碰它的翻页游标。
	pending, e := s.Authority.Pending(ctx)
	if e != nil {
		logx.Error("全球匹配恢复查询失败")
		return
	}
	// 已提交成局的恢复按 (模式, 模板) 与新成局同样分给负责副本，副本之间不再抢同一批 Pending；
	// 成员表为空（etcd 不可用、本实例尚未注册时仍有其它成员则交给它们）时全部副本都恢复，退化为旧行为。
	// 恢复的正确性只靠 AcquireWork 的工作租约与行锁，归属只是分工。
	members, self := s.PoolMembers(), s.PoolMember()
	for _, f := range pending {
		if len(members) == 0 || poolowner.Owns(self, catalog.OwnerKey(pb.MatchMode(f.Mode), f.ConfigId), members) {
			launchGlobalFormation(s, f)
		}
	}
}

// launchGlobalFormation 为已提交的成局占一个备战槽并在后台推进；占不到槽就留给下一轮 Pending 恢复。
func launchGlobalFormation(s *svc.ServiceContext, f *pb.MatchFormationRecord) {
	if s.GlobalGatherSlots == nil {
		_ = s.RunRelay(func() error { runPlacedFormation(s, f); return nil })
		return
	}
	select {
	case s.GlobalGatherSlots <- struct{}{}:
	default:
		return
	}
	launchReservedFormation(s, f)
}

// launchReservedFormation 推进一个已经占好备战槽的成局，结束时释放槽。
func launchReservedFormation(s *svc.ServiceContext, f *pb.MatchFormationRecord) {
	if !s.LaunchRelayTask("match.global.gather", func() { defer func() { <-s.GlobalGatherSlots }(); runPlacedFormation(s, f) }) {
		<-s.GlobalGatherSlots
	}
}

// runPlacedFormation 在机房选点器里记一笔在途，再推进成局；在途数让同一机房的后续成局摊到别的节点。
func runPlacedFormation(s *svc.ServiceContext, f *pb.MatchFormationRecord) {
	if s.Placement != nil {
		s.Placement.Started(f.TargetInstanceId)
		defer s.Placement.Finished(f.TargetInstanceId)
	}
	runGlobalFormation(s, f)
}

func runGlobalFormation(s *svc.ServiceContext, f *pb.MatchFormationRecord) {
	metrics.ObserveGlobalFlow("recover", "attempt")
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	var acquireErr error
	f, acquireErr = s.Authority.AcquireWork(ctx, f.BattleId)
	if acquireErr != nil {
		if !errors.Is(acquireErr, authority.ErrWorkLease) && !errors.Is(acquireErr, authority.ErrAwaitingExpiredOutcome) {
			s.HoldRelay(acquireErr)
		}
		return
	}
	if f.State == 1 {
		var actx *battlepb.BattleActivityContext
		if len(f.ActivityContext) > 0 {
			actx = &battlepb.BattleActivityContext{}
			if proto.Unmarshal(f.ActivityContext, actx) != nil {
				return
			}
		}
		runFormationGather(s, f, actx)
		return
	}
	if len(f.CreateRequest) == 0 {
		if globalDestroy(s.Authority, f) {
			globalAbortPrepared(s, f)
		}
		return
	}
	conn, e := discovery.DialEndpoint(f.TargetEndpoint)
	if e != nil {
		s.HoldRelay(errors.New("匹配外部结局 UNKNOWN"))
		s.HoldRelay(s.Authority.Unknown(ctx, f))
		return
	}
	client := battlepb.NewBattleNodeClient(conn)
	qctx, qcancel := context.WithTimeout(ctx, 3*time.Second)
	result, e := client.QueryBattleCreation(qctx, &battlepb.QueryBattleCreationRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId})
	qcancel()
	if e != nil || result.State == battlepb.BattleCreationState_BATTLE_CREATION_UNKNOWN {
		metrics.ObserveGlobalFlow("recover", "unknown")
		s.HoldRelay(errors.New("匹配外部结局 UNKNOWN"))
		s.HoldRelay(s.Authority.Unknown(ctx, f))
		return
	}
	if result.State == battlepb.BattleCreationState_BATTLE_CREATION_PRESENT {
		if f.OutcomeProtocolVersion != 1 || result.GetTargetInstanceId() != f.TargetInstanceId {
			s.HoldRelay(errors.New("匹配外部结局 UNKNOWN"))
			s.HoldRelay(s.Authority.Unknown(ctx, f))
			return
		}
		if e = s.Authority.RecordBattleAdopted(ctx, f, result.TargetInstanceId, result.AdoptedOutcomeProtocolVersion); e != nil {
			s.HoldRelay(e)
			return
		}
		if result.AdoptedOutcomeProtocolVersion != 1 {
			s.HoldRelay(errors.New("匹配外部结局 UNKNOWN"))
			s.HoldRelay(s.Authority.Unknown(ctx, f))
			return
		}
		metrics.ObserveGlobalFlow("recover", "present")
		s.HoldRelay(s.Authority.Finish(ctx, f, true))
		return
	}
	if result.State == battlepb.BattleCreationState_BATTLE_CREATION_ABORTED {
		globalAbortPrepared(s, f)
		return
	}
	request := &battlepb.CreateBattleRequest{}
	if proto.Unmarshal(f.CreateRequest, request) != nil {
		s.HoldRelay(errors.New("匹配外部结局 UNKNOWN"))
		s.HoldRelay(s.Authority.Unknown(ctx, f))
		return
	}
	n, e := s.Authority.Now(ctx)
	if e != nil {
		return
	}
	if n < request.AdmitBeforeMs {
		// 只重放发起前已持久的v1能力原件；不向旧实例补发或追认历史0。
		if f.OutcomeProtocolVersion != 1 || f.PreflightSupportedVersion != 1 || f.PreflightInstanceId != f.TargetInstanceId || request.OutcomeProtocolVersion != 1 || result.GetTargetInstanceId() != f.TargetInstanceId || result.GetSupportedOutcomeProtocolVersion() != 1 {
			s.HoldRelay(errors.New("匹配外部结局 UNKNOWN"))
			s.HoldRelay(s.Authority.Unknown(ctx, f))
			return
		}
		if e = createBattle(discovery.NodeEntry{NodeId: f.TargetNodeId, NodeUuid: f.TargetInstanceId, Endpoint: f.TargetEndpoint, PhysicalRegion: f.PhysicalRegion}, request); e == nil {
			if e = s.Authority.RecordBattleAdopted(ctx, f, f.TargetInstanceId, 1); e == nil {
				s.HoldRelay(s.Authority.Finish(ctx, f, true))
			} else {
				s.HoldRelay(e)
			}
			return
		}
		var adoption *outcomeAdoptionError
		if errors.As(e, &adoption) {
			s.HoldRelay(s.Authority.RecordBattleAdopted(ctx, f, adoption.instance, adoption.version))
			s.HoldRelay(errors.New("匹配外部结局 UNKNOWN"))
			s.HoldRelay(s.Authority.Unknown(ctx, f))
			return
		}
	}
	if globalDestroy(s.Authority, f) {
		globalAbortPrepared(s, f)
	} else {
		s.HoldRelay(errors.New("匹配外部结局 UNKNOWN"))
		s.HoldRelay(s.Authority.Unknown(ctx, f))
	}
}

// formationDestroyAuthority 是 globalDestroy 对成局权威的全部依赖，生产实现就是 *authority.Store。
// 收窄成接口只为测试隔离（2026-10-22 评审补，social-match-battle#0）：「无原件不等目标 Battle」这条短路
// 要有一条不起 TiDB、不起 Battle 的回归（global_destroy_test.go）。
type formationDestroyAuthority interface {
	CreateIntentPersisted(ctx context.Context, work *pb.MatchFormationRecord) (bool, error)
	RecordBattleAdopted(ctx context.Context, work *pb.MatchFormationRecord, instance string, version uint32) error
	Finish(ctx context.Context, work *pb.MatchFormationRecord, success bool) error
}

// globalDestroy 返回 true 表示原 operation 已确定不会在目标 Battle 上建房，调用方才可按发房前失败收尾。
// 只是判定，不提交任何释放：释放与计数在 globalAbortPrepared。
func globalDestroy(store formationDestroyAuthority, f *pb.MatchFormationRecord) bool {
	check, stop := context.WithTimeout(context.Background(), 3*time.Second)
	intentPersisted, err := store.CreateIntentPersisted(check, f)
	stop()
	if err != nil {
		return false
	}
	if !intentPersisted {
		// 2026-10-22 修复（social-match-battle#0）：本代租约下没有 Create 原件，就证明原 operation 从未、
		// 也不会再发出 Create（见 CreateIntentPersisted），目标 Battle 上不可能有这局，不必等它应答。
		// 原先这里要求目标实例回 ABORTED：实例从注册中心消失、拨号或查询失败、同端点换了新 UUID
		// （目标 UUID 不符时 Battle 直接空回包）都拿不到，state 2 票据就被永久卡住。
		// 真正的释放仍是 globalAbortPrepared → Finish(false)，它在行锁内复核同一租约且仍无原件。
		return true
	}
	conn, e := discovery.DialEndpoint(f.TargetEndpoint)
	if e != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, e = battlepb.NewBattleNodeClient(conn).DestroyBattle(ctx, &battlepb.DestroyBattleRequest{BattleId: f.BattleId, Reason: "global_match_abort_if_absent", PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId})
	if e != nil {
		return false
	}
	q, e := battlepb.NewBattleNodeClient(conn).QueryBattleCreation(ctx, &battlepb.QueryBattleCreationRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId})
	if e != nil {
		return false
	}
	if q.State == battlepb.BattleCreationState_BATTLE_CREATION_PRESENT {
		if f.OutcomeProtocolVersion == 1 && q.GetTargetInstanceId() == f.TargetInstanceId {
			if e = store.RecordBattleAdopted(ctx, f, q.TargetInstanceId, q.AdoptedOutcomeProtocolVersion); e == nil && q.AdoptedOutcomeProtocolVersion == 1 {
				_ = store.Finish(ctx, f, true)
			}
		}
	}
	return q.State == battlepb.BattleCreationState_BATTLE_CREATION_ABORTED && q.GetTargetInstanceId() == f.TargetInstanceId
}

// 薄协议适配与全局编排同层；唯一业务提交点仍为Authority原事务。
func CommitBattleOutcome(ctx context.Context, s *svc.ServiceContext, in *pb.CommitBattleOutcomeRequest) (*pb.CommitBattleOutcomeResponse, error) {
	if details, client := ctxkeys.GetSessionDetails(ctx); client && details != nil {
		return nil, status.Error(codes.PermissionDenied, "客户端不能提交内部战斗终局")
	}
	if s == nil || s.BattleOutcomeAuth == nil {
		return nil, status.Error(codes.Unavailable, "战斗终局独立签名未装配")
	}
	method := pb.MatchInternal_CommitBattleOutcome_FullMethodName
	caller, err := s.BattleOutcomeAuth.Verify(ctx, method, in)
	if err != nil || caller != "battle" {
		return nil, status.Error(codes.PermissionDenied, "Battle原终局证明无效")
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "终局原件无效")
	}
	decision, err := s.Authority.CommitBattleOutcome(ctx, in)
	if err != nil {
		if errors.Is(err, authority.ErrBattleOutcomeOriginal) {
			logx.Error("metric=match_terminal_outcome_rejected reason=original_conflict")
			return nil, status.Error(codes.FailedPrecondition, "终局原件存在永久冲突")
		}
		logx.Error("metric=match_terminal_outcome_unconfirmed reason=storage_or_transport")
		return nil, status.Error(codes.Unavailable, "终局原提交尚未确认")
	}
	// 终局已落库,这场的观战数据不再需要。尽力而为,失败只记日志,不影响终局回执。
	releaseSpectateOnOutcome(s, in.GetBattleId())
	out := &pb.CommitBattleOutcomeResponse{Decision: decision, RequestDigest: digest}
	if err = serviceauth.Sign(method+"/reply", out, "match", s.BattleOutcomeSecret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, status.Error(codes.Unavailable, "终局回执无法签名")
	}
	return out, nil
}

// 补签只读持久创建意图，Redis投影丢失不构成“战斗已结束”的证据。
func globalRequestBattleTicket(ctx context.Context, s *svc.ServiceContext, pid, id uint64) (*battlepb.RequestBattleTicketResponse, error) {
	observerOperation, authErr := s.Authority.AuthorizeBattleTicket(ctx, pid, id)
	if authErr != nil {
		return nil, authErr
	}
	f, e := s.Authority.Formation(ctx, id)
	if errors.Is(e, sql.ErrNoRows) {
		return &battlepb.RequestBattleTicketResponse{ErrorMessage: tipErr(uint32(table.CommonError_kInvalidParameter), "该战斗不存在或已结束")}, nil
	}
	if e != nil {
		return nil, e
	}
	// battle 自己验证参战者和已登记观众，SQL 落点不依赖 Redis 展示索引。
	if f.State == 5 {
		return &battlepb.RequestBattleTicketResponse{ErrorMessage: tipErr(uint32(table.CommonError_kInvalidParameter), "该战斗不存在或已结束")}, nil
	}
	conn, e := discovery.DialEndpoint(f.TargetEndpoint)
	if e != nil {
		return nil, e
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := battlepb.NewBattleNodeClient(conn)
	q, e := client.QueryBattleCreation(qctx, &battlepb.QueryBattleCreationRequest{BattleId: id, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId})
	if e != nil {
		return nil, e
	}
	if q.State == battlepb.BattleCreationState_BATTLE_CREATION_UNKNOWN || f.State < 2 {
		return &battlepb.RequestBattleTicketResponse{ErrorMessage: tipErr(constants.ErrInternal, "战斗创建结果正在恢复，请稍后重试")}, nil
	}
	if q.State != battlepb.BattleCreationState_BATTLE_CREATION_PRESENT {
		if f.State != 4 && q.State != battlepb.BattleCreationState_BATTLE_CREATION_ABORTED {
			return &battlepb.RequestBattleTicketResponse{ErrorMessage: tipErr(constants.ErrInternal, "战斗尚未确认，请稍后重试")}, nil
		}
		return &battlepb.RequestBattleTicketResponse{ErrorMessage: tipErr(uint32(table.CommonError_kInvalidParameter), "该战斗不存在或已结束")}, nil
	}
	r, e := client.IssueBattleTicket(qctx, &battlepb.IssueBattleTicketRequest{BattleId: id, PlayerId: pid, SpectatorOperationId: observerOperation, TargetInstanceId: f.TargetInstanceId})
	if e != nil {
		return nil, e
	}
	return &battlepb.RequestBattleTicketResponse{ErrorMessage: r.ErrorMessage, Assignment: r.Assignment}, nil
}

// globalAbortPrepared 收尾发房前失败的成局。culprits 是确定由自己导致失败的成员(0 忽略):
// 没有 Create 原件时,单排肇事者的票终止、其余成员回队(authority.FinishFailed);有原件时等唯一 SQL 终局。
func globalAbortPrepared(s *svc.ServiceContext, f *pb.MatchFormationRecord, culprits ...uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	latest, e := s.Authority.Formation(ctx, f.BattleId)
	cancel()
	if e != nil || latest.RetryAtMs != f.RetryAtMs {
		return
	}
	f = latest
	var prepared []preparedMember
	for _, m := range f.Prepared {
		prepared = append(prepared, preparedMember{playerId: m.PlayerId, sceneEndpoint: m.Endpoint})
	}
	if len(f.CreateRequest) != 0 {
		// 已发房的runtime ABORTED也不能先释放原票；仅唯一SQL ABORT允许清原备战。
		// UNKNOWN等原工作租约/固定deadline巡检收尾，COMPLETE仍等待个人APPLIED。
		terminalCtx, terminalCancel := context.WithTimeout(context.Background(), 3*time.Second)
		decision, err := s.Authority.RecoverExpiredOutcome(terminalCtx, f)
		s.HoldRelay(err)
		terminalCancel()
		if err == nil && decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT {
			cancelPrepared(s, prepared, f.BattleId)
		}
		return
	}
	check, stop := context.WithTimeout(context.Background(), 3*time.Second)
	e = s.Authority.CheckWork(check, f)
	stop()
	if e != nil {
		return
	}
	cancelPrepared(s, prepared, f.BattleId)
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer finishCancel()
	// 走到这里的成局都没有 Create 原件（上面有原件的已经返回）。计数只在提交之后打（2026-10-22 评审补）：
	// 原先在 globalDestroy 判定时就计 abort/no_create_intent，之后租约变化、CheckWork 或 Finish 失败都没有释放，
	// 值班会把「尝试」读成「已释放」。
	if e := s.Authority.FinishFailed(finishCtx, f, culprits); e != nil {
		s.HoldRelay(e)
		metrics.ObserveGlobalFlow("abort", "no_create_intent_unconfirmed")
		logx.Error("全球匹配中止提交未确认")
		return
	}
	metrics.ObserveGlobalFlow("abort", "no_create_intent_released")
	logx.Infof("global formation released before create: no persisted create intent battle_id=%d", f.BattleId)
}
