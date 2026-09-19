package logic

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"scene_manager/internal/svc"
)

// 回归:initWorldScenesForZone 曾把「CreateScene RPC 不可达(含 5s 超时)」直接当成节点已死 ——
// 把节点 ZREM 出负载集、改写 scene:{id}:node 并在别的节点上再建同一个 scene_id。
// 再入屏障拦不住:它对没有 death_at 的节点一律放行,而那条路径从不写 death_at。
// 于是高负载下一个只是慢了的活节点,名下的世界频道会在两个节点上各有一份活副本。
//
// 这两个用例钉住修复后的契约:不可达只让本轮跳过;改写归属必须以「已从 etcd 注册表消失」为前提。

// worldInitTestConfIDs:WorldChannelCount=1 时,FNV-1a(confId*1000) % 2 在排序后的
// {"10","20"} 上给出 1002/1004→"10"、1001/1003→"20"。用例里仍用 require 复核这个前提,
// 哈希或分配规则将来变了会响亮地失败,而不是空转通过。
var worldInitTestConfIDs = []uint64{1001, 1002, 1003, 1004}

const (
	worldInitDownNode = "10"
	worldInitLiveNode = "20"
)

// bufnetResolverForTest 把端点解析固定成 "bufnet:<nodeId>"。registerKnownNodeForTest 放进注册表镜像的
// 条目没有 ip/port,包级默认解析器对「已注册节点」会去读这些空字段;这里从第一轮 init 起就绕开,
// 让用例只依赖「是否在注册表里」这一个事实。
func bufnetResolverForTest(t *testing.T) {
	t.Helper()
	restore := SetNodeEndpointResolverForTest(func(_ uint32, id string) (string, bool) {
		return "bufnet:" + id, true
	})
	t.Cleanup(restore)
}

// sceneNodeDownForTest 让指定节点的拨号以 Unavailable 失败,其余节点照常走包级共享的 fake 节点。
// 依赖 bufnetResolverForTest 给出的端点形状。先驱逐该节点已缓存的连接,否则第二次 init 直接
// 复用旧连接,根本走不到拨号。
func sceneNodeDownForTest(t *testing.T, zoneID uint32, nodeID string) {
	t.Helper()
	downEndpoint := "bufnet:" + nodeID
	prevDialer := nodeDialer
	restoreDialer := SetNodeDialerForTest(func(ctx context.Context, endpoint string) (*grpc.ClientConn, error) {
		if endpoint == downEndpoint {
			return nil, status.Error(codes.Unavailable, "scene node is down")
		}
		return prevDialer(ctx, endpoint)
	})
	RemoveNodeConn(zoneID, nodeID)
	t.Cleanup(func() {
		restoreDialer()
		RemoveNodeConn(zoneID, nodeID)
	})
}

// worldSceneOwnersForTest 返回 scene_id -> 当前属主节点(scene:{id}:node)。
func worldSceneOwnersForTest(t *testing.T, ctx context.Context, sc *svc.ServiceContext, mr *miniredis.Miniredis) map[uint64]string {
	t.Helper()
	owners := make(map[uint64]string)
	for _, confID := range worldInitTestConfIDs {
		sceneIDs, err := GetAllWorldChannels(ctx, sc, confID, testZoneId)
		require.NoError(t, err)
		for _, sceneID := range sceneIDs {
			node, err := mr.Get(fmt.Sprintf("scene:%d:node", sceneID))
			require.NoError(t, err, "scene %d 没有属主映射", sceneID)
			owners[sceneID] = node
		}
	}
	return owners
}

// seedWorldOnTwoRegisteredNodes:两个节点都在负载集、都在 etcd 注册表镜像里、都可达,
// 跑一轮 init 把频道铺好,返回铺好之后的属主映射。
func seedWorldOnTwoRegisteredNodes(t *testing.T, ctx context.Context, sc *svc.ServiceContext, mr *miniredis.Miniredis) map[uint64]string {
	t.Helper()
	bufnetResolverForTest(t)
	markKnownNodesSyncedForTest(t)
	registerKnownNodeForTest(t, "world-init-test/"+worldInitDownNode, testZoneId, worldInitDownNode)
	registerKnownNodeForTest(t, "world-init-test/"+worldInitLiveNode, testZoneId, worldInitLiveNode)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, worldInitDownNode)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, worldInitLiveNode)

	initWorldScenesForZone(ctx, sc, testZoneId, worldInitTestConfIDs, true)

	owners := worldSceneOwnersForTest(t, ctx, sc, mr)
	onDown, onLive := 0, 0
	for _, node := range owners {
		switch node {
		case worldInitDownNode:
			onDown++
		case worldInitLiveNode:
			onLive++
		}
	}
	require.Positive(t, onDown, "前提不成立:没有任何频道初始落在节点 %s 上,用例会空转", worldInitDownNode)
	require.Positive(t, onLive, "前提不成立:没有任何频道初始落在节点 %s 上", worldInitLiveNode)
	return owners
}

// 节点不可达、但仍在 etcd 注册表里(= 拿不到它已死的证据,可能只是慢):
// 归属不动、负载集不动,活节点上也不会冒出它名下频道的第二份副本。
func TestInitWorldScenes_UnreachableButRegisteredNodeKeepsOwnershipAndLoadSet(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	before := seedWorldOnTwoRegisteredNodes(t, ctx, sc, mr)

	sceneNodeDownForTest(t, testZoneId, worldInitDownNode)
	initWorldScenesForZone(ctx, sc, testZoneId, worldInitTestConfIDs, true)

	assert.Equal(t, before, worldSceneOwnersForTest(t, ctx, sc, mr),
		"节点只是不可达且仍在注册表里时,不得改写任何频道的属主")
	_, err := mr.ZScore(nodeLoadKey(testZoneId), worldInitDownNode)
	assert.NoError(t, err,
		"world_init 不得把不可达节点摘出负载集:IsNodeAlive 把「不在负载集」当作已死的唯一证据")
	_, err = mr.ZScore(nodeLoadKey(testZoneId), worldInitLiveNode)
	assert.NoError(t, err)
}

// 节点不可达、且已从 etcd 注册表消失(租约到期 / 主动注销):这才是死亡证据,
// 它名下的频道改派到活节点;活节点自己的频道留在原地。
func TestInitWorldScenes_NodeGoneFromRegistryIsReassignedToLiveNode(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	before := seedWorldOnTwoRegisteredNodes(t, ctx, sc, mr)

	// 模拟 etcd DELETE:节点从注册表镜像里消失。本用例没有写 death_at,
	// 再入屏障按「没观察到近期死亡」放行,与生产里屏障已过的状态等价。
	knownNodesMu.Lock()
	delete(knownNodes, "world-init-test/"+worldInitDownNode)
	knownNodesMu.Unlock()
	require.True(t, isNodeGoneFromRegistry(testZoneId, worldInitDownNode))

	sceneNodeDownForTest(t, testZoneId, worldInitDownNode)
	initWorldScenesForZone(ctx, sc, testZoneId, worldInitTestConfIDs, true)

	after := worldSceneOwnersForTest(t, ctx, sc, mr)
	require.Len(t, after, len(before))
	for sceneID, oldNode := range before {
		assert.Equal(t, worldInitLiveNode, after[sceneID],
			"scene %d 原属主 %s:死节点的频道必须改派到活节点,活节点的频道必须留在原地", sceneID, oldNode)
	}
}
