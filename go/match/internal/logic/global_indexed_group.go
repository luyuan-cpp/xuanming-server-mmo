package logic

import (
	"context"
	"sort"

	"match/internal/authority"
	"match/internal/discovery"
	"match/internal/svc"
	pb "proto/match"
)

type globalPeerReader func(context.Context, *pb.MatchAuthorityTicketRecord, uint32, bool, uint64, float64) ([]*pb.MatchAuthorityTicketRecord, error)

// 每轮只处理至多32个锚点，但候选通过region/lane索引查完整队列，不受锚点页边界限制。
// 所有本区尝试均先于扩区；单个锚点至多触及其已签名的16个可达机房。
func pickIndexedGlobalGroup(ctx context.Context, s *svc.ServiceContext, anchors []*pb.MatchAuthorityTicketRecord, nodes []discovery.NodeEntry, now uint64, allowCross bool, peers globalPeerReader) (*globalChoice, error) {
	byRegion := map[uint32]discovery.NodeEntry{}
	for _, node := range nodes {
		if node.PhysicalRegion == 0 || node.NodeUuid == "" {
			continue
		}
		if prior, ok := byRegion[node.PhysicalRegion]; !ok || node.NodeId < prior.NodeId {
			byRegion[node.PhysicalRegion] = node
		}
	}
	for pass := 0; pass < 2; pass++ {
		if pass == 1 && !allowCross {
			break
		}
		for _, anchor := range anchors {
			if anchor.Mode == uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM) || anchor.Mode == uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) || anchor.TeamId != 0 {
				continue // PVE_TEAM由完整unit装箱调度，不能逐人消耗队伍票。
			}
			if anchor.NotBeforeMs > now || anchor.ExpiresMs <= now || anchor.TeamSize == 0 || anchor.TeamSize > 10 {
				continue
			}
			profile, err := authority.DecodeProfile(anchor.NetworkProfile)
			if err != nil || len(profile.GetReceipts()) > authority.MaxProbeRegions {
				continue
			}
			preferred := authority.PreferredRegion(profile, now)
			if preferred == 0 || anchor.PreferredRegion != preferred {
				continue // 缺投影必须先修复，不能换首选区后把它当跨区授权。
			}
			latencies := authority.Latencies(profile, now, s.Config.GlobalMatch.HardLatencyMs)
			regions := make([]uint32, 0, len(latencies))
			for region := range latencies {
				if _, ok := byRegion[region]; ok && (pass == 1 || region == preferred) {
					regions = append(regions, region)
				}
			}
			sort.Slice(regions, func(i, j int) bool { return regions[i] < regions[j] })
			waitMs := now - min(now, anchor.EnqueuedMs)
			if pass == 1 && waitMs < uint64(s.Config.GlobalMatch.CrossRegionWaitSeconds)*1000 {
				continue
			}
			tolerance := anchorToleranceFor(s, isRatedMode(pb.MatchMode(anchor.Mode)), int64(waitMs/1000)) * 100
			for _, region := range regions {
				found, err := peers(ctx, anchor, region, pass == 0, now, tolerance)
				if err != nil {
					return nil, err
				}
				if len(found)+1 != int(anchor.TeamSize) {
					continue
				}
				group := make([]*pb.MatchAuthorityTicketRecord, 1, anchor.TeamSize)
				group[0] = anchor
				group = append(group, found...)
				lastAttempted := anchor
				if pass == 1 {
					lastAttempted = anchors[len(anchors)-1]
				}
				return &globalChoice{group: group, node: byRegion[region], region: region, scannedUntil: lastAttempted}, nil
			}
		}
	}
	return nil, nil
}
