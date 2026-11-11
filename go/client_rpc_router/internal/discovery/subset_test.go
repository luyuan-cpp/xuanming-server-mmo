package discovery

import (
	"testing"

	"shared/releasetrack"

	"github.com/stretchr/testify/require"
)

// coverage 逐格枚举每台路由服弧上的每一格:perTarget[j] 是目标 j 被覆盖的格数(各路由服负载相同时,
// 它正比于目标 j 收到的请求),perRouter[i] 是路由服 i 实际会挑到的目标集合。
func coverage(peers, targets, size int) (perTarget []int, perRouter []map[int]bool) {
	perTarget = make([]int, targets)
	perRouter = make([]map[int]bool, peers)
	for rank := 0; rank < peers; rank++ {
		arc := newAperture(rank, peers, targets, size)
		perRouter[rank] = map[int]bool{}
		for offset := 0; offset < arc.width; offset++ {
			index := arc.pick(func(int) int { return offset })
			perTarget[index]++
			perRouter[rank][index] = true
		}
	}
	return perTarget, perRouter
}

// 各路由服负载相同时每台目标收到的请求一样多,而且一台都不漏(subset.go 头注释的证明):
// 对一大片 R、T、k 组合逐格枚举,每台目标被覆盖的格数必须完全相等且大于 0。
// HRW 取前 k 台做不到这一条,这正是路由服侧不用 HRW 的原因。
func TestApertureBalancesEveryTargetExactly(t *testing.T) {
	for peers := 1; peers <= 12; peers++ {
		for targets := 1; targets <= 30; targets++ {
			for _, size := range []int{0, 2, 3, 4, 8} {
				perTarget, _ := coverage(peers, targets, size)
				for index, count := range perTarget {
					require.Greater(t, count, 0, "R=%d T=%d k=%d:目标 %d 一台路由服都不连", peers, targets, size, index)
					require.Equal(t, perTarget[0], count, "R=%d T=%d k=%d:目标 %d 与目标 0 收到的份额不同", peers, targets, size, index)
				}
			}
		}
	}
}

// 每台路由服至少能挑两台(一台目标重启时本路由服仍有别的可挑),而且确实收窄到约 k 台。
func TestApertureSubsetSize(t *testing.T) {
	for peers := 2; peers <= 12; peers++ {
		for targets := 2; targets <= 30; targets++ {
			for _, size := range []int{2, 3, 4, 8} {
				_, perRouter := coverage(peers, targets, size)
				for rank, picked := range perRouter {
					require.GreaterOrEqual(t, len(picked), 2, "R=%d T=%d k=%d:路由服 %d 只挑得到一台目标", peers, targets, size, rank)
				}
			}
		}
	}

	cases := []struct {
		name                 string
		peers, targets, size int
		min, max             int
	}{
		// m = round(4×10/8) = 5:弧长半圈 = 4 台;起点没对齐目标边界时两头各分一部分,最多 5 台。
		{name: "常规收窄", peers: 10, targets: 8, size: 4, min: 4, max: 5},
		// R × k < T:m 取 1,每台路由服连 T/R = 10 台,多于 k,保证每台目标都有路由服连。
		{name: "目标多于路由服数乘 k", peers: 4, targets: 40, size: 4, min: 10, max: 10},
		{name: "实例不多于 k 取全部", peers: 3, targets: 4, size: 4, min: 4, max: 4},
		{name: "只有一台路由服取全部", peers: 1, targets: 20, size: 4, min: 20, max: 20},
		{name: "k 为 0 不取子集", peers: 10, targets: 20, size: 0, min: 20, max: 20},
	}
	for _, tc := range cases {
		_, perRouter := coverage(tc.peers, tc.targets, tc.size)
		for rank, picked := range perRouter {
			require.GreaterOrEqual(t, len(picked), tc.min, "%s:路由服 %d", tc.name, rank)
			require.LessOrEqual(t, len(picked), tc.max, "%s:路由服 %d", tc.name, rank)
		}
	}
}

// 名次不合法(还不知道自己是谁)时在全部实例里挑,不会选出空集或越界。
func TestApertureFallsBackToAllOnUnknownRank(t *testing.T) {
	for _, rank := range []int{-1, 4, 100} {
		arc := newAperture(rank, 4, 10, 2)
		require.Equal(t, aperture{cellsPerTarget: 1, cells: 10, start: 0, width: 10}, arc, "rank=%d", rank)
	}
	require.Equal(t, aperture{cellsPerTarget: 1, cells: 10, start: 0, width: 10}, newAperture(0, 0, 10, 2), "peers=0:还没设身份")
}

// 名次只在同轨道路由服里算,按 uuid 去重、排序;镜像里还没有自己时把自己算进去。
func TestPeerRank(t *testing.T) {
	peers := []NodeEntry{
		{NodeUuid: "c"},
		{NodeUuid: "a"},
		{NodeUuid: "b"},
		{NodeUuid: "a"}, // 同一台路由服的旧登记还没过期:去重
		{NodeUuid: "aa", Track: releasetrack.Canary}, // 别的轨道不算;算进去的话 b 会变成第 2 名
		{NodeUuid: ""}, // 没有 uuid 的条目不算
	}
	rank, count := PeerRank(peers, "b", releasetrack.Stable)
	require.Equal(t, 1, rank)
	require.Equal(t, 3, count)

	rank, count = PeerRank(peers, "bb", releasetrack.Stable)
	require.Equal(t, 2, rank, "镜像里还没有自己:a、b、bb、c")
	require.Equal(t, 4, count)

	rank, count = PeerRank(peers, "z", releasetrack.Canary)
	require.Equal(t, 1, rank, "canary 路由服只和 canary 比:aa、z")
	require.Equal(t, 2, count)
}
