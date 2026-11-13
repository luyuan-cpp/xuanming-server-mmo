package logic

// 回归(2026-11-06):世界频道的放置代次(docs/design/world-channel-placement-generation.md)。
//
// 改派落空后回收预建实体的 DestroyScene 可能晚到(本端超时,请求卡在连接里,分区恢复后才送达)。晚到时频道可能已被
// 合法地改派到同一个节点,按 scene_id 拆就拆掉了那次合法放置的实体。上午的 60 秒回收栅栏只盖住 60 秒以内的晚到;现在每次
// 换节点分配一个代次,建场景、提交、回收、拆老实体都带它,节点忽略比自己见过的建场景更旧的拆场景(节点那一半见
// cpp/tests/cross_zone_test/scene_placement_fence_test.cpp)。这里守 scene_manager 这一半:
//   - 同一次放置的建场景、CAS、拆老实体带同一个代次,落地时高水位写成它;
//   - 代次不比高水位新的 CAS 落空;
//   - 回收先把高水位抬到回收的代次,所以之后落地的放置一定比晚到的回收新;回收得晚、代次却更旧的那次拆不到新放置;
//   - init 补建带频道当前的代次;拆除任务删高水位。
//
// 交错用 go-redis hook 注入(beforeEvalHook):改派 CAS 的 EVAL 发出之前,同步跑一次「另一个写者的回收」。
//
// 修复前红的取法(修复前没有代次,这些用例编不过,按变异取红;每次一种,在 %TEMP% 的副本里做):
//   - G1 删掉 luaReassignSceneNode 里的 `if not generation_after(ARGV[5], ...) then return 3 end`:
//     StaleProposalChangesNothing、NewerReclaimInvalidatesOlderMove 红。
//   - G2 删掉 luaReassignSceneNode 里的 `redis.call('SET', KEYS[7], ARGV[5])`:MoveCarriesOneGeneration、
//     StaleProposalChangesNothing、OlderReclaimCannotHitNewerMove 红。
//   - G3 删掉 luaFenceReclaim 里抬高水位的 SET:NewerReclaimInvalidatesOlderMove、ReclaimKeepsOwnersEntityAndRaisesHighWater、
//     ReclaimOfTornDownChannelWritesNothing(后半段)、ResolveSceneAdoptRaisesHighWater、ResolveSceneReclaimCarriesItsGeneration 红。
//   - G4 luaFenceReclaim 不论频道还在不在都抬(去掉 owner / SISMEMBER 条件):ReclaimOfTornDownChannelWritesNothing 红。
//   - G5 generation_after 换成直接 `return a > b`(字典序):StaleProposalChangesNothing 的「10 比 9 新」红。
//   - G6 init 补建换回不带代次(传 0):InitEnsureCreatesAtCurrentGeneration 红。
//   - G7 migrateWorldChannel 拆老实体换回 RequestNodeDestroyScene(不带代次):MoveCarriesOneGeneration 红。
//   - G8 删掉 runWorldDrainTeardown / runWorldOrphanTeardown 里的 deletePlacementGeneration:TeardownDeletesHighWater 红。
//   - G9 删掉 DestroySceneLogic 里 `membership != worldChannelNotMember` 那段拒绝:ExplicitDestroyScene_RejectsWorldChannels 红。
//   - G10 DestroySceneLogic 把 locateWorldChannel 的读错当成「不是线」放过去:ExplicitDestroyScene_MembershipReadErrorRejects 红。

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

const (
	placementZone   uint32 = 1
	placementConfID uint64 = 623
)

// 节点号不与别的用例重名,理由同 reassign_cas_test.go 的 casNode* 常量。
const (
	placementNodeOwner  = "8740"
	placementNodeTarget = "8750"
)

// freshPlacementGeneration 分配一个新的放置代次,给「模拟另一个写者提交一次改派」的用例用:生产里的改派者都先分配代次、
// 带着它建场景,再用同一个代次提交。
func freshPlacementGeneration(t *testing.T, sc *svc.ServiceContext, zoneID uint32) uint64 {
	t.Helper()
	generation, err := allocatePlacementGeneration(sc, zoneID)
	require.NoError(t, err)
	return generation
}

// placementGenOf 读频道的代次高水位,键不存在时为 0。
func placementGenOf(t *testing.T, mr *miniredis.Miniredis, sceneID uint64) uint64 {
	t.Helper()
	if !mr.Exists(scenePlacementGenKey(placementZone, sceneID)) {
		return 0
	}
	raw, err := mr.Get(scenePlacementGenKey(placementZone, sceneID))
	require.NoError(t, err)
	generation, err := strconv.ParseUint(raw, 10, 64)
	require.NoError(t, err)
	return generation
}

// beforeEvalHook 在第一次执行 script 的 EVAL 发出之前,同步执行一次 inject。inject 里发出的命令同样经过本 hook;
// fired 先置位,所以不会重入。arm 之前一律放行。
type beforeEvalHook struct {
	script string
	inject func()
	armed  atomic.Bool
	fired  atomic.Bool
}

func (h *beforeEvalHook) arm(inject func()) {
	h.inject = inject
	h.armed.Store(true)
}

func (h *beforeEvalHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *beforeEvalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *beforeEvalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.armed.Load() && isEvalOf(cmd, h.script) && h.fired.CompareAndSwap(false, true) {
			h.inject()
		}
		return next(ctx, cmd)
	}
}

// placementMoveCase 是 rebalance 把频道从活节点 owner 迁到 target 的现场:两个活的世界节点,频道 sceneID 归 owner。
type placementMoveCase struct {
	sc            *svc.ServiceContext
	mr            *miniredis.Miniredis
	owner, target *sceneNodeHarness
	sceneID       uint64
}

func newPlacementMoveCase(t *testing.T, sceneID uint64, opts ...redis.Option) *placementMoveCase {
	t.Helper()
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)

	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, opts...)
	owner := startFakeSceneNode(t, placementNodeOwner)
	target := startFakeSceneNode(t, placementNodeTarget)
	installBufconnDialer(t, owner, target)
	registerWorldNodeFromHarness(t, sc, placementZone, owner)
	registerWorldNodeFromHarness(t, sc, placementZone, target)

	_, err := mr.SAdd(worldChannelsKey(placementZone, placementConfID), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	seedSceneOwnedBy(t, mr, placementZone, sceneID, placementNodeOwner)
	return &placementMoveCase{sc: sc, mr: mr, owner: owner, target: target, sceneID: sceneID}
}

func (c *placementMoveCase) migrate(ctx context.Context) bool {
	return migrateWorldChannel(ctx, c.sc, placementZone, placementConfID, c.sceneID, placementNodeOwner, placementNodeTarget, reasonBetterHome)
}

// 一次迁移的建场景、CAS、拆老实体带同一个代次,落地时高水位写成它。
func TestPlacementGeneration_MoveCarriesOneGeneration(t *testing.T) {
	c := newPlacementMoveCase(t, 87401)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.True(t, c.migrate(ctx))

	generation := c.target.fake.lastCreateGeneration.Load()
	require.NotZero(t, generation, "换节点的建场景必须带代次")
	require.Equal(t, generation, placementGenOf(t, c.mr, c.sceneID), "落地时高水位写成这次的代次")
	require.EqualValues(t, 1, c.owner.fake.destroyCalls.Load())
	require.Equal(t, generation, c.owner.fake.lastDestroyGeneration.Load(),
		"拆老实体带同一个代次:频道之后迁回 owner 时,晚到的这次拆除会被节点当成过期的忽略")
}

// 代次不比高水位新的 CAS 一个键都不动;更新的代次照常落地,高水位跟着走。代次按数值比,不按字典序。
func TestPlacementGeneration_StaleProposalChangesNothing(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 87402
	seedSceneOwnedBy(t, mr, placementZone, sceneID, placementNodeOwner)
	require.NoError(t, mr.Set(scenePlacementGenKey(placementZone, sceneID), "5"))

	require.False(t, reassignSceneNode(sc, placementZone, sceneID, placementNodeOwner, placementNodeTarget, 5), "与高水位相同的代次必须落空")
	require.False(t, reassignSceneNode(sc, placementZone, sceneID, placementNodeOwner, placementNodeTarget, 4), "更旧的代次必须落空")
	require.False(t, reassignSceneNode(sc, placementZone, sceneID, placementNodeOwner, placementNodeTarget, 0), "没有代次的改派必须落空")
	requireSceneOwner(t, sc, sceneID, placementNodeOwner)
	requireSceneIndexedOnlyOn(t, sc, placementZone, sceneID, placementNodeOwner, placementNodeTarget)
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(placementZone, placementNodeOwner)))
	require.False(t, mr.Exists(nodeSceneCountKey(placementZone, placementNodeTarget)), "落空的一方不得建出计数键")
	require.EqualValues(t, 5, placementGenOf(t, mr, sceneID))

	require.True(t, reassignSceneNode(sc, placementZone, sceneID, placementNodeOwner, placementNodeTarget, 6))
	requireSceneOwner(t, sc, sceneID, placementNodeTarget)
	require.EqualValues(t, 6, placementGenOf(t, mr, sceneID))

	require.NoError(t, mr.Set(scenePlacementGenKey(placementZone, sceneID), "9"))
	require.True(t, reassignSceneNode(sc, placementZone, sceneID, placementNodeTarget, placementNodeOwner, 10), "10 比 9 新(按字典序比会判反)")
	require.EqualValues(t, 10, placementGenOf(t, mr, sceneID))
}

// 另一个写者在 rebalance 分配代次之后才开始,它改派到 target 落空,在 rebalance 建完场景、提交之前回收 target 上的实体。
// 回收把高水位抬过了 rebalance 的代次:rebalance 的 CAS 落空,按自己的代次再回收一次同一份实体;归属留在 owner,
// target 不记账。高水位停在更新的那次回收上,不被 rebalance 自己的回收拉低。
func TestPlacementGeneration_NewerReclaimInvalidatesOlderMove(t *testing.T) {
	hook := &beforeEvalHook{script: luaReassignSceneNode}
	c := newPlacementMoveCase(t, 87403, redis.WithHook(hook))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var otherGeneration uint64
	hook.arm(func() {
		otherGeneration = freshPlacementGeneration(t, c.sc, placementZone)
		destroyUnownedMigrationTarget(ctx, c.sc, placementZone, placementConfID, c.sceneID, placementNodeTarget, otherGeneration)
	})

	migrated := c.migrate(ctx)

	require.True(t, hook.fired.Load(), "前提:另一个写者的回收插在 rebalance 的 CAS 之前")
	require.False(t, migrated, "回收已把高水位抬过本次的代次,本次改派必须落空")
	requireSceneOwner(t, c.sc, c.sceneID, placementNodeOwner, "归属必须留在 owner")
	requireSceneIndexedOnlyOn(t, c.sc, placementZone, c.sceneID, placementNodeOwner, placementNodeTarget)
	require.EqualValues(t, 1, readInt64(c.sc, nodeSceneCountKey(placementZone, placementNodeOwner)))
	require.False(t, c.mr.Exists(nodeSceneCountKey(placementZone, placementNodeTarget)), "target 不得记账")

	moveGeneration := c.target.fake.lastCreateGeneration.Load()
	require.Less(t, moveGeneration, otherGeneration)
	require.Equal(t, otherGeneration, placementGenOf(t, c.mr, c.sceneID))
	require.EqualValues(t, 2, c.target.fake.destroyCalls.Load(), "另一个写者拆一次,rebalance 落空后按自己的代次再拆一次")
	require.Equal(t, moveGeneration, c.target.fake.lastDestroyGeneration.Load())
	require.EqualValues(t, 0, c.owner.fake.destroyCalls.Load(), "迁移作废,owner 上的活实体不得拆")
}

// 本条是这次要关掉的残余本身。另一个写者比 rebalance 先分配代次,它的回收却在 rebalance 建完场景之后、提交之前才跑
// (换成生产上的形状:它的 DestroyScene 在路上卡了很久)。rebalance 的代次更新,改派照常落地,不用等任何时限;
// target 先按 rebalance 的代次建了场景、后收到按更旧代次的拆除,节点据此把那次拆除当成过期的忽略。
// 修复前(上午的 60 秒栅栏):回收立了栅栏,这次改派落空;栅栏过期之后的改派落地,再晚到的 DestroyScene 照样拆掉它的实体。
func TestPlacementGeneration_OlderReclaimCannotHitNewerMove(t *testing.T) {
	hook := &beforeEvalHook{script: luaReassignSceneNode}
	c := newPlacementMoveCase(t, 87404, redis.WithHook(hook))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	otherGeneration := freshPlacementGeneration(t, c.sc, placementZone)
	hook.arm(func() {
		destroyUnownedMigrationTarget(ctx, c.sc, placementZone, placementConfID, c.sceneID, placementNodeTarget, otherGeneration)
	})

	migrated := c.migrate(ctx)

	require.True(t, hook.fired.Load(), "前提:更旧的回收插在 rebalance 建完场景之后、CAS 之前")
	require.True(t, migrated, "本次的代次比回收的新,改派必须落地")
	requireSceneOwner(t, c.sc, c.sceneID, placementNodeTarget)
	moveGeneration := c.target.fake.lastCreateGeneration.Load()
	require.Greater(t, moveGeneration, otherGeneration)
	require.Equal(t, moveGeneration, placementGenOf(t, c.mr, c.sceneID))
	require.EqualValues(t, 1, c.target.fake.destroyCalls.Load())
	require.Equal(t, otherGeneration, c.target.fake.lastDestroyGeneration.Load(),
		"发给 target 的拆除带的是更旧的代次,节点已按更新的代次建过这个场景,会忽略它")
}

// 比对时 node 已是属主:实体在用,不回收;高水位照样抬到这次的代次(node 上的实体可能正是按它建的),更旧的回收不把它拉低。
func TestPlacementGeneration_ReclaimKeepsOwnersEntityAndRaisesHighWater(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const sceneID uint64 = 87405
	seedSceneOwnedBy(t, mr, placementZone, sceneID, placementNodeTarget)
	require.NoError(t, mr.Set(scenePlacementGenKey(placementZone, sceneID), "3"))
	destroysBefore := sharedFakeSceneNode.destroyCalls.Load()

	destroyUnownedMigrationTarget(context.Background(), sc, placementZone, placementConfID, sceneID, placementNodeTarget, 8)
	require.Equal(t, destroysBefore, sharedFakeSceneNode.destroyCalls.Load(), "属主就是 target,不得回收它的实体")
	require.EqualValues(t, 8, placementGenOf(t, mr, sceneID), "高水位抬到这次的代次")

	destroyUnownedMigrationTarget(context.Background(), sc, placementZone, placementConfID, sceneID, placementNodeTarget, 2)
	require.EqualValues(t, 8, placementGenOf(t, mr, sceneID), "更旧的回收不把高水位拉低")
}

// 频道已被拆除(没有映射,也不在在役集合):预建的实体照样回收,但不再写出高水位 —— 拆除任务删过它,再写就没人删。
// 仍在役、只是没有映射的频道要抬:懒改派可能以空属主把它派到这个节点。
func TestPlacementGeneration_ReclaimOfTornDownChannelWritesNothing(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const tornDown, unmapped uint64 = 87406, 87407
	destroysBefore := sharedFakeSceneNode.destroyCalls.Load()

	destroyUnownedMigrationTarget(context.Background(), sc, placementZone, placementConfID, tornDown, placementNodeTarget, 4)
	require.Equal(t, destroysBefore+1, sharedFakeSceneNode.destroyCalls.Load(), "没有属主,预建的实体照样回收")
	require.EqualValues(t, 4, sharedFakeSceneNode.lastDestroyGeneration.Load())
	require.False(t, mr.Exists(scenePlacementGenKey(placementZone, tornDown)), "频道已拆除,不得再写出高水位")

	_, err := mr.SAdd(worldChannelsKey(placementZone, placementConfID), strconv.FormatUint(unmapped, 10))
	require.NoError(t, err)
	destroyUnownedMigrationTarget(context.Background(), sc, placementZone, placementConfID, unmapped, placementNodeTarget, 4)
	require.EqualValues(t, 4, placementGenOf(t, mr, unmapped))
}

// init 向属主补建带频道当前的代次;从没换过节点的频道带 0。
func TestPlacementGeneration_InitEnsureCreatesAtCurrentGeneration(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	confIDs := []uint64{readUnknownWorldConf}

	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	channels, err := GetAllWorldChannels(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, channels[0], sharedFakeSceneNode.lastCreateScene.Load())
	require.Zero(t, sharedFakeSceneNode.lastCreateGeneration.Load(), "新频道按代次 0 建")

	require.NoError(t, mr.Set(scenePlacementGenKey(testZoneId, channels[0]), "7"))
	createsBefore := sharedFakeSceneNode.createCalls.Load()
	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	require.Equal(t, createsBefore+1, sharedFakeSceneNode.createCalls.Load())
	require.Equal(t, channels[0], sharedFakeSceneNode.lastCreateScene.Load())
	require.EqualValues(t, 7, sharedFakeSceneNode.lastCreateGeneration.Load(), "补建必须带频道当前的代次")
}

// 高水位的值损坏时本轮不补建这个频道(按 0 去建会被已记下更新拆除的节点拒收),计入 read_unknown。
func TestPlacementGeneration_CorruptHighWaterSkipsEnsure(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	confIDs := []uint64{readUnknownWorldConf}
	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	channels, err := GetAllWorldChannels(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	require.Len(t, channels, 1)

	require.NoError(t, mr.Set(scenePlacementGenKey(testZoneId, channels[0]), "not-a-number"))
	createsBefore := sharedFakeSceneNode.createCalls.Load()
	before := readUnknownCount(t, testZoneId, readUnknownSiteWorldInit)
	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	require.Equal(t, createsBefore, sharedFakeSceneNode.createCalls.Load())
	require.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteWorldInit))
	requireSceneOwner(t, sc, channels[0], "10")
}

// 世界频道的两种拆除任务(排空收尾、孤儿清理)都删高水位:这把键没有 TTL,漏删就一直留着。
func TestPlacementGeneration_TeardownDeletesHighWater(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	for i, kind := range []string{teardownKindWorldDrain, teardownKindWorldOrphan} {
		sceneID := uint64(87408 + i)
		require.NoError(t, mr.Set(scenePlacementGenKey(testZoneId, sceneID), "12"))
		job := sceneTeardownJob{ZoneID: testZoneId, SceneID: sceneID, Kind: kind, Reason: "placement_generation_test"}

		require.NoError(t, runSceneTeardown(context.Background(), sc, job, nil), kind)
		require.False(t, mr.Exists(scenePlacementGenKey(testZoneId, sceneID)), "%s 的拆除任务必须删高水位", kind)
	}
}

// resolveScene 的死节点自愈 CAS 落空、属主恰是本次挑中的节点(另一个写者以更旧的代次派到了同一个节点):采用预建的实体,
// 高水位抬到本次的代次 —— 节点上的这份实体就是按它建的,之后迁出时按更新代次发的拆除才拆得到它。
func TestPlacementGeneration_ResolveSceneAdoptRaisesHighWater(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 87410)
	otherGeneration := freshPlacementGeneration(t, c.sc, resolveCASZone)
	require.True(t, reassignSceneNode(c.sc, resolveCASZone, c.sceneID, resolveNodeDead, resolveNodeLate, otherGeneration))

	_, nid, err := c.logic.reassignSceneOffDeadNode(c.sceneID, resolveCASConfID, resolveCASZone, resolveNodeDead)
	require.NoError(t, err)
	require.Equal(t, resolveNodeLate, nid)

	tried := c.late.fake.lastCreateGeneration.Load()
	require.Greater(t, tried, otherGeneration)
	require.Equal(t, tried, placementGenOf(t, c.mr, c.sceneID))
	require.EqualValues(t, 0, c.late.fake.destroyCalls.Load(), "late 上是属主的活场景,不得回收")
}

// resolveScene 的死节点自愈 CAS 落空、属主是别的活节点:按本次的代次回收预建的实体,并把高水位抬到它。
func TestPlacementGeneration_ResolveSceneReclaimCarriesItsGeneration(t *testing.T) {
	c := newResolveDeadOwnerCase(t, 87411)
	require.True(t, reassignSceneNode(c.sc, resolveCASZone, c.sceneID, resolveNodeDead, resolveNodeFirst,
		freshPlacementGeneration(t, c.sc, resolveCASZone)))

	_, nid, err := c.logic.reassignSceneOffDeadNode(c.sceneID, resolveCASConfID, resolveCASZone, resolveNodeDead)
	require.NoError(t, err)
	require.Equal(t, resolveNodeFirst, nid)

	tried := c.late.fake.lastCreateGeneration.Load()
	require.NotZero(t, tried)
	require.EqualValues(t, 1, c.late.fake.destroyCalls.Load())
	require.Equal(t, tried, c.late.fake.lastDestroyGeneration.Load(), "回收按这次尝试的代次拆")
	require.Equal(t, tried, placementGenOf(t, c.mr, c.sceneID), "回收之前把高水位抬到这次的代次")
}

// 显式 DestroyScene 只拆副本与镜像:目标是世界频道(在役或回收中)时拒绝,一个键不动、不发 C++ DestroyScene;副本照常拆。
// 修复前:世界频道也按副本销毁,删了映射却留在在役集合里,随后被懒改派以空属主复活;那次拆除不带放置代次,晚到时会拆掉
// 复活在同一节点上的实体(docs/design/world-channel-placement-generation.md §7)。
func TestExplicitDestroyScene_RejectsWorldChannels(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	t.Cleanup(SetWorldConfIdsForTest([]uint64{placementConfID}))
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	const inService, draining uint64 = 87412, 87413
	_, err := mr.SAdd(worldChannelsKey(testZoneId, placementConfID), strconv.FormatUint(inService, 10))
	require.NoError(t, err)
	_, err = mr.SAdd(worldDrainingSetKey(testZoneId, placementConfID), strconv.FormatUint(draining, 10))
	require.NoError(t, err)
	for _, id := range []uint64{inService, draining} {
		seedSceneOwnedBy(t, mr, testZoneId, id, "10")
	}
	destroysBefore := sharedFakeSceneNode.destroyCalls.Load()

	for _, id := range []uint64{inService, draining} {
		_, err := NewDestroySceneLogic(ctx, sc).DestroyScene(&scene_manager.DestroySceneRequest{SceneId: id, ZoneId: testZoneId})
		require.ErrorIs(t, err, errWorldChannelNotDestroyable, "scene %d", id)
		requireSceneOwner(t, sc, id, "10", "拒绝时映射一个字节都不动")
		requireSceneIndexedOnlyOn(t, sc, testZoneId, id, "10")
	}
	stillListed, err := sc.Redis.Sismember(worldChannelsKey(testZoneId, placementConfID), strconv.FormatUint(inService, 10))
	require.NoError(t, err)
	require.True(t, stillListed, "拒绝时频道仍在役")
	require.Equal(t, destroysBefore, sharedFakeSceneNode.destroyCalls.Load(), "拒绝时不发 C++ DestroyScene")

	// 对照:副本照常拆。
	resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001, ZoneId: testZoneId, SceneType: scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)
	require.Zero(t, resp.ErrorCode)
	_, err = NewDestroySceneLogic(ctx, sc).DestroyScene(&scene_manager.DestroySceneRequest{SceneId: resp.SceneId, ZoneId: testZoneId})
	require.NoError(t, err)
	require.False(t, mr.Exists(sceneNodeKey(testZoneId, resp.SceneId)), "副本照常拆")
}

// 查不清目标是不是世界频道(读线集合失败)时拒绝,一个键不动;读恢复之后照常拆副本。
func TestExplicitDestroyScene_MembershipReadErrorRejects(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	t.Cleanup(SetWorldConfIdsForTest([]uint64{placementConfID}))
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)
	resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001, ZoneId: testZoneId, SceneType: scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)
	require.Zero(t, resp.ErrorCode)
	owner, err := mr.Get(sceneNodeKey(testZoneId, resp.SceneId))
	require.NoError(t, err)

	fired := injectRedisFaultsForTest(t, mr,
		redisFaultRule{cmd: "SISMEMBER", key: worldChannelsKey(testZoneId, placementConfID), times: 1})
	_, err = NewDestroySceneLogic(ctx, sc).DestroyScene(&scene_manager.DestroySceneRequest{SceneId: resp.SceneId, ZoneId: testZoneId})
	require.EqualValues(t, 1, fired.Load(), "前提:读线集合失败了一次")
	require.Error(t, err)
	require.NotErrorIs(t, err, errWorldChannelNotDestroyable, "读失败不是「是世界频道」")
	requireSceneOwner(t, sc, resp.SceneId, owner, "查不清时一个键都不动")

	_, err = NewDestroySceneLogic(ctx, sc).DestroyScene(&scene_manager.DestroySceneRequest{SceneId: resp.SceneId, ZoneId: testZoneId})
	require.NoError(t, err)
	require.False(t, mr.Exists(sceneNodeKey(testZoneId, resp.SceneId)))
}
