package logic

import (
	"errors"
	"fmt"
	"testing"

	"match/internal/authority"
	"match/internal/catalog"
	"match/internal/discovery"
	"match/internal/poolgroup"
	"match/internal/poolowner"

	base "proto/common/base"
	pb "proto/match"
	"shared/authoritysql"

	"github.com/stretchr/testify/require"
)

// 撮合段换成读页条件与成组阶段（docs/design/global-match-hierarchical-pools.md §5.2.1 第 1～3 条）。

func TestSegmentSliceReadsTheSegmentWindow(t *testing.T) {
	pool := catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_PVE_TEAM, ConfigId: 1, TeamSize: 3}
	const now, merge, cross = uint64(1_000_000), uint64(5_000), uint64(30_000)

	slice, ok := segmentSlice(poolowner.Segment{Pool: pool, Role: poolowner.RoleSub, Region: 2, SubPools: 4, Sub: 3}, now, merge, cross)
	require.True(t, ok)
	require.Equal(t, authority.PoolSlice{Pool: pool, Region: 2, SubPools: 4, Sub: 3, AfterMs: now - merge}, slice, "子池段：本机房、本子池、入队不满合并时长")

	slice, ok = segmentSlice(poolowner.Segment{Pool: pool, Role: poolowner.RoleRegion, Region: 2, SubPools: 4}, now, merge, cross)
	require.True(t, ok)
	require.Equal(t, authority.PoolSlice{Pool: pool, Region: 2, UntilMs: now - merge}, slice, "机房段读全部子池、等满合并时长的")

	slice, ok = segmentSlice(poolowner.Segment{Pool: pool, Role: poolowner.RoleRegion, Region: 2, SubPools: 1}, now, merge, cross)
	require.True(t, ok)
	require.Equal(t, authority.PoolSlice{Pool: pool, Region: 2}, slice, "不拆子池的机房段读这个机房的全部单位")

	slice, ok = segmentSlice(poolowner.Segment{Pool: pool, Role: poolowner.RoleCross}, now, merge, cross)
	require.True(t, ok)
	require.Equal(t, authority.PoolSlice{Pool: pool, UntilMs: now - cross}, slice, "跨机房段不分机房、只读等满跨机房阈值的")

	slice, ok = segmentSlice(poolowner.Segment{Pool: pool, Role: poolowner.RolePool}, now, merge, cross)
	require.True(t, ok)
	require.Equal(t, authority.WholePool(pool), slice)

	_, ok = segmentSlice(poolowner.Segment{Pool: pool, Role: poolowner.RoleCross}, cross, merge, cross)
	require.False(t, ok, "时钟没走过阈值时跳过这一段")
}

func TestSegmentPassesFollowRole(t *testing.T) {
	require.Equal(t, poolgroup.PassesBoth, segmentPasses(poolowner.RolePool))
	require.Equal(t, poolgroup.PassesLocal, segmentPasses(poolowner.RoleRegion))
	require.Equal(t, poolgroup.PassesLocal, segmentPasses(poolowner.RoleSub))
	require.Equal(t, poolgroup.PassesCross, segmentPasses(poolowner.RoleCross))
}

// 机房列表只看注册：自报 CLOSED 的节点所在机房照样有机房段（可分配与否在成组时看），没填机房的节点不算。
func TestRegisteredRegionsIgnoreAdmission(t *testing.T) {
	nodes := []discovery.NodeEntry{
		{NodeId: 1, PhysicalRegion: 2, Admission: base.NodeAdmission_NODE_ADMISSION_CLOSED},
		{NodeId: 2, PhysicalRegion: 0},
		{NodeId: 3, PhysicalRegion: 1, Admission: base.NodeAdmission_NODE_ADMISSION_OPEN},
	}
	require.Equal(t, []uint32{2, 1}, registeredRegions(nodes))
	duel := catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_1V1, ConfigId: 1, TeamSize: 2}
	require.Equal(t, []poolowner.Segment{
		{Pool: duel, Role: poolowner.RoleRegion, Region: 1, SubPools: 1},
		{Pool: duel, Role: poolowner.RoleRegion, Region: 2, SubPools: 1},
		{Pool: duel, Role: poolowner.RoleCross},
	}, poolowner.Segments([]catalog.Pool{duel}, registeredRegions(nodes), nil))
}

// 成局提交失败只在结局未知时记排空 HOLD(§5.2.1 第 7 条)。撮合段或副本之间的撞车、提交前就回滚的库故障都是结局已知:
// 组队池里两个副本(失联实例与接管实例)抢同一支整队时,后到的一方在入场守卫上被拒、整笔回滚
// (team 包 TestTeamTiDBSecondClaimOfClaimedUnitsIsRejectedWholly 钉住它回的就是 ErrMatchEntryOccupied)。
// 修复前红:ErrMatchEntryOccupied 不在原来的白名单里,每撞一次整队就多一个要人工处理的 HOLD。
func TestClaimHoldsOnlyWhenOutcomeUnknown(t *testing.T) {
	for name, err := range map[string]error{
		"票已被别的段先提交":  fmt.Errorf("claim: %w", authority.ErrClaimStale),
		"整队已被先成的局占住": fmt.Errorf("claim: %w", authority.ErrMatchEntryOccupied),
		"提交前连接断了":    fmt.Errorf("claim: %w", authoritysql.ErrTxAbortedBeforeCommit),
		"服务端拒绝语句":    errors.New("database unavailable"),
	} {
		require.False(t, claimOutcomeUnknown(err), name)
	}
	require.True(t, claimOutcomeUnknown(fmt.Errorf("claim: %w", authoritysql.ErrOutcomeUnknown)), "COMMIT 结局未知")
}
