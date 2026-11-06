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

	"scene_manager/internal/svc"
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

// seedClaimScene 铺一个有属主、有名额、在 active 集合里的实例,登记在 claimTestZone。
func seedClaimScene(t *testing.T, mr *miniredis.Miniredis, sceneID uint64, node, gs string, members int) {
	t.Helper()
	seedClaimSceneInZone(t, mr, claimTestZone, sceneID, node, gs, members)
}

// seedClaimSceneInZone 同 seedClaimScene,实例登记在 zone(scene:{id}:zone 与 active 集合都按它写)。
func seedClaimSceneInZone(t *testing.T, mr *miniredis.Miniredis, zone uint32, sceneID uint64, node, gs string, members int) {
	t.Helper()
	require.NoError(t, mr.Set(sceneNodeKey(sceneID), node))
	require.NoError(t, mr.Set(sceneZoneKey(sceneID), strconv.FormatUint(uint64(zone), 10)))
	require.NoError(t, mr.Set(sceneAgonesGsKey(sceneID), gs))
	seedSceneMembers(mr, sceneID, members)
	_, err := mr.ZAdd(activeInstancesKey(zone), 1, strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
}

// requireTeardownPending 断言场景的拆除任务是否还在待办里(任务键与待办集合两边一致)。
func requireTeardownPending(t *testing.T, mr *miniredis.Miniredis, zoneID uint32, sceneID uint64, want bool) {
	t.Helper()
	require.Equal(t, want, mr.Exists(sceneTeardownJobKey(sceneID)), "拆除任务键")
	_, err := mr.ZScore(sceneTeardownPendingKey(zoneID), strconv.FormatUint(sceneID, 10))
	require.Equal(t, want, err == nil, "待办集合成员")
}

// force 认领已执行、Eval 回错:调用方拿到错误。旧代码(2026-11-01 之前)下一次认领读不到映射,按「已被别人销毁」
// 跳过全部副作用,它们永久丢失。现在认领的同一步写下了拆除任务:之后的认领一律落空,副作用由扫描在租期之后做完
// (scene_teardown_job_test.go)。
func TestSceneClaim_ForceReplyLostLeavesTheTeardownJob(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	const sceneID uint64 = 89601
	seedClaimScene(t, mr, sceneID, "node-a", "gs-a", 2)

	_, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.Error(t, err)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.False(t, mr.Exists(sceneNodeKey(sceneID)), "脚本已执行")
	requireTeardownPending(t, mr, claimTestZone, sceneID, true)

	claim, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.False(t, claim.Claimed, "已被认领过,后来的认领落空")
	requireTeardownPending(t, mr, claimTestZone, sceneID, true)
}

// 空闲回收同理。旧代码(2026-11-01 之前)把重发当成「有人进场」,重新 ZADD 并丢掉 agones_gs,实例成永久僵尸。
func TestSceneClaim_IdleReplyLostLeavesTheTeardownJob(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstance}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	const sceneID uint64 = 89602
	seedClaimScene(t, mr, sceneID, "node-a", "gs-a", 0)

	_, err := AtomicDestroyIfIdle(sc, claimTestZone, sceneID)
	require.Error(t, err)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
	requireTeardownPending(t, mr, claimTestZone, sceneID, true)
	require.Equal(t, "gs-a", mustTeardownSnapshot(t, sc, sceneID).Claim.AgonesGs, "名额记在任务里")
}

// mustTeardownSnapshot 读出场景的拆除任务,任务必须在。
func mustTeardownSnapshot(t *testing.T, sc *svc.ServiceContext, sceneID uint64) sceneTeardownJob {
	t.Helper()
	job, ok, err := loadSceneTeardownJob(sc, claimTestZone, sceneID)
	require.NoError(t, err)
	require.True(t, ok, "场景 %d 的拆除任务必须在", sceneID)
	return job
}

// go-redis 内部重发:同一 token 原样返回当时的快照,一个键都不再动;别的 token 落空。任务不带 TTL:做完才删。
func TestSceneClaim_SameTokenReplaysAndOtherTokenDoesNot(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89603
	seedClaimScene(t, mr, sceneID, "node-a", "gs-a", 3)
	require.NoError(t, mr.Set(nodeSceneCountKey(claimTestZone, "node-a"), "2"))
	req := sceneTeardownRequest{
		ZoneID: claimTestZone, SceneID: sceneID, Kind: teardownKindInstance, Reason: "explicit",
		ExpectedOwner: "node-a", CountOwner: true,
	}

	first, err := evalSceneClaimWithToken(sc, luaAtomicDestroyInstanceForce, req, "token-1")
	require.NoError(t, err)
	require.True(t, first.Claimed)
	require.Len(t, first.Members, 3)
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, "node-a")), "认领在同一步扣属主的计数")

	again, err := evalSceneClaimWithToken(sc, luaAtomicDestroyInstanceForce, req, "token-1")
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, "node-a")), "重发不得再扣一次")

	other, err := evalSceneClaimWithToken(sc, luaAtomicDestroyInstanceForce, req, "token-2")
	require.NoError(t, err)
	require.False(t, other.Claimed, "别的认领不得拿到快照")

	require.Zero(t, mr.TTL(sceneTeardownJobKey(sceneID)), "拆除任务不带 TTL,做完副作用才删")
	requireTeardownPending(t, mr, claimTestZone, sceneID, true)
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

	claim, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.False(t, claim.Claimed)
	require.Empty(t, claim.AgonesGs)
	require.Equal(t, "gs-x", mustGet(t, mr, sceneAgonesGsKey(sceneID)))
	requireTeardownPending(t, mr, claimTestZone, sceneID, false)
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

	// 与 atomicRollbackInstanceAllocation 的请求相同,只是 token 固定,好验证重发。
	req := sceneTeardownRequest{
		ZoneID: claimTestZone, SceneID: sceneID, Kind: teardownKindRollback, Reason: "create_rpc_failed",
		ExpectedOwner: node, CountOwner: true,
	}
	first, err := evalSceneClaimWithToken(sc, luaAtomicDestroyInstanceForce, req, "token-1")
	require.NoError(t, err)
	require.True(t, first.Claimed)
	require.Equal(t, "gs-a", first.AgonesGs)
	require.Equal(t, strconv.FormatUint(source, 10), first.Source)
	require.Equal(t, "1", first.MirrorFlag)

	again, err := evalSceneClaimWithToken(sc, luaAtomicDestroyInstanceForce, req, "token-1")
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

	// 镜像源的子集合由回滚的副作用按快照里的镜像源摘(源在别的 zone 时登记没写 scene:{id}:source,快照里就没有)。
	job := sceneTeardownJob{ZoneID: claimTestZone, SceneID: sceneID, Kind: teardownKindRollback, Reason: "create_rpc_failed", Claim: first}
	require.NoError(t, runRollbackTeardown(context.Background(), sc, job))
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

	claim, err := atomicRollbackInstanceAllocation(sc, claimTestZone, sceneID, "node-old")
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
