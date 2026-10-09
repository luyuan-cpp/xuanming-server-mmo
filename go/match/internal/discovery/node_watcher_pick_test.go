package discovery

// PickRandomExcept(k8s-client-entry D82:CreateBattle 被 battle_not_allocatable 拒绝后
// 换一个没试过的节点重试)的选择语义:按 Endpoint 排除、全被排除 / 镜像为空返回 false、
// 同一进程的多条残留注册(key 不同、endpoint 相同)一起被排除、exclude 只读。
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

// 空镜像:两种选法都返回 false,不 panic(rand.Intn(0) 会 panic)。
func TestPickRandomExceptEmptyWatcher(t *testing.T) {
	w := newPickWatcher(t, nil)

	_, ok := w.PickRandom()
	require.False(t, ok)
	_, ok = w.PickRandomExcept(nil)
	require.False(t, ok)
	_, ok = w.PickRandomExcept(map[string]bool{"battle-1": true})
	require.False(t, ok)
}

// 排除掉除一个以外的全部节点:结果恒为剩下那一个(确定性,不依赖随机)。
func TestPickRandomExceptReturnsOnlyRemainingNode(t *testing.T) {
	w := newPickWatcher(t, map[string]NodeEntry{
		"1": {NodeId: 1, Endpoint: "battle-1"},
		"2": {NodeId: 2, Endpoint: "battle-2"},
		"3": {NodeId: 3, Endpoint: "battle-3"},
	})
	exclude := map[string]bool{"battle-1": true, "battle-2": true}

	for i := 0; i < 50; i++ {
		entry, ok := w.PickRandomExcept(exclude)
		require.True(t, ok)
		require.Equal(t, "battle-3", entry.Endpoint)
		require.Equal(t, uint32(3), entry.NodeId)
	}
	require.Equal(t, map[string]bool{"battle-1": true, "battle-2": true}, exclude, "exclude 只读,不得被改写")
}

// 全部被排除:返回 false,调用方据此放弃重试(不能退回已试过的节点)。
func TestPickRandomExceptAllExcluded(t *testing.T) {
	w := newPickWatcher(t, map[string]NodeEntry{
		"1": {NodeId: 1, Endpoint: "battle-1"},
		"2": {NodeId: 2, Endpoint: "battle-2"},
	})

	_, ok := w.PickRandomExcept(map[string]bool{"battle-1": true, "battle-2": true})
	require.False(t, ok)
}

// 同一进程的两条残留注册(key 不同、endpoint 相同):按 endpoint 排除时两条一起出局,
// 不会「换节点」换回同一个进程。
func TestPickRandomExceptExcludesDuplicateRegistrationsByEndpoint(t *testing.T) {
	w := newPickWatcher(t, map[string]NodeEntry{
		"7":     {NodeId: 7, Endpoint: "battle-7"},
		"7-old": {NodeId: 17, Endpoint: "battle-7"},
		"8":     {NodeId: 8, Endpoint: "battle-8"},
	})
	exclude := map[string]bool{"battle-7": true}

	for i := 0; i < 50; i++ {
		entry, ok := w.PickRandomExcept(exclude)
		require.True(t, ok)
		require.Equal(t, "battle-8", entry.Endpoint)
	}
}

// exclude 为 nil / 只含未知 endpoint:与 PickRandom 同语义,结果必在镜像内,且每个节点都
// 选得到(200 次抽样漏掉某个节点的概率是 3×(2/3)^200 ≈ 3e-35,可视为确定)。
func TestPickRandomExceptWithoutEffectiveExcludeCoversAllNodes(t *testing.T) {
	w := newPickWatcher(t, map[string]NodeEntry{
		"1": {NodeId: 1, Endpoint: "battle-1"},
		"2": {NodeId: 2, Endpoint: "battle-2"},
		"3": {NodeId: 3, Endpoint: "battle-3"},
	})
	known := map[string]bool{"battle-1": true, "battle-2": true, "battle-3": true}

	for _, exclude := range []map[string]bool{nil, {"battle-404": true}} {
		seen := make(map[string]bool)
		for i := 0; i < 200; i++ {
			entry, ok := w.PickRandomExcept(exclude)
			require.True(t, ok)
			require.True(t, known[entry.Endpoint], "选中的节点必须在镜像内: %s", entry.Endpoint)
			seen[entry.Endpoint] = true
		}
		require.Equal(t, known, seen, "每个未被排除的节点都必须选得到 exclude=%v", exclude)
	}

	entry, ok := w.PickRandom()
	require.True(t, ok)
	require.True(t, known[entry.Endpoint])
}
