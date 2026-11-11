package logic

// 节点号持有者(缺口清单第 33 条,node_holder.go)的回归。
//
// C++ 节点据它认出「自己的号已经被新进程接走」(scene_ownership_reconciler.cpp)。这里钉住四件事:
//   - 写入按 etcd 修订号单调:乱序晚到的旧登记盖不回新持有者;修订号按十进制串比,不丢精度;
//   - 每个副本的 watch PUT 都写,DELETE 不删(节点死了,最后一次登记仍是事实);
//   - 全量同步只由领导者写,补上漏掉的 PUT 并续 TTL;没有 node_uuid 或修订号的登记不写;
//   - etcd 修订号倒退按「先读水位、后线性一致读当前修订号」判断:正常轮严格单调,不会把跟随者写进的新登记盖回旧的;
//     倒退时换纪元,倒退之前的记录(在不在快照里都一样)让位给新登记;快照读得早、还没有水位、读不到 etcd 都不误判。
// 节点号 879x、73xx 与 zone 73~79 不与别的用例重名。

import (
	"context"
	"errors"
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
	return stringValueForTest(t, mr, nodeHolderKey(zoneID, nodeID))
}

// stringValueForTest 读一个字符串键,不存在时返回空串。
func stringValueForTest(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	if !mr.Exists(key) {
		return ""
	}
	value, err := mr.Get(key)
	require.NoError(t, err)
	return value
}

func publishHolderForTest(sc *svc.ServiceContext, nodeID, uuid string, revision int64) {
	publishHolderInZoneForTest(sc, testZoneId, nodeID, uuid, revision)
}

func publishHolderInZoneForTest(sc *svc.ServiceContext, zoneID uint32, nodeID, uuid string, revision int64) {
	publishNodeHolder(sc, nodeHolderRecord{ZoneID: zoneID, NodeID: nodeID, NodeUUID: uuid, ModRevision: revision})
}

// useEtcdRevisionForTest 让全量同步读到的 etcd 当前修订号固定为 revision;err 非空时读失败。
func useEtcdRevisionForTest(t *testing.T, revision int64, err error) {
	t.Helper()
	t.Cleanup(SetCurrentEtcdRevisionForTest(func(context.Context, *svc.ServiceContext) (int64, error) {
		return revision, err
	}))
}

func etcdRevisionRegressionsForTest(t *testing.T) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_etcd_revision_regressions_total", nil)
}

func TestNodeHolderKeyShape(t *testing.T) {
	// 跨语言契约:C++ scene_ownership::NodeHolderKey 读同一个键。以 node:zone: 开头(合服按前缀清),不带 hash tag。
	assert.Equal(t, "node:zone:3:17:holder", nodeHolderKey(3, "17"))
	// 纪元旁记与持有者同一个前缀,合服时一起清掉;两个 zone 级键与死节点收尾任务表同一命名。
	assert.Equal(t, "node:zone:3:17:holder_epoch", nodeHolderEpochKey(3, "17"))
	assert.Equal(t, "scene_manager:etcd_revision_seen:zone:3", etcdRevisionSeenKey(3))
	assert.Equal(t, "scene_manager:etcd_epoch:zone:3", etcdEpochKey(3))
}

func TestPublishNodeHolder_NewerRegistrationWins(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const node = "8791"
	key := nodeHolderKey(testZoneId, node)
	epochKey := nodeHolderEpochKey(testZoneId, node)

	publishHolderForTest(sc, node, "inst-a", 100)
	assert.Equal(t, "100:inst-a", nodeHolderValueForTest(t, mr, testZoneId, node))
	assert.Equal(t, time.Duration(nodeHolderTTLSeconds)*time.Second, mr.TTL(key))
	assert.Equal(t, "0", stringValueForTest(t, mr, epochKey), "还没换过纪元时按 0 记")
	assert.Equal(t, time.Duration(nodeHolderTTLSeconds)*time.Second, mr.TTL(epochKey), "纪元旁记与持有者同写同过期")

	// 副本之间乱序到达的旧登记:不覆盖。
	publishHolderForTest(sc, node, "inst-old", 90)
	assert.Equal(t, "100:inst-a", nodeHolderValueForTest(t, mr, testZoneId, node), "更旧的登记盖不回新持有者")

	// 同号新进程登记,修订号更大:覆盖。
	publishHolderForTest(sc, node, "inst-b", 150)
	assert.Equal(t, "150:inst-b", nodeHolderValueForTest(t, mr, testZoneId, node))

	// 同一次登记再写一遍(全量同步):值不变,TTL 续上。
	mr.SetTTL(key, time.Second)
	mr.SetTTL(epochKey, time.Second)
	publishHolderForTest(sc, node, "inst-b", 150)
	assert.Equal(t, "150:inst-b", nodeHolderValueForTest(t, mr, testZoneId, node))
	assert.Equal(t, time.Duration(nodeHolderTTLSeconds)*time.Second, mr.TTL(key), "同一次登记重写时续期")
	assert.Equal(t, time.Duration(nodeHolderTTLSeconds)*time.Second, mr.TTL(epochKey), "纪元旁记一起续期")
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

// watch PUT 写持有者;DELETE 不删;同号新进程的 PUT 覆盖。跟随者的 PUT 一样写(与死亡标记的清除同一口径)。
func TestWatchPutPublishesTheNodeHolder(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	const node = "8796"
	deleteRegistration := func() {
		handleWatchEvent(context.Background(), sc, &clientv3.Event{
			Type: clientv3.EventTypeDelete,
			Kv:   &mvccpb.KeyValue{Key: []byte(holderWatchKeyForTest(node))},
		})
	}
	// 进程级镜像里不留本用例的节点(t.Cleanup 后进先出:此时仍是跟随者)。
	t.Cleanup(deleteRegistration)

	putHolderRegistrationForTest(sc, node, "inst-a", 200)
	assert.Equal(t, "200:inst-a", nodeHolderValueForTest(t, mr, testZoneId, node))

	deleteRegistration()
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

// newHolderSnapshotCtx:领导者、干净的存活集闸门、本 zone = zone。快照头修订号固定传 50(holderSnapshotForTest 里最大 43)。
func newHolderSnapshotCtx(t *testing.T, zone uint32) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	sc, mr := newLoadRefreshScopeCtx(t)
	useNodeViewGateForTest(t)
	sc.Config.ZoneId = zone
	return sc, mr
}

const holderSnapshotHeaderRevision = int64(50)

func applyHolderSnapshotForTest(t *testing.T, sc *svc.ServiceContext, zone uint32) {
	t.Helper()
	_, err := applyNodeSnapshot(context.Background(), sc, holderSnapshotForTest(zone), holderSnapshotHeaderRevision)
	require.NoError(t, err)
}

// 全量同步:领导者给快照里每条有 node_uuid 的登记写持有者,并把水位记成读到的 etcd 当前修订号。
func TestApplyNodeSnapshot_LeaderPublishesHoldersOfRegisteredNodes(t *testing.T) {
	const zone = uint32(73)
	sc, mr := newHolderSnapshotCtx(t, zone)
	useEtcdRevisionForTest(t, 60, nil)

	applyHolderSnapshotForTest(t, sc, zone)
	assert.Equal(t, "41:inst-7301", nodeHolderValueForTest(t, mr, zone, "7301"))
	assert.Equal(t, "42:inst-7302", nodeHolderValueForTest(t, mr, zone, "7302"))
	assert.Empty(t, nodeHolderValueForTest(t, mr, zone, "7303"), "没有 node_uuid 的登记不写")
	assert.Equal(t, "60", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)), "水位是读到的当前修订号,不是快照头")
}

// 正常轮严格单调:快照读完之后跟随者写进的新登记(修订号比快照头大),不被快照里同号的上一次登记盖回。
// 拿快照头修订号判断残留(比它新的记录当成换集群之前留下的、直接覆盖)的写法,这里会被盖成 "41:inst-7301"。
func TestApplyNodeSnapshot_NormalRoundKeepsANewerFollowerWrite(t *testing.T) {
	const zone = uint32(76)
	sc, mr := newHolderSnapshotCtx(t, zone)
	require.NoError(t, mr.Set(etcdRevisionSeenKey(zone), "45"))
	publishHolderInZoneForTest(sc, zone, "7301", "inst-7301-next", 55) // 跟随者的 watch PUT,晚于快照头 50
	useEtcdRevisionForTest(t, 60, nil)
	before := etcdRevisionRegressionsForTest(t)

	applyHolderSnapshotForTest(t, sc, zone)
	assert.Equal(t, "55:inst-7301-next", nodeHolderValueForTest(t, mr, zone, "7301"), "更新的登记不被快照盖回")
	assert.Equal(t, "42:inst-7302", nodeHolderValueForTest(t, mr, zone, "7302"))
	assert.Equal(t, "60", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)), "水位只升不降,抬到当前修订号")
	assert.False(t, mr.Exists(etcdEpochKey(zone)), "没有倒退就不换纪元")
	assert.Equal(t, before, etcdRevisionRegressionsForTest(t))
}

// 倒退:当前修订号小于水位。换纪元、水位降到当前修订号、计一次;快照里的号立刻换成新纪元的记录 ——
// 不论旧记录的修订号是大于当前修订号(7301),还是落在快照登记与当前修订号之间(7302,只覆盖「大于当前修订号」会漏掉它)。
// 不在快照里的号(7309)旧记录先留着,下一次登记不论修订号大小都覆盖它;覆盖之后回到同一纪元里的单调。
func TestApplyNodeSnapshot_EtcdRevisionRegressionStartsANewEpoch(t *testing.T) {
	const zone = uint32(75)
	sc, mr := newHolderSnapshotCtx(t, zone)
	require.NoError(t, mr.Set(etcdRevisionSeenKey(zone), "900000"))
	publishHolderInZoneForTest(sc, zone, "7301", "inst-before-reset", 900000)
	publishHolderInZoneForTest(sc, zone, "7302", "inst-before-reset", 45)
	publishHolderInZoneForTest(sc, zone, "7309", "inst-gone", 880000)
	useEtcdRevisionForTest(t, 60, nil)
	before := etcdRevisionRegressionsForTest(t)

	applyHolderSnapshotForTest(t, sc, zone)
	assert.Equal(t, "1", stringValueForTest(t, mr, etcdEpochKey(zone)), "换了一次纪元")
	assert.Equal(t, "60", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)), "水位降到当前修订号")
	assert.Equal(t, before+1, etcdRevisionRegressionsForTest(t))
	assert.Equal(t, "41:inst-7301", nodeHolderValueForTest(t, mr, zone, "7301"), "修订号大于当前修订号的旧记录让位")
	assert.Equal(t, "42:inst-7302", nodeHolderValueForTest(t, mr, zone, "7302"), "修订号不大于当前修订号的旧记录同样让位")
	assert.Equal(t, "1", stringValueForTest(t, mr, nodeHolderEpochKey(zone, "7301")), "快照里的号记成新纪元")
	assert.Equal(t, "880000:inst-gone", nodeHolderValueForTest(t, mr, zone, "7309"), "不在快照里的号先不动")

	publishHolderInZoneForTest(sc, zone, "7309", "inst-new", 30)
	assert.Equal(t, "30:inst-new", nodeHolderValueForTest(t, mr, zone, "7309"), "旧纪元的记录让位给新登记,不比修订号")
	publishHolderInZoneForTest(sc, zone, "7309", "inst-late", 20)
	assert.Equal(t, "30:inst-new", nodeHolderValueForTest(t, mr, zone, "7309"), "同一纪元里照旧单调")

	// 下一轮全量同步:当前修订号不小于新水位,不再计倒退。
	useEtcdRevisionForTest(t, 70, nil)
	applyHolderSnapshotForTest(t, sc, zone)
	assert.Equal(t, "1", stringValueForTest(t, mr, etcdEpochKey(zone)))
	assert.Equal(t, "70", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)))
	assert.Equal(t, before+1, etcdRevisionRegressionsForTest(t))
}

// 快照读得早的领导者(交接时的旧任,或者这一轮读得慢):快照头 50 小于别的领导者已经抬到的水位 100,
// 但倒退按读完水位之后读到的当前修订号判断(120),不误判;快照里更旧的登记也盖不掉已存的新登记。
func TestApplyNodeSnapshot_EarlySnapshotIsNotARegression(t *testing.T) {
	const zone = uint32(77)
	sc, mr := newHolderSnapshotCtx(t, zone)
	require.NoError(t, mr.Set(etcdRevisionSeenKey(zone), "100"))
	publishHolderInZoneForTest(sc, zone, "7301", "inst-7301-next", 90)
	useEtcdRevisionForTest(t, 120, nil)
	before := etcdRevisionRegressionsForTest(t)

	applyHolderSnapshotForTest(t, sc, zone)
	assert.False(t, mr.Exists(etcdEpochKey(zone)), "快照头比水位小不算倒退")
	assert.Equal(t, before, etcdRevisionRegressionsForTest(t))
	assert.Equal(t, "90:inst-7301-next", nodeHolderValueForTest(t, mr, zone, "7301"))
	assert.Equal(t, "120", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)))
}

// 还没有水位(第一次运行,或控制域丢过数据):判断不了倒退,只单调覆盖并建立水位。损坏的水位同样按没有处理、重写。
func TestApplyNodeSnapshot_WithoutAWatermarkOnlyRecordsIt(t *testing.T) {
	for name, stored := range map[string]string{"没有水位": "", "水位损坏": "garbage"} {
		t.Run(name, func(t *testing.T) {
			const zone = uint32(78)
			sc, mr := newHolderSnapshotCtx(t, zone)
			if stored != "" {
				require.NoError(t, mr.Set(etcdRevisionSeenKey(zone), stored))
			}
			publishHolderInZoneForTest(sc, zone, "7301", "inst-unknown-origin", 900000)
			useEtcdRevisionForTest(t, 60, nil)

			applyHolderSnapshotForTest(t, sc, zone)
			assert.Equal(t, "900000:inst-unknown-origin", nodeHolderValueForTest(t, mr, zone, "7301"), "没有水位时不覆盖")
			assert.Equal(t, "42:inst-7302", nodeHolderValueForTest(t, mr, zone, "7302"))
			assert.Equal(t, "60", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)), "只建立水位")
			assert.False(t, mr.Exists(etcdEpochKey(zone)))
		})
	}
}

// 读不到 etcd 当前修订号:这一轮不判断倒退,只单调覆盖,水位不动。
func TestApplyNodeSnapshot_UnreadableEtcdRevisionOnlyPublishesMonotonically(t *testing.T) {
	const zone = uint32(79)
	sc, mr := newHolderSnapshotCtx(t, zone)
	require.NoError(t, mr.Set(etcdRevisionSeenKey(zone), "900000"))
	publishHolderInZoneForTest(sc, zone, "7301", "inst-before-reset", 900000)
	useEtcdRevisionForTest(t, 0, errors.New("etcd unavailable"))

	applyHolderSnapshotForTest(t, sc, zone)
	assert.Equal(t, "900000:inst-before-reset", nodeHolderValueForTest(t, mr, zone, "7301"))
	assert.Equal(t, "42:inst-7302", nodeHolderValueForTest(t, mr, zone, "7302"))
	assert.Equal(t, "900000", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)), "水位不动")
	assert.False(t, mr.Exists(etcdEpochKey(zone)))
}

// 换纪元是比较并写:水位已被别的写者改过就不动;纪元键损坏时脚本在写入之前报错,水位不会单独降下去。
func TestStartNewEtcdEpoch_ComparesAndSets(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const zone = uint32(73)
	require.NoError(t, mr.Set(etcdRevisionSeenKey(zone), "500"))

	assert.Zero(t, startNewEtcdEpoch(sc, zone, "400", 400, 60), "水位已经不是读到的值")
	assert.Equal(t, "500", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)))
	assert.False(t, mr.Exists(etcdEpochKey(zone)))

	assert.Equal(t, int64(1), startNewEtcdEpoch(sc, zone, "500", 500, 60))
	assert.Equal(t, "60", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)))
	assert.Equal(t, "1", stringValueForTest(t, mr, etcdEpochKey(zone)))

	require.NoError(t, mr.Set(etcdRevisionSeenKey(zone), "800"))
	require.NoError(t, mr.Set(etcdEpochKey(zone), "garbage"))
	assert.Zero(t, startNewEtcdEpoch(sc, zone, "800", 800, 60), "纪元键损坏")
	assert.Equal(t, "800", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)), "纪元没换成,水位不降")
}

// 换纪元前再确认一次领导权:已经不是领导者的副本即使读到倒退也不换纪元(新任领导者的全量同步会重新判断)。
func TestPublishSnapshotHolders_NonLeaderDoesNotStartANewEpoch(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	const zone = uint32(74)
	require.NoError(t, mr.Set(etcdRevisionSeenKey(zone), "900000"))
	useEtcdRevisionForTest(t, 60, nil)

	publishSnapshotHolders(context.Background(), sc, zone, []nodeHolderRecord{
		{ZoneID: zone, NodeID: "7301", NodeUUID: "inst-7301", ModRevision: 41},
	})
	assert.False(t, mr.Exists(etcdEpochKey(zone)))
	assert.Equal(t, "900000", stringValueForTest(t, mr, etcdRevisionSeenKey(zone)))
}

// 全量同步的写入只在领导者上做(变更类动作单写者);跟随者只换镜像,持有者与水位都不写。
func TestApplyNodeSnapshot_FollowerDoesNotPublishHolders(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	useNodeViewGateForTest(t)
	const zone = uint32(74)
	sc.Config.ZoneId = zone
	useEtcdRevisionForTest(t, 60, nil)

	_, err := applyNodeSnapshot(context.Background(), sc, holderSnapshotForTest(zone), 60)
	require.NoError(t, err)
	assert.Empty(t, nodeHolderValueForTest(t, mr, zone, "7301"))
	assert.Empty(t, nodeHolderValueForTest(t, mr, zone, "7302"))
	assert.False(t, mr.Exists(etcdRevisionSeenKey(zone)))
}
