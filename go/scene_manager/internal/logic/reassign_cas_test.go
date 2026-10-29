package logic

// 回归:reassignSceneNode 曾无条件 SET scene:{id}:node。改派者不止一个、彼此也不共用锁 ——
// 领导者的 rebalance(migrateWorldChannel,不持 zone 锁)与任意副本上的 initWorldScenesForZone
// (持 zone 锁),而且两边选新节点的哈希输入不同(sceneId 对 confId*1000+序号)。两边都判定
// 老节点已死时,同一个频道会被派给两个节点:各 CreateScene 一次、留下孤儿实体,scene_count
// 双减或漏记,Agones 名额被转移两次。
//
// 交错不靠 goroutine 抢跑,而是用确定的顺序复现:
//   - 单元层:两个写者都在「属主是 old」时做了决定,先后提交;
//   - 集成层:用真实的 PlanWorldChannelRebalance / migrateWorldChannel / initWorldScenesForZone,
//     把一个写者的完整提交插进另一个写者「做决定」与「提交」之间。

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"scene_manager/internal/svc"
)

const reassignCASZone uint32 = 1

// seedSceneOwnedBy 摆出「scene 归 nodeID 所有」的完整记账:属主映射、反向索引、scene_count=1。
func seedSceneOwnedBy(t *testing.T, mr *miniredis.Miniredis, zoneID uint32, sceneID uint64, nodeID string) {
	t.Helper()
	require.NoError(t, mr.Set(sceneNodeKey(sceneID), nodeID))
	_, err := mr.SAdd(nodeScenesKey(zoneID, nodeID), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	require.NoError(t, mr.Set(nodeSceneCountKey(zoneID, nodeID), "1"))
}

// requireSceneIndexedOnlyOn 断言 scene 在 owner 的反向索引里,且不在 others 的反向索引里。
func requireSceneIndexedOnlyOn(t *testing.T, sc *svc.ServiceContext, zoneID uint32, sceneID uint64, owner string, others ...string) {
	t.Helper()
	member := strconv.FormatUint(sceneID, 10)
	in, err := sc.Redis.Sismember(nodeScenesKey(zoneID, owner), member)
	require.NoError(t, err)
	require.True(t, in, "scene %d 必须在属主 %s 的反向索引里", sceneID, owner)
	for _, n := range others {
		in, err := sc.Redis.Sismember(nodeScenesKey(zoneID, n), member)
		require.NoError(t, err)
		require.False(t, in, "scene %d 不得留在 %s 的反向索引里", sceneID, n)
	}
}

// 单元用例的节点号刻意不与别的用例重名:再入屏障会读进程内的推迟摘除队列,别的用例若在
// zone 1 留下 "10" 之类常用节点的推迟态,这里的改派会被静默挡下,报出与本缺陷无关的失败。
const (
	casNodeOld   = "8810"
	casNodeFirst = "8820"
	casNodeLate  = "8830"
)

// 两个写者都在「属主是 old」时做了决定:先到的改到 first,后到的拿着陈旧判断想改到 late。
// 修复前后到的一方无条件 SET 覆盖成 late 并返回 true,first 上的实体成了孤儿。
func TestReassignSceneNode_StaleSecondWriterChangesNothing(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 88001
	seedSceneOwnedBy(t, mr, reassignCASZone, sceneID, casNodeOld)

	require.True(t, reassignSceneNode(sc, reassignCASZone, sceneID, casNodeOld, casNodeFirst))
	require.False(t, reassignSceneNode(sc, reassignCASZone, sceneID, casNodeOld, casNodeLate),
		"属主已不是 old,后到的改派必须落空")

	owner, err := mr.Get(sceneNodeKey(sceneID))
	require.NoError(t, err)
	require.Equal(t, casNodeFirst, owner)
	requireSceneIndexedOnlyOn(t, sc, reassignCASZone, sceneID, casNodeFirst, casNodeOld, casNodeLate)
	require.EqualValues(t, 0, readInt64(sc, nodeSceneCountKey(reassignCASZone, casNodeOld)))
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(reassignCASZone, casNodeFirst)))
	require.False(t, mr.Exists(nodeSceneCountKey(reassignCASZone, casNodeLate)), "落空的一方不得建出计数键")
}

// 懒改派处理「频道没有属主映射」时以空串作 oldNode:只有映射确实不存在才能认领。
func TestReassignSceneNode_EmptyOldNodeClaimsOnlyUnmappedScene(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 88002

	require.True(t, reassignSceneNode(sc, reassignCASZone, sceneID, "", casNodeFirst))
	require.False(t, reassignSceneNode(sc, reassignCASZone, sceneID, "", casNodeLate),
		"映射已被别的懒改派认领,空串期望不得命中")

	owner, err := mr.Get(sceneNodeKey(sceneID))
	require.NoError(t, err)
	require.Equal(t, casNodeFirst, owner)
	requireSceneIndexedOnlyOn(t, sc, reassignCASZone, sceneID, casNodeFirst, casNodeLate)
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(reassignCASZone, casNodeFirst)))
	require.False(t, mr.Exists(nodeSceneCountKey(reassignCASZone, casNodeLate)))
	require.False(t, mr.Exists(nodeScenesKey(reassignCASZone, "")), "没有老属主时不得碰空节点名拼出的键")
	require.False(t, mr.Exists(nodeSceneCountKey(reassignCASZone, "")))
}

// 新节点计数键里是非整数时 INCR 会报错;Lua 报错不回滚已执行的写,所以 INCR 必须排在改归属之前,
// 否则留下「归属已改、计数没记」的半截改派,调用方却以为没改成。
func TestReassignSceneNode_UnincrementableCounterLeavesOwnershipUntouched(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 88003
	seedSceneOwnedBy(t, mr, reassignCASZone, sceneID, casNodeOld)
	require.NoError(t, mr.Set(nodeSceneCountKey(reassignCASZone, casNodeFirst), "not-a-number"))

	require.False(t, reassignSceneNode(sc, reassignCASZone, sceneID, casNodeOld, casNodeFirst),
		"新节点计数键无法 INCR 时,改派必须整体失败")

	owner, err := mr.Get(sceneNodeKey(sceneID))
	require.NoError(t, err)
	require.Equal(t, casNodeOld, owner, "脚本报错时归属一个字节都不改")
	requireSceneIndexedOnlyOn(t, sc, reassignCASZone, sceneID, casNodeOld, casNodeFirst)
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(reassignCASZone, casNodeOld)))
}

// ---------------------------------------------------------------------------
// 两写者竞态(集成层)
// ---------------------------------------------------------------------------

const worldChannelRaceZone uint32 = 1

// worldChannelRace 是两写者竞态的现场:世界频道 sceneID 的属主 old 已死(不在 etcd 注册表、
// 不在负载集,拨号 Unavailable),x、y 是两个活的世界节点。
//   - initWorldScenesForZone 会把它改派到 x(哈希输入 confID*1000+序号);
//   - rebalance 会把它改派到 y(哈希输入 sceneID)。
//
// 两边目标不同,正是生产上的竞态形状。
type worldChannelRace struct {
	sc      *svc.ServiceContext
	mr      *miniredis.Miniredis
	old     string
	x, y    *sceneNodeHarness
	confID  uint64
	sceneID uint64
	// onDialOld 非空时,下一次拨号 old 先执行它(只执行一次)再返回 Unavailable。
	// initWorldScenesForZone 先对属主发 CreateScene、失败后才判死改派,所以这是把另一个
	// 写者插进 init「读到属主」与「提交改派」之间的确定性的缝。
	onDialOld atomic.Pointer[func()]
}

func newWorldChannelRace(t *testing.T) *worldChannelRace {
	t.Helper()
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)
	markKnownNodesSyncedForTest(t)

	sc, mr := newIntegrationSvcCtx(t)
	hA := startFakeSceneNode(t, "7102")
	hB := startFakeSceneNode(t, "7103")
	installBufconnDialer(t, hA, hB)
	registerWorldNodeFromHarness(t, sc, worldChannelRaceZone, hA)
	registerWorldNodeFromHarness(t, sc, worldChannelRaceZone, hB)

	r := &worldChannelRace{sc: sc, mr: mr, old: "7101", confID: 610}
	// 已按字典序排好,与 init / rebalance 里的 sort.Strings 一致。
	ring := []string{hA.nodeID, hB.nodeID}
	r.x, r.y = hA, hB
	// init 处理的是本 conf 的第一个频道,序号为 0。
	if assignNodeByHash(r.confID*1000, ring) == hB.nodeID {
		r.x, r.y = hB, hA
	}
	r.sceneID = findSceneIdHashingTo(t, ring, r.y.nodeID, 200)

	// 未注册节点由 TestMain 的解析器解析成 "bufnet:<id>"。
	oldEndpoint := "bufnet:" + r.old
	prevDialer := nodeDialer
	restore := SetNodeDialerForTest(func(ctx context.Context, endpoint string) (*grpc.ClientConn, error) {
		if endpoint == oldEndpoint {
			if hook := r.onDialOld.Swap(nil); hook != nil {
				(*hook)()
			}
			return nil, status.Error(codes.Unavailable, "old scene node is dead")
		}
		return prevDialer(ctx, endpoint)
	})
	t.Cleanup(restore)

	// 频道当前归 old,记账按正常建频道后的样子摆好。
	_, err := mr.SAdd(worldChannelsKey(worldChannelRaceZone, r.confID), strconv.FormatUint(r.sceneID, 10))
	require.NoError(t, err)
	require.NoError(t, mr.Set(sceneZoneKey(r.sceneID), strconv.FormatUint(uint64(worldChannelRaceZone), 10)))
	seedSceneOwnedBy(t, mr, worldChannelRaceZone, r.sceneID, r.old)

	// 前提:old 已死且有死亡证据,两个写者都会改派它。
	require.True(t, isNodeGoneFromRegistry(worldChannelRaceZone, r.old))
	require.False(t, IsNodeAlive(sc, worldChannelRaceZone, r.old))
	return r
}

// planOnlyMigration 取 rebalance 对本频道的迁移计划(old -> y)。
func (r *worldChannelRace) planOnlyMigration(t *testing.T) channelMigration {
	t.Helper()
	urgent, opportunistic, _ := PlanWorldChannelRebalance(r.sc, worldChannelRaceZone, []uint64{r.confID})
	require.Len(t, urgent, 1)
	require.Empty(t, opportunistic)
	plan := urgent[0]
	require.Equal(t, r.sceneID, plan.SceneId)
	require.Equal(t, r.old, plan.OldNode)
	require.Equal(t, r.y.nodeID, plan.NewNode)
	return plan
}

func (r *worldChannelRace) migrate(ctx context.Context, plan channelMigration) bool {
	return migrateWorldChannel(ctx, r.sc, worldChannelRaceZone, plan.ConfId, plan.SceneId, plan.OldNode, plan.NewNode, plan.Reason)
}

// requireOwnedOnlyBy 断言频道的记账全部指向 owner:属主映射、反向索引、scene_count 都只改了一次。
func (r *worldChannelRace) requireOwnedOnlyBy(t *testing.T, owner, loser *sceneNodeHarness) {
	t.Helper()
	got, err := r.mr.Get(sceneNodeKey(r.sceneID))
	require.NoError(t, err)
	require.Equal(t, owner.nodeID, got, "属主映射必须指向先提交的写者")
	requireSceneIndexedOnlyOn(t, r.sc, worldChannelRaceZone, r.sceneID, owner.nodeID, r.old, loser.nodeID)
	require.EqualValues(t, 1, readInt64(r.sc, nodeSceneCountKey(worldChannelRaceZone, owner.nodeID)),
		"属主的 scene_count 恰好加一次")
	require.EqualValues(t, 0, readInt64(r.sc, nodeSceneCountKey(worldChannelRaceZone, loser.nodeID)),
		"落空的一方不得记账")
	require.EqualValues(t, 0, readInt64(r.sc, nodeSceneCountKey(worldChannelRaceZone, r.old)),
		"老属主的 scene_count 恰好减一次")
}

// init 先提交,rebalance 拿着陈旧计划后到:CAS 落空,并回收自己在 y 上预建的实体。
// 修复前 rebalance 无条件覆盖成 y 并返回成功:x 上 init 建的实体成了孤儿,计数也乱了。
// 同时钉住 init 改派时 scene_count 跟着走(修复前 init 改派完全不记账)。
func TestWorldChannelRace_InitCommitsFirst_StaleRebalanceBacksOut(t *testing.T) {
	r := newWorldChannelRace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plan := r.planOnlyMigration(t)

	initWorldScenesForZone(ctx, r.sc, worldChannelRaceZone, []uint64{r.confID}, true)
	owner, err := r.mr.Get(sceneNodeKey(r.sceneID))
	require.NoError(t, err)
	require.Equal(t, r.x.nodeID, owner, "前提:init 已把频道改派到 x")
	require.EqualValues(t, 1, r.x.fake.createCalls.Load())

	require.False(t, r.migrate(ctx, plan), "计划里的老属主已被 init 改走,这次迁移必须作废")

	r.requireOwnedOnlyBy(t, r.x, r.y)
	require.EqualValues(t, 1, r.y.fake.createCalls.Load(), "陈旧计划照常先在 y 上建了实体")
	require.EqualValues(t, 1, r.y.fake.destroyCalls.Load(), "作废的迁移必须回收 y 上没有归属的实体")
	require.Equal(t, r.sceneID, r.y.fake.lastDestroyScene.Load())
	require.EqualValues(t, 0, r.x.fake.destroyCalls.Load(), "x 上是活场景,不得销毁")
}

// rebalance 在 init「读到属主」与「提交改派」之间完成了整次迁移:init 的 CAS 落空,
// 不得转移名额、不得在 x 上建实体。修复前 init 无条件覆盖成 x 并 CreateScene,
// y 上 rebalance 建的实体成了孤儿。
func TestWorldChannelRace_RebalanceCommitsFirst_StaleInitSkips(t *testing.T) {
	r := newWorldChannelRace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var hookRan atomic.Bool
	hook := func() {
		hookRan.Store(true)
		RebalanceWorldChannelsForZone(ctx, r.sc, worldChannelRaceZone, []uint64{r.confID})
	}
	r.onDialOld.Store(&hook)

	initWorldScenesForZone(ctx, r.sc, worldChannelRaceZone, []uint64{r.confID}, true)

	require.True(t, hookRan.Load(), "前提:init 先对老属主发 CreateScene,rebalance 插在 init 判死改派之前")
	r.requireOwnedOnlyBy(t, r.y, r.x)
	require.EqualValues(t, 1, r.y.fake.createCalls.Load())
	require.EqualValues(t, 0, r.y.fake.destroyCalls.Load())
	require.EqualValues(t, 0, r.x.fake.createCalls.Load(), "init 的改派落空,不得在 x 上建实体")
}

// 同一份计划执行两次(降级窗口里新旧领导者各跑一遍,或 watch 事件与 fullSync 同拍触发):
// 第二次 CAS 落空。它选的是同一个节点,y 上的实体就是活场景,不得回收;计数只记一次。
// 修复前第二次照样成功:old 的 scene_count 双减、y 的双加。
func TestWorldChannelRace_DuplicateRebalanceCountsOnceAndKeepsLiveEntity(t *testing.T) {
	r := newWorldChannelRace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plan := r.planOnlyMigration(t)

	require.True(t, r.migrate(ctx, plan))
	require.False(t, r.migrate(ctx, plan), "第二次执行同一份计划必须落空")

	r.requireOwnedOnlyBy(t, r.y, r.x)
	require.EqualValues(t, 2, r.y.fake.createCalls.Load(), "两次都先建实体(按 scene_id 幂等)")
	require.EqualValues(t, 0, r.y.fake.destroyCalls.Load(), "落空的一方与赢家同节点,那是活场景,不得销毁")
}

// 计划之后老节点的死亡时刻才落地:第二道屏障拦下改派,y 上预建的实体同样没有归属,回收掉。
// 修复前它被留在 y 上「等下一拍复用」;可下一拍先跑的是 init,会把频道派给 x,
// y 上那份就没人认领了。
func TestWorldChannelRace_BarrierAfterPlanReclaimsPreCreatedEntity(t *testing.T) {
	r := newWorldChannelRace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plan := r.planOnlyMigration(t)
	require.NoError(t, markNodeDeath(r.sc, worldChannelRaceZone, r.old))

	require.False(t, r.migrate(ctx, plan))

	owner, err := r.mr.Get(sceneNodeKey(r.sceneID))
	require.NoError(t, err)
	require.Equal(t, r.old, owner, "屏障未到,归属一个字节都不改")
	requireSceneIndexedOnlyOn(t, r.sc, worldChannelRaceZone, r.sceneID, r.old, r.x.nodeID, r.y.nodeID)
	require.EqualValues(t, 1, readInt64(r.sc, nodeSceneCountKey(worldChannelRaceZone, r.old)))
	require.EqualValues(t, 0, readInt64(r.sc, nodeSceneCountKey(worldChannelRaceZone, r.y.nodeID)))
	require.EqualValues(t, 1, r.y.fake.createCalls.Load())
	require.EqualValues(t, 1, r.y.fake.destroyCalls.Load(), "屏障拦下的迁移也要回收 y 上预建的实体")
}
