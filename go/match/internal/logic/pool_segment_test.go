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

// 票行复核失败是撮合段之间的正常撞车：不记排空 HOLD（§5.2.1 第 7 条）；库故障之类的未知失败照旧不算。
func TestExpectedClaimRejectionCoversStaleClaims(t *testing.T) {
	require.True(t, expectedClaimRejection(authority.ErrClaimStale))
	require.True(t, expectedClaimRejection(fmt.Errorf("claim: %w", authority.ErrClaimStale)))
	require.False(t, expectedClaimRejection(errors.New("database unavailable")))
}
