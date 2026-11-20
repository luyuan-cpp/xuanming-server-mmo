package logic

import (
	"slices"
	"strconv"
	"testing"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/discovery"

	base "proto/common/base"
	"shared/generated/tip"

	"github.com/stretchr/testify/require"
)

// 探测目标在机房内按玩家分散(network_probe.go 的 probeTargets,
// docs/design/global-match-hierarchical-pools.md §5.1「探测目标」)。

// probeNode 造一条可探测的 battle 登记:可分配状态没上报(UNSPECIFIED),与旧版本 battle 的注册值相同。
func probeNode(id, region uint32) discovery.NodeEntry {
	return discovery.NodeEntry{NodeId: id, NodeUuid: "battle-" + strconv.FormatUint(uint64(id), 10), PhysicalRegion: region,
		ClientHost: "10.0.0.1", ClientPort: 7000 + id}
}

// targetIn 返回 targets 里 region 机房的目标节点号;计划里没有这个机房返回 0。
func targetIn(targets []discovery.NodeEntry, region uint32) uint32 {
	for _, node := range targets {
		if node.PhysicalRegion == region {
			return node.NodeId
		}
	}
	return 0
}

func targetIDs(targets []discovery.NodeEntry) []uint32 {
	ids := make([]uint32, 0, len(targets))
	for _, node := range targets {
		ids = append(ids, node.NodeId)
	}
	return ids
}

func TestProbeTargetsSpreadPlayersAcrossRegionNodes(t *testing.T) {
	nodes := []discovery.NodeEntry{probeNode(1, 1), probeNode(2, 1), probeNode(3, 1), probeNode(4, 1), probeNode(9, 2)}
	const players = 4000
	perNode := map[uint32]int{}
	for pid := uint64(1); pid <= players; pid++ {
		targets := probeTargets(slices.Clone(nodes), pid)
		require.Len(t, targets, 2, "每个机房一个目标")
		perNode[targetIn(targets, 1)]++
		require.Equal(t, uint32(9), targetIn(targets, 2), "只有一台的机房照旧是那一台")
	}
	require.Len(t, perNode, 4, "机房 1 的四台都分到玩家")
	for id, count := range perNode {
		// 均分是每台 1000、标准差约 27;输入固定、结果确定,按 SHA-256 均匀打分,偏离超过 200 几乎不可能。
		require.InDelta(t, players/4, count, players/20, "node %d", id)
	}
}

func TestProbeTargetsSamePlayerStaysOnItsNode(t *testing.T) {
	nodes := []discovery.NodeEntry{probeNode(1, 1), probeNode(2, 1), probeNode(3, 1)}
	reversed := slices.Clone(nodes)
	slices.Reverse(reversed)
	withFourth := append(slices.Clone(nodes), probeNode(4, 1))
	withoutSecond := []discovery.NodeEntry{probeNode(1, 1), probeNode(3, 1)}
	movedToFourth := 0
	for pid := uint64(1); pid <= 2000; pid++ {
		before := targetIn(probeTargets(slices.Clone(nodes), pid), 1)
		require.Equal(t, before, targetIn(probeTargets(slices.Clone(nodes), pid), 1), "重探落同一台")
		require.Equal(t, before, targetIn(probeTargets(slices.Clone(reversed), pid), 1), "与快照顺序无关")
		if after := targetIn(probeTargets(slices.Clone(withFourth), pid), 1); after != before {
			require.Equal(t, uint32(4), after, "加一台只会把玩家挪到新加的那台")
			movedToFourth++
		}
		if before != 2 {
			require.Equal(t, before, targetIn(probeTargets(slices.Clone(withoutSecond), pid), 1), "摘掉一台只挪动原来落在它上面的玩家")
		}
	}
	require.Positive(t, movedToFourth, "新加的那台分到玩家")
}

func TestProbeTargetsSingleNodeRegionsKeepTheOldPlan(t *testing.T) {
	// 20 个机房各一台、快照乱序;节点号越大机房号越小,区分「按节点号排」和「按机房号排」。
	var nodes []discovery.NodeEntry
	for i := uint32(1); i <= 20; i++ {
		nodes = append(nodes, probeNode(100+i, 21-i))
	}
	slices.Reverse(nodes)
	for _, pid := range []uint64{1, 2, 7, 1 << 40} {
		targets := probeTargets(slices.Clone(nodes), pid)
		require.Len(t, targets, authority.MaxProbeRegions, "超过上限的机房不进计划")
		for i, node := range targets {
			require.Equal(t, uint32(101+i), node.NodeId, "机房按其中最小的节点号排,目标就是那一台")
			require.Equal(t, uint32(20-i), node.PhysicalRegion)
		}
	}
}

func TestProbeTargetsOnlyPickProbeableNodes(t *testing.T) {
	require.Empty(t, probeTargets(nil, 1), "没有节点就没有目标(上层回「地域探测端点暂不可用」)")
	breakages := []struct {
		name  string
		apply func(*discovery.NodeEntry)
	}{
		{"没有机房", func(n *discovery.NodeEntry) { n.PhysicalRegion = 0 }},
		{"没有客户端地址", func(n *discovery.NodeEntry) { n.ClientHost = "" }},
		{"端口为 0", func(n *discovery.NodeEntry) { n.ClientPort = 0 }},
		{"端口越界", func(n *discovery.NodeEntry) { n.ClientPort = 65536 }},
		{"没有实例 UUID", func(n *discovery.NodeEntry) { n.NodeUuid = "" }},
		{"自报 CLOSED", func(n *discovery.NodeEntry) { n.Admission = base.NodeAdmission_NODE_ADMISSION_CLOSED }},
	}
	for _, breakage := range breakages {
		t.Run(breakage.name, func(t *testing.T) {
			// bad 是机房 1 里节点号最小的一台(旧做法会选它);onlyBad 是机房 2 唯一的一台。
			bad, onlyBad := probeNode(1, 1), probeNode(5, 2)
			breakage.apply(&bad)
			breakage.apply(&onlyBad)
			for pid := uint64(1); pid <= 200; pid++ {
				require.Equal(t, []uint32{9}, targetIDs(probeTargets([]discovery.NodeEntry{bad, onlyBad, probeNode(9, 1)}, pid)),
					"不可探测的节点不选;机房里没有可探测节点时整个机房不进计划")
			}
		})
	}

	open := probeNode(1, 1)
	open.Admission = base.NodeAdmission_NODE_ADMISSION_OPEN
	seen := map[uint32]bool{}
	for pid := uint64(1); pid <= 200; pid++ {
		seen[targetIn(probeTargets([]discovery.NodeEntry{open, probeNode(2, 1)}, pid), 1)] = true
	}
	require.Equal(t, map[uint32]bool{1: true, 2: true}, seen, "OPEN 与没上报(UNSPECIFIED)的节点都可探测")
}

// 计划一个目标都没有:镜像同步过(确实没有可用的 battle,如维护、整机房排空)是业务拒绝;镜像没同步过是本服务没就绪,算故障(D8)。
func TestNoProbeTargetCodeSeparatesUnavailableFromNotReady(t *testing.T) {
	require.Equal(t, constants.ErrNoRegionWithinLatency, noProbeTargetCode(true))
	require.False(t, tip.IsFault(noProbeTargetCode(true)), "维护、排空时没有可探测的 battle 不计服务端故障")
	require.Equal(t, constants.ErrInternal, noProbeTargetCode(false))
	require.True(t, tip.IsFault(noProbeTargetCode(false)), "battle 镜像没同步过是本服务没就绪")
}
