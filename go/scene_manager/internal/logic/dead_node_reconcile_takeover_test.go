package logic

// 回归:死节点收尾的水平扩展缺口(docs/PROGRESS.md 2026-10-23「死节点收尾与显式 DestroyScene 的销毁改为按认领时
// 属主」条目的残余 R1,2026-10-30 修)。修复前收尾任务与判死快照只在领导者进程内存里:
//   - drain 没有领导者闸门,旧领导者降级后仍以跟随者身份删死节点的计数并出队;
//   - 换了领导者(另一个进程)之后新领导者看不到任务:它的 fullSync 只扫负载集,死节点已经摘出去了;
//   - 收尾有场景出错(Redis 报错,结论未知)也照样删计数、出队,没有重试。
//
// 现在任务落 Redis(reentry_barrier.go「被屏障推迟的死节点收尾」),任何新领导者都能接手。
//
// 「换领导者」的模拟:新领导者是另一个进程,手里没有旧领导者的任何进程内状态 —— 用 resetNodeDeathQueuesForTest
// 抹掉进程内的死节点状态,Redis 原样保留。fullSync 的领导者分支调的就是 drainPendingDeadNodeReconciles,这里直接调它。
// 用例只从修复前就存在的入口进(removeNodeFromRedis、drainPendingDeadNodeReconciles),只用修复前已有的辅助函数,
// 断言只看 Redis 与计数器,所以能原样放到修复前代码上取红。
//
// 修复前红的取法:不改主仓文件,禁止 stash / reset。在 %TEMP% 下 git worktree add --detach <tmp> 720179638
// (修复前代码),把本文件拷进 <tmp>/go/scene_manager/internal/logic/,在 <tmp>/go/scene_manager 下跑
// go test ./internal/logic/ -count=1 -v -run 'TestDeadNodeReconcileTakeover_',期望四条都红:
//   - NewLeaderFinishesTaskQueuedInBarrierWindow:失败信息含「新领导者必须接手」;
//   - FollowerLeavesCountersAndTaskAlone:失败信息含「跟随者不得删死节点的计数」;
//   - FailedSceneRetriedNextTick:失败信息含「有场景没结论时不得删死节点的计数」;
//   - LeadershipLostMidSweepRestTakenOver:失败信息含「剩下的场景必须由新领导者接手」。
// 修复前的 resetNodeDeathQueuesForTest 清的是进程内的收尾队列,同样等价于「换成另一个进程」。

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"scene_manager/internal/svc"
)

const takeoverZone = testZoneId

// 每个实例里的玩家数。
const takeoverScenePlayers = 2

// takeoverCase:一个场景节点刚死,名下有几个实例(属主映射、反向索引、active 集合、实例人数齐全),
// 死节点的 scene_count / 成员集合与之对应。leading 控制本进程是不是领导者。
// 节点号 8990~8993、场景号 899xx 不与别的用例重名:再入屏障会读进程内的推迟摘除队列。
type takeoverCase struct {
	sc      *svc.ServiceContext
	mr      *miniredis.Miniredis
	leading *atomic.Bool
	node    string
	scenes  []uint64
}

func newTakeoverCase(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, node string, scenes ...uint64) *takeoverCase {
	t.Helper()
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	leading := &atomic.Bool{}
	leading.Store(true)
	t.Cleanup(SetLeaderCheckForTest(leading.Load))

	_, err := mr.ZAdd(nodeLoadKey(takeoverZone), 0, node)
	require.NoError(t, err)
	for _, id := range scenes {
		member := strconv.FormatUint(id, 10)
		require.NoError(t, mr.Set(sceneZoneKey(id), strconv.FormatUint(uint64(takeoverZone), 10)))
		require.NoError(t, mr.Set(sceneNodeKey(id), node))
		_, err = mr.SAdd(nodeScenesKey(takeoverZone, node), member)
		require.NoError(t, err)
		_, err = mr.ZAdd(activeInstancesKey(takeoverZone), float64(nowUnix()), member)
		require.NoError(t, err)
		seedSceneMembers(mr, id, takeoverScenePlayers)
	}
	require.NoError(t, mr.Set(nodeSceneCountKey(takeoverZone, node), strconv.Itoa(len(scenes))))
	seedNodeMembers(mr, takeoverZone, node, takeoverScenePlayers*len(scenes))
	return &takeoverCase{sc: sc, mr: mr, leading: leading, node: node, scenes: scenes}
}

// declareDead 走领导者的判死路径:写 death_at、摘负载集、入收尾队列。屏障从此刻起算。
func (c *takeoverCase) declareDead(t *testing.T) {
	t.Helper()
	removeNodeFromRedis(c.sc, deadNodeEntryForTest(takeoverZone, c.node))
	require.NotContains(t, loadSetMembersForTest(t, c.mr, takeoverZone), c.node, "前提:死节点已摘出负载集")
	require.True(t, c.mr.Exists(nodeDeathAtKey(takeoverZone, c.node)), "前提:death_at 已写")
}

// elapseBarrier 把 death_at 改到两个屏障之前,CanReclaimDeadNode 随即放行。
func (c *takeoverCase) elapseBarrier(t *testing.T) {
	t.Helper()
	past := time.Now().Add(-2 * sceneReentryBarrier(c.sc)).UnixMilli()
	require.NoError(t, c.mr.Set(nodeDeathAtKey(takeoverZone, c.node), strconv.FormatInt(past, 10)))
}

func (c *takeoverCase) drain() int {
	return drainPendingDeadNodeReconciles(context.Background(), c.sc)
}

// switchToNewLeaderProcess:另一个进程当选。进程内的死节点状态全没了,Redis 原样。
func (c *takeoverCase) switchToNewLeaderProcess(t *testing.T) {
	t.Helper()
	resetNodeDeathQueuesForTest(t)
	c.leading.Store(true)
}

// valueOrMissing 读一个字符串键,不存在时返回 "<missing>",让断言信息直接说明「被删了」。
func (c *takeoverCase) valueOrMissing(key string) string {
	v, err := c.mr.Get(key)
	if err != nil {
		return "<missing>"
	}
	return v
}

// requireIntact:死节点的计数与名下实例都原样(还没收尾)。why 进每条断言信息。
func (c *takeoverCase) requireIntact(t *testing.T, why string) {
	t.Helper()
	assert.Equal(t, strconv.Itoa(len(c.scenes)), c.valueOrMissing(nodeSceneCountKey(takeoverZone, c.node)), "%s: scene_count", why)
	assert.Equal(t, strconv.Itoa(takeoverScenePlayers*len(c.scenes)), nodeMemberCount(c.mr, takeoverZone, c.node),
		"%s: 节点成员", why)
	for _, id := range c.scenes {
		assert.Equal(t, c.node, c.valueOrMissing(sceneNodeKey(id)), "%s: scene %d 的属主映射", why, id)
		in, err := c.sc.Redis.Sismember(nodeScenesKey(takeoverZone, c.node), strconv.FormatUint(id, 10))
		require.NoError(t, err)
		assert.True(t, in, "%s: scene %d 必须留在死节点的反向索引里", why, id)
		requireActiveInstance(t, c.sc, takeoverZone, id, true)
	}
}

// requireCleanedUp:名下实例全部销毁、从反向索引摘掉,死节点的计数已删。why 进每条断言信息。
func (c *takeoverCase) requireCleanedUp(t *testing.T, why string) {
	t.Helper()
	for _, id := range c.scenes {
		assert.Equal(t, "<missing>", c.valueOrMissing(sceneNodeKey(id)), "%s: scene %d 是死节点的孤儿实例,必须销毁", why, id)
		assert.False(t, c.mr.Exists(instanceMembersKey(id)), "%s: scene %d 的成员集合", why, id)
		in, err := c.sc.Redis.Sismember(nodeScenesKey(takeoverZone, c.node), strconv.FormatUint(id, 10))
		require.NoError(t, err)
		assert.False(t, in, "%s: scene %d 不得留在死节点的反向索引里", why, id)
		requireActiveInstance(t, c.sc, takeoverZone, id, false)
	}
	assert.Equal(t, "<missing>", c.valueOrMissing(nodeSceneCountKey(takeoverZone, c.node)), "%s: 收完后删死节点的 scene_count", why)
	assert.False(t, c.mr.Exists(nodeMembersKey(takeoverZone, c.node)), "%s: 收完后删死节点的成员集合", why)
}

func nodeDeathDestroyedCountForTest(t *testing.T) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_instance_destroyed_total", map[string]string{
		"zone_id": strconv.FormatUint(uint64(takeoverZone), 10), "kind": "instance", "reason": "node_death",
	})
}

// 判死后屏障还没过就换了领导者:新领导者(另一个进程)在屏障过后接手 —— 实例销毁、计数删除、任务出队。
// 修复前:任务在旧领导者的内存里,新领导者什么都看不到,孤儿实例与计数永久残留(只等同一 node_id 再次判死)。
func TestDeadNodeReconcileTakeover_NewLeaderFinishesTaskQueuedInBarrierWindow(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8990", 89901, 89902)
	c.declareDead(t)
	assert.Equal(t, 0, c.drain(), "屏障没过,不得收尾")
	c.requireIntact(t, "屏障没过,不得收尾")

	// 屏障窗口里易主:旧领导者降级(这一拍它什么都不做),新领导者是另一个进程。
	c.leading.Store(false)
	assert.Equal(t, 0, c.drain())
	c.requireIntact(t, "旧领导者降级后不得收尾")
	c.switchToNewLeaderProcess(t)
	c.elapseBarrier(t)

	assert.Equal(t, 1, c.drain(), "新领导者必须接手旧领导者入队的收尾任务")
	c.requireCleanedUp(t, "新领导者必须接手旧领导者入队的收尾任务")
	assert.Equal(t, 0, c.drain(), "收完的任务已出队,不再处理")
}

// 屏障已过、但本副本已降级:跟随者不碰任务、计数与实例;重新当选后照常收完。
// 修复前:drain 没有领导者闸门,跟随者走进收尾,发现自己不是领导者就停,随后照样删死节点的计数并出队 ——
// 实例一个没销毁,计数却没了,任务也丢了。
func TestDeadNodeReconcileTakeover_FollowerLeavesCountersAndTaskAlone(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8991", 89911)
	c.declareDead(t)
	c.elapseBarrier(t)
	c.leading.Store(false)

	assert.Equal(t, 0, c.drain())
	c.requireIntact(t, "跟随者不得删死节点的计数,也不得收尾")

	c.leading.Store(true)
	assert.Equal(t, 1, c.drain(), "现任领导者照常收完")
	c.requireCleanedUp(t, "现任领导者照常收完")
}

// 销毁脚本第一次报错(结论未知):这一轮不删计数、不出队;下一拍只重试没结论的场景,收完后才删计数、出队。
// 修复前:报错的场景留在反向索引里,但计数照删、任务照出队,没有任何重试。
func TestDeadNodeReconcileTakeover_FailedSceneRetriedNextTick(t *testing.T) {
	hook := &failFirstEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	c := newTakeoverCase(t, sc, mr, "8992", 89921)
	c.declareDead(t)
	c.elapseBarrier(t)

	assert.Equal(t, 0, c.drain(), "有场景没结论的一轮不算收完")
	require.True(t, hook.fired.Load(), "注入的脚本错误必须真的发生")
	c.requireIntact(t, "有场景没结论时不得删死节点的计数(认领成功时还要对它做减法)")

	assert.Equal(t, 1, c.drain(), "下一拍重试没结论的场景并收完")
	c.requireCleanedUp(t, "没结论的场景必须在下一拍重试")
}

// demoteAfterFirstOwnerReadHook:收尾第一次 GET 任一 keys 之后,同步执行一次 demote。arm 之前一律放行。
type demoteAfterFirstOwnerReadHook struct {
	keys   map[string]struct{}
	demote func()
	armed  atomic.Bool
	fired  atomic.Bool
}

func (h *demoteAfterFirstOwnerReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *demoteAfterFirstOwnerReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *demoteAfterFirstOwnerReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if !h.armed.Load() || cmd.Name() != "get" || len(cmd.Args()) != 2 {
			return err
		}
		key, _ := cmd.Args()[1].(string)
		if _, ok := h.keys[key]; ok && h.fired.CompareAndSwap(false, true) {
			h.demote()
		}
		return err
	}
}

// 收尾扫到一半失去领导权:已认领的那个场景照常销毁,剩下的一个字节不写回;新领导者(另一个进程)从 Redis 里
// 剩下的接着做,每个场景的销毁只发生一次。
// 修复前:旧领导者中途停下后照样删计数、出队,剩下的场景再没有人收。
func TestDeadNodeReconcileTakeover_LeadershipLostMidSweepRestTakenOver(t *testing.T) {
	hook := &demoteAfterFirstOwnerReadHook{keys: map[string]struct{}{
		sceneNodeKey(89931): {},
		sceneNodeKey(89932): {},
	}}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	c := newTakeoverCase(t, sc, mr, "8993", 89931, 89932)
	hook.demote = func() { c.leading.Store(false) }
	c.declareDead(t)
	c.elapseBarrier(t)
	destroyedBefore := nodeDeathDestroyedCountForTest(t)

	hook.armed.Store(true)
	assert.Equal(t, 0, c.drain(), "中途降级的一轮不算收完")
	require.True(t, hook.fired.Load(), "降级必须真的插在收尾读第一个属主之后")
	assert.Equal(t, destroyedBefore+1, nodeDeathDestroyedCountForTest(t), "降级前已认领的那个场景照常销毁,剩下的不动")

	c.switchToNewLeaderProcess(t)
	assert.Equal(t, 1, c.drain(), "剩下的场景必须由新领导者接手")
	c.requireCleanedUp(t, "剩下的场景必须由新领导者接手")
	assert.Equal(t, destroyedBefore+2, nodeDeathDestroyedCountForTest(t), "每个场景只销毁一次")
}
