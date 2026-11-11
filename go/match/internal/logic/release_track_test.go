package logic

import (
	"context"
	"strconv"
	"testing"

	"match/internal/battleplace"
	"match/internal/discovery"
	"match/internal/svc"

	pb "proto/match"
	"shared/releasetrack"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// 战斗按整局选轨道(docs/design/release-track-gray-canary.md §2.6)的判定与选点。

func TestSessionReleaseTrack(t *testing.T) {
	track, err := sessionReleaseTrack(context.Background())
	require.NoError(t, err)
	require.Equal(t, releasetrack.Stable, track, "读不到 metadata 按 stable(服务间调用、旧链路)")

	withTrack := func(values ...string) context.Context {
		md := metadata.MD{}
		for _, v := range values {
			md.Append(releasetrack.MetadataKey, v)
		}
		return metadata.NewIncomingContext(context.Background(), md)
	}
	track, err = sessionReleaseTrack(withTrack("canary"))
	require.NoError(t, err)
	require.Equal(t, releasetrack.Canary, track)
	track, err = sessionReleaseTrack(withTrack("stable"))
	require.NoError(t, err)
	require.Equal(t, releasetrack.Stable, track)
	_, err = sessionReleaseTrack(withTrack("beta"))
	require.Error(t, err, "认不出的轨道要拒绝,和路由服同口径")
	_, err = sessionReleaseTrack(withTrack("canary", "stable"))
	require.Error(t, err, "写了多份的会话轨道说明链路有错,不猜用哪一份")
}

func TestBattleTrackOfNeedsEveryMemberCanary(t *testing.T) {
	canary := &pb.MatchNetworkProfile{ReleaseTrack: "canary"}
	stable := &pb.MatchNetworkProfile{ReleaseTrack: "stable"}
	legacy := &pb.MatchNetworkProfile{}
	cases := []struct {
		name     string
		profiles []*pb.MatchNetworkProfile
		want     releasetrack.Track
	}{
		{name: "no members", want: releasetrack.Stable},
		{name: "all canary", profiles: []*pb.MatchNetworkProfile{canary, canary}, want: releasetrack.Canary},
		{name: "one stable member", profiles: []*pb.MatchNetworkProfile{canary, stable}, want: releasetrack.Stable},
		{name: "profile written before the field", profiles: []*pb.MatchNetworkProfile{canary, legacy}, want: releasetrack.Stable},
		{name: "missing profile", profiles: []*pb.MatchNetworkProfile{canary, nil}, want: releasetrack.Stable},
		{name: "unknown value", profiles: []*pb.MatchNetworkProfile{canary, {ReleaseTrack: "beta"}}, want: releasetrack.Stable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, battleTrackOf(tc.profiles))
		})
	}
}

func TestPoolClaimTrackReadsEveryTicketProfile(t *testing.T) {
	ticketOn := func(track string) *pb.MatchAuthorityTicketRecord {
		payload, err := proto.Marshal(&pb.MatchNetworkProfile{PlanId: "plan", SessionBinding: "binding", ReleaseTrack: track})
		require.NoError(t, err)
		return &pb.MatchAuthorityTicketRecord{NetworkProfile: payload}
	}
	allCanary := poolClaim{units: []poolUnit{
		{tickets: []*pb.MatchAuthorityTicketRecord{ticketOn("canary")}},
		{tickets: []*pb.MatchAuthorityTicketRecord{ticketOn("canary"), ticketOn("canary")}},
	}}
	require.Equal(t, releasetrack.Canary, poolClaimTrack(allCanary))
	mixed := poolClaim{units: []poolUnit{
		{tickets: []*pb.MatchAuthorityTicketRecord{ticketOn("canary")}},
		{tickets: []*pb.MatchAuthorityTicketRecord{ticketOn("canary"), ticketOn("stable")}},
	}}
	require.Equal(t, releasetrack.Stable, poolClaimTrack(mixed), "整队里有一个 stable 成员,整局就是 stable")
	broken := poolClaim{units: []poolUnit{{tickets: []*pb.MatchAuthorityTicketRecord{ticketOn("canary"), {NetworkProfile: []byte{0xff}}}}}}
	require.Equal(t, releasetrack.Stable, poolClaimTrack(broken), "解不开的档案不能让一局变成 canary")
}

// 两个机房:1 号机房 stable、canary 各一台,2 号机房只有 stable。
func newTrackPlacementCtx(t *testing.T) (*svc.ServiceContext, []discovery.NodeEntry) {
	t.Helper()
	s := &svc.ServiceContext{Placement: battleplace.New()}
	s.BattleNodes = discovery.NewNodeWatcher("battle", svc.BattleNodeRpcPrefix, nil, nil)
	s.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"1", discovery.NodeEntry{NodeId: 1, NodeUuid: "stable-1", Endpoint: "battle-1", PhysicalRegion: 1})
	s.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"2", discovery.NodeEntry{NodeId: 2, NodeUuid: "stable-2", Endpoint: "battle-2", PhysicalRegion: 2})
	s.CanaryBattleNodes = discovery.NewNodeWatcher("battle-canary", svc.BattleNodeRpcPrefix, nil, nil)
	s.CanaryBattleNodes.Upsert(svc.BattleNodeRpcPrefix+"9", discovery.NodeEntry{NodeId: 9, NodeUuid: "canary-9", Endpoint: "battle-9", PhysicalRegion: 1, Track: releasetrack.Canary})
	return s, s.BattleNodes.Snapshot()
}

func TestPickBattleNodeFollowsTheBattleTrack(t *testing.T) {
	s, stableNodes := newTrackPlacementCtx(t)

	node, ok := pickBattleNode(s, releasetrack.Canary, stableNodes, 1)
	require.True(t, ok)
	require.Equal(t, "canary-9", node.NodeUuid, "canary 局在选定机房里先用 canary 节点")

	node, ok = pickBattleNode(s, releasetrack.Canary, stableNodes, 2)
	require.True(t, ok)
	require.Equal(t, "stable-2", node.NodeUuid, "选定机房没有 canary 节点时退回 stable")

	node, ok = pickBattleNode(s, releasetrack.Stable, stableNodes, 1)
	require.True(t, ok)
	require.Equal(t, "stable-1", node.NodeUuid, "stable 局永远不进 canary 节点")

	s.CanaryBattleNodes = nil
	node, ok = pickBattleNode(s, releasetrack.Canary, stableNodes, 1)
	require.True(t, ok)
	require.Equal(t, "stable-1", node.NodeUuid, "没装配 canary 镜像时按 stable 选")

	_, ok = pickBattleNode(s, releasetrack.Stable, stableNodes, 3)
	require.False(t, ok, "机房里一台可选节点都没有就选不到")
}

func TestBattleLookupsCoverBothTracks(t *testing.T) {
	s, _ := newTrackPlacementCtx(t)

	entry, err := battleEntryOfNode(s, 9)
	require.NoError(t, err)
	require.Equal(t, "canary-9", entry.NodeUuid, "canary battle 上的对局也要认得出")
	entry, err = battleEntryOfNode(s, 1)
	require.NoError(t, err)
	require.Equal(t, "stable-1", entry.NodeUuid)
	_, err = battleEntryOfNode(s, 5)
	require.Error(t, err)

	require.True(t, battleInstanceRegistered(s, 9, "canary-9"))
	require.True(t, battleInstanceReplaced(s, 9, "old-canary-9"), "同 node_id 换了实例,原对局已不在")

	require.True(t, canaryBattlesMayExist(s), "canary 镜像里有节点")
	s.CanaryBattleNodes = discovery.NewNodeWatcher("battle-canary", svc.BattleNodeRpcPrefix, nil, nil)
	require.False(t, canaryBattlesMayExist(s), "canary 镜像是空的就不可能有活着的 canary 对局")
	s.CanaryBattleNodes = nil
	require.False(t, canaryBattlesMayExist(s))
}

// 观战的轨道规则(发布轨道设计 §2.6):只看成局原件记的轨道,stable 观众只看 stable 对局,canary 观众都能看。
func TestViewerMayWatchFollowsTheRecordedTrack(t *testing.T) {
	cases := []struct {
		name     string
		viewer   releasetrack.Track
		recorded string
		want     bool
	}{
		{name: "stable viewer, stable battle", viewer: releasetrack.Stable, recorded: "stable", want: true},
		{name: "stable viewer, battle recorded before the field", viewer: releasetrack.Stable, recorded: "", want: true},
		{name: "stable viewer, canary battle", viewer: releasetrack.Stable, recorded: "canary", want: false},
		{name: "stable viewer, unknown track fails closed", viewer: releasetrack.Stable, recorded: "beta", want: false},
		{name: "canary viewer, canary battle", viewer: releasetrack.Canary, recorded: "canary", want: true},
		{name: "canary viewer, stable battle", viewer: releasetrack.Canary, recorded: "stable", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, viewerMayWatch(tc.viewer, tc.recorded))
		})
	}
}

// 选点结果决定原件上的轨道:canary 局退回 stable 节点时原件记 stable,观战按它判(选中节点的 Track 写进原件)。
func TestPickedNodeTrackIsWhatTheFormationRecords(t *testing.T) {
	s, stableNodes := newTrackPlacementCtx(t)
	node, ok := pickBattleNode(s, releasetrack.Canary, stableNodes, 1)
	require.True(t, ok)
	require.Equal(t, "canary", node.Track.String())
	node, ok = pickBattleNode(s, releasetrack.Canary, stableNodes, 2)
	require.True(t, ok)
	require.Equal(t, "stable", node.Track.String(), "退回 stable 节点的局按 stable 记,stable 观众照样能看")
}

// listIdsAs 以给定会话轨道("" = 不带 metadata,按 stable)拉观战列表,返回场次与赛事局标记。
func listIdsAs(t *testing.T, svcCtx *svc.ServiceContext, track string, limit uint32) ([]uint64, []bool) {
	t.Helper()
	ctx := context.Background()
	if track != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(releasetrack.MetadataKey, track))
	}
	resp, err := NewListWatchableBattlesLogic(ctx, svcCtx).ListWatchableBattles(&pb.ListWatchableBattlesRequest{Limit: limit})
	require.NoError(t, err)
	ids := make([]uint64, 0, len(resp.GetBattles()))
	flags := make([]bool, 0, len(resp.GetBattles()))
	for _, battle := range resp.GetBattles() {
		ids = append(ids, battle.GetBattleId())
		flags = append(flags, battle.GetFeatured())
	}
	return ids, flags
}

// 观战列表按观众轨道过滤(发布轨道设计 §2.6):stable 观众看不到原件记着 canary 的场,赛事局也一样;canary 观众两条轨道都能看。
// 过滤只影响这一次的结果,不摘索引。canary 镜像里有节点时 stable 观众多读一倍候选,最新几场都是 canary 时这一页也不会变空。
func TestListWatchableBattlesFiltersByViewerTrack(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.CanaryBattleNodes = discovery.NewNodeWatcher("battle-canary", svc.BattleNodeRpcPrefix, nil, nil)
	svcCtx.CanaryBattleNodes.Upsert(svc.BattleNodeRpcPrefix+"9", discovery.NodeEntry{NodeId: 9, NodeUuid: "canary-9", Endpoint: "battle-9", PhysicalRegion: 1, Track: releasetrack.Canary})
	now := nowMs()
	const stableOld, canaryFeatured, canaryNew = uint64(885001), uint64(885002), uint64(885003)
	registerBattle(t, svcCtx, stableOld, now-3000)
	registerBattle(t, svcCtx, canaryFeatured, now-2000)
	registerBattle(t, svcCtx, canaryNew, now-1000)
	formations := stubSpectateFormations(t).formations
	formations[canaryFeatured].ReleaseTrack = "canary"
	formations[canaryNew].ReleaseTrack = "canary"
	_, err := svcCtx.MatchRedis.Zadd(spectateFeaturedActiveKey, int64(testRelayDeadlineMs), strconv.FormatUint(canaryFeatured, 10))
	require.NoError(t, err)

	ids, flags := listIdsAs(t, svcCtx, "", 2)
	require.Equal(t, []uint64{stableOld}, ids, "最新的两场都是 canary:多读的候选补上了 stable 场,canary 赛事局也不出现")
	require.Equal(t, []bool{false}, flags)
	ids, _ = listIdsAs(t, svcCtx, "stable", 2)
	require.Equal(t, []uint64{stableOld}, ids)

	ids, flags = listIdsAs(t, svcCtx, "canary", 10)
	require.Equal(t, []uint64{canaryFeatured, canaryNew, stableOld}, ids, "canary 观众两条轨道都能看,赛事局仍排最前")
	require.Equal(t, []bool{true, false, false}, flags)

	require.Equal(t, 3, activeIndexSize(t, svcCtx), "按轨道滤掉的场不摘索引")
	featured, err := svcCtx.MatchRedis.Zcard(spectateFeaturedActiveKey)
	require.NoError(t, err)
	require.Equal(t, 1, featured, "赛事局索引也不动")

	beta := metadata.NewIncomingContext(context.Background(), metadata.Pairs(releasetrack.MetadataKey, "beta"))
	_, err = NewListWatchableBattlesLogic(beta, svcCtx).ListWatchableBattles(&pb.ListWatchableBattlesRequest{})
	require.Error(t, err, "认不出的会话轨道是链路故障,不按 stable 猜")
}
