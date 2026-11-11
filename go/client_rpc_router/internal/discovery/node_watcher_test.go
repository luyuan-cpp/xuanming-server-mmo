package discovery

import (
	"fmt"
	"testing"

	"shared/releasetrack"

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
	_, ok := w.PickRandomForTrack(releasetrack.Stable)
	require.False(t, ok)
	_, ok = w.PickRandomForTrack(releasetrack.Canary)
	require.False(t, ok)
	_, ok = w.PickRandomInZone(1)
	require.False(t, ok)
	require.Equal(t, 0, w.Count())
}

// 全局实例按会话轨道选(docs/design/release-track-gray-canary.md §2.4):
// canary 会话有 canary 就只用 canary,stable 会话永远不进 canary。
func TestPickRandomForTrackFollowsSessionTrack(t *testing.T) {
	w := NewNodeWatcher("GuildNodeService", "GuildNodeService.rpc/", nil, nil)
	w.Upsert("k/s1", NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "guild-stable-a"})
	w.Upsert("k/s2", NodeEntry{NodeId: 2, ZoneId: 1, Endpoint: "guild-stable-b"})
	w.Upsert("k/c1", NodeEntry{NodeId: 3, ZoneId: 1, Endpoint: "guild-canary-a", Track: releasetrack.Canary})

	stableSeen := map[string]bool{}
	for i := 0; i < 200; i++ {
		entry, ok := w.PickRandomForTrack(releasetrack.Stable)
		require.True(t, ok)
		require.Equal(t, releasetrack.Stable, entry.Track, "stable 会话被送进了 canary 实例 %s", entry.Endpoint)
		stableSeen[entry.Endpoint] = true

		canaryEntry, ok := w.PickRandomForTrack(releasetrack.Canary)
		require.True(t, ok)
		require.Equal(t, "guild-canary-a", canaryEntry.Endpoint, "有 canary 实例时 canary 会话只能用它")
	}
	require.Len(t, stableSeen, 2, "两个 stable 实例都应被随机到")
}

// canary 会话没有 canary 实例时退回 stable;只剩 canary 实例时 stable 会话无实例可用。
func TestPickRandomForTrackFallbackIsOneWay(t *testing.T) {
	w := NewNodeWatcher("GuildNodeService", "GuildNodeService.rpc/", nil, nil)
	w.Upsert("k/s1", NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "guild-stable-a"})
	entry, ok := w.PickRandomForTrack(releasetrack.Canary)
	require.True(t, ok)
	require.Equal(t, "guild-stable-a", entry.Endpoint)

	onlyCanary := NewNodeWatcher("GuildNodeService", "GuildNodeService.rpc/", nil, nil)
	onlyCanary.Upsert("k/c1", NodeEntry{NodeId: 3, ZoneId: 1, Endpoint: "guild-canary-a", Track: releasetrack.Canary})
	_, ok = onlyCanary.PickRandomForTrack(releasetrack.Stable)
	require.False(t, ok, "stable 会话不能退到 canary 实例")
}

// pickedForTrack 挑 picks 次,返回挑到过的 endpoint 集合。
func pickedForTrack(t *testing.T, w *NodeWatcher, session releasetrack.Track, picks int) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	for i := 0; i < picks; i++ {
		entry, ok := w.PickRandomForTrack(session)
		require.True(t, ok)
		seen[entry.Endpoint] = true
	}
	return seen
}

// 设了子集之后,全服池类型只在本路由服那段弧上挑(subset.go);轨道的选法(canary 优先、stable 不进 canary)不变;
// 之后镜像再变,沿用同一组参数重算。
func TestPickRandomForTrackStaysInArc(t *testing.T) {
	w := NewNodeWatcher("ChatNodeService", "ChatNodeService.rpc/", nil, nil)
	for i := 0; i < 8; i++ {
		uuid := fmt.Sprintf("s%d", i)
		w.Upsert("k/"+uuid, NodeEntry{NodeId: uint32(i + 1), ZoneId: 1, NodeUuid: uuid, Endpoint: "chat-" + uuid})
	}
	w.Upsert("k/c0", NodeEntry{NodeId: 20, ZoneId: 1, NodeUuid: "c0", Endpoint: "chat-c0", Track: releasetrack.Canary})

	// 同轨道 4 台路由服、本路由服排第 1、k = 2:m = round(2×4/8) = 1,弧是第 [8, 16) 格,每台目标占 4 格 → 环上的 s2、s3。
	w.SetSubset(1, 4, 2)
	require.Equal(t, map[string]bool{"chat-s2": true, "chat-s3": true}, pickedForTrack(t, w, releasetrack.Stable, 400),
		"stable 会话只在本路由服那段弧上挑")
	require.Equal(t, map[string]bool{"chat-c0": true}, pickedForTrack(t, w, releasetrack.Canary, 50),
		"canary 组只有 1 台(不多于 k),canary 会话只能用它")

	// canary 实例下线:canary 会话退回 stable,同样只在 stable 那段弧上挑。
	w.handleEvent(&clientv3.Event{Type: clientv3.EventTypeDelete, Kv: &mvccpb.KeyValue{Key: []byte("k/c0")}})
	require.Equal(t, map[string]bool{"chat-s2": true, "chat-s3": true}, pickedForTrack(t, w, releasetrack.Canary, 400))

	// 新实例 s8 上线:T = 9,m 仍为 1,弧是第 [9, 18) 格,每台目标占 4 格 → s2、s3、s4。
	w.Upsert("k/s8", NodeEntry{NodeId: 9, ZoneId: 1, NodeUuid: "s8", Endpoint: "chat-s8"})
	require.Equal(t, map[string]bool{"chat-s2": true, "chat-s3": true, "chat-s4": true}, pickedForTrack(t, w, releasetrack.Stable, 600))

	// 不取子集(size = 0):回到全部实例。
	w.SetSubset(1, 4, 0)
	require.Len(t, pickedForTrack(t, w, releasetrack.Stable, 900), 9)
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
	require.Equal(t, NodeEntry{NodeId: 1, ZoneId: 1, NodeUuid: "u", Endpoint: "10.0.0.1:53000"}, entry,
		"没写 releaseTrack 的节点按 stable")

	entry, ok = w.parse("LoginNodeService.rpc/zone/1/node_type/5/node_id/2",
		[]byte(`{"nodeId":2,"zoneId":1,"nodeUuid":"c","grpcEndpoint":{"ip":"10.0.0.2","port":53000},"releaseTrack":"canary"}`))
	require.True(t, ok)
	require.Equal(t, releasetrack.Canary, entry.Track)

	_, ok = w.parse("LoginNodeService.rpc/zone/1/node_type/5/node_id/3",
		[]byte(`{"nodeId":3,"zoneId":1,"nodeUuid":"x","grpcEndpoint":{"ip":"10.0.0.3","port":53000},"releaseTrack":"beta"}`))
	require.False(t, ok, "认不出的轨道既不能当 stable 也不能当 canary,整条跳过")
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

// 选点读的是镜像变更时重建的只读索引(缺口清单第 28 条):同一个 key 改 zone、改轨道、被删除之后,
// 下一次选点必须立刻看到新状态,不能留着旧索引里的条目。
func TestPickIndexFollowsEveryMirrorChange(t *testing.T) {
	w := NewNodeWatcher("LoginNodeService", "LoginNodeService.rpc/", nil, nil)
	key := "LoginNodeService.rpc/zone/1/node_type/5/node_id/1"
	w.Upsert(key, NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "login-a", Track: releasetrack.Canary})

	entry, ok := w.PickRandomInZone(1)
	require.True(t, ok)
	require.Equal(t, "login-a", entry.Endpoint)
	entry, ok = w.PickRandomForTrack(releasetrack.Canary)
	require.True(t, ok, "canary 会话应选到唯一的 canary 实例")
	require.Equal(t, "login-a", entry.Endpoint)
	_, ok = w.PickRandomForTrack(releasetrack.Stable)
	require.False(t, ok, "stable 会话永远不进 canary")

	// 同一个 key 改到 zone 2、改回 stable:旧 zone、旧轨道上都不能再选到它。
	w.Upsert(key, NodeEntry{NodeId: 1, ZoneId: 2, Endpoint: "login-a", Track: releasetrack.Stable})
	_, ok = w.PickRandomInZone(1)
	require.False(t, ok, "改 zone 之后旧 zone 不能再选到它")
	entry, ok = w.PickRandomInZone(2)
	require.True(t, ok)
	require.Equal(t, "login-a", entry.Endpoint)
	entry, ok = w.PickRandomForTrack(releasetrack.Canary)
	require.True(t, ok, "没有 canary 实例时 canary 会话退回 stable")
	require.Equal(t, "login-a", entry.Endpoint)

	w.handleEvent(&clientv3.Event{Type: clientv3.EventTypeDelete, Kv: &mvccpb.KeyValue{Key: []byte(key)}})
	_, ok = w.PickRandomInZone(2)
	require.False(t, ok, "删除之后不能再选到它")
	_, ok = w.PickRandomForTrack(releasetrack.Stable)
	require.False(t, ok, "删除之后不能再选到它")
	require.Equal(t, 0, w.Count())
}
