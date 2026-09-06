package discovery

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func newWatcherWithRemoved(removed *[]string) *NodeWatcher {
	return NewNodeWatcher("LoginNodeService", "LoginNodeService.rpc/", nil,
		func(entry NodeEntry) { *removed = append(*removed, entry.Endpoint) })
}

// zone 过滤只挑同 zone 实例,且该 zone 没实例时不回退到别的 zone(D33)。
func TestPickRandomInZoneOnlyReturnsSameZone(t *testing.T) {
	w := NewNodeWatcher("LoginNodeService", "LoginNodeService.rpc/", nil, nil)
	w.Upsert("k/z1/n1", NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "login-z1-a"})
	w.Upsert("k/z1/n2", NodeEntry{NodeId: 2, ZoneId: 1, Endpoint: "login-z1-b"})
	w.Upsert("k/z1/n3", NodeEntry{NodeId: 3, ZoneId: 1, Endpoint: "login-z1-c"})
	w.Upsert("k/z2/n4", NodeEntry{NodeId: 4, ZoneId: 2, Endpoint: "login-z2-a"})
	w.Upsert("k/z2/n5", NodeEntry{NodeId: 5, ZoneId: 2, Endpoint: "login-z2-b"})

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		entry, ok := w.PickRandomInZone(2)
		require.True(t, ok)
		require.Equal(t, uint32(2), entry.ZoneId, "挑到了别的 zone 的实例: %s", entry.Endpoint)
		seen[entry.Endpoint] = true
	}
	require.Len(t, seen, 2, "同 zone 的两个实例都应被随机到")

	_, ok := w.PickRandomInZone(9)
	require.False(t, ok, "zone 9 没有实例时不能回退到其它 zone")
}

func TestPickRandomOnEmptyWatcher(t *testing.T) {
	w := NewNodeWatcher("MatchNodeService", "MatchNodeService.rpc/", nil, nil)
	_, ok := w.PickRandom()
	require.False(t, ok)
	_, ok = w.PickRandomInZone(1)
	require.False(t, ok)
	require.Equal(t, 0, w.Count())
}

// etcd DELETE 事件:节点出镜像并触发 onRemove(清连接缓存)。
func TestDeleteEventInvokesOnRemove(t *testing.T) {
	var removed []string
	w := newWatcherWithRemoved(&removed)
	w.Upsert("LoginNodeService.rpc/zone/1/node_type/5/node_id/1", NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "10.0.0.1:53000"})
	require.Equal(t, 1, w.Count())

	w.handleEvent(&clientv3.Event{
		Type: clientv3.EventTypeDelete,
		Kv:   &mvccpb.KeyValue{Key: []byte("LoginNodeService.rpc/zone/1/node_type/5/node_id/1")},
	})
	require.Equal(t, 0, w.Count())
	require.Equal(t, []string{"10.0.0.1:53000"}, removed)

	// 删不存在的 key 不应再触发 onRemove。
	w.handleEvent(&clientv3.Event{
		Type: clientv3.EventTypeDelete,
		Kv:   &mvccpb.KeyValue{Key: []byte("LoginNodeService.rpc/zone/1/node_type/5/node_id/99")},
	})
	require.Len(t, removed, 1)
}

// 同 key 重登记且 endpoint 变了(重启换端口):旧连接必须废弃。
func TestUpsertEndpointChangeInvokesOnRemove(t *testing.T) {
	var removed []string
	w := newWatcherWithRemoved(&removed)
	w.Upsert("k", NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "10.0.0.1:53000"})
	w.Upsert("k", NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "10.0.0.1:53000"})
	require.Empty(t, removed, "endpoint 不变不清连接")
	w.Upsert("k", NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "10.0.0.1:53001"})
	require.Equal(t, []string{"10.0.0.1:53000"}, removed)
	require.Equal(t, 1, w.Count())
}

// 注册值解析:跳过 /allocated/ 占位 key、拒绝没有 grpcEndpoint 的节点。
func TestParseRegistrationValue(t *testing.T) {
	w := NewNodeWatcher("LoginNodeService", "LoginNodeService.rpc/", nil, nil)
	_, ok := w.parse("LoginNodeService.rpc/allocated/node_type/5/node_id/1", []byte(`{}`))
	require.False(t, ok)

	_, ok = w.parse("LoginNodeService.rpc/zone/1/node_type/5/node_id/1",
		[]byte(`{"nodeId":1,"zoneId":1,"endpoint":{"ip":"10.0.0.1","port":53000}}`))
	require.False(t, ok, "只有 raw TCP endpoint、没有 grpcEndpoint 的节点不能当 gRPC 目标")

	entry, ok := w.parse("LoginNodeService.rpc/zone/1/node_type/5/node_id/1",
		[]byte(`{"nodeId":1,"zoneId":1,"nodeUuid":"u","grpcEndpoint":{"ip":"10.0.0.1","port":53000}}`))
	require.True(t, ok)
	require.Equal(t, NodeEntry{NodeId: 1, ZoneId: 1, NodeUuid: "u", Endpoint: "10.0.0.1:53000"}, entry)
}

// diffRemoved 必须同时覆盖两种「旧连接该回收」的情形:key 整个消失,以及
// key 还在但 endpoint 变了(节点重启换端口)。只处理前者会让旧 endpoint 的
// 连接永久滞留在缓存里,之后拨到它必失败。
func TestDiffRemovedCoversEndpointChange(t *testing.T) {
	prev := map[string]NodeEntry{
		"gone":   {NodeId: 1, Endpoint: "127.0.0.1:1001"},
		"moved":  {NodeId: 2, Endpoint: "127.0.0.1:1002"},
		"stable": {NodeId: 3, Endpoint: "127.0.0.1:1003"},
	}
	next := map[string]NodeEntry{
		"moved":  {NodeId: 2, Endpoint: "127.0.0.1:2002"},
		"stable": {NodeId: 3, Endpoint: "127.0.0.1:1003"},
	}

	removed := diffRemoved(prev, next)
	endpoints := make(map[string]bool, len(removed))
	for _, entry := range removed {
		endpoints[entry.Endpoint] = true
	}

	require.Len(t, removed, 2)
	require.True(t, endpoints["127.0.0.1:1001"], "消失的 key 要回收")
	require.True(t, endpoints["127.0.0.1:2002"] == false, "回收的应是旧 endpoint,不是新的")
	require.True(t, endpoints["127.0.0.1:1002"], "换了 endpoint 的旧连接要回收")
	require.False(t, endpoints["127.0.0.1:1003"], "没变的不回收")
}
