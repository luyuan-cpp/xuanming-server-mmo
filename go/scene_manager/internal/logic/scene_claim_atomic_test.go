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

	_, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "")
	require.Error(t, err)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.False(t, mr.Exists(sceneNodeKey(sceneID)), "脚本已执行")

	claim, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "")
	require.NoError(t, err)
	require.True(t, claim.Claimed)
	require.Equal(t, "node-a", claim.NodeID)
	require.Equal(t, "gs-a", claim.AgonesGs)
	require.Len(t, claim.Members, 2)

	claim, err = AtomicDestroyForce(sc, claimTestZone, sceneID, "")
	require.NoError(t, err)
	require.False(t, claim.Claimed, "快照只交付一次")
	require.Empty(t, claim.NodeID)
	require.Empty(t, claim.AgonesGs)
	require.Empty(t, claim.Members)
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

	claim, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "")
	require.NoError(t, err)
	require.False(t, claim.Claimed)
	require.Empty(t, claim.AgonesGs)
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
	require.Equal(t, strconv.FormatUint(source, 10), first.Source)
	require.Equal(t, "1", first.MirrorFlag)

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

// 以下三条守「本进程接手结局未知的认领」(evalSceneClaim)在 destroyInstanceInternal 里的副作用:接手的一方与
// 第一次执行的不是同一个脚本,回放的快照仍要照常做完。旧代码在同一输入下的表现写在各用例注释里;修复前本文件
// 编不过(AtomicDestroyForce 改了返回值),取红按 docs/PROGRESS.md 2026-11-05「销毁认领接手」条目的三个变异。
// 三条都经 destroyInstanceInternal 进,hook 按脚本原文挑第一次 EVAL。

// force 认领已执行、回包丢失;同一场景下一次认领来自空闲回收,回放 force 的快照(带 2 个剩余成员)。
// 旧代码:空闲分支只取属主和 gs,成员丢掉,节点成员集合里那 2 人永远摘不掉,节点人数永久偏高。
func TestDestroyInstance_TakesOverLostForceClaimAndUnlinksMembers(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	ctx := context.Background()
	const sceneID uint64 = 89612
	const node = "node-a"
	seedClaimScene(t, mr, sceneID, node, "gs-a", 2)
	seedNodeMembers(mr, claimTestZone, node, 2)
	require.NoError(t, mr.Set(nodeSceneCountKey(claimTestZone, node), "1"))

	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, sceneID, "explicit")
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Error(t, err)
	require.False(t, destroyed)
	require.True(t, mr.Exists(nodeMembersKey(claimTestZone, node)), "前提:回包丢失时副作用一样都没做")

	destroyed, err = destroyInstanceInternal(ctx, sc, claimTestZone, sceneID, false, "", "idle")
	require.NoError(t, err)
	require.True(t, destroyed, "接手到 force 的快照,照常做完副作用")
	require.False(t, mr.Exists(nodeMembersKey(claimTestZone, node)), "快照里的剩余成员要从节点集合摘掉")
	require.Equal(t, "0", mustGet(t, mr, nodeSceneCountKey(claimTestZone, node)))
}

// 空闲回收清扫一个映射已不在的残留(带 agones_gs),回包丢失;下一次认领来自显式 DestroyScene,回放清扫的快照。
// 清扫成功时属主为空。旧代码:force 分支按「属主为空 = 落空」判,回 false,名额永久不还(token 已被拿走,原路径
// 再试只会落空)。
func TestDestroyInstanceForce_TakesOverLostResidueSweepAndReleasesRoom(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstance}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	ctx := context.Background()
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-left", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 4})
	withAgones(t, sc, alloc, true)
	const sceneID uint64 = 89613
	require.NoError(t, mr.Set(sceneAgonesGsKey(sceneID), "gs-left"))
	_, err := mr.ZAdd(activeInstancesKey(claimTestZone), 1, strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)

	destroyed, err := destroyInstanceInternal(ctx, sc, claimTestZone, sceneID, false, "", "idle")
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Error(t, err)
	require.False(t, destroyed)
	require.False(t, mr.Exists(sceneAgonesGsKey(sceneID)), "前提:清扫已执行")
	_, releases, _ := alloc.stats()
	require.Zero(t, releases, "前提:回包丢失时名额还没还")

	destroyed, err = destroyInstanceForce(ctx, sc, claimTestZone, sceneID, "explicit")
	require.NoError(t, err)
	require.True(t, destroyed, "接手到清扫的快照,属主为空也算认领成功")
	_, releases, _ = alloc.stats()
	require.Equal(t, 1, releases, "残留的名额还且只还一次")
	require.EqualValues(t, 0, alloc.roomsOf("gs-left"))

	destroyed, err = destroyInstanceForce(ctx, sc, claimTestZone, sceneID, "explicit")
	require.NoError(t, err)
	require.False(t, destroyed, "快照只交付一次")
	_, releases, _ = alloc.stats()
	require.Equal(t, 1, releases)
}

// 镜像的 force 认领回包丢失;空闲回收接手时 scene:{id}:source 与镜像标记早已删掉。
// 旧代码:接手前预读镜像源读到空,镜像不从源的子集合里摘掉,源的 channelHasMirrors 恒真;指标 kind 记成 instance。
func TestDestroyInstance_TakesOverLostMirrorClaimAndUnlinksFromSource(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	ctx := context.Background()
	const (
		mirror  uint64 = 89614
		source  uint64 = 89615
		sibling uint64 = 89616
	)
	mirrorStr := strconv.FormatUint(mirror, 10)
	siblingStr := strconv.FormatUint(sibling, 10)
	seedClaimScene(t, mr, mirror, "node-a", "", 0)
	require.NoError(t, mr.Set(sceneMirrorFlagKey(mirror), "1"))
	require.NoError(t, mr.Set(sceneSourceKey(mirror), strconv.FormatUint(source, 10)))
	_, err := mr.SAdd(sceneMirrorsKey(source), mirrorStr, siblingStr)
	require.NoError(t, err)
	mirrorDestroys := func() float64 {
		return counterValueForTest(t, "scene_manager_instance_destroyed_total", map[string]string{
			"zone_id": strconv.FormatUint(uint64(claimTestZone), 10), "kind": "mirror", "reason": "idle",
		})
	}
	before := mirrorDestroys()

	_, err = destroyInstanceForce(ctx, sc, claimTestZone, mirror, "explicit")
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Error(t, err)
	require.False(t, mr.Exists(sceneSourceKey(mirror)), "前提:脚本已执行,镜像源键已删")

	destroyed, err := destroyInstanceInternal(ctx, sc, claimTestZone, mirror, false, "", "idle")
	require.NoError(t, err)
	require.True(t, destroyed)
	members, err := mr.Members(sceneMirrorsKey(source))
	require.NoError(t, err)
	require.Equal(t, []string{siblingStr}, members, "镜像源取自认领快照,镜像要从源的子集合里摘掉")
	require.Equal(t, before+1, mirrorDestroys(), "kind 取自认领快照")
}
