package scenenode

import (
	"fmt"
	"testing"
)

// regJSON 拼一条与 noderegistry / C++ MessageToJsonString 同形状的注册值。
func regJSON(zoneID, nodeID uint32, uuid, ip string, grpcPort uint32) []byte {
	return []byte(fmt.Sprintf(
		`{"nodeId":%d,"nodeType":3,"nodeUuid":%q,"zoneId":%d,`+
			`"endpoint":{"ip":%q,"port":9000},"grpcEndpoint":{"ip":%q,"port":%d}}`,
		nodeID, uuid, zoneID, ip, ip, grpcPort))
}

func rpcKey(zoneID, nodeID uint32) string {
	return fmt.Sprintf("%szone/%d/node_type/3/node_id/%d", SceneNodeRpcPrefix, zoneID, nodeID)
}

// 镜像没同步过就是"不可信",Locator 据此 fail-closed。
func TestSyncedFalseBeforeFullSync(t *testing.T) {
	w := NewWatcher("scene", SceneNodeRpcPrefix, nil, nil)
	if w.Synced() {
		t.Fatal("首次 fullSync 之前 Synced 必须为 false")
	}
	if w.Count() != 0 {
		t.Fatalf("空 watcher Count = %d, want 0", w.Count())
	}
}

func TestEndpointOfHit(t *testing.T) {
	w := NewWatcher("scene", SceneNodeRpcPrefix, nil, nil)
	entry, ok := w.parse(rpcKey(1, 7), regJSON(1, 7, "uuid-a", "10.0.0.1", 9201))
	if !ok {
		t.Fatal("parse 应当接受一条完整注册")
	}
	w.Upsert(rpcKey(1, 7), entry)

	got, err := w.EndpointOf(1, "7")
	if err != nil {
		t.Fatalf("EndpointOf 未命中: %v", err)
	}
	if got != "10.0.0.1:9201" {
		t.Fatalf("EndpointOf = %q, want 10.0.0.1:9201", got)
	}
	full, err := w.EntryOf(1, "7")
	if err != nil || full.NodeUuid != "uuid-a" {
		t.Fatalf("EntryOf = %+v, err=%v", full, err)
	}
}

// node_id 只在 zone 内唯一:同号不同 zone 是两个进程,不能混。
func TestEndpointOfNotRegistered(t *testing.T) {
	w := NewWatcher("scene", SceneNodeRpcPrefix, nil, nil)
	entry, _ := w.parse(rpcKey(1, 7), regJSON(1, 7, "uuid-a", "10.0.0.1", 9201))
	w.Upsert(rpcKey(1, 7), entry)

	if _, err := w.EndpointOf(1, "8"); err == nil {
		t.Fatal("不存在的 node_id 必须报错")
	}
	if _, err := w.EndpointOf(2, "7"); err == nil {
		t.Fatal("同号不同 zone 必须报错(node_id 只在 zone 内唯一)")
	}
}

// 同身份两条注册 = 租约/部署链分叉,任何一条都不能被选中。
func TestEndpointOfAmbiguousRejected(t *testing.T) {
	w := NewWatcher("scene", SceneNodeRpcPrefix, nil, nil)
	a, _ := w.parse(rpcKey(1, 7), regJSON(1, 7, "uuid-a", "10.0.0.1", 9201))
	b, _ := w.parse("SceneNodeService.rpc/zone/1/node_type/3/node_id/7-stale",
		regJSON(1, 7, "uuid-b", "10.0.0.2", 9201))
	w.Upsert(rpcKey(1, 7), a)
	w.Upsert("SceneNodeService.rpc/zone/1/node_type/3/node_id/7-stale", b)

	if _, err := w.EndpointOf(1, "7"); err == nil {
		t.Fatal("同身份多条注册必须拒选")
	}
}

// /allocated/ 是 noderegistry 的占位 key,不是节点注册;缺 grpcEndpoint 的节点
// 无法作为 gRPC 对端(回退 endpoint 会拿 raw TCP 口当 gRPC 用,协议错乱)。
func TestParseSkipsAllocatedAndMissingGrpcEndpoint(t *testing.T) {
	w := NewWatcher("scene", SceneNodeRpcPrefix, nil, nil)

	if _, ok := w.parse(SceneNodeRpcPrefix+"allocated/node_type/3/node_id/7",
		regJSON(1, 7, "uuid-a", "10.0.0.1", 9201)); ok {
		t.Fatal("/allocated/ 占位 key 必须跳过")
	}
	if _, ok := w.parse(rpcKey(1, 7), regJSON(1, 7, "uuid-a", "10.0.0.1", 0)); ok {
		t.Fatal("缺 grpcEndpoint 的注册必须跳过")
	}
	if _, ok := w.parse(rpcKey(1, 7), []byte("{not json")); ok {
		t.Fatal("坏 JSON 必须跳过")
	}
}

// 节点重启换端口:旧 endpoint 的连接再没有持有者,必须通过 onRemove 废弃,
// 否则它永久滞留在连接缓存里,后续拨到它必失败。
func TestUpsertEndpointChangeTriggersOnRemove(t *testing.T) {
	var removed []string
	var counts []int
	w := NewWatcher("scene", SceneNodeRpcPrefix,
		func(kind string, n int) {
			if kind != "scene" {
				t.Errorf("onCount kind = %q, want scene", kind)
			}
			counts = append(counts, n)
		},
		func(e NodeEntry) { removed = append(removed, e.Endpoint) })

	key := rpcKey(1, 7)
	first, _ := w.parse(key, regJSON(1, 7, "uuid-a", "10.0.0.1", 9201))
	w.Upsert(key, first)
	if len(removed) != 0 {
		t.Fatalf("首次上线不该触发 onRemove: %v", removed)
	}

	same, _ := w.parse(key, regJSON(1, 7, "uuid-a", "10.0.0.1", 9201))
	w.Upsert(key, same)
	if len(removed) != 0 {
		t.Fatalf("endpoint 没变不该触发 onRemove: %v", removed)
	}

	restarted, _ := w.parse(key, regJSON(1, 7, "uuid-b", "10.0.0.1", 9301))
	w.Upsert(key, restarted)
	if len(removed) != 1 || removed[0] != "10.0.0.1:9201" {
		t.Fatalf("换端口后应废弃旧连接一次,实得 %v", removed)
	}

	if got, err := w.EndpointOf(1, "7"); err != nil || got != "10.0.0.1:9301" {
		t.Fatalf("换端口后 EndpointOf = %q, err=%v", got, err)
	}
	if w.Count() != 1 {
		t.Fatalf("同 key 覆盖后 Count = %d, want 1", w.Count())
	}
	if len(counts) != 3 {
		t.Fatalf("每次 Upsert 都该上报一次数量,实得 %v", counts)
	}
}
