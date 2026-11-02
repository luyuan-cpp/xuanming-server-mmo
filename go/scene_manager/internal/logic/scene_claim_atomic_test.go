package logic

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 写路径非原子残余的回归(docs/PROGRESS.md 2026-10-23 的 R3、R5,以及排空的两步移动)。
// 旧代码上各用例的表现写在用例注释里。

const claimTestZone uint32 = 7

// lostReplyEvalHook 让 script 的第一次 EVAL 真的执行,再把应答换成错误:脚本已执行、回包丢失。
type lostReplyEvalHook struct {
	script string
	fired  atomic.Bool
}

func (h *lostReplyEvalHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *lostReplyEvalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *lostReplyEvalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "eval" && len(args) >= 2 && args[1] == h.script && h.fired.CompareAndSwap(false, true) {
			if err := next(ctx, cmd); err != nil {
				return err
			}
			cmd.SetErr(errInjectedEval)
			return errInjectedEval
		}
		return next(ctx, cmd)
	}
}

// seedClaimScene 铺一个有属主、有名额、在 active 集合里的实例。
func seedClaimScene(t *testing.T, mr *miniredis.Miniredis, sceneID uint64, node, gs string, members int) {
	t.Helper()
	require.NoError(t, mr.Set(sceneNodeKey(sceneID), node))
	require.NoError(t, mr.Set(sceneZoneKey(sceneID), strconv.FormatUint(uint64(claimTestZone), 10)))
	require.NoError(t, mr.Set(sceneAgonesGsKey(sceneID), gs))
	seedSceneMembers(mr, sceneID, members)
	_, err := mr.ZAdd(activeInstancesKey(claimTestZone), 1, strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
}

// force 认领已执行、Eval 回错:旧代码下一次认领读不到映射,按「已被别人销毁」跳过全部副作用。
// 现在同一进程的下一次认领带原 token,拿回当时的快照,再下一次才落空。
func TestSceneClaim_ForceReplyLostNextAttemptGetsSnapshot(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	const sceneID uint64 = 89601
	seedClaimScene(t, mr, sceneID, "node-a", "gs-a", 2)

	_, _, _, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "")
	require.Error(t, err)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.False(t, mr.Exists(sceneNodeKey(sceneID)), "脚本已执行")

	node, gs, members, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "")
	require.NoError(t, err)
	require.Equal(t, "node-a", node)
	require.Equal(t, "gs-a", gs)
	require.Len(t, members, 2)

	node, gs, members, err = AtomicDestroyForce(sc, claimTestZone, sceneID, "")
	require.NoError(t, err)
	require.Empty(t, node, "快照只交付一次")
	require.Empty(t, gs)
	require.Empty(t, members)
}

// 空闲回收同理。旧代码把重发当成「有人进场」,重新 ZADD 并丢掉 agones_gs,实例成永久僵尸。
func TestSceneClaim_IdleReplyLostNextAttemptGetsSnapshot(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstance}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	const sceneID uint64 = 89602
	seedClaimScene(t, mr, sceneID, "node-a", "gs-a", 0)

	_, err := AtomicDestroyIfIdle(sc, claimTestZone, sceneID)
	require.Error(t, err)
	require.True(t, hook.fired.Load(), "注入必须真的发生")

	claim, err := AtomicDestroyIfIdle(sc, claimTestZone, sceneID)
	require.NoError(t, err)
	require.True(t, claim.Claimed)
	require.Equal(t, "node-a", claim.NodeID)
	require.Equal(t, "gs-a", claim.AgonesGs)
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// go-redis 内部重发:同一 token 原样返回当时的快照,一个键都不再动。
func TestSceneClaim_SameTokenReplaysAndOtherTokenDoesNot(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89603
	seedClaimScene(t, mr, sceneID, "node-a", "gs-a", 3)
	keys := sceneClaimKeys(claimTestZone, sceneID)

	first, err := evalSceneClaimWithToken(sc, sceneID, "token-1", luaAtomicDestroyInstanceForce, keys, "")
	require.NoError(t, err)
	require.True(t, first.Claimed)
	require.Len(t, first.Members, 3)

	again, err := evalSceneClaimWithToken(sc, sceneID, "token-1", luaAtomicDestroyInstanceForce, keys, "")
	require.NoError(t, err)
	require.Equal(t, first, again)

	other, err := evalSceneClaimWithToken(sc, sceneID, "token-2", luaAtomicDestroyInstanceForce, keys, "")
	require.NoError(t, err)
	require.False(t, other.Claimed, "别的认领不得拿到快照")

	ttl := mr.TTL(sceneClaimRecordKey(sceneID))
	require.Positive(t, ttl, "认领记录要带 TTL")
}

// 映射缺失不是有人进场:旧代码 ZADD 回 active 集合并丢掉 agones_gs。现在摘掉条目、交回 gs,之后不再出现。
func TestAtomicDestroyIfIdle_MappingMissingIsCleanedNotReseeded(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89604
	require.NoError(t, mr.Set(sceneAgonesGsKey(sceneID), "gs-left"))
	_, err := mr.ZAdd(activeInstancesKey(claimTestZone), 1, strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)

	claim, err := AtomicDestroyIfIdle(sc, claimTestZone, sceneID)
	require.NoError(t, err)
	require.True(t, claim.Claimed)
	require.Empty(t, claim.NodeID)
	require.Equal(t, "gs-left", claim.AgonesGs)
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
	require.False(t, mr.Exists(sceneAgonesGsKey(sceneID)))
}

// 同样的残留走完整的空闲销毁:不 ZADD 回去。旧代码的 ZADD 在脚本外。
func TestDestroyInstance_IdleMappingMissingStaysOutOfActiveSet(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89605
	_, err := mr.ZAdd(activeInstancesKey(claimTestZone), 1, strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)

	destroyed, err := destroyInstanceInternal(context.Background(), sc, claimTestZone, sceneID, false, "", "idle")
	require.NoError(t, err)
	require.True(t, destroyed)
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// 有人时重置空闲时钟,但不能把已经摘掉的实例加回去。
func TestAtomicDestroyIfIdle_BusyReseedsOnlyListedScene(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89606
	member := strconv.FormatUint(sceneID, 10)
	seedClaimScene(t, mr, sceneID, "node-a", "gs-a", 1)

	claim, err := AtomicDestroyIfIdle(sc, claimTestZone, sceneID)
	require.NoError(t, err)
	require.True(t, claim.Busy)
	score, err := mr.ZScore(activeInstancesKey(claimTestZone), member)
	require.NoError(t, err)
	require.Greater(t, score, float64(1), "空闲时钟要重置")
	require.Equal(t, "node-a", mustGet(t, mr, sceneNodeKey(sceneID)))
	require.Equal(t, "gs-a", mustGet(t, mr, sceneAgonesGsKey(sceneID)))

	_, err = mr.ZRem(activeInstancesKey(claimTestZone), member)
	require.NoError(t, err)
	claim, err = AtomicDestroyIfIdle(sc, claimTestZone, sceneID)
	require.NoError(t, err)
	require.True(t, claim.Busy)
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// 认领以映射存在为前提。旧脚本不带期望属主时照样认领并交回 gs,与回滚交错会双还名额。
func TestAtomicDestroyForce_NoOwnerDoesNotClaimUnmappedScene(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89607
	require.NoError(t, mr.Set(sceneAgonesGsKey(sceneID), "gs-x"))

	node, gs, members, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "")
	require.NoError(t, err)
	require.Empty(t, node)
	require.Empty(t, gs)
	require.Empty(t, members)
	require.Equal(t, "gs-x", mustGet(t, mr, sceneAgonesGsKey(sceneID)))
}

// 回滚一步抹掉全部状态,计数只减一次。旧代码逐条 DEL、不查错,重发时计数再减一次。
func TestRollbackInstanceAllocation_WipesEverythingOnce(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		sceneID uint64 = 89608
		source  uint64 = 89609
		sibling uint64 = 89610
		node           = "node-a"
	)
	member := strconv.FormatUint(sceneID, 10)
	seedClaimScene(t, mr, sceneID, node, "gs-a", 0)
	require.NoError(t, mr.Set(sceneMirrorFlagKey(sceneID), "1"))
	require.NoError(t, mr.Set(sceneSourceKey(sceneID), strconv.FormatUint(source, 10)))
	require.NoError(t, mr.Set(nodeSceneCountKey(claimTestZone, node), "3"))
	_, err := mr.SAdd(nodeScenesKey(claimTestZone, node), member, "other-scene")
	require.NoError(t, err)
	_, err = mr.SAdd(sceneMirrorsKey(source), member, strconv.FormatUint(sibling, 10))
	require.NoError(t, err)

	keys := append(sceneClaimKeys(claimTestZone, sceneID),
		nodeScenesKey(claimTestZone, node), nodeSceneCountKey(claimTestZone, node), sceneMirrorsKey(source))
	first, err := evalSceneClaimWithToken(sc, sceneID, "token-1", luaRollbackInstanceAllocation, keys, node)
	require.NoError(t, err)
	require.True(t, first.Claimed)
	require.Equal(t, "gs-a", first.AgonesGs)

	again, err := evalSceneClaimWithToken(sc, sceneID, "token-1", luaRollbackInstanceAllocation, keys, node)
	require.NoError(t, err)
	require.Equal(t, first, again, "重发回原快照")

	for _, key := range []string{sceneNodeKey(sceneID), sceneZoneKey(sceneID), sceneAgonesGsKey(sceneID),
		sceneMirrorFlagKey(sceneID), sceneSourceKey(sceneID), instanceMembersKey(sceneID)} {
		require.False(t, mr.Exists(key), key)
	}
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
	require.Equal(t, "2", mustGet(t, mr, nodeSceneCountKey(claimTestZone, node)), "计数只减一次")
	nodeScenes, err := mr.Members(nodeScenesKey(claimTestZone, node))
	require.NoError(t, err)
	require.Equal(t, []string{"other-scene"}, nodeScenes)
	mirrors, err := mr.Members(sceneMirrorsKey(source))
	require.NoError(t, err)
	require.Equal(t, []string{strconv.FormatUint(sibling, 10)}, mirrors)
}

// 属主已不是这次分配的节点:被改派或已被销毁,一个键都不动。旧代码无条件 DEL,抹掉新属主的场景。
func TestRollbackInstanceAllocation_OwnerChangedTouchesNothing(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89611
	seedClaimScene(t, mr, sceneID, "node-new", "gs-a", 1)
	require.NoError(t, mr.Set(nodeSceneCountKey(claimTestZone, "node-old"), "3"))

	claim, err := atomicRollbackInstanceAllocation(sc, claimTestZone, sceneID, "node-old", 0)
	require.NoError(t, err)
	require.False(t, claim.Claimed)
	require.Equal(t, "node-new", mustGet(t, mr, sceneNodeKey(sceneID)))
	require.Equal(t, "gs-a", mustGet(t, mr, sceneAgonesGsKey(sceneID)))
	require.Equal(t, "3", mustGet(t, mr, nodeSceneCountKey(claimTestZone, "node-old")))
	requireActiveInstance(t, sc, claimTestZone, sceneID, true)
}

// 排空的移动一步完成,期望频道数和排空标记在同一步里写好。
func TestBeginDrain_MovesChannelInOneStep(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})

	moved := beginDrainWorldChannel(context.Background(), sc, testZoneId, autoscaleTestConfID,
		channelLoad{sceneID: "102", sceneID64: 102, players: 40})
	require.True(t, moved)

	routed, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	require.Equal(t, []string{"101"}, routed)
	draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	require.Equal(t, []string{"102"}, draining)
	require.Positive(t, mr.TTL(sceneDrainingKey(102)), "排空标记要带 TTL")
	require.Equal(t, 1, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))

	again := beginDrainWorldChannel(context.Background(), sc, testZoneId, autoscaleTestConfID,
		channelLoad{sceneID: "102", sceneID64: 102, players: 40})
	require.False(t, again, "已经摘掉的不重复处理")
	require.Equal(t, 1, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID), "不再减一次")
}

// 脚本没执行时什么都没变。旧代码 SREM 成功、SADD 失败只记日志,频道既不可路由也不再收敛。
func TestBeginDrain_ScriptFailureLeavesChannelRouted(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, _ ...string) bool {
		if cmd == "EVAL" || cmd == "EVALSHA" {
			c.WriteError("ERR injected: script not run")
			return true
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	moved := beginDrainWorldChannel(context.Background(), sc, testZoneId, autoscaleTestConfID,
		channelLoad{sceneID: "102", sceneID64: 102, players: 40})
	mr.Server().SetPreHook(nil)
	require.False(t, moved)

	routed, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"101", "102"}, routed)
	draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	require.Empty(t, draining)
	require.False(t, mr.Exists(sceneDrainingKey(102)))
	require.Equal(t, 2, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
}
