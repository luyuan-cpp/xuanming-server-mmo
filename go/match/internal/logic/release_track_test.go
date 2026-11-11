package logic

import (
	"context"
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

	require.True(t, battleOnCanaryNode(s, 9, "canary-9"))
	require.False(t, battleOnCanaryNode(s, 9, "old-canary-9"), "实例对不上不算 canary 对局,交给已换实例的判定")
	require.False(t, battleOnCanaryNode(s, 1, "stable-1"))
	s.CanaryBattleNodes = nil
	require.False(t, battleOnCanaryNode(s, 9, "canary-9"))
}
