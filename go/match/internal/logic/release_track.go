package logic

import (
	"context"
	"database/sql"
	"errors"
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

// battleRunsOnStable 判定成局原件(或由它组出的观战记录)上记的轨道是 stable。轨道在选点时按选中节点写进原件,之后不变,
// 所以观战判定不看实时节点镜像,结论只取决于原件。空串是本字段之前的原件(迁移补列后读出 NULL),按 stable;
// 认不出的取值不算 stable:观战拦截据此 fail-closed,宁可让 stable 观众看不到这场,也不把它连到不明轨道的节点上。
func battleRunsOnStable(recordedTrack string) bool {
	track, err := releasetrack.Parse(recordedTrack)
	return err == nil && !track.IsCanary()
}

// viewerMayWatch 是观战的轨道规则:stable 观众只看 stable 对局(stable 会话永远不进 canary,设计文档 §3),
// canary 观众两条轨道都能看。观战入口、观战列表和随机选场都用这一条。
func viewerMayWatch(viewer releasetrack.Track, recordedTrack string) bool {
	return viewer.IsCanary() || battleRunsOnStable(recordedTrack)
}

// canaryBattlesMayExist:canary battle 镜像里有节点时才可能有活着的 canary 对局。只用来决定 stable 观众的列表多读几条候选,
// 不参与判定:镜像暂时为空而原件其实是 canary 的场,照样按原件滤掉,只是这一页可能短一点。
func canaryBattlesMayExist(s *svc.ServiceContext) bool {
	return s.CanaryBattleNodes != nil && len(s.CanaryBattleNodes.Snapshot()) > 0
}

// spectateRandomTrackAttempts 是 stable 观众随机选场时最多换几次场:canary 对局占比 p 时选不到的概率是 p 的这个次方。
const spectateRandomTrackAttempts = 8

// pickRandomBattleFor 给一个观众随机挑一场能看的战斗。canary 观众两条轨道都能看,直接用 pickRandomBattle;
// stable 观众每挑一场都读成局原件,挑到原件记着 canary 的场就换一场,最多 spectateRandomTrackAttempts 次,都挑不到返回 (0, nil),
// 与没有可观战的战斗同口径。判定只看原件,不看节点镜像:金丝雀池撤掉之后残留的 canary 场也不会随机给 stable 观众。
// 被跳过的场不摘索引(canary 观众照样能看);挑中的场在 SQL 里已经没有时原样返回,交给观战入口按残留摘索引(与原有随机选场同一出口)。
// 代价是 stable 观众每次随机多一次按主键读原件。
func pickRandomBattleFor(ctx context.Context, s *svc.ServiceContext, viewer releasetrack.Track) (uint64, error) {
	if viewer.IsCanary() {
		return pickRandomBattle(s)
	}
	for attempt := 0; attempt < spectateRandomTrackAttempts; attempt++ {
		battle, err := pickRandomBattle(s)
		if err != nil || battle == 0 {
			return battle, err
		}
		formation, err := s.Authority.Formation(ctx, battle)
		if errors.Is(err, sql.ErrNoRows) {
			return battle, nil
		}
		if err != nil {
			return 0, err
		}
		if viewerMayWatch(viewer, formation.GetReleaseTrack()) {
			return battle, nil
		}
	}
	return 0, nil
}
