package logic

// 回归(2026-11-05):世界频道「改派提交之后」与「排空收尾死属主分支」的读后写竞态,即 docs/PROGRESS.md
// 2026-10-23「死节点收尾与显式 DestroyScene…」条目登记的残余 R2、R4(R2 原文把对象写成了 resolveScene,
// 实际仍是「CAS 之后再建」的是 initWorldScenesForZone 的死节点改派,见 PROGRESS 2026-11-05 条目)。
//
//   - init 的死节点改派(initWorldScenesForZone → moveWorldChannelOffDeadNode)以前是「CAS → 转名额 → CreateScene」。
//     CAS 落地之后、CreateScene 到达新节点之前,频道若被按新属主拆掉(这里用排空收尾:先向属主发 DestroyScene,
//     再按属主原子拆除),晚到的 CreateScene 在新节点上留下没有任何 Redis 状态的幽灵实体;CreateScene 失败时
//     归属也已经改了过去。现在先建后改,提交走带在役核对的 reassignWorldChannelNode。
//   - 排空收尾的死属主分支(drainOrDestroyChannel)以前不过再入屏障;收尾对死属主照扣 scene_count,死节点收尾
//     删掉计数键之后再扣,会把键重建成 -1。
//
// 交错的注入:
//   - 「改派提交之后」用 go-redis hook(afterEvalHook,经 newIntegrationSvcCtxWithRedisOptions 挂上):改派 CAS 脚本
//     执行完、结果交还调用方之前,同步跑一次排空收尾;
//   - 「改派提交之前」用 miniredis 前置钩子 identHookOnceBeforeEval(enterscene_identity_test.go):CAS 脚本执行之前
//     把频道摘出在役集合。
//
// 修复前红的取法:本文件只经修复前就有的入口(initWorldScenesForZone、drainOrDestroyChannel、
// sweepDrainingWorldChannels)进,只用修复前已有的辅助函数与常量;新的屏障点位用字面量 "autoscale_drain"
// (它也是面板与告警看到的 label 值)。所以可以只把本文件放到修复前的代码上跑,期望:
//   - TeardownRightAfterCommitLeavesNoGhost、CreateFailureKeepsOwnership、ChannelLeftServiceBeforeCommitReclaims、
//     DrainDeadOwner_WaitsForReentryBarrier、DrainDeadOwner_DoesNotRecreateDeletedSceneCount 五条红;
//   - BarrierSkipsBeforeCreate 修复前后都绿(修复前 CAS 里的屏障同样挡住了建实体),它钉的是新加的屏障预判,
//     靠变异取红:删掉 moveWorldChannelOffDeadNode 里的 reentryBarrierBlocks 预判,它会因 live 上多出一建一拆而红。
//
// 人数以成员集合 instance:{id}:members 为准,集合不存在即 0 人,用例不另摆人数。

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"scene_manager/internal/svc"
)

const (
	commitOrderZone   uint32 = 1
	commitOrderConfID uint64 = 620
)

// 节点号不与别的用例重名,理由同 reassign_cas_test.go 的 casNode* 常量:再入屏障会读进程内的推迟摘除队列。
const (
	commitOrderDeadNode  = "8710"
	commitOrderLiveNode  = "8720"
	drainOrderDeadOwner  = "8730"
	drainOrderBarrierKey = "autoscale_drain" // barrierSiteAutoscaleDrain 的值,见文件头
)

// afterEvalHook 在第一次执行 script 的 EVAL 返回之后、把结果交还调用方之前,同步执行一次 inject。
// 与 owner_read_interleave_test.go 的 afterOwnerReadHook 同一写法,只是按脚本正文认 EVAL。
// inject 里发出的命令同样经过本 hook;fired 先置位,所以不会重入。arm 之前一律放行。
type afterEvalHook struct {
	script string
	inject func()
	armed  atomic.Bool
	fired  atomic.Bool
}

func (h *afterEvalHook) arm(inject func()) {
	h.inject = inject
	h.armed.Store(true)
}

func (h *afterEvalHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *afterEvalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *afterEvalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if h.armed.Load() && isEvalOf(cmd, h.script) && h.fired.CompareAndSwap(false, true) {
			h.inject()
		}
		return err
	}
}

// isEvalOf 报告 cmd 是不是执行 script 的 EVAL(go-zero 的 Eval 发的是 EVAL,不是 EVALSHA)。
func isEvalOf(cmd redis.Cmder, script string) bool {
	args := cmd.Args()
	if cmd.Name() != "eval" || len(args) < 2 {
		return false
	}
	s, ok := args[1].(string)
	return ok && s == script
}

// initMoveCase 是 init 死节点改派的现场:世界频道 sceneID 归已死的 commitOrderDeadNode(不在 etcd 注册表、
// 不在负载集,拨号 Unavailable),commitOrderLiveNode 是唯一的活世界节点。init 先对属主发 CreateScene,
// 失败后判死,把频道改派到 live。
type initMoveCase struct {
	sc      *svc.ServiceContext
	mr      *miniredis.Miniredis
	live    *sceneNodeHarness
	sceneID uint64
}

func newInitMoveCase(t *testing.T, sceneID uint64, opts ...redis.Option) *initMoveCase {
	t.Helper()
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	markKnownNodesSyncedForTest(t)

	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, opts...)
	live := startFakeSceneNode(t, commitOrderLiveNode)
	installBufconnDialer(t, live)
	registerWorldNodeFromHarness(t, sc, commitOrderZone, live)

	// 未注册节点由 TestMain 的解析器解析成 "bufnet:<id>"(同 reassign_cas_test.go newWorldChannelRace)。
	deadEndpoint := "bufnet:" + commitOrderDeadNode
	prevDialer := nodeDialer
	restore := SetNodeDialerForTest(func(ctx context.Context, endpoint string) (*grpc.ClientConn, error) {
		if endpoint == deadEndpoint {
			return nil, status.Error(codes.Unavailable, "dead scene node")
		}
		return prevDialer(ctx, endpoint)
	})
	t.Cleanup(restore)

	member := strconv.FormatUint(sceneID, 10)
	_, err := mr.SAdd(worldChannelsKey(commitOrderZone, commitOrderConfID), member)
	require.NoError(t, err)
	require.NoError(t, mr.Set(sceneZoneKey(sceneID), strconv.FormatUint(uint64(commitOrderZone), 10)))
	seedSceneOwnedBy(t, mr, commitOrderZone, sceneID, commitOrderDeadNode)

	require.True(t, isNodeGoneFromRegistry(commitOrderZone, commitOrderDeadNode))
	require.False(t, IsNodeAlive(sc, commitOrderZone, commitOrderDeadNode))
	return &initMoveCase{sc: sc, mr: mr, live: live, sceneID: sceneID}
}

func (c *initMoveCase) runInit(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initWorldScenesForZone(ctx, c.sc, commitOrderZone, []uint64{commitOrderConfID}, true)
}

// requireStillOnDeadNode 断言归属与记账一个字节都没改,live 没记账。
func (c *initMoveCase) requireStillOnDeadNode(t *testing.T, msg string) {
	t.Helper()
	requireSceneOwner(t, c.sc, c.sceneID, commitOrderDeadNode, msg)
	requireSceneIndexedOnlyOn(t, c.sc, commitOrderZone, c.sceneID, commitOrderDeadNode, commitOrderLiveNode)
	require.EqualValues(t, 1, readInt64(c.sc, nodeSceneCountKey(commitOrderZone, commitOrderDeadNode)))
	require.False(t, c.mr.Exists(nodeSceneCountKey(commitOrderZone, commitOrderLiveNode)), "live 不得记账")
}

// 改派刚落地,排空就按新属主 live 把频道收尾了(先向 live 发 DestroyScene,再按属主原子拆除)。
// 修复后实体在改派之前就建好了,拆除拆得到它,之后不再有 CreateScene 到达 live。
// 修复前:CAS 落地时 live 上还没有实体,DestroyScene 先到、CreateScene 后到,live 上留下一个没有任何 Redis 状态、
// 也不会有任何路径来收的幽灵实体。
func TestInitDeadNodeReassign_TeardownRightAfterCommitLeavesNoGhost(t *testing.T) {
	const sceneID uint64 = 87101
	hook := &afterEvalHook{script: luaReassignSceneNode}
	c := newInitMoveCase(t, sceneID, redis.WithHook(hook))
	member := strconv.FormatUint(sceneID, 10)

	var createsAtCommit, createsAfterTeardown int64
	hook.arm(func() {
		owner, err := c.sc.Redis.Get(sceneNodeKey(sceneID))
		require.NoError(t, err)
		require.Equal(t, commitOrderLiveNode, owner, "前提:注入点在改派 CAS 落地之后")
		createsAtCommit = c.live.fake.createCalls.Load()

		// 排空在这时开始,成员集合为空(0 人),一拍收尾。
		_, err = c.sc.Redis.Srem(worldChannelsKey(commitOrderZone, commitOrderConfID), member)
		require.NoError(t, err)
		_, err = c.sc.Redis.Sadd(worldDrainingSetKey(commitOrderZone, commitOrderConfID), member)
		require.NoError(t, err)
		drainOrDestroyChannel(context.Background(), c.sc, commitOrderZone, commitOrderConfID, sceneID)
		createsAfterTeardown = c.live.fake.createCalls.Load()
	})

	c.runInit(t)

	require.True(t, hook.fired.Load(), "前提:init 的改派 CAS 必须真的执行过")
	require.EqualValues(t, 1, c.live.fake.destroyCalls.Load(), "前提:排空收尾向新属主 live 发了 DestroyScene")
	require.Equal(t, createsAfterTeardown, c.live.fake.createCalls.Load(),
		"拆除之后不得再有 CreateScene 到达 live:晚到的建场景会留下没有 Redis 状态的幽灵实体")
	require.EqualValues(t, 1, createsAtCommit, "改派落地时 live 上必须已经建好实体(先建后改)")
	requireSceneOwner(t, c.sc, sceneID, "", "频道已被排空收尾拆除")
	require.EqualValues(t, 0, readInt64(c.sc, nodeSceneCountKey(commitOrderZone, commitOrderLiveNode)),
		"live 的 scene_count:改派加一、收尾减一")
}

// 新节点 CreateScene 失败:归属与记账一个字节都不改,下一轮 init 从头再来。
// 修复前:先 CAS 后建,CreateScene 失败时归属已经指向 live,要等下一轮 init 才补建。
func TestInitDeadNodeReassign_CreateFailureKeepsOwnership(t *testing.T) {
	const sceneID uint64 = 87102
	c := newInitMoveCase(t, sceneID)
	c.live.fake.createShouldFail = status.Error(codes.Internal, "injected CreateScene failure")

	c.runInit(t)

	require.EqualValues(t, 1, c.live.fake.createCalls.Load(), "前提:init 向 live 发过 CreateScene")
	c.requireStillOnDeadNode(t, "CreateScene 失败时归属不得改到 live")
}

// init 读到频道列表之后、改派提交之前,频道开始排空(摘出在役集合)。在役核对让 CAS 落空,live 上预建的实体
// 被回收;频道留给排空收尾按死属主处理。
// 修复前:reassignSceneNode 不核对在役,把一个已开始排空的频道搬到 live 并建实体,随后还要排空再拆一次。
func TestInitDeadNodeReassign_ChannelLeftServiceBeforeCommitReclaims(t *testing.T) {
	const sceneID uint64 = 87103
	c := newInitMoveCase(t, sceneID)
	member := strconv.FormatUint(sceneID, 10)
	// 钩子跑在 miniredis 的服务端协程上:只能用 t.Errorf,不能用 require。
	fired := identHookOnceBeforeEval(t, c.mr, sceneNodeKey(sceneID), func() {
		if _, err := c.mr.SRem(worldChannelsKey(commitOrderZone, commitOrderConfID), member); err != nil {
			t.Errorf("simulate drain start: %v", err)
		}
		if _, err := c.mr.SAdd(worldDrainingSetKey(commitOrderZone, commitOrderConfID), member); err != nil {
			t.Errorf("simulate drain start: %v", err)
		}
	})

	c.runInit(t)

	require.True(t, fired.Load(), "前提:改派的 CAS 带上了 scene 的属主键")
	c.requireStillOnDeadNode(t, "已开始排空的频道不得被改派")
	require.EqualValues(t, 1, c.live.fake.createCalls.Load())
	require.EqualValues(t, 1, c.live.fake.destroyCalls.Load(), "落空时要回收 live 上预建的实体")
	require.Equal(t, sceneID, c.live.fake.lastDestroyScene.Load())
}

// 死属主刚判死、屏障未到:不建实体、不改归属,屏障记在 reassign 点位。
// 修复前后都绿(修复前 CAS 里的屏障同样挡住了建实体);它钉的是先建后改之后新加的屏障预判 ——
// 少了它,屏障窗口里每一轮 init 都会在 live 上白建白拆一次。
func TestInitDeadNodeReassign_BarrierSkipsBeforeCreate(t *testing.T) {
	const sceneID uint64 = 87104
	c := newInitMoveCase(t, sceneID)
	require.NoError(t, markNodeDeath(c.sc, commitOrderZone, commitOrderDeadNode))
	blockedBefore := barrierBlockedCountForTest(t, commitOrderZone, barrierSiteReassign)

	c.runInit(t)

	c.requireStillOnDeadNode(t, "屏障未到,归属一个字节都不改")
	require.EqualValues(t, 0, c.live.fake.createCalls.Load(), "屏障窗口里不得在 live 上建实体")
	require.EqualValues(t, 0, c.live.fake.destroyCalls.Load())
	require.Greater(t, barrierBlockedCountForTest(t, commitOrderZone, barrierSiteReassign), blockedBefore)
}

// seedDrainingChannelOwnedByDeadNode:频道 102 已在排空(不在在役集合、在排空集合),属主 drainOrderDeadOwner
// 已摘出负载集,成员集合为空(0 人)。
func seedDrainingChannelOwnedByDeadNode(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	resetNodeDeathQueuesForTest(t)
	sc, mr := newAutoscaleCtx(t)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(102), drainOrderDeadOwner))
	require.NoError(t, sc.Redis.Set(sceneZoneKey(102), strconv.FormatUint(uint64(testZoneId), 10)))
	require.NoError(t, sc.Redis.Setex(sceneDrainingKey(102), "1", 300))
	_, err := sc.Redis.Sadd(worldDrainingSetKey(testZoneId, autoscaleTestConfID), "102")
	require.NoError(t, err)
	_, err = sc.Redis.Sadd(nodeScenesKey(testZoneId, drainOrderDeadOwner), "102")
	require.NoError(t, err)
	require.False(t, IsNodeAlive(sc, testZoneId, drainOrderDeadOwner))
	return sc, mr
}

func requireDrainingMember(t *testing.T, sc *svc.ServiceContext, sceneID uint64, want bool) {
	t.Helper()
	in, err := sc.Redis.Sismember(worldDrainingSetKey(testZoneId, autoscaleTestConfID), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	require.Equal(t, want, in)
}

// 排空中的频道属主刚判死:屏障未到时收尾什么都不做,频道留在排空集合;屏障过后照常收尾。
// 修复前:死属主分支不过屏障,老节点还在 emergency drain 里存盘时就删掉映射、成员集合与名额记录。
func TestDrainDeadOwner_WaitsForReentryBarrier(t *testing.T) {
	sc, mr := seedDrainingChannelOwnedByDeadNode(t)
	require.NoError(t, sc.Redis.Set(nodeSceneCountKey(testZoneId, drainOrderDeadOwner), "1"))
	require.NoError(t, markNodeDeath(sc, testZoneId, drainOrderDeadOwner))
	ctx := context.Background()
	blockedBefore := barrierBlockedCountForTest(t, testZoneId, drainOrderBarrierKey)

	sweepDrainingWorldChannels(ctx, sc, testZoneId)
	requireSceneOwner(t, sc, 102, drainOrderDeadOwner, "屏障未到,收尾不得删映射")
	requireDrainingMember(t, sc, 102, true)
	require.Equal(t, blockedBefore+1, barrierBlockedCountForTest(t, testZoneId, drainOrderBarrierKey))

	// 删掉 death_at 等同于屏障已过(同 node_detach_deferred_test.go)。
	mr.Del(nodeDeathAtKey(testZoneId, drainOrderDeadOwner))
	sweepDrainingWorldChannels(ctx, sc, testZoneId)
	requireSceneOwner(t, sc, 102, "", "屏障过后照常收尾")
	requireDrainingMember(t, sc, 102, false)
}

// 死属主的计数键已被死节点收尾删掉,随后排空收尾:不得把计数键重建出来。
// 修复前:无条件 Incrby -1,键被重建成 -1,复用同一 node_id 的新进程继承这个负数。
func TestDrainDeadOwner_DoesNotRecreateDeletedSceneCount(t *testing.T) {
	sc, mr := seedDrainingChannelOwnedByDeadNode(t)
	require.False(t, mr.Exists(nodeSceneCountKey(testZoneId, drainOrderDeadOwner)), "前提:计数键已被删掉")

	sweepDrainingWorldChannels(context.Background(), sc, testZoneId)

	requireSceneOwner(t, sc, 102, "", "前提:收尾已执行")
	require.False(t, mr.Exists(nodeSceneCountKey(testZoneId, drainOrderDeadOwner)), "死属主的 scene_count 键不得被重建")
	in, err := sc.Redis.Sismember(nodeScenesKey(testZoneId, drainOrderDeadOwner), "102")
	require.NoError(t, err)
	require.False(t, in, "反向索引照常摘掉")
}
