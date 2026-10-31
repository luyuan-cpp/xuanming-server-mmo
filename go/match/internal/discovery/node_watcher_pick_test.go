package discovery

// PickRandom 的选择语义:镜像为空返回 false、结果必在镜像内、每个节点都选得到。
// 节点用 Upsert 直接灌,不起 etcd。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const testBattlePrefix = "BattleNodeService.rpc/"

func newPickWatcher(t *testing.T, entries map[string]NodeEntry) *NodeWatcher {
	t.Helper()
	w := NewNodeWatcher("battle", testBattlePrefix, nil, nil)
	for key, entry := range entries {
		w.Upsert(testBattlePrefix+key, entry)
	}
	return w
}

// 空镜像:返回 false,不 panic(rand.Intn(0) 会 panic)。
func TestPickRandomEmptyWatcher(t *testing.T) {
	w := newPickWatcher(t, nil)

	_, ok := w.PickRandom()
	require.False(t, ok)
}

// 结果必在镜像内,且每个节点都选得到(200 次抽样漏掉某个节点的概率是 3×(2/3)^200 ≈ 3e-35,可视为确定)。
func TestPickRandomCoversAllNodes(t *testing.T) {
	w := newPickWatcher(t, map[string]NodeEntry{
		"1": {NodeId: 1, Endpoint: "battle-1"},
		"2": {NodeId: 2, Endpoint: "battle-2"},
		"3": {NodeId: 3, Endpoint: "battle-3"},
	})
	known := map[string]bool{"battle-1": true, "battle-2": true, "battle-3": true}

	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		entry, ok := w.PickRandom()
		require.True(t, ok)
		require.True(t, known[entry.Endpoint], "选中的节点必须在镜像内: %s", entry.Endpoint)
		seen[entry.Endpoint] = true
	}
	require.Equal(t, known, seen, "每个节点都必须选得到")
}
