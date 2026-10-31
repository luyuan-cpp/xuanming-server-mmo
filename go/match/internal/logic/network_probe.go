package logic

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"match/internal/authority"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"
	pb "proto/match"
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
	n, e := s.Authority.Now(ctx)
	if e != nil {
		return nil, e
	}
	expires := n + uint64(s.Config.GlobalMatch.ProbeTTLSeconds)*1000
	plan := uuid.NewString()
	resp := &pb.GetNetworkProbePlanResponse{PlanId: plan, ExpiresAtMs: expires, Required: true}
	nodes := s.BattleNodes.Snapshot()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeId < nodes[j].NodeId })
	seen := map[uint32]bool{}
	for _, node := range nodes {
		if node.PhysicalRegion == 0 || node.ClientHost == "" || node.ClientPort == 0 || node.ClientPort > 65535 || node.NodeUuid == "" || seen[node.PhysicalRegion] {
			continue
		}
		if len(resp.Targets) >= authority.MaxProbeRegions {
			break
		}
		g := &pb.NetworkProbeGrant{PlanId: plan, PlayerId: d.PlayerId, SessionBinding: binding, PhysicalRegion: node.PhysicalRegion, TargetNodeId: node.NodeId, TargetInstanceId: node.NodeUuid, ExpiresAtMs: expires}
		signed, e := authority.Signed(s.Authority.Secret, authority.GrantDomain, g)
		if e != nil {
			return nil, e
		}
		resp.Targets = append(resp.Targets, &pb.NetworkProbeTarget{PhysicalRegion: node.PhysicalRegion, Host: node.ClientHost, Port: node.ClientPort, Grant: signed})
		seen[node.PhysicalRegion] = true
	}
	if len(resp.Targets) == 0 {
		resp.ErrorMessage = tipErr(constants.ErrInternal, "地域探测端点暂不可用")
		return resp, nil
	}
	if e = s.Authority.SavePlan(ctx, d.PlayerId, &pb.MatchNetworkProfile{PlanId: plan, SessionBinding: binding}, expires); e != nil {
		return nil, e
	}
	metrics.ObserveGlobalFlow("probe_plan", "ok")
	return resp, nil
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
