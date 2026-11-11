package logic

// 节点号持有者(缺口清单第 33 条,node_holder.go)的回归。
//
// C++ 节点据它认出「自己的号已经被新进程接走」(scene_ownership_reconciler.cpp)。这里钉住三件事:
//   - 写入按 etcd 修订号单调:乱序晚到的旧登记盖不回新持有者;修订号按十进制串比,不丢精度;
//   - 每个副本的 watch PUT 都写,DELETE 不删(节点死了,最后一次登记仍是事实);
//   - 全量同步只由领导者写,补上漏掉的 PUT 并续 TTL;没有 node_uuid 或修订号的登记不写。
// 节点号 879x、73xx 与 zone 73 不与别的用例重名。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"scene_manager/internal/svc"
)

func nodeHolderValueForTest(t *testing.T, mr *miniredis.Miniredis, zoneID uint32, nodeID string) string {
	t.Helper()
	if !mr.Exists(nodeHolderKey(zoneID, nodeID)) {
		return ""
	}
	value, err := mr.Get(nodeHolderKey(zoneID, nodeID))
	require.NoError(t, err)
	return value
}

func publishHolderForTest(sc *svc.ServiceContext, nodeID, uuid string, revision int64) {
	publishNodeHolder(sc, nodeHolderRecord{ZoneID: testZoneId, NodeID: nodeID, NodeUUID: uuid, ModRevision: revision})
}

func TestNodeHolderKeyShape(t *testing.T) {
	// 跨语言契约:C++ scene_ownership::NodeHolderKey 读同一个键。以 node:zone: 开头(合服按前缀清),不带 hash tag。
	assert.Equal(t, "node:zone:3:17:holder", nodeHolderKey(3, "17"))
}

func TestPublishNodeHolder_NewerRegistrationWins(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const node = "8791"
	key := nodeHolderKey(testZoneId, node)

	publishHolderForTest(sc, node, "inst-a", 100)
	assert.Equal(t, "100:inst-a", nodeHolderValueForTest(t, mr, testZoneId, node))
	assert.Equal(t, time.Duration(nodeHolderTTLSeconds)*time.Second, mr.TTL(key))

	// 副本之间乱序到达的旧登记:不覆盖。
	publishHolderForTest(sc, node, "inst-old", 90)
	assert.Equal(t, "100:inst-a", nodeHolderValueForTest(t, mr, testZoneId, node), "更旧的登记盖不回新持有者")

	// 同号新进程登记,修订号更大:覆盖。
	publishHolderForTest(sc, node, "inst-b", 150)
	assert.Equal(t, "150:inst-b", nodeHolderValueForTest(t, mr, testZoneId, node))

	// 同一次登记再写一遍(全量同步):值不变,TTL 续上。
	mr.SetTTL(key, time.Second)
	publishHolderForTest(sc, node, "inst-b", 150)
	assert.Equal(t, "150:inst-b", nodeHolderValueForTest(t, mr, testZoneId, node))
	assert.Equal(t, time.Duration(nodeHolderTTLSeconds)*time.Second, mr.TTL(key), "同一次登记重写时续期")
}

func TestPublishNodeHolder_ComparesRevisionsAsDecimalStrings(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const node = "8792"

	// 长度不同:按字典序 "99" > "100",按数值 100 更新。
	publishHolderForTest(sc, node, "inst-99", 99)
	publishHolderForTest(sc, node, "inst-100", 100)
	assert.Equal(t, "100:inst-100", nodeHolderValueForTest(t, mr, testZoneId, node))
	publishHolderForTest(sc, node, "inst-99", 99)
	assert.Equal(t, "100:inst-100", nodeHolderValueForTest(t, mr, testZoneId, node))

	// 超过 2^53:转成双精度时两者相等,按串比较才分得清。
	const big = int64(9007199254740992)
	publishHolderForTest(sc, node, "inst-big-1", big+1)
	publishHolderForTest(sc, node, "inst-big-0", big)
	assert.Equal(t, fmt.Sprintf("%d:inst-big-1", big+1), nodeHolderValueForTest(t, mr, testZoneId, node))
}

func TestPublishNodeHolder_SkipsRecordsWithoutIdentityOrRevision(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	publishHolderForTest(sc, "8793", "", 5)
	publishHolderForTest(sc, "8794", "inst", 0)
	publishHolderForTest(sc, "", "inst", 5)
	assert.False(t, mr.Exists(nodeHolderKey(testZoneId, "8793")), "没有 node_uuid 不写")
	assert.False(t, mr.Exists(nodeHolderKey(testZoneId, "8794")), "没有修订号不写")
	assert.False(t, mr.Exists(nodeHolderKey(testZoneId, "")), "没有节点号不写")
}

func TestPublishNodeHolder_OverwritesAMalformedRecord(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const node = "8795"
	require.NoError(t, mr.Set(nodeHolderKey(testZoneId, node), "garbage"))
	publishHolderForTest(sc, node, "inst", 7)
	assert.Equal(t, "7:inst", nodeHolderValueForTest(t, mr, testZoneId, node), "损坏的记录按没有处理")
}

func holderWatchKeyForTest(node string) string {
	return fmt.Sprintf("SceneNodeService.rpc/zone/%d/holder-test/node/%s", testZoneId, node)
}

func putHolderRegistrationForTest(sc *svc.ServiceContext, node, uuid string, revision int64) {
	handleWatchEvent(context.Background(), sc, &clientv3.Event{
		Type: clientv3.EventTypePut,
		Kv: &mvccpb.KeyValue{
			Key:         []byte(holderWatchKeyForTest(node)),
			Value:       []byte(fmt.Sprintf(`{"nodeId":%s,"zoneId":%d,"sceneNodeType":1,"nodeUuid":%q}`, node, testZoneId, uuid)),
			ModRevision: revision,
		},
	})
}

// watch PUT 写持有者;DELETE 不删;同号新进程的 PUT 覆盖。跟随者的 PUT 一样写(与 clearNodeDeath 同一口径)。
func TestWatchPutPublishesTheNodeHolder(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	const node = "8796"

	putHolderRegistrationForTest(sc, node, "inst-a", 200)
	assert.Equal(t, "200:inst-a", nodeHolderValueForTest(t, mr, testZoneId, node))

	handleWatchEvent(context.Background(), sc, &clientv3.Event{
		Type: clientv3.EventTypeDelete,
		Kv:   &mvccpb.KeyValue{Key: []byte(holderWatchKeyForTest(node))},
	})
	assert.Equal(t, "200:inst-a", nodeHolderValueForTest(t, mr, testZoneId, node), "DELETE 不删持有者")

	putHolderRegistrationForTest(sc, node, "inst-b", 260)
	assert.Equal(t, "260:inst-b", nodeHolderValueForTest(t, mr, testZoneId, node), "同号新进程接走这个号")

	putHolderRegistrationForTest(sc, node, "inst-a", 200)
	assert.Equal(t, "260:inst-b", nodeHolderValueForTest(t, mr, testZoneId, node), "晚到的旧 PUT 不覆盖")
}

func holderSnapshotForTest(zone uint32) []*mvccpb.KeyValue {
	kv := func(node uint32, uuid string, revision int64) *mvccpb.KeyValue {
		return &mvccpb.KeyValue{
			Key:         []byte(fmt.Sprintf("SceneNodeService.rpc/holder-test/zone/%d/node/%d", zone, node)),
			Value:       []byte(fmt.Sprintf(`{"nodeId":%d,"zoneId":%d,"sceneNodeType":1,"nodeUuid":%q}`, node, zone, uuid)),
			ModRevision: revision,
		}
	}
	return []*mvccpb.KeyValue{kv(7301, "inst-7301", 41), kv(7302, "inst-7302", 42), kv(7303, "", 43)}
}

// 全量同步:领导者给快照里每条有 node_uuid 的登记写持有者。
func TestApplyNodeSnapshot_LeaderPublishesHoldersOfRegisteredNodes(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	useNodeViewGateForTest(t)
	const zone = uint32(73)
	sc.Config.ZoneId = zone

	_, err := applyNodeSnapshot(context.Background(), sc, holderSnapshotForTest(zone), 50)
	require.NoError(t, err)
	assert.Equal(t, "41:inst-7301", nodeHolderValueForTest(t, mr, zone, "7301"))
	assert.Equal(t, "42:inst-7302", nodeHolderValueForTest(t, mr, zone, "7302"))
	assert.Empty(t, nodeHolderValueForTest(t, mr, zone, "7303"), "没有 node_uuid 的登记不写")
}

// 全量同步的写入只在领导者上做(变更类动作单写者);跟随者只换镜像。
func TestApplyNodeSnapshot_FollowerDoesNotPublishHolders(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	useNodeViewGateForTest(t)
	const zone = uint32(74)
	sc.Config.ZoneId = zone

	_, err := applyNodeSnapshot(context.Background(), sc, holderSnapshotForTest(zone), 60)
	require.NoError(t, err)
	assert.Empty(t, nodeHolderValueForTest(t, mr, zone, "7301"))
	assert.Empty(t, nodeHolderValueForTest(t, mr, zone, "7302"))
}
