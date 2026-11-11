package logic

// 回归(缺口清单第 32 条,2026-11-05):节点失租后同一进程按原 node_id 重新注册,死节点收尾不得拆它的实例。
//
// C++ 节点 keepalive 回 TTL=0 或本地租约超时后只是重新拿租约、重新注册(etcd_service.cpp OnKeepAliveResponse),
// 进程和实例都还在。修复前:watch DELETE 判死入队,重新注册的 PUT 清掉 death_at,屏障随即放行,下一拍 drain 按判死时刻的
// 快照把这些实例强制销毁,节点已回到负载集,还会收到 DestroyScene。修复后收尾任务记下判死那一代的 node_uuid,
// 同一代回来就撤任务(reentry_barrier.go R1-I5)。
//
// 2026-11-10 起(infra-reconnect-overview.md §4.1 R15),只有判死的那一代回来才清 death_at;换了一代或认不出是哪一代时
// death_at 留着,老实例的再入屏障照常走完。所以这几种情形下,重新注册之后先断言屏障压着、实例原样,再用 elapseBarrier
// 推过屏障,断言照常收尾。
//
// 判死与重新注册都从 watch 事件进(handleWatchEvent),与生产同一条路径;实例、计数的布置与断言复用
// dead_node_reconcile_takeover_test.go 的 takeoverCase。节点号 8771~8778、场景号 877xx 不与别的用例重名,
// 本文件内也各用各的(进程内还留着「本副本看到它消失」的记录)。
//
// 修复前红的取法用变异,不回退文件:在副本里删掉 drainPendingDeadNodeReconciles 里撤任务的那一段(cancelDeadNodeReconcileIfInstanceBack
// 的 switch),SameInstanceKeepsItsInstances 与 SweepRecordsTheInstanceFromTheMirror 红在 requireIntact。
// 完整的变异清单与命令见 docs/PROGRESS.md 2026-11-05「死节点收尾认出失租后回来的同一进程」。
//
// 2026-11-11 R15 残余(infra-reconnect-overview.md §4.1 R15 一行)的四条用例在文件末尾,节点号 8779~8782。红的取法同样用变异,
// 每次只做一处:
//   - 变异 J:去掉 clearNodeDeathForInstance 里的 `|| !isLeader()`(跟随者照旧清 death_at),FollowerSeeingTheReturn 红;
//   - 变异 K:luaAtomicDestroyOrphanOfDeadNode 去掉 luaSceneClaimDeadNodeTaskCheck(认领不核对任务),ReturnMidSweep 与
//     ClaimChecksTheCleanupTask 红;
//   - 变异 L:luaFinishDeadNodeReconcile 只看成员在不在、不比入队时刻(退回改前),StaleFinishLeavesANewerDeathAlone 红。

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
)

// reregisterNodeKey 是用例里节点的 etcd 注册键。handleWatchEvent 只按键认人,键名本身不参与判断。
func reregisterNodeKey(node string) string {
	return fmt.Sprintf("SceneNodeService.rpc/zone/%d/reregister-test/node/%s", takeoverZone, node)
}

// putNodeRegistrationForTest 走一次 watch PUT:节点以 instance 这个 node_uuid 注册(或重新注册)。
func putNodeRegistrationForTest(t *testing.T, sc *svc.ServiceContext, node, instance string) {
	t.Helper()
	nodeID, err := strconv.ParseUint(node, 10, 32)
	require.NoError(t, err)
	handleWatchEvent(context.Background(), sc, &clientv3.Event{
		Type: clientv3.EventTypePut,
		Kv: &mvccpb.KeyValue{
			Key:   []byte(reregisterNodeKey(node)),
			Value: []byte(fmt.Sprintf(`{"nodeId":%d,"zoneId":%d,"sceneNodeType":1,"nodeUuid":%q}`, nodeID, takeoverZone, instance)),
		},
	})
	require.Contains(t, registeredInstancesOf(takeoverZone, node), instance, "前提:注册已进镜像")
}

// deleteNodeRegistrationForTest 走一次 watch DELETE:租约到期,领导者判死、写 death_at、摘负载集、入收尾队列。
func deleteNodeRegistrationForTest(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, node string) {
	t.Helper()
	handleWatchEvent(context.Background(), sc, &clientv3.Event{
		Type: clientv3.EventTypeDelete,
		Kv:   &mvccpb.KeyValue{Key: []byte(reregisterNodeKey(node))},
	})
	require.Empty(t, registeredInstancesOf(takeoverZone, node), "前提:注册已从镜像里删掉")
	require.True(t, mr.Exists(nodeDeathAtKey(takeoverZone, node)), "前提:death_at 已写")
}

func deadNodeReconcileCountForTest(t *testing.T, outcome string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_dead_node_reconcile_total", map[string]string{
		"zone_id": strconv.FormatUint(uint64(takeoverZone), 10), "outcome": outcome,
	})
}

// reregisterWhileFollowerForTest:本进程还不是领导者时收到这次注册(领导者缺位、刚换主)。PUT 分支只写镜像,不撤任务、
// 不清 death_at;当上领导者之后,由收尾按镜像认出它(cancelDeadNodeReconcileIfInstanceBack)。领导者自己收到注册时在
// PUT 分支当场就撤,见文件末尾的 FollowerSeeingTheReturnLeavesTheBarrier。
func reregisterWhileFollowerForTest(t *testing.T, c *takeoverCase, instance string) {
	t.Helper()
	c.leading.Store(false)
	putNodeRegistrationForTest(t, c.sc, c.node, instance)
	c.leading.Store(true)
	_, queued := pendingDeadNodeForTest(t, c.mr, takeoverZone, c.node)
	require.True(t, queued, "前提:不是领导者时收到的注册不撤收尾任务")
}

// 同一进程失租后回来(同一个 node_uuid):任务撤掉、death_at 清掉,实例、反向索引、计数原样,不发 DestroyScene。
// 这次注册是在本进程还不是领导者时收到的,所以由当上领导者之后的收尾按镜像撤。
func TestDeadNodeReregister_SameInstanceKeepsItsInstances(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8771", 87711, 87712)
	putNodeRegistrationForTest(t, sc, c.node, "inst-a")

	deleteNodeRegistrationForTest(t, sc, mr, c.node)
	recorded, err := mr.Get(nodeReconcileInstanceKey(takeoverZone, c.node))
	require.NoError(t, err)
	assert.Equal(t, "inst-a", recorded, "收尾任务要记下判死的是哪一代")

	reregisterWhileFollowerForTest(t, c, "inst-a")
	destroyBefore := sharedFakeSceneNode.destroyCalls.Load()
	backBefore := deadNodeReconcileCountForTest(t, metrics.DeadNodeReconcileOutcomeInstanceBack)

	assert.Equal(t, 1, c.drain(), "撤掉的任务算出队")
	c.requireIntact(t, "同一进程失租后回来,它名下的实例不得被拆")
	assert.Equal(t, destroyBefore, sharedFakeSceneNode.destroyCalls.Load(), "不得向回来的节点发 DestroyScene")
	_, queued := pendingDeadNodeForTest(t, mr, takeoverZone, c.node)
	assert.False(t, queued, "任务已撤")
	assert.False(t, mr.Exists(nodeReconcileScenesKey(takeoverZone, c.node)), "收尾范围随任务删掉")
	assert.False(t, mr.Exists(nodeReconcileInstanceKey(takeoverZone, c.node)), "实例号记录随任务删掉")
	assert.False(t, mr.Exists(nodeDeathAtKey(takeoverZone, c.node)), "撤任务的同一步清 death_at")
	assert.Equal(t, backBefore+1, deadNodeReconcileCountForTest(t, metrics.DeadNodeReconcileOutcomeInstanceBack))
	assert.Equal(t, 0, c.drain(), "撤掉之后不再处理")
	c.requireIntact(t, "撤掉之后不再处理")
}

// 换了一代进程复用同一个 node_id(node_uuid 不同):新一代的注册不清 death_at,屏障走完之后旧一代的实例照常收尾。
func TestDeadNodeReregister_NewInstanceReusingNodeIDStillCleansUp(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8772", 87721)
	putNodeRegistrationForTest(t, sc, c.node, "inst-old")
	deleteNodeRegistrationForTest(t, sc, mr, c.node)

	putNodeRegistrationForTest(t, sc, c.node, "inst-new")
	requireBarrierHoldsAfterReregistration(t, c, "换了一代")

	c.elapseBarrier(t)
	assert.Equal(t, 1, c.drain())
	c.requireCleanedUp(t, "旧一代的孤儿实例照常销毁")
	assert.False(t, mr.Exists(nodeReconcileInstanceKey(takeoverZone, c.node)), "实例号记录随任务删掉")
}

// 不知道判死的是哪一代(没有 node_uuid 的条目入队):认不出回来的是谁,death_at 留着,屏障走完之后照旧收尾。
func TestDeadNodeReregister_UnknownInstanceKeepsOldBehavior(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8773", 87731)
	c.declareDead(t) // removeNodeFromRedis 的条目不带 node_uuid
	assert.False(t, mr.Exists(nodeReconcileInstanceKey(takeoverZone, c.node)))

	putNodeRegistrationForTest(t, sc, c.node, "inst-a")
	requireBarrierHoldsAfterReregistration(t, c, "不知道判死的是哪一代")

	c.elapseBarrier(t)
	assert.Equal(t, 1, c.drain())
	c.requireCleanedUp(t, "不知道是哪一代时照旧收尾")
}

// 同一 node_id 再次判死、这次不知道是哪一代:旧的实例号记录作废,不能拿更早那一代判「回来了」。
func TestDeadNodeReregister_UnknownLatestDeathForgetsTheOlderInstance(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8774", 87741)
	putNodeRegistrationForTest(t, sc, c.node, "inst-a")
	deleteNodeRegistrationForTest(t, sc, mr, c.node)
	require.True(t, mr.Exists(nodeReconcileInstanceKey(takeoverZone, c.node)))

	c.declareDead(t) // 同一 node_id 又被判死一次,条目不带 node_uuid
	assert.False(t, mr.Exists(nodeReconcileInstanceKey(takeoverZone, c.node)), "最近一次判死不知道是哪一代:记录删掉")

	putNodeRegistrationForTest(t, sc, c.node, "inst-a")
	requireBarrierHoldsAfterReregistration(t, c, "最近一次判死认不出是哪一代")

	c.elapseBarrier(t)
	assert.Equal(t, 1, c.drain())
	c.requireCleanedUp(t, "认不出时照旧收尾")
}

// requireBarrierHoldsAfterReregistration:重新注册的不是判死那一代(或认不出)时,death_at 留着(R15),
// 屏障没过的这一拍任务不出队、实例与计数原样。why 进每条断言信息。
func requireBarrierHoldsAfterReregistration(t *testing.T, c *takeoverCase, why string) {
	t.Helper()
	assert.True(t, c.mr.Exists(nodeDeathAtKey(takeoverZone, c.node)), "%s: 重新注册不得清掉 death_at", why)
	assert.Equal(t, 0, c.drain(), "%s: 屏障没过,任务不出队", why)
	c.requireIntact(t, why+": 屏障没过,不得收尾")
	_, queued := pendingDeadNodeForTest(t, c.mr, takeoverZone, c.node)
	assert.True(t, queued, "%s: 收尾任务留在表里,等屏障过后再做", why)
}

// fullSync 的陈旧清扫从旧镜像补上判死那一代:同一进程随后回来(本进程还不是领导者时收到),收尾一样认得出它。
func TestDeadNodeReregister_SweepRecordsTheInstanceFromTheMirror(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	useNodeViewGateForTest(t)
	c := newTakeoverCase(t, sc, mr, "8775", 87751)
	putNodeRegistrationForTest(t, sc, c.node, "inst-s")
	// 快照里只剩另一个节点:已知 2 个,少于存活集闸门的 4 个下限,照常放行、照常清扫。
	putNodeRegistrationForTest(t, sc, "8776", "inst-t")

	_, err := applyNodeSnapshot(context.Background(), sc, sceneNodeKVsForTest(takeoverZone, 8776), 50)
	require.NoError(t, err)
	require.True(t, mr.Exists(nodeDeathAtKey(takeoverZone, c.node)), "前提:清扫判死了 8775")
	recorded, err := mr.Get(nodeReconcileInstanceKey(takeoverZone, c.node))
	require.NoError(t, err)
	assert.Equal(t, "inst-s", recorded, "清扫要从旧镜像补上判死的是哪一代")

	reregisterWhileFollowerForTest(t, c, "inst-s")
	assert.Equal(t, 1, c.drain())
	c.requireIntact(t, "清扫判死的同一进程回来,它的实例不得被拆")
}

// 任务已被撤掉(或已被别的领导者做完)之后,手里还拿着旧列表的领导者来结束它:一个键都不动,不删回来那一代的计数。
func TestDeadNodeReregister_StaleFinishLeavesCountersOfTheLiveInstance(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8778", 87781)
	putNodeRegistrationForTest(t, sc, c.node, "inst-a")
	deleteNodeRegistrationForTest(t, sc, mr, c.node)
	tasks, _, err := listDeadNodeReconcileTasks(sc)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	stale := tasks[0]

	reregisterWhileFollowerForTest(t, c, "inst-a")
	require.Equal(t, 1, c.drain(), "前提:任务已撤")

	finished, err := finishDeadNodeReconcile(sc, stale, false)
	require.NoError(t, err)
	assert.True(t, finished, "任务已不在表里,按已出队回报")
	c.requireIntact(t, "迟到的结束不得删回来那一代的计数")
	finished, err = finishDeadNodeReconcile(sc, stale, true)
	require.NoError(t, err)
	assert.True(t, finished)
	c.requireIntact(t, "迟到的放弃同样不得删计数")
}

// lastInstancesByIdentity:同一身份挂着两个不同 node_uuid 时记为不知道,不认错人。
func TestLastInstancesByIdentity_AmbiguousIdentityIsUnknown(t *testing.T) {
	entry := func(zone uint32, node, instance string) nodeEntry {
		e := nodeEntry{nodeID: node}
		e.reg.ZoneId = zone
		e.reg.NodeUuid = instance
		return e
	}
	got := lastInstancesByIdentity(map[string]nodeEntry{
		"k1": entry(1, "10", "inst-a"),
		"k2": entry(1, "10", "inst-b"),
		"k3": entry(1, "20", "inst-c"),
		"k4": entry(1, "30", "inst-d"),
		"k5": entry(1, "30", "inst-d"),
	})
	assert.Equal(t, "", got[1]["10"], "两代并存:不知道判死的是哪一代")
	assert.Equal(t, "inst-c", got[1]["20"])
	assert.Equal(t, "inst-d", got[1]["30"], "同一代挂在两个键上不算歧义")
}

// ---------------------------------------------------------------------------
// R15 残余(2026-11-11,infra-reconnect-overview.md §4.1 R15 一行):判死的那一代回来时,只有领导者在同一步撤收尾任务、
// 清 death_at;一轮收尾里的逐个认领与结束任务都核对任务身份(reentry_barrier.go R1-I5、R1-I6)。
// ---------------------------------------------------------------------------

// seedDeadNodeTaskForTest 在任务表里放一条 (zone,node) 的收尾任务并返回它的身份,给直接调 reconcileDeadNodeScenes /
// destroyOrphanOfDeadNode 的用例用(生产里由入队脚本写,入队时刻同样取此刻的 Unix 毫秒)。
func seedDeadNodeTaskForTest(t *testing.T, sc *svc.ServiceContext, zoneID uint32, nodeID string) deadNodeTaskFence {
	t.Helper()
	task := deadNodeTaskFence{
		tasksKey:   deadNodeReconcileTasksKey(zoneID),
		member:     pendingDeadNodeKey(zoneID, nodeID),
		queuedAtMs: time.Now().UnixMilli(),
	}
	_, err := sc.Redis.Zadd(task.tasksKey, task.queuedAtMs, task.member)
	require.NoError(t, err)
	return task
}

// 跟随者先收到判死那一代的重新注册、领导者的镜像还没收到:跟随者什么都不写,death_at 与收尾任务原样,屏障照常压着;
// 领导者自己收到这次注册时才在同一步撤任务、清标记。修复前跟随者清掉 death_at,领导者见不到标记就当屏障已过,
// 按判死时刻的快照拆掉回来的活实例。本进程的注册表镜像在这里代表领导者的镜像,跟随者那一步不写它。
func TestDeadNodeReregister_FollowerSeeingTheReturnLeavesTheBarrier(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8779", 87791, 87792)
	putNodeRegistrationForTest(t, sc, c.node, "inst-a")
	deleteNodeRegistrationForTest(t, sc, mr, c.node)

	c.leading.Store(false)
	clearNodeDeathForInstance(sc, takeoverZone, c.node, "inst-a")
	c.leading.Store(true)

	assert.True(t, mr.Exists(nodeDeathAtKey(takeoverZone, c.node)), "跟随者不得清 death_at")
	_, queued := pendingDeadNodeForTest(t, mr, takeoverZone, c.node)
	assert.True(t, queued, "跟随者不得撤收尾任务")
	assert.Equal(t, 0, c.drain(), "领导者还没看到它回来:屏障照常压着,这一拍不收")
	c.requireIntact(t, "屏障没过,不得收尾")

	backBefore := deadNodeReconcileCountForTest(t, metrics.DeadNodeReconcileOutcomeInstanceBack)
	putNodeRegistrationForTest(t, sc, c.node, "inst-a") // 领导者收到同一次注册
	assert.False(t, mr.Exists(nodeDeathAtKey(takeoverZone, c.node)), "领导者看到同一代回来:death_at 清掉")
	_, queued = pendingDeadNodeForTest(t, mr, takeoverZone, c.node)
	assert.False(t, queued, "与清标记同一步撤掉任务")
	assert.False(t, mr.Exists(nodeReconcileScenesKey(takeoverZone, c.node)), "收尾范围随任务删掉")
	assert.False(t, mr.Exists(nodeReconcileInstanceKey(takeoverZone, c.node)), "实例号记录随任务删掉")
	assert.Equal(t, backBefore+1, deadNodeReconcileCountForTest(t, metrics.DeadNodeReconcileOutcomeInstanceBack))
	assert.Equal(t, 0, c.drain(), "任务已撤,没有可做的")
	c.requireIntact(t, "同一代回来,它名下的实例与计数原样")
}

// 一轮收尾进行到一半(第一个场景已认领)时,判死的那一代回来、任务被撤:剩下的场景一个都不拆,计数不删,任务按已出队计。
// 生产里撤销来自领导者的 watch goroutine,与收尾 goroutine 并发;这里在第一次认领脚本返回之后同步插进去。
// 修复前逐个认领只比对属主,回来的就是这个 node_id,剩下的照拆。
func TestDeadNodeReregister_ReturnMidSweepStopsTheRest(t *testing.T) {
	hook := &afterEvalHook{script: luaAtomicDestroyOrphanOfDeadNode}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	c := newTakeoverCase(t, sc, mr, "8780", 87801, 87802)
	putNodeRegistrationForTest(t, sc, c.node, "inst-a")
	deleteNodeRegistrationForTest(t, sc, mr, c.node)
	c.elapseBarrier(t)
	hook.arm(func() {
		verdict, err := withdrawDeadNodeForReturningInstance(sc, takeoverZone, c.node, "inst-a")
		require.NoError(t, err)
		require.Equal(t, withdrawnReturningInstance, verdict, "注入的撤销必须真的撤掉任务")
	})

	assert.Equal(t, 1, c.drain(), "任务已撤,按已出队计")
	require.True(t, hook.fired.Load(), "撤销必须插在第一次认领之后")

	var destroyed, intact []uint64
	for _, id := range c.scenes {
		if mr.Exists(sceneNodeKey(id)) {
			intact = append(intact, id)
		} else {
			destroyed = append(destroyed, id)
		}
	}
	require.Len(t, destroyed, 1, "撤销之前认领的那一个已经拆了,不可恢复")
	require.Len(t, intact, 1, "撤销之后的一个都不拆")
	survivor := intact[0]
	assert.Equal(t, c.node, mustGet(t, mr, sceneNodeKey(survivor)), "留下的实例属主原样")
	in, err := sc.Redis.Sismember(nodeScenesKey(takeoverZone, c.node), strconv.FormatUint(survivor, 10))
	require.NoError(t, err)
	assert.True(t, in, "留下的实例必须留在反向索引里")
	requireActiveInstance(t, sc, takeoverZone, survivor, true)
	assert.Equal(t, "1", c.valueOrMissing(nodeSceneCountKey(takeoverZone, c.node)), "只扣了拆掉的那一个,计数不删")
	_, queued := pendingDeadNodeForTest(t, mr, takeoverZone, c.node)
	assert.False(t, queued, "任务已撤")
	assert.Equal(t, 0, c.drain(), "任务已撤,没有可做的")
	assert.Equal(t, c.node, mustGet(t, mr, sceneNodeKey(survivor)), "下一拍也不拆")
}

// 逐个认领核对任务身份:入队时刻对不上(撤销之后同一 node_id 又判死了一次,表里是新任务)时一个键都不动、
// 回 errDeadNodeCleanupWithdrawn;对得上才拆。任务整条不在表里的情形见 ReturnMidSweepStopsTheRest。
func TestDeadNodeReregister_ClaimChecksTheCleanupTask(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8781", 87811)
	c.declareDead(t)
	tasks, _, err := listDeadNodeReconcileTasks(sc)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	current := tasks[0].fence()
	older := current
	older.queuedAtMs -= time.Minute.Milliseconds()
	ctx := context.Background()

	ok, err := destroyOrphanOfDeadNode(ctx, sc, takeoverZone, c.scenes[0], c.node, older)
	require.ErrorIs(t, err, errDeadNodeCleanupWithdrawn)
	assert.False(t, ok)
	c.requireIntact(t, "拿着上一次判死的任务身份:一个键都不动")

	ok, err = destroyOrphanOfDeadNode(ctx, sc, takeoverZone, c.scenes[0], c.node, current)
	require.NoError(t, err)
	assert.True(t, ok, "任务身份对得上才拆")
	assert.False(t, mr.Exists(sceneNodeKey(c.scenes[0])))
}

// 撤销之后同一 node_id 又判死了一次,表里是新任务(同一成员,入队时刻更晚)。手里还拿着上一次任务的领导者来结束或放弃它,
// 一个键都不动:新任务、它的收尾范围与死节点的计数都原样。
func TestDeadNodeReregister_StaleFinishLeavesANewerDeathAlone(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8782", 87821)
	c.declareDead(t)
	tasks, _, err := listDeadNodeReconcileTasks(sc)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	older := tasks[0]
	older.queuedAt = older.queuedAt.Add(-time.Minute)

	for _, abandon := range []bool{false, true} {
		finished, err := finishDeadNodeReconcile(sc, older, abandon)
		require.NoError(t, err)
		assert.True(t, finished, "上一次的任务已不在表里,按已出队回报(abandon=%v)", abandon)
		_, queued := pendingDeadNodeForTest(t, mr, takeoverZone, c.node)
		assert.True(t, queued, "新任务原样留着(abandon=%v)", abandon)
		assert.True(t, mr.Exists(nodeReconcileScenesKey(takeoverZone, c.node)), "新任务的收尾范围原样(abandon=%v)", abandon)
		c.requireIntact(t, fmt.Sprintf("上一次任务的结束不得删计数(abandon=%v)", abandon))
	}
}
