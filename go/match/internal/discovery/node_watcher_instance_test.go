package discovery

// EntryOfInstance:按实例 uuid 定位。观战原件固定了 target_instance_id,取消时靠它把 RemoveObserver 发回原实例,
// 所以它只能命中同一个实例:node_id 被新实例占用后查旧 uuid 必须查不到,不能退化成按 node_id 的结果。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEntryOfInstanceFindsOnlyTheExactInstance(t *testing.T) {
	w := newPickWatcher(t, map[string]NodeEntry{
		"7": {NodeId: 7, NodeUuid: "instance-a", Endpoint: "battle-7"},
		"8": {NodeId: 8, NodeUuid: "instance-b", Endpoint: "battle-8"},
	})

	entry, err := w.EntryOfInstance("instance-b")
	require.NoError(t, err)
	require.Equal(t, uint32(8), entry.NodeId)
	require.Equal(t, "battle-8", entry.Endpoint)

	_, err = w.EntryOfInstance("instance-unknown")
	require.Error(t, err, "没登记的实例必须报错,调用方不能拿到别的节点")

	// 进程重启:同一 node_id 换了实例 uuid。旧 uuid 查不到,新 uuid 查得到。
	w.Upsert(testBattlePrefix+"7", NodeEntry{NodeId: 7, NodeUuid: "instance-a2", Endpoint: "battle-7"})
	_, err = w.EntryOfInstance("instance-a")
	require.Error(t, err, "实例被替换后,旧 uuid 不能命中同 node_id 的新实例")
	entry, err = w.EntryOfInstance("instance-a2")
	require.NoError(t, err)
	require.Equal(t, uint32(7), entry.NodeId)
}

// 空 uuid 必须报错:镜像里可能有没带 uuid 的登记,空串不能匹配到它们。
func TestEntryOfInstanceRejectsEmptyId(t *testing.T) {
	w := newPickWatcher(t, map[string]NodeEntry{
		"1": {NodeId: 1, Endpoint: "battle-1"},
	})

	_, err := w.EntryOfInstance("")
	require.Error(t, err)
}
