package logic

// 死节点收尾任务表(reentry_barrier.go「被屏障推迟的死节点收尾」)的契约:有界放弃、收尾范围是判死时刻的快照、
// 解析不了的任务被摘掉。现场搭建复用 dead_node_reconcile_takeover_test.go 的 takeoverCase。
//
// 本文件用到修复后才有的内部函数,不能放到修复前代码上跑;红的取法按变异(每次只做一处,跑
// -run 'TestDeadNodeReconcileTakeover_|TestDeadNodeReconcile_'):
//   - 变异 G:把 retryOrAbandonDeadNodeReconcile 里的 `waited < deadNodeReconcileGiveUpAfter` 换成 `true`(永不放弃),
//     只有 AbandonsAfterGiveUpDeadline 红;
//   - 变异 H:只把 reconcileDeadNodeTask 里 `svcCtx.Redis.Smembers(scopeKey)` 的参数换成
//     nodeScenesKey(task.zoneID, task.nodeID)(收尾时现读反向索引,scopeKey 本身不改),ScopeIsDeathTimeSnapshot 红
//     (判死后的新场景被销毁),takeover 文件的 LeadershipLostMidSweepRestTakenOver 也红(已销毁的成员不在反向索引里,
//     收尾范围永远清不空),其余绿;
//   - 变异 I:删掉 drainPendingDeadNodeReconciles 里摘 malformed 成员那个循环体中的 Zrem 调用(if 连同它的三行),
//     只有 DropsMalformedTask 红。

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"scene_manager/internal/metrics"
)

// failEvalHook 在 failing 打开期间让 script 的每次 EVAL 不发给 Redis、直接回错误(脚本没执行)。
type failEvalHook struct {
	script  string
	failing atomic.Bool
}

func (h *failEvalHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failEvalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failEvalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if h.failing.Load() && cmd.Name() == "eval" && len(args) >= 2 && args[1] == h.script {
			cmd.SetErr(errInjectedEval)
			return errInjectedEval
		}
		return next(ctx, cmd)
	}
}

// setDeadNodeReconcileRetrySinceForTest 把 (zone,node) 收尾任务在本进程的重试起点改到 at。
func setDeadNodeReconcileRetrySinceForTest(t *testing.T, zoneID uint32, nodeID string, at time.Time) {
	t.Helper()
	member := pendingDeadNodeKey(zoneID, nodeID)
	deadNodeReconcileLocalMu.Lock()
	_, ok := deadNodeReconcileRetrySince[member]
	if ok {
		deadNodeReconcileRetrySince[member] = at
	}
	deadNodeReconcileLocalMu.Unlock()
	require.True(t, ok, "前提不成立:zone=%d node=%s 还没有重试计时", zoneID, nodeID)
}

func deadNodeReconcileOutcomeCountForTest(t *testing.T, outcome string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_dead_node_reconcile_total", map[string]string{
		"zone_id": strconv.FormatUint(uint64(takeoverZone), 10), "outcome": outcome,
	})
}

// 销毁脚本一直报错:放弃期限之内每拍重试,计数与任务都留着;满期限就放弃 —— 删计数、删任务与收尾范围,
// 没结论的场景原样留在反向索引里(同一 node_id 再次判死时还能抄进收尾范围)。整个用例只注入 2 次失败。
func TestDeadNodeReconcile_AbandonsAfterGiveUpDeadline(t *testing.T) {
	const node = "8994"
	hook := &failEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	c := newTakeoverCase(t, sc, mr, node, 89941)
	c.declareDead(t)
	c.elapseBarrier(t)
	hook.failing.Store(true)
	retryBefore := deadNodeReconcileOutcomeCountForTest(t, metrics.DeadNodeReconcileOutcomeRetry)
	abandonedBefore := deadNodeReconcileOutcomeCountForTest(t, metrics.DeadNodeReconcileOutcomeAbandoned)

	assert.Equal(t, 0, c.drain())
	assert.Equal(t, retryBefore+1, deadNodeReconcileOutcomeCountForTest(t, metrics.DeadNodeReconcileOutcomeRetry))
	c.requireIntact(t, "放弃期限之内只重试,不删计数")
	_, pending := pendingDeadNodeForTest(t, mr, takeoverZone, node)
	assert.True(t, pending)

	setDeadNodeReconcileRetrySinceForTest(t, takeoverZone, node, time.Now().Add(-(deadNodeReconcileGiveUpAfter + time.Second)))
	assert.Equal(t, 1, c.drain(), "满放弃期限就出队,不无限重试")
	assert.Equal(t, abandonedBefore+1, deadNodeReconcileOutcomeCountForTest(t, metrics.DeadNodeReconcileOutcomeAbandoned))
	_, pending = pendingDeadNodeForTest(t, mr, takeoverZone, node)
	assert.False(t, pending)
	assert.False(t, mr.Exists(nodeReconcileScenesKey(takeoverZone, node)), "收尾范围随任务一起删")
	assert.Equal(t, "<missing>", c.valueOrMissing(nodeSceneCountKey(takeoverZone, node)), "放弃时同样删计数,不留无界的正残留")
	assert.False(t, mr.Exists(nodeMembersKey(takeoverZone, node)))
	assert.Equal(t, node, c.valueOrMissing(sceneNodeKey(89941)), "没结论的场景不动")
	in, err := sc.Redis.Sismember(nodeScenesKey(takeoverZone, node), "89941")
	require.NoError(t, err)
	assert.True(t, in, "没结论的场景留在反向索引里")

	hook.failing.Store(false)
	assert.Equal(t, 0, c.drain(), "放弃之后任务已不在表里")
	assert.Equal(t, node, c.valueOrMissing(sceneNodeKey(89941)))
}

// 收尾范围是判死时刻的快照(R1-I2):判死之后、屏障过去之前,同一 node_id 的新化身建的场景(属主同样是这个
// node_id)不在收尾范围里,收尾不得销毁它。
func TestDeadNodeReconcile_ScopeIsDeathTimeSnapshot(t *testing.T) {
	const node = "8995"
	const lateScene uint64 = 89952
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, node, 89951)
	c.declareDead(t)
	scope, ok := pendingDeadNodeForTest(t, mr, takeoverZone, node)
	require.True(t, ok, "判死时必须写收尾任务")
	assert.Equal(t, []string{"89951"}, scope)

	late := strconv.FormatUint(lateScene, 10)
	require.NoError(t, mr.Set(sceneNodeKey(lateScene), node))
	_, err := mr.SAdd(nodeScenesKey(takeoverZone, node), late)
	require.NoError(t, err)
	_, err = mr.ZAdd(activeInstancesKey(takeoverZone), float64(nowUnix()), late)
	require.NoError(t, err)
	c.elapseBarrier(t)

	assert.Equal(t, 1, c.drain())
	assert.Equal(t, "<missing>", c.valueOrMissing(sceneNodeKey(89951)), "判死时名下的实例照常销毁")
	assert.Equal(t, node, c.valueOrMissing(sceneNodeKey(lateScene)), "判死之后建到同一 node_id 上的场景不在收尾范围里")
	in, err := sc.Redis.Sismember(nodeScenesKey(takeoverZone, node), late)
	require.NoError(t, err)
	assert.True(t, in, "新场景的反向索引不得被摘")
	requireActiveInstance(t, sc, takeoverZone, lateScene, true)
}

// 任务表里解析不了的成员(不是本代码写的)对不上任何节点:摘掉,别的任务照常处理。
func TestDeadNodeReconcile_DropsMalformedTask(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	c := newTakeoverCase(t, sc, mr, "8996", 89961)
	c.declareDead(t)
	c.elapseBarrier(t)
	for _, bad := range []string{"not-a-task", "x/8996", "1/"} {
		_, err := mr.ZAdd(deadNodeReconcileTasksKey, 1, bad)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, c.drain())
	tasks, err := mr.ZMembers(deadNodeReconcileTasksKey)
	if !errors.Is(err, miniredis.ErrKeyNotFound) {
		require.NoError(t, err)
		assert.Empty(t, tasks, "解析不了的成员必须被摘掉")
	}
	c.requireCleanedUp(t, "别的任务照常处理")
}
