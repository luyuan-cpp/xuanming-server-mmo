package authority

import (
	"context"
	"fmt"
	"testing"

	"match/internal/catalog"

	pb "proto/match"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// 撮合段按首选机房、子池号与入队时刻读票（docs/design/global-match-hierarchical-pools.md §5.2.1）。
// 前半是不碰 SQL 的纯函数，后半是真 TiDB（MATCH_IT_TIDB / MATCH_IT_REQUIRE_TIDB，夹具见 store_test.go 的 testStore）。

var sliceTestPool = catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_1V1, ConfigId: 1, TeamSize: 2}

func TestUnitPoolRegionNeedsEveryMemberToAgree(t *testing.T) {
	require.Equal(t, uint32(2), unitPoolRegion([]uint32{2, 2, 2}))
	require.Equal(t, uint32(0), unitPoolRegion([]uint32{2, 1}), "成员首选机房不一致的整队只有跨机房段读")
	require.Equal(t, uint32(0), unitPoolRegion(nil))
}

func TestPoolSliceFilterBindsOnlySetConditions(t *testing.T) {
	where, args := WholePool(sliceTestPool).filter("player_id")
	require.Empty(t, where)
	require.Empty(t, args)
	where, args = PoolSlice{Pool: sliceTestPool, Region: 3, SubPools: 4, Sub: 1, AfterMs: 10, UntilMs: 20}.filter("actor_id")
	require.Equal(t, " AND pool_region=? AND MOD(actor_id,?)=? AND enqueued_ms>? AND enqueued_ms<=?", where)
	require.Equal(t, []any{uint32(3), uint32(4), uint32(1), uint64(10), uint64(20)}, args)
	where, _ = PoolSlice{Pool: sliceTestPool, Region: 3, SubPools: 1, Sub: 0}.filter("player_id")
	require.Equal(t, " AND pool_region=?", where, "子池数 1 = 不拆，不加取模条件")
}

// sliceReceipts 造一份到各机房 RTT 已知的探测回执，首选机房 = RTT 最小的那个。
func sliceReceipts(pid uint64, plan string, now uint64, rtts map[uint32]uint32) []*pb.NetworkProbeReceipt {
	var out []*pb.NetworkProbeReceipt
	for region, rtt := range rtts {
		out = append(out, &pb.NetworkProbeReceipt{Grant: &pb.NetworkProbeGrant{PlayerId: pid, PlanId: plan, SessionBinding: "bound", PhysicalRegion: region, ExpiresAtMs: now + 120000}, RttMs: rtt, SampleCount: 3, MeasuredAtMs: now, ExpiresAtMs: now + 120000})
	}
	return out
}

// joinPreferring 让玩家按给定 RTT 入队一张 1V1 单排票（与 testJoin 同一池）。
func joinPreferring(t *testing.T, s *Store, pid uint64, rtts map[uint32]uint32) *pb.MatchAuthorityTicketRecord {
	t.Helper()
	ctx := context.Background()
	n, err := s.Now(ctx)
	require.NoError(t, err)
	plan := fmt.Sprintf("plan-%d", pid)
	require.NoError(t, s.SavePlan(ctx, pid, &pb.MatchNetworkProfile{PlanId: plan, SessionBinding: "bound", Receipts: sliceReceipts(pid, plan, n, rtts)}, n+120000))
	ticket, created, err := s.Join(ctx, &pb.MatchAuthorityTicketRecord{PlayerId: pid, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: uint32(sliceTestPool.Mode), ConfigId: sliceTestPool.ConfigId, TeamSize: sliceTestPool.TeamSize, ZoneId: 1, RatingCenti: 150000}, plan, "bound")
	require.NoError(t, err)
	require.True(t, created)
	return ticket
}

func ticketPoolRegion(t *testing.T, s *Store, pid uint64) uint32 {
	t.Helper()
	var region uint32
	require.NoError(t, s.DB.QueryRow("SELECT pool_region FROM match_authority_ticket WHERE player_id=?", pid).Scan(&region))
	return region
}

func setTicketEnqueued(t *testing.T, s *Store, enqueued uint64, pids ...uint64) {
	t.Helper()
	for _, pid := range pids {
		_, err := s.DB.Exec("UPDATE match_authority_ticket SET enqueued_ms=? WHERE player_id=?", enqueued, pid)
		require.NoError(t, err)
	}
}

func slicePlayers(t *testing.T, s *Store, slice PoolSlice, after PoolCursor, limit int) []uint64 {
	t.Helper()
	tickets, err := s.SliceTickets(context.Background(), slice, after, limit)
	require.NoError(t, err)
	out := make([]uint64, 0, len(tickets))
	for _, ticket := range tickets {
		out = append(out, ticket.PlayerId)
	}
	return out
}

var (
	preferEast = map[uint32]uint32{1: 20, 2: 80}
	preferWest = map[uint32]uint32{1: 90, 2: 30}
)

// 入队按刚落库的原件写首选机房列；机房段只读本机房，整池段读全部；新索引建好后启动核对通过。
func TestPoolSliceTiDBJoinRecordsPreferredRegion(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.CheckCandidateIndexes(context.Background()), "迁移按新 proto 建出两条按首选机房读页的索引")
	joinPreferring(t, s, 11, preferEast)
	joinPreferring(t, s, 12, preferWest)
	joinPreferring(t, s, 13, preferEast)
	require.Equal(t, uint32(1), ticketPoolRegion(t, s, 11))
	require.Equal(t, uint32(2), ticketPoolRegion(t, s, 12))
	n, err := s.Now(context.Background())
	require.NoError(t, err)
	setTicketEnqueued(t, s, n-3000, 11, 12, 13) // 同一入队时刻，按 player_id 定序

	require.Equal(t, []uint64{11, 13}, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, Region: 1}, PoolCursor{}, 64))
	require.Equal(t, []uint64{12}, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, Region: 2}, PoolCursor{}, 64))
	require.Empty(t, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, Region: 3}, PoolCursor{}, 64))
	require.Equal(t, []uint64{11, 12, 13}, slicePlayers(t, s, WholePool(sliceTestPool), PoolCursor{}, 64))
	// 机房段照样按 (enqueued_ms, player_id) 翻页。
	require.Equal(t, []uint64{11}, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, Region: 1}, PoolCursor{}, 1))
	require.Equal(t, []uint64{13}, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, Region: 1}, PoolCursor{EnqueuedMs: n - 3000, PlayerId: 11}, 1))
}

// 子池段只读本子池、入队不满合并时长的；拆了子池的机房段只读等满的（全部子池）；跨机房段不分机房、只读等满跨机房阈值的。
func TestPoolSliceTiDBSubPoolAndWindows(t *testing.T) {
	s := testStore(t)
	for pid := uint64(101); pid <= 106; pid++ {
		joinPreferring(t, s, pid, preferEast)
	}
	joinPreferring(t, s, 107, preferWest)
	n, err := s.Now(context.Background())
	require.NoError(t, err)
	const merge, cross = uint64(5_000), uint64(15_000)
	setTicketEnqueued(t, s, n-20_000, 101, 102, 103, 107) // 等满跨机房阈值
	setTicketEnqueued(t, s, n-1_000, 104, 105, 106)       // 还在子池阶段

	sub := func(k uint32) PoolSlice {
		return PoolSlice{Pool: sliceTestPool, Region: 1, SubPools: 2, Sub: k, AfterMs: n - merge}
	}
	require.Equal(t, []uint64{104, 106}, slicePlayers(t, s, sub(0), PoolCursor{}, 64), "player_id 取模为 0 的新票")
	require.Equal(t, []uint64{105}, slicePlayers(t, s, sub(1), PoolCursor{}, 64), "player_id 取模为 1 的新票")
	require.Equal(t, []uint64{101, 102, 103}, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, Region: 1, UntilMs: n - merge}, PoolCursor{}, 64), "机房段读全部子池、等满合并时长的")
	require.Equal(t, []uint64{101, 102, 103, 107}, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, UntilMs: n - cross}, PoolCursor{}, 64), "跨机房段不分机房")
}

// 探测刷新换了首选机房：同一条 UPDATE 把首选机房列改到新机房，票离开旧机房段、进入新机房段（§5.2.1 第 6 条）。
func TestPoolSliceTiDBProfileRefreshMovesTicket(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	before := joinPreferring(t, s, 21, preferEast)
	require.Equal(t, uint32(1), ticketPoolRegion(t, s, 21))
	n, err := s.Now(ctx)
	require.NoError(t, err)
	require.NoError(t, s.SavePlan(ctx, 21, &pb.MatchNetworkProfile{PlanId: "plan-21-refresh", SessionBinding: "bound"}, n+120000))
	require.NoError(t, s.CommitProfile(ctx, 21, "plan-21-refresh", "bound", sliceReceipts(21, "plan-21-refresh", n, preferWest), n+120000))
	require.Equal(t, uint32(2), ticketPoolRegion(t, s, 21))
	after, err := s.Ticket(ctx, 21)
	require.NoError(t, err)
	require.Equal(t, before.Ticket, after.Ticket, "刷新不换票号")
	require.Equal(t, before.EnqueuedMs, after.EnqueuedMs, "刷新不改最初等待时刻")
	require.Empty(t, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, Region: 1}, PoolCursor{}, 64))
	require.Equal(t, []uint64{21}, slicePlayers(t, s, PoolSlice{Pool: sliceTestPool, Region: 2}, PoolCursor{}, 64))
}

// 整队原件按自己的首选机房列、发起者取模与入队时刻读；成员首选机房不一致（列为 0）的只有不分机房的段读得到。
func TestPoolSliceTiDBTeamLaunchSlices(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	n, err := s.Now(ctx)
	require.NoError(t, err)
	pool := catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_PVE_TEAM, ConfigId: 1, TeamSize: 3}
	insert := func(actor uint64, region uint32, enqueued uint64) {
		t.Helper()
		_, err := s.DB.Exec("INSERT INTO team_launch("+teamLaunchColumns+",pool_region) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			actor, uint64(1), uint64(1), actor+1000, uint64(1), "token", pool.ConfigId, pool.TeamSize, uint32(2), uint32(2), enqueued, uint64(0), uint64(0), []byte{}, "owner", uint64(7), uint32(pool.Mode), region)
		require.NoError(t, err)
	}
	insert(201, 1, n-20_000)
	insert(202, 2, n-20_000)
	insert(203, 1, n-1_000)
	insert(204, 0, n-20_000)
	actors := func(slice PoolSlice) []uint64 {
		launches, err := s.SliceTeamLaunches(ctx, slice, 64)
		require.NoError(t, err)
		out := make([]uint64, 0, len(launches))
		for _, launch := range launches {
			out = append(out, launch.ActorId)
		}
		return out
	}
	require.Equal(t, []uint64{201, 203}, actors(PoolSlice{Pool: pool, Region: 1}))
	require.Equal(t, []uint64{203}, actors(PoolSlice{Pool: pool, Region: 1, SubPools: 2, Sub: 1, AfterMs: n - 5_000}), "发起者取模为 1 的新整队")
	require.Empty(t, actors(PoolSlice{Pool: pool, Region: 1, SubPools: 2, Sub: 0, AfterMs: n - 5_000}))
	require.Equal(t, []uint64{201, 202, 204}, actors(PoolSlice{Pool: pool, UntilMs: n - 15_000}), "跨机房段读得到首选机房不一致的整队")
	require.Equal(t, []uint64{201, 202, 204, 203}, actors(WholePool(pool)))
	other, err := s.SliceTeamLaunches(ctx, WholePool(sliceTestPool), 64)
	require.NoError(t, err)
	require.Empty(t, other, "1V1 池没有整队")
}

// 撮合段之间撞车：同一组票第二次认领在票行复核上失败，回 ErrClaimStale（撮合侧当正常冲突、不记排空 HOLD）。
func TestPoolSliceTiDBSecondClaimOfSameTicketsIsStale(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, b := testJoin(t, s, 31), testJoin(t, s, 32)
	formation := func(battle uint64) *pb.MatchFormationRecord {
		return &pb.MatchFormationRecord{BattleId: battle, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "127.0.0.1:1", Mode: 3, ConfigId: 1}
	}
	require.NoError(t, s.Claim(ctx, formation(310), []*pb.MatchAuthorityTicketRecord{a, b}))
	require.ErrorIs(t, s.Claim(ctx, formation(311), []*pb.MatchAuthorityTicketRecord{a, b}), ErrClaimStale)
	var count int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_formation WHERE battle_id=311").Scan(&count))
	require.Zero(t, count, "输的一方整笔回滚")
}
