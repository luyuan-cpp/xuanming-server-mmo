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
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"
	battlepb "proto/battle"
	pb "proto/match"
	"shared/generated/pb/table"
	"shared/serviceauth"
)

func globalJoin(l *JoinQueueLogic, in *pb.JoinQueueRequest, required, zone uint32) (*pb.JoinQueueResponse, error) {
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
		return &pb.JoinQueueResponse{ErrorCode: constants.ErrInternal, ErrorMessage: tipErr(constants.ErrInternal, "请完成地域延迟探测后重试")}, nil
	}

	t := &pb.MatchAuthorityTicketRecord{PlayerId: d.PlayerId, Ticket: uuid.NewString(), RequestId: in.RequestId, Mode: uint32(in.Mode), ConfigId: in.BattleConfigId, TeamSize: required, ZoneId: zone, RatingCenti: int64(math.Round(loadRatingOrDefault(l.svcCtx, d.PlayerId) * 100))}
	prepared, exit, pending, e := l.svcCtx.Authority.PrepareJoin(l.ctx, t, in, binding)
	if e != nil {
		return &pb.JoinQueueResponse{ErrorCode: constants.ErrInternal, ErrorMessage: tipErr(constants.ErrInternal, "匹配原意图暂不可用")}, nil
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
		return &pb.JoinQueueResponse{ErrorCode: constants.ErrInternal, ErrorMessage: tipErr(constants.ErrInternal, "匹配权威暂不可用或探测已过期")}, nil
	}
	metrics.ObserveGlobalFlow("join", "accepted")
	if !created && old.RequestId != in.RequestId {
		return &pb.JoinQueueResponse{QueueTicket: old.Ticket, ErrorCode: constants.ErrAlreadyQueued, ErrorMessage: tipErr(constants.ErrAlreadyQueued, "已在匹配队列中")}, nil
	}
	return &pb.JoinQueueResponse{QueueTicket: old.Ticket}, nil
}

type globalChoice struct {
	group        []*pb.MatchAuthorityTicketRecord
	node         discovery.NodeEntry
	region       uint32
	scannedUntil *pb.MatchAuthorityTicketRecord
}

func reserveGlobalDirect(ctx context.Context, s *svc.ServiceContext, key *pb.MatchDirectIntentRecord, payload []byte, mode pb.MatchMode, config uint32, members []uint64, tickets map[uint64]string, opts gatherOptions) (*pb.MatchFormationRecord, error) {
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
		f, group, err = buildGlobalDirect(ctx, s, mode, config, members, tickets, opts)
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

func buildGlobalDirect(ctx context.Context, s *svc.ServiceContext, mode pb.MatchMode, config uint32, members []uint64, tickets map[uint64]string, opts gatherOptions) (*pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord, error) {
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
	var target discovery.NodeEntry
	bestMax, bestSum := uint32(math.MaxUint32), uint64(math.MaxUint64)
	for _, node := range s.BattleNodes.Snapshot() {
		if node.PhysicalRegion == 0 || node.NodeUuid == "" {
			continue
		}
		maxRTT := uint32(0)
		sum := uint64(0)
		eligible := true
		for _, pid := range members {
			rtt, ok := lat[pid][node.PhysicalRegion]
			if !ok {
				eligible = false
				break
			}
			maxRTT = max(maxRTT, rtt)
			sum += uint64(rtt)
		}
		if eligible && (maxRTT < bestMax || (maxRTT == bestMax && (sum < bestSum || (sum == bestSum && node.PhysicalRegion < target.PhysicalRegion)))) {
			target = node
			bestMax = maxRTT
			bestSum = sum
		}
	}
	if target.PhysicalRegion == 0 {
		return nil, nil, errors.New("没有满足全员硬延迟的共同机房")
	}
	id := opts.presetBattleID
	if id == 0 {
		id, e = s.BattleIDGen.Generate()
		if e != nil {
			return nil, nil, e
		}
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
		group = append(group, &pb.MatchAuthorityTicketRecord{PlayerId: pid, Ticket: ticket, RequestId: f.OperationId, Mode: uint32(mode), ConfigId: config, TeamSize: uint32(len(members)), TeamId: opts.directTeamID, OwnerAccount: profiles[pid].OwnerAccount, OwnerRevision: profiles[pid].OwnerRevision, RatingCenti: int64(math.Round(loadRatingOrDefault(s, pid) * 100))})
	}
	return f, group, nil
}

// 后台入口只能恢复已经在业务成功回包之前提交的成局，绝不现场再发一个 operation。
func globalDirectGather(s *svc.ServiceContext, mode pb.MatchMode, config uint32, members []uint64, tickets map[uint64]string, opts gatherOptions) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var f *pb.MatchFormationRecord
	var e error
	if opts.presetBattleID != 0 {
		f, e = s.Authority.Formation(ctx, opts.presetBattleID)
	} else if len(members) > 0 {
		t, err := s.Authority.Ticket(ctx, members[0])
		e = err
		if err == nil && t != nil && t.FormationId != 0 && t.Ticket == tickets[members[0]] {
			f, e = s.Authority.Formation(ctx, t.FormationId)
		}
	}
	if e != nil || f == nil || f.Mode != uint32(mode) || f.ConfigId != config || len(f.Members) != len(members) {
		return false
	}
	for i, m := range f.Members {
		if m.PlayerId != members[i] || (len(tickets) > 0 && m.Ticket != tickets[m.PlayerId]) {
			return false
		}
	}
	if f.State == 4 {
		return true
	}
	if f.State == 5 {
		return false
	}
	runGlobalFormation(s, f)
	read, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	f, e = s.Authority.Formation(read, f.BattleId)
	return e == nil && f.State == 4
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
	// 成功开房的state4不在旧Pending集合内，必须独立巡检；不依赖Guild取消或Battle存活。
	recoverCtx, recoverCancel := context.WithTimeout(ctx, 3*time.Second)
	var recoverErr error
	s.GlobalOutcomeRetryMs, s.GlobalOutcomeBattle, recoverErr = s.Authority.RecoverExpiredOutcomes(recoverCtx, s.GlobalOutcomeRetryMs, s.GlobalOutcomeBattle)
	recoverCancel()
	if recoverErr != nil {
		s.HoldRelay(recoverErr)
		logx.Error("全球匹配到期终局恢复未确认，保留原件等待下轮")
	}
	pending, e := s.Authority.Pending(ctx)
	if e != nil {
		logx.Error("全球匹配恢复查询失败")
		return
	}
	for _, f := range pending {
		launchGlobalFormation(s, f)
	}
	// 候选投影故障不能挡住已经提交的formation恢复。
	if !s.GlobalRegionRepairActive {
		throughMs, throughPlayer, err := s.Authority.CandidateRegionRepairHighWater(ctx)
		if err != nil {
			logx.Error("全球匹配候选投影尾界读取失败")
			return
		}
		s.GlobalRegionRepairThroughMs, s.GlobalRegionRepairThroughPlayer = throughMs, throughPlayer
		s.GlobalRegionRepairActive = true
	}
	repairMs, repairPlayer, repairErr := s.Authority.RepairCandidateRegionsThrough(ctx, s.GlobalRegionRepairMs, s.GlobalRegionRepairPlayer, s.GlobalRegionRepairThroughMs, s.GlobalRegionRepairThroughPlayer)
	if repairErr != nil {
		s.HoldRelay(repairErr)
		logx.Error("全球匹配候选投影维护失败")
		return
	}
	s.GlobalRegionRepairMs, s.GlobalRegionRepairPlayer = repairMs, repairPlayer
	if repairMs == 0 && repairPlayer == 0 {
		s.GlobalRegionProjectionReady.Store(true)
		s.GlobalRegionRepairActive = false
	}
	if s.GlobalGatherSlots != nil && len(s.GlobalGatherSlots) == cap(s.GlobalGatherSlots) {
		return
	}
	all, e := s.Authority.CandidatesAfter(ctx, s.GlobalScanMs, s.GlobalScanPlayer)
	if e != nil {
		logx.Error("全球匹配候选读取失败")
		return
	}
	if len(all) == 0 {
		s.GlobalScanMs = 0
		s.GlobalScanPlayer = 0
		return
	}
	n, e := s.Authority.Now(ctx)
	if e != nil {
		return
	}
	choice, e := pickIndexedGlobalGroup(ctx, s, all, s.BattleNodes.Snapshot(), n, s.GlobalRegionProjectionReady.Load(), s.Authority.CandidatePeers)
	if e != nil {
		logx.Error("全球匹配机房候选读取失败")
		return
	}
	last := all[len(all)-1]
	if choice != nil && choice.scannedUntil != nil {
		last = choice.scannedUntil
	}
	s.GlobalScanMs, s.GlobalScanPlayer = last.EnqueuedMs, last.PlayerId
	if choice == nil {
		return
	}
	id, e := s.BattleIDGen.Generate()
	if e != nil {
		return
	}
	a := choice.group[0]
	f := &pb.MatchFormationRecord{BattleId: id, OperationId: uuid.NewString(), PhysicalRegion: choice.region, TargetNodeId: choice.node.NodeId, TargetInstanceId: choice.node.NodeUuid, TargetEndpoint: choice.node.Endpoint, Mode: a.Mode, ConfigId: a.ConfigId, RequeueOnFailure: true}
	if e = s.Authority.Claim(ctx, f, choice.group); e != nil {
		s.HoldRelay(e)
		metrics.ObserveGlobalFlow("claim", "conflict")
		return
	}
	metrics.ObserveGlobalFlow("claim", "committed")
	launchGlobalFormation(s, f)
}

func launchGlobalFormation(s *svc.ServiceContext, f *pb.MatchFormationRecord) {
	if s.GlobalGatherSlots == nil {
		_ = s.RunRelay(func() error { runGlobalFormation(s, f); return nil })
		return
	}
	select {
	case s.GlobalGatherSlots <- struct{}{}:
	default:
		return
	}
	if !s.LaunchRelayTask("match.global.gather", func() { defer func() { <-s.GlobalGatherSlots }(); runGlobalFormation(s, f) }) {
		<-s.GlobalGatherSlots
	}
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
		members := make([]uint64, 0, len(f.Members))
		tickets := map[uint64]string{}
		for _, m := range f.Members {
			members = append(members, m.PlayerId)
			tickets[m.PlayerId] = m.Ticket
		}
		var actx *battlepb.BattleActivityContext
		if len(f.ActivityContext) > 0 {
			actx = &battlepb.BattleActivityContext{}
			if proto.Unmarshal(f.ActivityContext, actx) != nil {
				return
			}
		}
		runGatherWithOptions(s, pb.MatchMode(f.Mode), f.ConfigId, members, false, true, tickets, gatherOptions{presetBattleID: f.BattleId, formation: f, activityContext: actx})
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
	if s == nil || s.Authority == nil || s.BattleOutcomeAuth == nil {
		return nil, status.Error(codes.Unavailable, "战斗终局权威或独立签名未装配")
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
	out := &pb.CommitBattleOutcomeResponse{Decision: decision, RequestDigest: digest}
	if err = serviceauth.Sign(method+"/reply", out, "match", s.BattleOutcomeSecret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, status.Error(codes.Unavailable, "终局回执无法签名")
	}
	return out, nil
}

// 全球模式补签只读持久创建意图，Redis投影丢失不构成“战斗已结束”的证据。
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
func globalAbortPrepared(s *svc.ServiceContext, f *pb.MatchFormationRecord) {
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
	if e := s.Authority.Finish(finishCtx, f, false); e != nil {
		s.HoldRelay(e)
		metrics.ObserveGlobalFlow("abort", "no_create_intent_unconfirmed")
		logx.Error("全球匹配中止提交未确认")
		return
	}
	metrics.ObserveGlobalFlow("abort", "no_create_intent_released")
	logx.Infof("global formation released before create: no persisted create intent battle_id=%d", f.BattleId)
}
