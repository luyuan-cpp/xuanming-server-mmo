package logic

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"

	"github.com/google/uuid"
	"match/internal/authority"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"
	base "proto/common/base"
	pb "proto/match"
	"shared/workshard"
)

func playerSessionStaleFor(ctx context.Context, s *svc.ServiceContext, pid uint64) (bool, error) {
	return NewJoinQueueLogic(ctx, s).callerSessionStale(pid)
}

func GetNetworkProbePlan(ctx context.Context, s *svc.ServiceContext) (*pb.GetNetworkProbePlanResponse, error) {
	d, ok := ctxkeys.GetSessionDetails(ctx)
	binding := authority.SessionBinding(ctx, d)
	if !ok || binding == "" {
		return nil, errors.New("地域探测需要当前已登录会话")
	}
	if stale, e := playerSessionStaleFor(ctx, s, d.PlayerId); e != nil || stale {
		return nil, errors.New("会话已经失效")
	}
	// 会话轨道随计划一起落库(发布轨道设计 §2.6):回执提交沿用计划里的值,成局时据此判这局放哪条轨道的 battle。
	track, e := sessionReleaseTrack(ctx)
	if e != nil {
		return nil, e
	}
	n, e := s.Authority.Now(ctx)
	if e != nil {
		return nil, e
	}
	expires := n + uint64(s.Config.GlobalMatch.ProbeTTLSeconds)*1000
	plan := uuid.NewString()
	resp := &pb.GetNetworkProbePlanResponse{PlanId: plan, ExpiresAtMs: expires, Required: true}
	for _, node := range probeTargets(s.BattleNodes.Snapshot(), d.PlayerId) {
		g := &pb.NetworkProbeGrant{PlanId: plan, PlayerId: d.PlayerId, SessionBinding: binding, PhysicalRegion: node.PhysicalRegion, TargetNodeId: node.NodeId, TargetInstanceId: node.NodeUuid, ExpiresAtMs: expires}
		signed, e := authority.Signed(s.Authority.Secret, authority.GrantDomain, g)
		if e != nil {
			return nil, e
		}
		// WsUrl:小游戏客户端测速连的 WebSocket 地址,同一台 battle 自报;没自报为空串,原生客户端不读它。
		resp.Targets = append(resp.Targets, &pb.NetworkProbeTarget{PhysicalRegion: node.PhysicalRegion, Host: node.ClientHost, Port: node.ClientPort, WsUrl: node.ClientWsURL, Grant: signed})
	}
	if len(resp.Targets) == 0 {
		resp.ErrorMessage = tipErr(noProbeTargetCode(s.BattleNodes.Synced()), "地域探测端点暂不可用")
		return resp, nil
	}
	if e = s.Authority.SavePlan(ctx, d.PlayerId, &pb.MatchNetworkProfile{PlanId: plan, SessionBinding: binding, ReleaseTrack: track.String()}, expires); e != nil {
		return nil, e
	}
	metrics.ObserveGlobalFlow("probe_plan", "ok")
	return resp, nil
}

// noProbeTargetCode 是探测计划一个目标都没有时回的码。battle 镜像从 etcd 同步过、确实没有可探测的节点(都没注册、缺客户端地址,
// 或全部自报 CLOSED,比如维护、整机房排空)是「此刻没有可用的服务器」,回业务拒绝 ErrNoRegionWithinLatency,不计服务端故障;
// 镜像还没同步过时不知道有没有节点,是本服务自己没就绪,回 ErrInternal(按设计稿 D8,这个码只给服务端内部错误、标为 fault)。
// 客户端对两者给同一句提示,不用区分。
func noProbeTargetCode(mirrorSynced bool) uint32 {
	if mirrorSynced {
		return constants.ErrNoRegionWithinLatency
	}
	return constants.ErrInternal
}

// probeTargets 给 playerID 的探测计划选目标:每个机房一台,机房按其中最小的可探测 NodeId 排,最多 authority.MaxProbeRegions 个。
// nodes 会被就地按 NodeId 排序(调用方传的是快照副本)。
//
// 机房内按玩家分散(2026-11-19 前只取 NodeId 最小的一台,全服的探测都打到它,碰上 battle 直连的新建速率闸就被 RST):
// 在本机房可探测节点上做最高随机权重哈希(workshard.Owner,键 = player_id,成员 = 实例 UUID)。各台分到的玩家大致相等,
// 节点集合不变时同一玩家总落同一台,节点增减只挪动落在增减节点上的玩家,结果与快照顺序和 match 副本无关。
// 同一机房哪一台测的 RTT 都代表这个机房,回执校验(CommitNetworkProbe)也不看是哪一台。
//
// 自报 CLOSED 的节点不探:启动未完成时直连面可能还没装好,停机或维护关源后新直连在 accept 处就被 RST,计划排空的节点马上要走;
// 探测失败时 Unity 客户端这一轮缺这个机房,robot 整轮不提交。机房里只剩 CLOSED 时整个机房不进计划(它本来也不可放置),代价是它重新开放后,
// 期间拿到计划的玩家要到下一次重探才有它的回执。UNSPECIFIED 照常可探测(理由同 battleplace);本副本的本地熔断不看:
// 它记的是建房失败,各副本不同。推导见 docs/design/global-match-hierarchical-pools.md §5.1「探测目标」。
func probeTargets(nodes []discovery.NodeEntry, playerID uint64) []discovery.NodeEntry {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeId < nodes[j].NodeId })
	var regions []uint32
	candidates := map[uint32][]discovery.NodeEntry{}
	for _, node := range nodes {
		if node.PhysicalRegion == 0 || node.ClientHost == "" || node.ClientPort == 0 || node.ClientPort > 65535 || node.NodeUuid == "" || node.Admission == base.NodeAdmission_NODE_ADMISSION_CLOSED {
			continue
		}
		if _, listed := candidates[node.PhysicalRegion]; !listed {
			if len(regions) >= authority.MaxProbeRegions {
				continue
			}
			regions = append(regions, node.PhysicalRegion)
		}
		candidates[node.PhysicalRegion] = append(candidates[node.PhysicalRegion], node)
	}
	key := strconv.FormatUint(playerID, 10)
	targets := make([]discovery.NodeEntry, 0, len(regions))
	for _, region := range regions {
		inRegion := candidates[region]
		members := make([]string, len(inRegion))
		for i, node := range inRegion {
			members[i] = node.NodeUuid
		}
		owner := workshard.Owner(key, members)
		// 成员都非空,Owner 必返回其中之一;同一 UUID 有两条登记时取 NodeId 小的那条。
		targets = append(targets, inRegion[slices.IndexFunc(inRegion, func(node discovery.NodeEntry) bool { return node.NodeUuid == owner })])
	}
	return targets
}

func CommitNetworkProbe(ctx context.Context, s *svc.ServiceContext, in *pb.CommitNetworkProbeRequest) (*pb.CommitNetworkProbeResponse, error) {
	d, ok := ctxkeys.GetSessionDetails(ctx)
	binding := authority.SessionBinding(ctx, d)
	if !ok || binding == "" || in.PlanId == "" || len(in.Receipts) == 0 || len(in.Receipts) > authority.MaxProbeRegions {
		return nil, errors.New("地域回执参数无效")
	}
	if stale, e := playerSessionStaleFor(ctx, s, d.PlayerId); e != nil || stale {
		return nil, errors.New("会话已经失效")
	}
	n, e := s.Authority.Now(ctx)
	if e != nil {
		return nil, e
	}
	expires := ^uint64(0)
	seen := map[uint32]bool{}
	var verified []*pb.NetworkProbeReceipt
	for _, signed := range in.Receipts {
		r := &pb.NetworkProbeReceipt{}
		if e = authority.Verify(s.Authority.Secret, authority.ReceiptDomain, signed, r); e != nil {
			return nil, e
		}
		g := r.GetGrant()
		if g == nil || g.PlanId != in.PlanId || g.PlayerId != d.PlayerId || g.SessionBinding != binding || g.PhysicalRegion == 0 || seen[g.PhysicalRegion] || r.SampleCount != 3 || r.RttMs == 0 || r.MeasuredAtMs > n || r.ExpiresAtMs <= n || r.ExpiresAtMs > g.ExpiresAtMs {
			return nil, errors.New("地域回执身份或时效不符")
		}
		seen[g.PhysicalRegion] = true
		metrics.ObserveProbeRTT(r.RttMs)
		verified = append(verified, r)
		if r.ExpiresAtMs < expires {
			expires = r.ExpiresAtMs
		}
	}
	if e = s.Authority.CommitProfile(ctx, d.PlayerId, in.PlanId, binding, verified, expires); e != nil {
		return nil, e
	}
	metrics.ObserveGlobalFlow("probe_commit", "ok")
	return &pb.CommitNetworkProbeResponse{ExpiresAtMs: expires}, nil
}
