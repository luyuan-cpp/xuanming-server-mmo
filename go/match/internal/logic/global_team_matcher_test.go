package logic

import (
	"testing"

	pb "proto/match"
	teampb "proto/team"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func fixtureGlobalTeamUnit(players ...uint64) globalTeamUnit {
	unit := globalTeamUnit{}
	for _, player := range players {
		profile, _ := proto.Marshal(&pb.MatchNetworkProfile{PlanId: "fixture", SessionBinding: "original", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: player, PhysicalRegion: 1, ExpiresAtMs: 300000}, RttMs: 20, SampleCount: 3, MeasuredAtMs: 1, ExpiresAtMs: 300000}, {Grant: &pb.NetworkProbeGrant{PlayerId: player, PhysicalRegion: 2, ExpiresAtMs: 300000}, RttMs: 100, SampleCount: 3, MeasuredAtMs: 1, ExpiresAtMs: 300000}}})
		unit.tickets = append(unit.tickets, &pb.MatchAuthorityTicketRecord{PlayerId: player, ZoneId: 1, State: 1, Mode: uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1, TeamSize: 5, EnqueuedMs: 1000, ExpiresMs: 300000, NetworkProfile: profile})
	}
	if len(players) > 1 {
		unit.launch = &teampb.TeamLaunchRecord{ActorId: players[0], EnqueuedMs: 1000}
	}
	return unit
}

func TestGlobalTeamPackPreservesWholeUnits(t *testing.T) {
	a, b := fixtureGlobalTeamUnit(1, 2, 3), fixtureGlobalTeamUnit(4, 5)
	group := packTeamUnits(a, []globalTeamUnit{b}, 5, 0)
	require.Len(t, group, 2)
	require.Len(t, group[0].tickets, 3)
	require.Len(t, group[1].tickets, 2)
	require.Nil(t, packTeamUnits(a, []globalTeamUnit{fixtureGlobalTeamUnit(4, 5, 6)}, 5, 0), "不能从三人队拆两人凑五人")
	group = packTeamUnits(fixtureGlobalTeamUnit(1, 2), []globalTeamUnit{fixtureGlobalTeamUnit(3, 4), fixtureGlobalTeamUnit(5)}, 5, 0)
	require.Len(t, group, 3)
}

func TestGlobalTeamPackCrossHomeRuleCoversFinalRoster(t *testing.T) {
	a, other := fixtureGlobalTeamUnit(1, 2, 3), fixtureGlobalTeamUnit(4, 5)
	for _, ticket := range other.tickets {
		ticket.ZoneId = 2
	}
	require.Nil(t, packTeamUnits(a, []globalTeamUnit{other}, 5, 1), "各unit内部同home不代表最终整队同home")
	require.Len(t, packTeamUnits(a, []globalTeamUnit{other}, 5, 0), 2, "显式允许跨home时保留完整unit组合")
	same := fixtureGlobalTeamUnit(6, 7)
	require.Len(t, packTeamUnits(a, []globalTeamUnit{other, same}, 5, 1), 2)
	a.tickets[1].ZoneId = 2
	require.Nil(t, packTeamUnits(a, []globalTeamUnit{same}, 5, 1), "混合anchor不能以队长home绕过")
	a.tickets[1].ZoneId = 0
	require.Nil(t, packTeamUnits(a, []globalTeamUnit{same}, 5, 0), "缺原home即使允许跨home也拒绝")
}

func TestGlobalTeamRegionsRequireAllPreferredAndEveryWait(t *testing.T) {
	unit := fixtureGlobalTeamUnit(1, 2)
	require.Equal(t, []uint32{1}, teamUnitRegions(unit, 100000, 180, true, 30000))
	require.Equal(t, []uint32{1, 2}, teamUnitRegions(unit, 100000, 180, false, 30000))
	unit.tickets[1].EnqueuedMs = 90000
	require.Empty(t, teamUnitRegions(unit, 100000, 180, false, 30000), "一人未等满不能以队长等待替全队放行")
	profile := &pb.MatchNetworkProfile{PlanId: "fixture", SessionBinding: "original", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PhysicalRegion: 2, ExpiresAtMs: 300000}, RttMs: 10, SampleCount: 3, MeasuredAtMs: 1, ExpiresAtMs: 300000}}}
	payload, err := proto.Marshal(profile)
	require.NoError(t, err)
	unit.tickets[1].NetworkProfile = payload
	require.Empty(t, teamUnitRegions(unit, 100000, 180, true, 30000), "跨首选区队伍没有伪造的共同本区")
}

func TestGlobalTeamHardLatencyAndExpiryApplyToEveryMember(t *testing.T) {
	unit := fixtureGlobalTeamUnit(1, 2)
	require.True(t, validTeamUnit(unit, 100000, 180))
	require.Equal(t, []uint32{1}, teamUnitRegions(unit, 100000, 80, false, 30000))
	unit.tickets[1].ExpiresMs = 99999
	require.False(t, validTeamUnit(unit, 100000, 180))
}

func fixtureGlobalPvpUnit(target uint32, players ...uint64) globalTeamUnit {
	unit := fixtureGlobalTeamUnit(players...)
	for _, ticket := range unit.tickets {
		ticket.Mode = uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)
		ticket.TeamSize = target
	}
	return unit
}

func TestGlobalPvpPackSameSizeAndWholeTeams(t *testing.T) {
	for size := 1; size <= 5; size++ {
		var left, right []uint64
		for i := 0; i < size; i++ {
			left = append(left, uint64(i+1))
			right = append(right, uint64(i+11))
		}
		a, b := fixtureGlobalPvpUnit(uint32(size), left...), fixtureGlobalPvpUnit(uint32(size), right...)
		require.Nil(t, packPvpTeamUnits(a, nil, size, 1), "没有真人对手只能等待")
		group := packPvpTeamUnits(a, []globalTeamUnit{b}, size, 1)
		require.Len(t, group, 2)
		require.Equal(t, uint32(0), group[0].side)
		require.Equal(t, uint32(1), group[1].side)
		require.Equal(t, left, func() []uint64 {
			var ids []uint64
			for _, ticket := range group[0].tickets {
				ids = append(ids, ticket.PlayerId)
			}
			return ids
		}())
	}
}

func TestGlobalPvpPackFillsSidesWithoutSplittingParty(t *testing.T) {
	a := fixtureGlobalPvpUnit(5, 1, 2, 3)
	group := packPvpTeamUnits(a, []globalTeamUnit{fixtureGlobalPvpUnit(5, 4, 5), fixtureGlobalPvpUnit(5, 6, 7, 8, 9), fixtureGlobalPvpUnit(5, 10)}, 5, 1)
	require.Len(t, group, 4)
	var counts [2]int
	for _, unit := range group {
		counts[unit.side] += len(unit.tickets)
	}
	require.Equal(t, [2]int{5, 5}, counts)
	require.Equal(t, group[0].side, group[1].side)
	require.Equal(t, group[2].side, group[3].side)
}

func TestGlobalPvpPackRejectsMixedModesSizesAndOverlappingPlayers(t *testing.T) {
	a := fixtureGlobalPvpUnit(2, 1, 2)
	for _, peer := range []globalTeamUnit{fixtureGlobalTeamUnit(3, 4), fixtureGlobalPvpUnit(3, 3, 4), fixtureGlobalPvpUnit(2, 2, 3)} {
		require.Nil(t, packPvpTeamUnits(a, []globalTeamUnit{peer}, 2, 1))
	}
}
