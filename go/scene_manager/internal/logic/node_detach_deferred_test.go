package logic

// GO-6:death_at 没落地,就不把死节点摘出负载集(reentry_barrier.go「推迟的摘除」一段)。
//
// 这些用例钉住三件事:
//   - 写 death_at 失败 / 摘负载集失败时节点留在负载集(IsNodeAlive 为真),本领导者的
//     CanReclaimDeadNode 对它恒拒,推迟队列每拍重试,且有截止时间(一个屏障 / 10 分钟);
//   - 摘除一定按「先 SET death_at、后 ZREM 负载集」的顺序,满一个屏障后才允许不带标记摘;
//   - 推迟期间所有改派 / 接管 / 孤儿清理点都拒绝(三层保证),队列丢了(换领导者)也拒绝。

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
)

// nodeDetachFaultForTest 按命令注入「写 death_at 失败」与「摘负载集 ZREM 失败」,并按到达顺序
// 记下被放行的这两类写,用来断言「先 SET death_at、后 ZREM」。
type nodeDetachFaultForTest struct {
	failDeathAt  atomic.Bool
	failLoadZrem atomic.Bool

	mu  sync.Mutex
	ops []string // 放行的 "SET <death_at 键>" / "ZREM <负载集键>"
}

func (f *nodeDetachFaultForTest) record(op string) {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	f.mu.Unlock()
}

func (f *nodeDetachFaultForTest) passedOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// installNodeDetachFaultsForTest 给 miniredis 装上故障钩子。两条约束:
//   - 钩子会被 Lua 里的 redis.call 带锁重入(外层 EVAL 已持有 miniredis 的锁,见 withTx 的 nested
//     分支),所以钩子里只读写 f 自身的状态,**绝不调 mr.***,否则死锁;
//   - 每个 svcCtx 注入的失败控制在 5 次以内:go-zero 的 Redis 客户端自带 Google SRE 熔断
//     (protection=5),失败再多会被本地熔断拒绝,用例就不再测它想测的东西。
//
// miniredis 的 SetError 与 SetPreHook 共用同一个钩子槽位,装了本钩子的用例不要再用 SetError。
func installNodeDetachFaultsForTest(t *testing.T, mr *miniredis.Miniredis) *nodeDetachFaultForTest {
	t.Helper()
	f := &nodeDetachFaultForTest{}
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if len(args) == 0 {
			return false
		}
		key := args[0]
		switch {
		case cmd == "SET" && strings.HasSuffix(key, ":death_at"):
			if f.failDeathAt.Load() {
				c.WriteError("ERR injected death_at write failure")
				return true
			}
			f.record("SET " + key)
		case cmd == "ZREM" && strings.HasSuffix(key, ":load"):
			if f.failLoadZrem.Load() {
				c.WriteError("ERR injected load-set ZREM failure")
				return true
			}
			f.record("ZREM " + key)
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	return f
}

// resetNodeDeathQueuesForTest 在用例前后都清空推迟摘除队列与死节点收尾队列(两者都是包级全局表)。
func resetNodeDeathQueuesForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		deferredNodeDetachMu.Lock()
		deferredNodeDetaches = make(map[string]deferredNodeDetach)
		deferredNodeDetachMu.Unlock()
		pendingDeadNodesMu.Lock()
		pendingDeadNodes = make(map[string]deadNodeReconcile)
		pendingDeadNodesMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// setDeferredNodeDetachFirstObservedAtForTest 把推迟任务的首次观察时刻改到 at。
// map 的值是结构体,不能直接改字段,只能持锁读-改-写。
func setDeferredNodeDetachFirstObservedAtForTest(t *testing.T, zoneID uint32, nodeID string, at time.Time) {
	t.Helper()
	key := pendingDeadNodeKey(zoneID, nodeID)
	deferredNodeDetachMu.Lock()
	task, ok := deferredNodeDetaches[key]
	if ok {
		task.firstObservedAt = at
		deferredNodeDetaches[key] = task
	}
	deferredNodeDetachMu.Unlock()
	require.True(t, ok, "前提不成立:zone=%d node=%s 不在推迟队列里", zoneID, nodeID)
}

func deadNodeEntryForTest(zoneID uint32, nodeID string) nodeEntry {
	entry := nodeEntry{nodeID: nodeID}
	entry.reg.ZoneId = zoneID
	return entry
}

func pendingDeadNodeForTest(zoneID uint32, nodeID string) (deadNodeReconcile, bool) {
	pendingDeadNodesMu.Lock()
	defer pendingDeadNodesMu.Unlock()
	task, ok := pendingDeadNodes[pendingDeadNodeKey(zoneID, nodeID)]
	return task, ok
}

// loadSetMembersForTest 返回负载集成员。**不能**用 mr.ZScore 判断成员资格:键存在、成员不存在时它
// 返回 (0, nil)(见 world_init_unreachable_test.go 的说明)。集合被摘空后键会消失,按空集处理。
func loadSetMembersForTest(t *testing.T, mr *miniredis.Miniredis, zoneID uint32) []string {
	t.Helper()
	members, err := mr.ZMembers(nodeLoadKey(zoneID))
	if errors.Is(err, miniredis.ErrKeyNotFound) {
		return nil
	}
	require.NoError(t, err)
	return members
}

// counterValueForTest 从默认注册表读一个计数器取值(写法同 owner_epoch_test.go 的
// enterSceneRejectedCount)。labels 是要匹配的标签子集;还没发射过的组合读作 0,用例只比较前后差值。
func counterValueForTest(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			got := make(map[string]string, len(metric.GetLabel()))
			for _, pair := range metric.GetLabel() {
				got[pair.GetName()] = pair.GetValue()
			}
			matched := true
			for k, v := range labels {
				if got[k] != v {
					matched = false
					break
				}
			}
			if matched {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func detachOutcomeCountForTest(t *testing.T, zoneID uint32, outcome string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_node_detach_deferred_total", map[string]string{
		"zone_id": strconv.FormatUint(uint64(zoneID), 10), "outcome": outcome,
	})
}

func barrierBlockedCountForTest(t *testing.T, zoneID uint32, site string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_reentry_barrier_blocked_total", map[string]string{
		"zone_id": strconv.FormatUint(uint64(zoneID), 10), "site": site,
	})
}

// newNodeDetachTestCtx:干净的注册表镜像与两张队列 + 自己的 miniredis(熔断计数按 svcCtx 隔离)+ 故障钩子。
func newNodeDetachTestCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, *nodeDetachFaultForTest) {
	t.Helper()
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	sc, mr := newTestSvcCtx(t, "")
	return sc, mr, installNodeDetachFaultsForTest(t, mr)
}

// seedDeadNodeForDetachTest:zone1 负载集放 10 和 20;10 有类型镜像,名下场景为 {7001}。
func seedDeadNodeForDetachTest(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()
	_, err := mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
	require.NoError(t, err)
	require.NoError(t, mr.Set(nodeSceneNodeTypeKey(testZoneId, "10"), "1"))
	_, err = mr.SAdd(nodeScenesKey(testZoneId, "10"), "7001")
	require.NoError(t, err)
}

// deferNode10ForTest:按「death_at 写失败」让节点 10 进推迟队列(注入 1 次失败,故障保持打开)。
func deferNode10ForTest(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, faults *nodeDetachFaultForTest) {
	t.Helper()
	seedDeadNodeForDetachTest(t, mr)
	faults.failDeathAt.Store(true)
	removeNodeFromRedis(sc, deadNodeEntryForTest(testZoneId, "10"))
	require.True(t, isNodeDetachDeferred(testZoneId, "10"), "前提不成立:节点 10 没进推迟队列")
}

// death_at 写失败:节点留在负载集、类型镜像保留、不入收尾队列,本领导者的 CanReclaimDeadNode 恒拒。
func TestRemoveNodeFromRedis_DeathMarkFailureKeepsNodeInLoadSet(t *testing.T) {
	sc, mr, faults := newNodeDetachTestCtx(t)
	seedDeadNodeForDetachTest(t, mr)
	faults.failDeathAt.Store(true)
	deferredBefore := detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeDeferred)

	removeNodeFromRedis(sc, deadNodeEntryForTest(testZoneId, "10"))

	assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10",
		"death_at 没落地就摘负载集,读者会看到「不在负载集 + 没有 death_at ⇒ 屏障已过」")
	assert.False(t, mr.Exists(nodeDeathAtKey(testZoneId, "10")))
	assert.True(t, mr.Exists(nodeSceneNodeTypeKey(testZoneId, "10")),
		"类型镜像必须保留:没有镜像的节点会被当作未分类放进所有用途的池子")
	assert.True(t, IsNodeAlive(sc, testZoneId, "10"))
	allowed, _ := CanReclaimDeadNode(sc, testZoneId, "10")
	assert.False(t, allowed, "推迟态:本领导者的 CanReclaimDeadNode 必须恒拒")

	_, pending := pendingDeadNodeForTest(testZoneId, "10")
	assert.False(t, pending, "节点还在负载集里,不能提前入收尾队列")
	task, ok := lookupDeferredNodeDetach(testZoneId, "10")
	require.True(t, ok)
	assert.True(t, task.scenesCaptured)
	assert.Equal(t, []string{"7001"}, task.scenes)

	assert.NotContains(t, faults.passedOps(), "ZREM "+nodeLoadKey(testZoneId))
	assert.Equal(t, deferredBefore+1, detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeDeferred))
}

// 故障解除后的重试:先写 death_at 再摘负载集;收尾用的是判死时刻的快照,不是现读。
func TestRetryDeferredNodeDetaches_WritesDeathMarkBeforeLeavingLoadSet(t *testing.T) {
	sc, mr, faults := newNodeDetachTestCtx(t)
	deferNode10ForTest(t, sc, mr, faults)
	// 推迟期间同一 node_id 上的新写入:不属于这次死亡的收尾范围。
	_, err := mr.SAdd(nodeScenesKey(testZoneId, "10"), "7002")
	require.NoError(t, err)
	recoveredBefore := detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeRecovered)

	assert.Equal(t, 0, retryDeferredNodeDetaches(sc), "故障还在、屏障未满:不摘")
	assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10")

	faults.failDeathAt.Store(false)
	assert.Equal(t, 1, retryDeferredNodeDetaches(sc))

	ops := faults.passedOps()
	markIdx := slices.Index(ops, "SET "+nodeDeathAtKey(testZoneId, "10"))
	zremIdx := slices.Index(ops, "ZREM "+nodeLoadKey(testZoneId))
	require.GreaterOrEqual(t, markIdx, 0, "没看到 SET death_at: %v", ops)
	require.GreaterOrEqual(t, zremIdx, 0, "没看到 ZREM 负载集: %v", ops)
	assert.Less(t, markIdx, zremIdx, "必须先写 death_at 再摘负载集: %v", ops)

	raw, err := mr.Get(nodeDeathAtKey(testZoneId, "10"))
	require.NoError(t, err)
	deathMs, err := strconv.ParseInt(raw, 10, 64)
	require.NoError(t, err)
	assert.LessOrEqual(t, deathMs, time.Now().UnixMilli())

	assert.NotContains(t, loadSetMembersForTest(t, mr, testZoneId), "10")
	assert.False(t, mr.Exists(nodeSceneNodeTypeKey(testZoneId, "10")))
	reconcile, ok := pendingDeadNodeForTest(testZoneId, "10")
	require.True(t, ok)
	assert.Equal(t, []string{"7001"}, reconcile.scenes, "收尾范围必须是判死时刻的快照")
	assert.False(t, isNodeDetachDeferred(testZoneId, "10"))
	allowed, _ := CanReclaimDeadNode(sc, testZoneId, "10")
	assert.False(t, allowed, "屏障从刚写入的 death_at 起算")
	assert.Equal(t, recoveredBefore+1, detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeRecovered))
}

// death_at 一直写不进:未满一个屏障不摘;满了才不带标记摘(记 expired)。整个用例共注入 3 次失败。
func TestRetryDeferredNodeDetaches_DetachesWithoutMarkOnlyAfterBarrier(t *testing.T) {
	sc, mr, faults := newNodeDetachTestCtx(t)
	deferNode10ForTest(t, sc, mr, faults)
	expiredBefore := detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeExpired)

	assert.Equal(t, 0, retryDeferredNodeDetaches(sc))
	assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10", "未满一个屏障,写不进就不摘")

	setDeferredNodeDetachFirstObservedAtForTest(t, testZoneId, "10", time.Now().Add(-2*sceneReentryBarrier(sc)))
	assert.Equal(t, 1, retryDeferredNodeDetaches(sc))

	assert.NotContains(t, loadSetMembersForTest(t, mr, testZoneId), "10")
	assert.False(t, mr.Exists(nodeDeathAtKey(testZoneId, "10")))
	reconcile, ok := pendingDeadNodeForTest(testZoneId, "10")
	require.True(t, ok)
	assert.Equal(t, []string{"7001"}, reconcile.scenes)
	assert.False(t, isNodeDetachDeferred(testZoneId, "10"))
	assert.Equal(t, expiredBefore+1, detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeExpired))
	allowed, _ := CanReclaimDeadNode(sc, testZoneId, "10")
	assert.True(t, allowed, "推迟已兜满一个屏障:不带标记摘除后等价于「屏障已过」")
}

// death_at 已写、摘负载集失败:同样进推迟队列,不再等 fullSync;10 分钟仍摘不出就放弃。
func TestRetryDeferredNodeDetaches_KeepsTaskWhileLoadSetRemovalFails(t *testing.T) {
	t.Run("recovers", func(t *testing.T) {
		sc, mr, faults := newNodeDetachTestCtx(t)
		seedDeadNodeForDetachTest(t, mr)
		faults.failLoadZrem.Store(true)
		recoveredBefore := detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeRecovered)

		removeNodeFromRedis(sc, deadNodeEntryForTest(testZoneId, "10"))

		assert.True(t, mr.Exists(nodeDeathAtKey(testZoneId, "10")))
		assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10")
		assert.True(t, mr.Exists(nodeSceneNodeTypeKey(testZoneId, "10")), "ZREM 失败时类型镜像也不删")
		_, pending := pendingDeadNodeForTest(testZoneId, "10")
		assert.False(t, pending, "ZREM 失败时不入收尾队列")
		assert.True(t, isNodeDetachDeferred(testZoneId, "10"))
		allowed, _ := CanReclaimDeadNode(sc, testZoneId, "10")
		assert.False(t, allowed)

		faults.failLoadZrem.Store(false)
		assert.Equal(t, 1, retryDeferredNodeDetaches(sc))
		assert.NotContains(t, loadSetMembersForTest(t, mr, testZoneId), "10")
		_, pending = pendingDeadNodeForTest(testZoneId, "10")
		assert.True(t, pending)
		assert.False(t, isNodeDetachDeferred(testZoneId, "10"))
		assert.Equal(t, recoveredBefore+1, detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeRecovered))
	})

	t.Run("abandons_after_give_up", func(t *testing.T) {
		sc, mr, faults := newNodeDetachTestCtx(t)
		seedDeadNodeForDetachTest(t, mr)
		faults.failLoadZrem.Store(true)
		abandonedBefore := detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeAbandoned)

		removeNodeFromRedis(sc, deadNodeEntryForTest(testZoneId, "10"))
		setDeferredNodeDetachFirstObservedAtForTest(t, testZoneId, "10",
			time.Now().Add(-(deferredNodeDetachGiveUpAfter + time.Second)))

		assert.Equal(t, 0, retryDeferredNodeDetaches(sc))
		assert.False(t, isNodeDetachDeferred(testZoneId, "10"), "过了放弃期限就出队,不无限重试")
		assert.Equal(t, abandonedBefore+1, detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeAbandoned))
		assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10", "放弃后节点留在负载集按存活处理")
		assert.True(t, mr.Exists(nodeSceneNodeTypeKey(testZoneId, "10")))
		_, pending := pendingDeadNodeForTest(testZoneId, "10")
		assert.False(t, pending)
	})
}

// 推迟期间节点重新注册 / 本副本失去领导权:任务丢弃(dropped),都不写 death_at、不摘负载集。
func TestRetryDeferredNodeDetaches_DropsTaskOnReregistrationOrLostLeadership(t *testing.T) {
	t.Run("node_reregistered", func(t *testing.T) {
		sc, mr, faults := newNodeDetachTestCtx(t)
		deferNode10ForTest(t, sc, mr, faults)
		faults.failDeathAt.Store(false)
		droppedBefore := detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeDropped)
		// 模拟漏掉了 PUT:重试时才在注册表镜像里看到它。
		registerKnownNodeForTest(t, "SceneNodeService.rpc/rereg-10", testZoneId, "10")

		assert.Equal(t, 0, retryDeferredNodeDetaches(sc))
		assert.False(t, isNodeDetachDeferred(testZoneId, "10"))
		assert.Equal(t, droppedBefore+1, detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeDropped))
		assert.False(t, mr.Exists(nodeDeathAtKey(testZoneId, "10")), "不能给活节点打死亡标记")
		assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10")
		reconcile, ok := pendingDeadNodeForTest(testZoneId, "10")
		require.True(t, ok, "旧化身的快照要交给收尾队列")
		assert.Equal(t, []string{"7001"}, reconcile.scenes)
	})

	t.Run("not_leader", func(t *testing.T) {
		sc, mr, faults := newNodeDetachTestCtx(t)
		deferNode10ForTest(t, sc, mr, faults)
		faults.failDeathAt.Store(false)
		droppedBefore := detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeDropped)
		t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))

		assert.Equal(t, 0, retryDeferredNodeDetaches(sc))
		assert.False(t, isNodeDetachDeferred(testZoneId, "10"))
		assert.Equal(t, droppedBefore+1, detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeDropped))
		assert.False(t, mr.Exists(nodeDeathAtKey(testZoneId, "10")), "跟随者不碰 Redis")
		assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10", "留给新领导者的 fullSync 重扫")
		_, pending := pendingDeadNodeForTest(testZoneId, "10")
		assert.False(t, pending)
	})
}

// watch PUT(节点重新注册)取消推迟摘除,并把判死时刻快照交给收尾队列。
func TestHandleWatchPut_CancelsDeferredNodeDetachAndHandsOverSnapshot(t *testing.T) {
	// 空的 World 表:PUT 分支不去补频道 / rebalance,只测注册与推迟队列的交互。
	t.Cleanup(SetWorldConfIdsForTest([]uint64{}))
	sc, mr, faults := newNodeDetachTestCtx(t)
	deferNode10ForTest(t, sc, mr, faults)
	faults.failDeathAt.Store(false)
	droppedBefore := detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeDropped)

	handleWatchEvent(context.Background(), sc, &clientv3.Event{
		Type: clientv3.EventTypePut,
		Kv: &mvccpb.KeyValue{
			Key:   []byte("SceneNodeService.rpc/put-10"),
			Value: []byte(`{"nodeId":10,"zoneId":1,"sceneNodeType":1}`),
		},
	})

	assert.False(t, isNodeDetachDeferred(testZoneId, "10"))
	assert.Equal(t, droppedBefore+1, detachOutcomeCountForTest(t, testZoneId, metrics.NodeDetachOutcomeDropped))
	reconcile, ok := pendingDeadNodeForTest(testZoneId, "10")
	require.True(t, ok)
	assert.Equal(t, []string{"7001"}, reconcile.scenes)
	assert.False(t, mr.Exists(nodeDeathAtKey(testZoneId, "10")))
	assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10")
}

// fullSync 的陈旧清扫与 watch DELETE 走同一条摘除路径:写不进就推迟,不 ZREM。
func TestSweepStaleLoadSetMembers_DefersInsteadOfZremOnDeathMarkFailure(t *testing.T) {
	const otherZone = uint32(3)
	sc, mr, faults := newNodeDetachTestCtx(t)
	for zone, nodes := range map[uint32][]string{testZoneId: {"10", "20"}, otherZone: {"30"}} {
		for _, n := range nodes {
			_, err := mr.ZAdd(nodeLoadKey(zone), 0, n)
			require.NoError(t, err)
		}
	}
	seen := map[uint32]map[string]struct{}{testZoneId: {"20": {}}}
	faults.failDeathAt.Store(true)

	require.True(t, sweepStaleLoadSetMembers(sc, seen, []uint32{testZoneId}))
	assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "10")
	assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "20")
	assert.Contains(t, loadSetMembersForTest(t, mr, otherZone), "30", "快照外的 zone 同样按推迟处理")
	assert.True(t, isNodeDetachDeferred(testZoneId, "10"))
	assert.True(t, isNodeDetachDeferred(otherZone, "30"))
	assert.False(t, isNodeDetachDeferred(testZoneId, "20"))
	assert.False(t, mr.Exists(nodeDeathAtKey(testZoneId, "20")), "快照里的活节点不能被碰")

	faults.failDeathAt.Store(false)
	assert.Equal(t, 2, retryDeferredNodeDetaches(sc))
	assert.True(t, mr.Exists(nodeDeathAtKey(testZoneId, "10")))
	assert.True(t, mr.Exists(nodeDeathAtKey(otherZone, "30")))
	assert.NotContains(t, loadSetMembersForTest(t, mr, testZoneId), "10")
	assert.NotContains(t, loadSetMembersForTest(t, mr, otherZone), "30")
	assert.Contains(t, loadSetMembersForTest(t, mr, testZoneId), "20")

	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	assert.False(t, sweepStaleLoadSetMembers(sc, seen, []uint32{testZoneId}), "中途失去领导权必须报告给调用方")
	assert.False(t, mr.Exists(nodeDeathAtKey(testZoneId, "20")))
}

// GO-6 的主场景:进程首次 fullSync,本副本没有「看到它消失」的本地记录,只剩 Redis 这一份证据。
// death_at 写不进时旧代码照样摘负载集,playerLocationOwnerDead 看到「不在负载集 + 没有 death_at」就放行接管。
func TestPlayerLocationOwnerDead_DeathMarkFailureWithoutLocalObservationDoesNotAllowTakeover(t *testing.T) {
	sc, mr, faults := newNodeDetachTestCtx(t)
	markKnownNodesSyncedForTest(t)
	forgetLocalObservation := func() {
		nodeGoneObservedMu.Lock()
		delete(nodeGoneObservedAt, nodeGoneObservedKey(testZoneId, "10"))
		nodeGoneObservedMu.Unlock()
	}
	forgetLocalObservation()
	t.Cleanup(forgetLocalObservation)
	_, err := mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
	require.NoError(t, err)
	faults.failDeathAt.Store(true)
	removeNodeFromRedis(sc, deadNodeEntryForTest(testZoneId, "10"))

	loc := &scene_manager.PlayerLocation{SceneId: 7140, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1}
	logic := NewEnterSceneLogic(context.Background(), sc)
	assert.False(t, logic.playerLocationOwnerDead(loc, testZoneId), "death_at 没落地:节点留在负载集,不得接管")

	faults.failDeathAt.Store(false)
	require.Equal(t, 1, retryDeferredNodeDetaches(sc))
	assert.False(t, logic.playerLocationOwnerDead(loc, testZoneId), "death_at 刚写入,屏障压着")

	mr.Del(nodeDeathAtKey(testZoneId, "10"))
	assert.True(t, logic.playerLocationOwnerDead(loc, testZoneId), "屏障过后照常判死,不能永久挡住")
}

// 第 1 层:只看 death_at 的 reassignSceneNode 在推迟态也必须拒绝。
func TestReassignSceneNode_BlockedWhileDeathMarkDeferred(t *testing.T) {
	sc, mr, faults := newNodeDetachTestCtx(t)
	markKnownNodesSyncedForTest(t)
	_, err := mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
	require.NoError(t, err)
	faults.failDeathAt.Store(true)
	removeNodeFromRedis(sc, deadNodeEntryForTest(testZoneId, "10"))
	require.NoError(t, mr.Set(sceneNodeKey(9001), "10"))
	blockedBefore := barrierBlockedCountForTest(t, testZoneId, barrierSiteReassign)

	assert.False(t, reassignSceneNode(sc, testZoneId, 9001, "10", "20"))
	owner, err := mr.Get(sceneNodeKey(9001))
	require.NoError(t, err)
	assert.Equal(t, "10", owner)
	assert.Equal(t, blockedBefore+1, barrierBlockedCountForTest(t, testZoneId, barrierSiteReassign))
}

// 第 1、2 层:world_init 对「注册表没了但还在负载集」的节点不改派;摘除后由 death_at 屏障接着压;
// 屏障过后照常自愈。queue_lost_after_leader_change 里推迟队列被清空(另一副本 / 新领导者手里没有它),
// 只剩第 2 层(!IsNodeAlive)在起作用。
func TestInitWorldScenes_DeadNodeStillInLoadSetKeepsOwnership(t *testing.T) {
	setup := func(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, *nodeDetachFaultForTest, map[uint64]string) {
		t.Helper()
		clearKnownNodesForTest()
		t.Cleanup(clearKnownNodesForTest)
		resetNodeDeathQueuesForTest(t)
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		faults := installNodeDetachFaultsForTest(t, mr)
		before := seedWorldOnTwoRegisteredNodes(t, context.Background(), sc, mr)

		knownNodesMu.Lock()
		delete(knownNodes, "world-init-test/"+worldInitDownNode)
		knownNodesMu.Unlock()
		faults.failDeathAt.Store(true)
		removeNodeFromRedis(sc, deadNodeEntryForTest(testZoneId, worldInitDownNode))
		require.True(t, isNodeDetachDeferred(testZoneId, worldInitDownNode))
		sceneNodeDownForTest(t, testZoneId, worldInitDownNode)
		return sc, mr, faults, before
	}

	t.Run("deferred_on_this_leader", func(t *testing.T) {
		sc, mr, faults, before := setup(t)
		ctx := context.Background()

		initWorldScenesForZone(ctx, sc, testZoneId, worldInitTestConfIDs, true)
		assert.Equal(t, before, worldSceneOwnersForTest(t, ctx, sc, mr),
			"注册表没了但还在负载集(death_at 没落地):不得改派")

		faults.failDeathAt.Store(false)
		require.Equal(t, 1, retryDeferredNodeDetaches(sc))
		require.True(t, mr.Exists(nodeDeathAtKey(testZoneId, worldInitDownNode)))
		blockedBefore := barrierBlockedCountForTest(t, testZoneId, barrierSiteReassign)
		initWorldScenesForZone(ctx, sc, testZoneId, worldInitTestConfIDs, true)
		assert.Equal(t, before, worldSceneOwnersForTest(t, ctx, sc, mr), "摘除后 death_at 屏障接着压")
		assert.Greater(t, barrierBlockedCountForTest(t, testZoneId, barrierSiteReassign), blockedBefore)

		mr.Del(nodeDeathAtKey(testZoneId, worldInitDownNode))
		initWorldScenesForZone(ctx, sc, testZoneId, worldInitTestConfIDs, true)
		after := worldSceneOwnersForTest(t, ctx, sc, mr)
		require.Len(t, after, len(before))
		for sceneID, oldNode := range before {
			assert.Equal(t, worldInitLiveNode, after[sceneID],
				"scene %d 原属主 %s:屏障过后死节点的频道必须改派到活节点(能自愈)", sceneID, oldNode)
		}
	})

	t.Run("queue_lost_after_leader_change", func(t *testing.T) {
		sc, mr, _, before := setup(t)
		ctx := context.Background()
		resetNodeDeathQueuesForTest(t)
		require.False(t, isNodeDetachDeferred(testZoneId, worldInitDownNode))

		initWorldScenesForZone(ctx, sc, testZoneId, worldInitTestConfIDs, true)
		assert.Equal(t, before, worldSceneOwnersForTest(t, ctx, sc, mr),
			"手里没有推迟队列的副本:只凭负载集成员资格也不得改派")
	})
}

// 孤儿清理对推迟态节点整组跳过(它在负载集里、IsNodeAlive 为真,只看 !IsNodeAlive 会整组直接删)。
func TestOrphanCleanup_SkipsGroupWhileOwnerDetachDeferred(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	sc, mr := newAutoscaleCtx(t)
	faults := installNodeDetachFaultsForTest(t, mr)
	ctx := context.Background()
	_, err := sc.Redis.Zadd(testLoadKey(), 0, "10")
	require.NoError(t, err)
	const orphanConfID = uint64(777) // 不在 World 表里(newAutoscaleCtx 只登记了 autoscaleTestConfID)
	seedChannels(t, sc, testZoneId, orphanConfID, map[uint64]int64{701: 0})
	faults.failDeathAt.Store(true)
	removeNodeFromRedis(sc, deadNodeEntryForTest(testZoneId, "10"))
	require.True(t, isNodeDetachDeferred(testZoneId, "10"))
	blockedBefore := barrierBlockedCountForTest(t, testZoneId, barrierSiteOrphanCleanup)

	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc))
	owner, err := mr.Get(sceneNodeKey(701))
	require.NoError(t, err)
	assert.Equal(t, "10", owner)
	assert.True(t, mr.Exists(worldChannelsKey(testZoneId, orphanConfID)))
	assert.Equal(t, blockedBefore+1, barrierBlockedCountForTest(t, testZoneId, barrierSiteOrphanCleanup))

	faults.failDeathAt.Store(false)
	require.Equal(t, 1, retryDeferredNodeDetaches(sc))
	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc), "摘除后 death_at 屏障接着压")

	mr.Del(nodeDeathAtKey(testZoneId, "10"))
	assert.Equal(t, 1, CleanupOrphanWorldChannels(ctx, sc), "屏障过后照常清理")
}
