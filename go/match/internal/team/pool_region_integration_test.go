package team

import (
	"context"
	"testing"

	"match/internal/authority"
	"match/internal/catalog"

	matchpb "proto/match"
	teampb "proto/team"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// 整队原件的首选机房列（docs/design/global-match-hierarchical-pools.md §5.2.1 第 6 条），真 TiDB（MATCH_IT_TIDB）。
// 夹具 nativeTeamFixture 的缺省探测是「机房 1 延迟 20、机房 2 延迟 80」，首选机房 1。

var (
	fixtureEast = map[uint32]uint32{1: 20, 2: 80}
	fixtureWest = map[uint32]uint32{1: 90, 2: 30}
)

// profileRTT 与夹具的 profile 相同，只是到各机房的 RTT 由用例给。
func (f *nativeTeamFixture) profileRTT(t *testing.T, player uint64, rtts map[uint32]uint32) {
	t.Helper()
	stamp, err := f.owner.Now(context.Background())
	require.NoError(t, err)
	var receipts []*matchpb.NetworkProbeReceipt
	for region, rtt := range rtts {
		receipts = append(receipts, &matchpb.NetworkProbeReceipt{Grant: &matchpb.NetworkProbeGrant{PlayerId: player, PhysicalRegion: region, ExpiresAtMs: stamp + 120000}, RttMs: rtt, SampleCount: 3, MeasuredAtMs: stamp, ExpiresAtMs: stamp + 120000})
	}
	profile := &matchpb.MatchNetworkProfile{PlanId: uuid.NewString(), SessionBinding: "native-team", Receipts: receipts}
	require.NoError(t, f.owner.SavePlan(f.actor(t, player), player, profile, stamp+120000))
}

// queuedPreferring 与夹具的 queued 相同（5 人档 PVE_TEAM 整队排队），成员的探测按 rtts 给，没给的用缺省（首选机房 1）。
func (f *nativeTeamFixture) queuedPreferring(t *testing.T, leader, teamID uint64, rtts map[uint64]map[uint32]uint32) *teampb.TeamLaunchRecord {
	t.Helper()
	snap, err := f.store.ReadFree(f.actor(t, leader), leader)
	require.NoError(t, err)
	roster := MatchRoster(snap.Record)
	for _, player := range roster {
		profile, ok := rtts[player]
		if !ok {
			profile = fixtureEast
		}
		f.profileRTT(t, player, profile)
	}
	request := &teampb.StartTeamMatchRequest{BattleConfigId: 1, ExpectedTeamId: teamID, TargetTeamSize: 5, RequestId: uuid.NewString()}
	pinned, launch, err := f.store.sql.createLaunch(f.actor(t, leader), snap, leader, request, uuid.NewString(), roster, 100)
	require.NoError(t, err)
	require.NotNil(t, pinned.Commit)
	require.Equal(t, uint32(2), launch.State)
	return launch
}

func launchPoolRegion(t *testing.T, f *nativeTeamFixture, launch *teampb.TeamLaunchRecord) uint32 {
	t.Helper()
	var region uint32
	require.NoError(t, f.owner.DB.QueryRow("SELECT pool_region FROM team_launch WHERE actor_id=? AND request_high=? AND request_low=?", launch.ActorId, launch.RequestHigh, launch.RequestLow).Scan(&region))
	return region
}

// 整队排队时写入成员首选机房的共同值；成员不一致写 0，机房段读不到它，不分机房的段照常读到。
func TestTeamTiDBQueuedLaunchRecordsCommonPreferredRegion(t *testing.T) {
	f := newNativeTeamFixture(t)
	ctx := context.Background()
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	f.create(t, 4, 404)
	f.inviteAndAccept(t, 4, 5, 404)
	west := f.queuedPreferring(t, 1, 101, map[uint64]map[uint32]uint32{1: fixtureWest, 2: fixtureWest})
	mixed := f.queuedPreferring(t, 4, 404, map[uint64]map[uint32]uint32{5: fixtureWest}) // 4 首选机房 1，5 首选机房 2
	require.Equal(t, uint32(2), launchPoolRegion(t, f, west))
	require.Equal(t, uint32(0), launchPoolRegion(t, f, mixed))

	pool := catalog.Pool{Mode: matchpb.MatchMode_MATCH_MODE_PVE_TEAM, ConfigId: 1, TeamSize: 5}
	byRegion, err := f.owner.SliceTeamLaunches(ctx, authority.PoolSlice{Pool: pool, Region: 2}, 64)
	require.NoError(t, err)
	require.Equal(t, []uint64{west.ActorId}, launchActors(byRegion))
	east, err := f.owner.SliceTeamLaunches(ctx, authority.PoolSlice{Pool: pool, Region: 1}, 64)
	require.NoError(t, err)
	require.Empty(t, east, "成员首选机房不一致的整队不进任何机房段")
	whole, err := f.owner.SliceTeamLaunches(ctx, authority.WholePool(pool), 64)
	require.NoError(t, err)
	require.ElementsMatch(t, []uint64{west.ActorId, mixed.ActorId}, launchActors(whole))

	// 成员票本身不写首选机房列（单排读页只读 team_id=0 的票），整队成员不会出现在单排的机房段里。
	solo, err := f.owner.SliceTickets(ctx, authority.PoolSlice{Pool: pool, Region: 2}, authority.PoolCursor{}, 64)
	require.NoError(t, err)
	require.Empty(t, solo)
}
