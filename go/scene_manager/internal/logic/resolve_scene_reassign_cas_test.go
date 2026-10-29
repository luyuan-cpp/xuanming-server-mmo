package logic

// 回归:resolveScene 的死节点自愈(reassignSceneOffDeadNode)。
//
// 修复前它无条件 SET scene:{id}:node,再 CreateScene,有四处问题:
//   - 别的写者在它「读到属主是死节点」与「写入」之间先改派了同一场景时,它照样覆盖,并在自己挑的节点上
//     建实体。先到者那份成了孤儿,同一场景的玩家被派到两个节点上。另一个写者可以是另一个副本上的
//     EnterScene、rebalance、init 或懒改派。
//   - SET 不动反向索引与 scene_count。
//   - 先改归属再建实体,CreateScene 失败只打日志:归属已指向一个没有这个场景的节点,后续请求读到属主活着
//     就直接用,要等下一次 initWorldScenesForZone 被触发(不是周期任务)才补建;副本则永远没人补。
//   - 用请求带来的 scene_conf_id 重建任何场景:副本 / 镜像被重建成空壳,对不上的 conf 能把场景建成别的地图。
//
// 现在只重建在役的大世界频道(以 world_channels 成员资格确认 conf,提交时在 CAS 脚本里再核对一次),
// 先建实体、再 CAS、最后转名额。
//
// 交错用确定的顺序复现(与 reassign_cas_test.go 单元层同一手法):先用 reassignSceneNode 提交「另一写者」,
// 再拿陈旧的属主观察(dead)直接调用 reassignSceneOffDeadNode —— 与 resolveScene 读完属主、提交之前被
// 另一写者插队的现场相同。AnotherWriterFirst、SameTargetAdoptsPreCreatedEntity 与 AgonesRoom 用例里的
// lostScene / adoptScene 两段这样直接调用;ChannelDrainedBeforeCommit 直接调用提交原语
// reassignWorldChannelNode;其余都从 resolveScene 进。(也可以给 go-zero Redis 挂 hook,在 resolveScene 读完
// scene:{id}:node 之后注入另一写者,从入口复现同样的交错;这里没有用。)
//
// 修复前红的取法(按变异,修复前的代码没有这些方法,新用例编不过):
//   - 变异 A:把 reassignSceneOffDeadNode 里的
//     `if !reassignWorldChannelNode(...) { return l.adoptOwnerAfterLostReassign(...) }`
//     换回无条件的 `l.svcCtx.Redis.Set(sceneNodeKey(sceneId), newNodeId)`。
//     AnotherWriterFirst、ScriptFailure、MovesIndexAndSceneCount、AgonesRoom 红;CreateSceneFailure 的后半段
//     (重试成功后的反向索引 / scene_count 断言)也红,共 5 条。
//   - 变异 B:把 CreateScene 挪回 CAS 成功之后,失败只打日志、照常返回成功。CreateSceneFailure 红;
//     AnotherWriterFirst、SameTargetAdoptsPreCreatedEntity、ScriptFailure 的「CAS 之前已预建」断言跟着红。
//   - 变异 C:删掉 reassignSceneOffDeadNode 开头的 world_channels 成员资格检查。InstanceAndMismatchedConf 红。
//   - 变异 D:resolveScene 的 zone 守卫换回 GetSceneZone(读错 / 值损坏都当 0 放行)。CorruptZoneRecord 红。
//   - 变异 F:删掉 luaReassignSceneNode 里 `#KEYS >= 6` 那段在役核对。ChannelDrainedBeforeCommit 红。
// BarrierPending 是行为守护,上面各种变异下都绿。

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

const resolveCASZone uint32 = 1

// 节点号不与别的用例重名,理由同 reassign_cas_test.go 的 casNode* 常量:再入屏障会读进程内的推迟摘除
// 队列,别的用例在 zone 1 留下的常用节点推迟态会把这里的改派静默挡下。
const (
	resolveNodeDead  = "8910"
	resolveNodeFirst = "8920" // 先提交的另一写者挑中的节点
	resolveNodeLate  = "8930" // 本次请求(拿着陈旧判断)挑中的节点
)

// resolveCASConfID 是用例里那张大世界地图;场景是它在 zone 1 的频道(world_channels 成员)。
const resolveCASConfID uint64 = 4791

type resolveDeadOwnerCase struct {
	sc          *svc.ServiceContext
	mr          *miniredis.Miniredis
	first, late *sceneNodeHarness
	logic       *EnterSceneLogic
	sceneID     uint64
}

// newResolveDeadOwnerCase 摆出现场:sceneID 是 resolveCASConfID 在 zone 1 的频道,归 dead 所有(属主映射、
// 反向索引、scene_count=1 齐全);dead 已摘出负载集且没有 death_at(屏障放行);first、late 两个活的
// 世界节点,负载分让本次请求挑 late。
func newResolveDeadOwnerCase(t *testing.T, sceneID uint64) *resolveDeadOwnerCase {
	t.Helper()
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)

	sc, mr := newIntegrationSvcCtx(t)
	first := startFakeSceneNode(t, resolveNodeFirst)
	late := startFakeSceneNode(t, resolveNodeLate)
	installBufconnDialer(t, first, late)
	registerWorldNodeFromHarness(t, sc, resolveCASZone, first)
	registerWorldNodeFromHarness(t, sc, resolveCASZone, late)
	setResolveCASLoadScores(t, mr)
	seedWorldChannelOwnedByDeadNode(t, mr, sceneID)

	// 前提:dead 已判死、屏障放行,本次请求挑的是 late。
	require.False(t, IsNodeAlive(sc, resolveCASZone, resolveNodeDead))
	allowed, _ := CanReclaimDeadNode(sc, resolveCASZone, resolveNodeDead)
	require.True(t, allowed)
	best, err := GetBestNodeForPurpose(context.Background(), sc, resolveCASZone, constants.NodePurposeWorld)
	require.NoError(t, err)
	require.Equal(t, resolveNodeLate, best)

	return &resolveDeadOwnerCase{sc: sc, mr: mr, first: first, late: late,
		logic: NewEnterSceneLogic(context.Background(), sc), sceneID: sceneID}
}

// setResolveCASLoadScores 把两个活节点的负载分拉开:GetBestNodeForPurpose 挑 late,先提交的另一写者挑 first。
func setResolveCASLoadScores(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()
	_, err := mr.ZAdd(nodeLoadKey(resolveCASZone), 5, resolveNodeFirst)
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(resolveCASZone), 0, resolveNodeLate)
	require.NoError(t, err)
}

// seedWorldChannelOwnedByDeadNode 把 sceneID 登记成 resolveCASConfID 在 zone 1 的频道,并按正常建频道后的
// 样子记到 dead 名下。
func seedWorldChannelOwnedByDeadNode(t *testing.T, mr *miniredis.Miniredis, sceneID uint64) {
	t.Helper()
	_, err := mr.SAdd(worldChannelsKey(resolveCASZone, resolveCASConfID), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	require.NoError(t, mr.Set(sceneZoneKey(sceneID), strconv.FormatUint(uint64(resolveCASZone), 10)))
	seedSceneOwnedBy(t, mr, resolveCASZone, sceneID, resolveNodeDead)
}

// requireOwnedOnlyBy 断言场景的记账全部指向 owner,且只改了一次:属主映射、反向索引、两侧 scene_count;
// bystander 是没拿到归属的那个活节点,不得记账。
func (c *resolveDeadOwnerCase) requireOwnedOnlyBy(t *testing.T, owner, bystander string) {
	t.Helper()
	got, err := c.mr.Get(sceneNodeKey(c.sceneID))
	require.NoError(t, err)
	require.Equal(t, owner, got)
	requireSceneIndexedOnlyOn(t, c.sc, resolveCASZone, c.sceneID, owner, resolveNodeDead, bystander)
	require.EqualValues(t, 1, readInt64(c.sc, nodeSceneCountKey(resolveCASZone, owner)), "属主的 scene_count 恰好加一次")
	require.EqualValues(t, 0, readInt64(c.sc, nodeSceneCountKey(resolveCASZone, resolveNodeDead)), "老属主的 scene_count 恰好减一次")
	require.EqualValues(t, 0, readInt64(c.sc, nodeSceneCountKey(resolveCASZone, bystander)), "没拿到归属的节点不得记账")
}

// requireStillOnDeadNode 断言归属与记账一个字节都没改。
func (c *resolveDeadOwnerCase) requireStillOnDeadNode(t *testing.T) {
	t.Helper()
	owner, err := c.mr.Get(sceneNodeKey(c.sceneID))
	require.NoError(t, err)
	require.Equal(t, resolveNodeDead, owner, "归属一个字节都不改")
	requireSceneIndexedOnlyOn(t, c.sc, resolveCASZone, c.sceneID, resolveNodeDead, resolveNodeFirst, resolveNodeLate)
	require.EqualValues(t, 1, readInt64(c.sc, nodeSceneCountKey(resolveCASZone, resolveNodeDead)))
	require.EqualValues(t, 0, readInt64(c.sc, nodeSceneCountKey(resolveCASZone, resolveNodeLate)))
}

// 另一写者在本次请求「读到属主是 dead」之后先把频道改到了 first。本次在 late 上预建了实体,CAS 落空,
// 重读属主后直接用 first,并回收 late 上那份没有归属的实体;不覆盖,也不替 first 建实体。
// 修复前:无条件覆盖成 late,first 上的实体成了孤儿。
func TestResolveScene_DeadOwner_AnotherWriterFirst_AdoptsItsOwner(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 89001)
	require.True(t, reassignSceneNode(c.sc, resolveCASZone, c.sceneID, resolveNodeDead, resolveNodeFirst))

	sid, nid, err := c.logic.reassignSceneOffDeadNode(c.sceneID, resolveCASConfID, resolveCASZone, resolveNodeDead)
	require.NoError(t, err, "落空后重读到活的新属主,本次进场照常放行")
	require.Equal(t, c.sceneID, sid)
	require.Equal(t, resolveNodeFirst, nid, "必须用先提交者的节点,而不是自己挑的 late")

	c.requireOwnedOnlyBy(t, resolveNodeFirst, resolveNodeLate)
	require.EqualValues(t, 1, c.late.fake.createCalls.Load(), "先建后改:CAS 之前已在 late 上预建")
	require.EqualValues(t, 1, c.late.fake.destroyCalls.Load(), "落空后必须回收 late 上没有归属的实体")
	require.Equal(t, c.sceneID, c.late.fake.lastDestroyScene.Load())
	require.EqualValues(t, 0, c.first.fake.createCalls.Load(), "first 上的实体由先提交的写者负责")
	require.EqualValues(t, 0, c.first.fake.destroyCalls.Load(), "first 上是活场景,不得销毁")
}

// 先提交的写者恰好挑了同一个节点(并发的另一个 EnterScene 挑中同一个最低负载节点;Redis 报错而本次脚本
// 其实已执行,在本次看来也是这个现场)。属主就是 late:预建的实体正是属主的实体,直接采用,不回收,计数只记一次。
func TestResolveScene_DeadOwner_SameTargetAdoptsPreCreatedEntity(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 89002)
	require.True(t, reassignSceneNode(c.sc, resolveCASZone, c.sceneID, resolveNodeDead, resolveNodeLate))

	sid, nid, err := c.logic.reassignSceneOffDeadNode(c.sceneID, resolveCASConfID, resolveCASZone, resolveNodeDead)
	require.NoError(t, err)
	require.Equal(t, c.sceneID, sid)
	require.Equal(t, resolveNodeLate, nid)

	c.requireOwnedOnlyBy(t, resolveNodeLate, resolveNodeFirst)
	require.EqualValues(t, 1, c.late.fake.createCalls.Load())
	require.Equal(t, c.sceneID, c.late.fake.lastCreateScene.Load())
	require.Equal(t, resolveCASConfID, c.late.fake.lastCreateConf.Load())
	require.EqualValues(t, 0, c.late.fake.destroyCalls.Load(), "late 上是属主的活场景,不得回收")
	require.EqualValues(t, 0, c.first.fake.createCalls.Load())
}

// late 的计数键损坏,脚本在 INCR 处报错:CAS 没被抢,是没跑成,归属一个字节都没改。本次请求拒绝(不是
// 屏障拒绝),并回收 late 上预建的实体。修复前:SET 不经脚本,照样改成 late。
func TestResolveScene_DeadOwner_ScriptFailureChangesNothing(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 89003)
	require.NoError(t, c.mr.Set(nodeSceneCountKey(resolveCASZone, resolveNodeLate), "not-a-number"))

	_, _, err := c.logic.resolveScene(c.sceneID, resolveCASConfID, resolveCASZone)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrReentryBarrierPending), "屏障早已放行,这不是屏障拒绝")

	owner, err := c.mr.Get(sceneNodeKey(c.sceneID))
	require.NoError(t, err)
	require.Equal(t, resolveNodeDead, owner, "脚本报错时归属一个字节都不改")
	requireSceneIndexedOnlyOn(t, c.sc, resolveCASZone, c.sceneID, resolveNodeDead, resolveNodeFirst, resolveNodeLate)
	require.EqualValues(t, 1, readInt64(c.sc, nodeSceneCountKey(resolveCASZone, resolveNodeDead)))
	require.EqualValues(t, 1, c.late.fake.createCalls.Load())
	require.EqualValues(t, 1, c.late.fake.destroyCalls.Load(), "归属没改到 late,预建的实体必须回收")
	require.EqualValues(t, 0, c.first.fake.createCalls.Load())
}

// 没有竞争写者时从 resolveScene 入口走完整次改派:先在 late 上建实体,再把属主映射、两侧反向索引与
// scene_count 同批改到 late,记账口径与 createscenelogic.go allocateScene 一致。
// 修复前:只 SET 属主映射,late 的反向索引与 scene_count 都没有,dead 的还挂着这个频道。
func TestResolveScene_DeadOwner_MovesIndexAndSceneCount(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 89004)

	sid, nid, err := c.logic.resolveScene(c.sceneID, resolveCASConfID, resolveCASZone)
	require.NoError(t, err)
	require.Equal(t, c.sceneID, sid)
	require.Equal(t, resolveNodeLate, nid)

	c.requireOwnedOnlyBy(t, resolveNodeLate, resolveNodeFirst)
	require.EqualValues(t, 1, c.late.fake.createCalls.Load())
	require.Equal(t, c.sceneID, c.late.fake.lastCreateScene.Load())
	require.Equal(t, resolveCASConfID, c.late.fake.lastCreateConf.Load())
	require.EqualValues(t, 0, c.late.fake.destroyCalls.Load())
	require.EqualValues(t, 0, c.first.fake.createCalls.Load())
}

// 新节点建场景失败(节点拒绝、请求 ctx 被截断):归属一个字节都不改,本次拒绝;下一次请求从头再来并建成。
// 修复前:先改归属再建,失败只打日志、照常返回 late,后续请求读到属主活着就直接用,要等下一次 init 被触发才补建。
func TestResolveScene_DeadOwner_CreateSceneFailureChangesNothing(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 89005)
	c.late.fake.createShouldFail = status.Error(codes.Unavailable, "scene_node_shutting_down")

	_, _, err := c.logic.resolveScene(c.sceneID, resolveCASConfID, resolveCASZone)
	require.Error(t, err, "建场景失败必须拒绝,不能把玩家派到没有这个场景的节点")
	require.False(t, errors.Is(err, ErrReentryBarrierPending))
	c.requireStillOnDeadNode(t)
	require.EqualValues(t, 1, c.late.fake.createCalls.Load())
	require.EqualValues(t, 0, c.late.fake.destroyCalls.Load(), "没写归属,没有要回收的东西")

	c.late.fake.createShouldFail = nil
	_, nid, err := c.logic.resolveScene(c.sceneID, resolveCASConfID, resolveCASZone)
	require.NoError(t, err, "下一次请求从头再来")
	require.Equal(t, resolveNodeLate, nid)
	c.requireOwnedOnlyBy(t, resolveNodeLate, resolveNodeFirst)
	require.EqualValues(t, 2, c.late.fake.createCalls.Load())
}

// 只重建大世界频道,频道身份以 world_channels 成员资格为准:
//   - 副本 / 镜像(不是任何地图的频道)不在别处重建,留给死节点收尾销毁;
//   - 请求带来的 conf 对不上频道所属的地图时拒绝,不按它把频道重建成别的地图。
//
// 两种都一个字节不改、哪个节点都不建实体。修复前:按请求 conf 定用途并重建。
func TestResolveScene_DeadOwner_InstanceAndMismatchedConfAreNotRebuilt(t *testing.T) {
	t.Run("instance", func(t *testing.T) {
		c := newResolveDeadOwnerCase(t, 89006)
		member := strconv.FormatUint(c.sceneID, 10)
		_, err := c.mr.SRem(worldChannelsKey(resolveCASZone, resolveCASConfID), member)
		require.NoError(t, err)
		_, err = c.mr.ZAdd(activeInstancesKey(resolveCASZone), 1, member)
		require.NoError(t, err)

		_, _, err = c.logic.resolveScene(c.sceneID, resolveCASConfID, resolveCASZone)
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrReentryBarrierPending))
		c.requireStillOnDeadNode(t)
		require.EqualValues(t, 0, c.late.fake.createCalls.Load(), "副本不在别处重建")
		require.EqualValues(t, 0, c.first.fake.createCalls.Load())
	})
	t.Run("mismatched_conf", func(t *testing.T) {
		c := newResolveDeadOwnerCase(t, 89007)

		_, _, err := c.logic.resolveScene(c.sceneID, resolveCASConfID+1, resolveCASZone)
		require.Error(t, err)
		c.requireStillOnDeadNode(t)
		require.EqualValues(t, 0, c.late.fake.createCalls.Load(), "conf 对不上不得按它重建")
		require.EqualValues(t, 0, c.first.fake.createCalls.Load())
	})
}

// 缩容排空先把频道从在役集合里 SREM,之后才读属主、非原子地删映射。reassignSceneOffDeadNode 建实体之前读的
// 那次成员资格可能已经过期,所以提交原语要在 CAS 脚本里再核对一次:频道已不在役时什么都不写,
// 由调用方回收预建的实体;否则改派落地后会被排空删掉映射,新节点上留下孤儿实体与记账。
// 这里直接调用提交原语,模拟「建实体之后、提交之前,排空摘掉了频道」。
func TestResolveScene_DeadOwner_ChannelDrainedBeforeCommitChangesNothing(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 89013)
	channelSet := worldChannelsKey(resolveCASZone, resolveCASConfID)
	_, err := c.mr.SRem(channelSet, strconv.FormatUint(c.sceneID, 10))
	require.NoError(t, err)

	require.False(t, reassignWorldChannelNode(c.sc, resolveCASZone, c.sceneID, resolveNodeDead, resolveNodeLate, channelSet),
		"频道已被排空摘出在役集合,改派必须落空")
	c.requireStillOnDeadNode(t)
	require.False(t, c.mr.Exists(nodeSceneCountKey(resolveCASZone, resolveNodeLate)), "落空的一方不得建出计数键")

	// 对照:仍在役时,同一调用落地。
	_, err = c.mr.SAdd(channelSet, strconv.FormatUint(c.sceneID, 10))
	require.NoError(t, err)
	require.True(t, reassignWorldChannelNode(c.sc, resolveCASZone, c.sceneID, resolveNodeDead, resolveNodeLate, channelSet))
	c.requireOwnedOnlyBy(t, resolveNodeLate, resolveNodeFirst)
}

// scene:{id}:zone 读不出来(值损坏)时拒绝,不能当成「没记录 zone」放过跨 zone 守卫。NodeId 只在 zone 内
// 唯一:守卫被跳过时,别的 zone 的场景会被当成本 zone 的来判活、改派。修复前:GetSceneZone 把它读成 0 放行,
// 本用例的频道随后被重建到 late。
func TestResolveScene_DeadOwner_CorruptZoneRecordRejects(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 89008)
	require.NoError(t, c.mr.Set(sceneZoneKey(c.sceneID), "not-a-zone"))

	_, _, err := c.logic.resolveScene(c.sceneID, resolveCASConfID, resolveCASZone)
	require.Error(t, err)
	c.requireStillOnDeadNode(t)
	require.EqualValues(t, 0, c.late.fake.createCalls.Load())
}

// 老属主刚判死时回可重试的 ErrReentryBarrierPending,一个字节都不改,哪个节点都不建实体。
func TestResolveScene_DeadOwner_BarrierPendingChangesNothing(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 89009)
	require.NoError(t, markNodeDeath(c.sc, resolveCASZone, resolveNodeDead))

	_, _, err := c.logic.resolveScene(c.sceneID, resolveCASConfID, resolveCASZone)
	require.ErrorIs(t, err, ErrReentryBarrierPending)
	c.requireStillOnDeadNode(t)
	require.EqualValues(t, 0, c.late.fake.createCalls.Load())
	require.EqualValues(t, 0, c.first.fake.createCalls.Load())
}

// Agones 名额只跟着归属走:
//   - lostScene:CAS 落空、属主是别的节点 —— 一个名额都不动,那是改派成功的写者的事;
//   - adoptScene:CAS 落空、属主恰是本次挑中的节点,而先提交的写者没转名额 —— 由这里补转;
//   - wonScene:改派成功 —— 名额转到新节点,死节点 GameServer 上还掉一份。
//
// 修复前:实例与频道在这条路径上都不转名额,新节点永远少记一份。
//
// Agones 的节点 → Pod 映射要 knownNodes 里有独立的 Pod IP,所以本用例不用 bufconn 桩节点,
// CreateScene / DestroyScene 落到 TestMain 的共享桩上。
func TestResolveScene_DeadOwner_AgonesRoomFollowsOwnership(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)

	sc, mr := newIntegrationSvcCtx(t)
	installDefaultFakeSceneNode(t)
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-dead", podIP: "10.0.89.10", state: "Allocated", count: 3, capacity: 4},
		&fakeGameServer{name: "gs-first", podIP: "10.0.89.20", state: "Ready", capacity: 4},
		&fakeGameServer{name: "gs-late", podIP: "10.0.89.30", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, resolveNodeFirst, "10.0.89.20", resolveCASZone, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, resolveNodeLate, "10.0.89.30", resolveCASZone, constants.SceneNodeTypeMainWorld)
	setResolveCASLoadScores(t, mr)

	const lostScene, adoptScene, wonScene uint64 = 89010, 89011, 89012
	for _, id := range []uint64{lostScene, adoptScene, wonScene} {
		seedWorldChannelOwnedByDeadNode(t, mr, id)
		require.NoError(t, mr.Set(sceneAgonesGsKey(id), "gs-dead"))
	}
	logic := NewEnterSceneLogic(context.Background(), sc)

	// 落空,属主是 first:名额一个都不动。
	require.True(t, reassignSceneNode(sc, resolveCASZone, lostScene, resolveNodeDead, resolveNodeFirst))
	_, nid, err := logic.reassignSceneOffDeadNode(lostScene, resolveCASConfID, resolveCASZone, resolveNodeDead)
	require.NoError(t, err)
	require.Equal(t, resolveNodeFirst, nid)
	_, release, acquireOnGs := alloc.stats()
	require.Zero(t, acquireOnGs, "属主不是本次挑中的节点,不得在任何节点上占名额")
	require.Zero(t, release, "属主不是本次挑中的节点,不得归还死节点的名额")
	gs, err := mr.Get(sceneAgonesGsKey(lostScene))
	require.NoError(t, err)
	require.Equal(t, "gs-dead", gs)

	// 落空,属主恰是 late,先提交的写者没转名额:由这里补转。
	require.True(t, reassignSceneNode(sc, resolveCASZone, adoptScene, resolveNodeDead, resolveNodeLate))
	_, nid, err = logic.reassignSceneOffDeadNode(adoptScene, resolveCASConfID, resolveCASZone, resolveNodeDead)
	require.NoError(t, err)
	require.Equal(t, resolveNodeLate, nid)
	require.EqualValues(t, 1, alloc.roomsOf("gs-late"))
	require.EqualValues(t, 2, alloc.roomsOf("gs-dead"))
	gs, err = mr.Get(sceneAgonesGsKey(adoptScene))
	require.NoError(t, err)
	require.Equal(t, "gs-late", gs)

	// 改派成功:名额跟到 late,死节点 GameServer 上再还一份。
	_, nid, err = logic.resolveScene(wonScene, resolveCASConfID, resolveCASZone)
	require.NoError(t, err)
	require.Equal(t, resolveNodeLate, nid)
	require.EqualValues(t, 2, alloc.roomsOf("gs-late"), "新节点必须记账,否则 rooms Counter 少算")
	require.EqualValues(t, 1, alloc.roomsOf("gs-dead"), "死节点上只剩 lostScene 那一份")
	require.EqualValues(t, 0, alloc.roomsOf("gs-first"))
	gs, err = mr.Get(sceneAgonesGsKey(wonScene))
	require.NoError(t, err)
	require.Equal(t, "gs-late", gs)
}
