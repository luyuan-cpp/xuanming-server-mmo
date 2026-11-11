package logic

import (
	"context"
	"fmt"

	"match/internal/authority"
	"match/internal/discovery"
	"match/internal/svc"
	pb "proto/match"
	"shared/releasetrack"

	"google.golang.org/grpc/metadata"
)

// 战斗按整局选轨道(docs/design/release-track-gray-canary.md §2.6)。
//
//   - 会话轨道由路由服(或 gate 直连)写在 metadata releasetrack.MetadataKey 里,探测计划落库时记进玩家网络档案;
//   - 一局的轨道看全员档案:全员都是 canary 才是 canary,有一个 stable(或旧档案没记轨道)就是 stable;
//   - 机房始终按 stable 节点选(探测目标、机房集合都只看 stable 镜像),canary 局在选定的机房里先找 canary 节点,
//     找不到退回 stable:和客户端请求链路一样,退回只有 canary → stable 一个方向,stable 局永远不进 canary 节点。

// sessionReleaseTrack 读本次请求的会话轨道。读不到按 stable 算(服务间调用、路由服之前的旧链路);
// 写了多份或认不出就拒绝,和路由服对 ForwardRequest.release_track 的口径一致,不猜它属于哪条轨道。
func sessionReleaseTrack(ctx context.Context) (releasetrack.Track, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(releasetrack.MetadataKey)
	switch len(values) {
	case 0:
		return releasetrack.Stable, nil
	case 1:
		return releasetrack.Parse(values[0])
	default:
		return releasetrack.Stable, fmt.Errorf("metadata %s carries %d values", releasetrack.MetadataKey, len(values))
	}
}

// battleTrackOf 是一局的轨道:每个成员的网络档案都记着 canary 才是 canary。没有成员、缺档案、
// 空串(本字段之前写的档案)或认不出的取值一律按 stable,宁可让金丝雀玩家打 stable 局,也不让 stable 玩家进 canary 局。
func battleTrackOf(profiles []*pb.MatchNetworkProfile) releasetrack.Track {
	if len(profiles) == 0 {
		return releasetrack.Stable
	}
	for _, profile := range profiles {
		track, err := releasetrack.Parse(profile.GetReleaseTrack())
		if err != nil || !track.IsCanary() {
			return releasetrack.Stable
		}
	}
	return releasetrack.Canary
}

// poolClaimTrack 是排队成局这一组的轨道:组里每张票上的网络档案(入队时复制,等待中随回执刷新)都记着 canary 才是 canary。
// 解不开的档案按 stable;撮合输入 poolUnitInput 已经拒过这样的票,这里只是不让它意外变成 canary。
func poolClaimTrack(claim poolClaim) releasetrack.Track {
	var profiles []*pb.MatchNetworkProfile
	for _, unit := range claim.units {
		for _, ticket := range unit.tickets {
			profile, err := authority.DecodeProfile(ticket.NetworkProfile)
			if err != nil {
				return releasetrack.Stable
			}
			profiles = append(profiles, profile)
		}
	}
	return battleTrackOf(profiles)
}

// pickBattleNode 在已选定的机房 region 里给一局选 battle 节点。stableNodes 是调用方选机房时用的 stable 快照,
// 选点与它同一份,机房集合和落点不会因为两次读镜像而对不上。canary 局先在 canary 镜像里选,选不到再用 stable。
func pickBattleNode(s *svc.ServiceContext, track releasetrack.Track, stableNodes []discovery.NodeEntry, region uint32) (discovery.NodeEntry, bool) {
	if track.IsCanary() && s.CanaryBattleNodes != nil {
		if node, ok := s.Placement.Pick(s.CanaryBattleNodes.Snapshot(), region); ok {
			return node, true
		}
	}
	return s.Placement.Pick(stableNodes, region)
}

// battleEntryOfNode 按 node_id 找 battle 节点的登记:先查 stable 镜像,查不到再查 canary 镜像(两者同一个节点类型,
// node_id 不会重号)。用于确认已有对局的实例,不分轨道;两边都查不到时返回 stable 镜像的错误。
func battleEntryOfNode(s *svc.ServiceContext, node uint32) (discovery.NodeEntry, error) {
	entry, err := s.BattleNodes.EntryOfNode(node)
	if err == nil || s.CanaryBattleNodes == nil {
		return entry, err
	}
	if canaryEntry, canaryErr := s.CanaryBattleNodes.EntryOfNode(node); canaryErr == nil {
		return canaryEntry, nil
	}
	return entry, err
}

// battleOnCanaryNode 判定这局固化的落点(node_id + 实例 uuid)此刻在 canary 镜像里有唯一、同一实例的登记。只读内存镜像。
// 观战入口据此拦 stable 会话(stable 会话永远不进 canary,设计文档 §3)。正面确认是 canary 才拦,拦了就是永久结论(16018)。
// 查不到不拦,交给原有判定:不存在、已换实例、L0 确认不到实例都按原口径处理(其中 L0 是可重试的失败)。
// 代价是一个很窄的放过窗口:canary 镜像还没同步到这台节点(match 刚启动)时,stable 会话可能看到 canary 对局。
// etcd 断开时镜像保留最后一次的登记,不会因此放过;反过来,要求「正面确认是 stable 才放行」会把镜像暂缺时可重试的情形
// 变成永久拒绝,所以不那样做。
func battleOnCanaryNode(s *svc.ServiceContext, node uint32, instance string) bool {
	if s.CanaryBattleNodes == nil {
		return false
	}
	entry, err := s.CanaryBattleNodes.EntryOfNode(node)
	return err == nil && entry.NodeUuid != "" && entry.NodeUuid == instance
}
