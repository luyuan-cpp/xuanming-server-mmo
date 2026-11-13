package logic

// 拆除任务(scene_teardown_job.go)的回归:认领之后的副作用在进程退出、回包丢失、某一步失败之后由扫描做完,
// 而且只做一次。
//
// 旧代码(2026-11-05 之前)认领之后的副作用只在认领方的内存里:Eval 回错、进程退出或滚动发布取消了 ctx,
// 副作用就没人做了(11-01 起只有「本进程下一次认领同一场景」能接手,而场景已摘出各个索引,通常没有下一次)。
// 本文件依赖新符号(sweepSceneTeardownJobs 等),在旧代码上编不过,取红按 docs/PROGRESS.md 2026-11-05
// 「场景拆除任务」条目的变异 M1~M5。
//
// 场景号 89620~89640,不与别的用例重名。claimTestZone、seedClaimScene、lostReplyEvalHook、requireTeardownPending
// 在 scene_claim_atomic_test.go。

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// pastLease 是一个超过租期的「现在」,让扫描立即接手刚认领的任务。
func pastLease() time.Time {
	return time.Now().Add(sceneTeardownJobLease + time.Second)
}

func teardownJobCount(t *testing.T, kind, outcome string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_scene_teardown_jobs_total", map[string]string{
		"zone_id": strconv.FormatUint(uint64(claimTestZone), 10), "kind": kind, "outcome": outcome,
	})
}

// teardownOldestPendingForTest 读 scene_manager_scene_teardown_oldest_pending_seconds{zone_id=claimTestZone}。
func teardownOldestPendingForTest(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	want := strconv.FormatUint(uint64(claimTestZone), 10)
	for _, family := range families {
		if family.GetName() != "scene_manager_scene_teardown_oldest_pending_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == "zone_id" && pair.GetValue() == want {
					return metric.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

// requireNotNodeScene 断言场景已从节点的反向索引里摘掉(集合空了 miniredis 会删键,读不到也算摘掉)。
func requireNotNodeScene(t *testing.T, mr *miniredis.Miniredis, nodeID string, sceneID uint64) {
	t.Helper()
	in, err := mr.IsMember(nodeScenesKey(claimTestZone, nodeID), strconv.FormatUint(sceneID, 10))
	if err == nil {
		require.False(t, in, "场景 %d 必须从节点 %s 的反向索引里摘掉", sceneID, nodeID)
	}
}

// 正常路径:认领方当场做完,任务删掉,计 completed。属主的计数与反向索引在认领脚本里就改好了。
func TestSceneTeardown_ClaimerFinishesAndRemovesTheJob(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const sceneID uint64 = 89620
	seedClaimScene(t, mr, sceneID, "node-a", "", 0)
	require.NoError(t, mr.Set(nodeSceneCountKey(claimTestZone, "node-a"), "1"))
	_, err := mr.SAdd(nodeScenesKey(claimTestZone, "node-a"), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	before := teardownJobCount(t, teardownKindInstance, "completed")

	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, sceneID, "explicit")
	require.NoError(t, err)
	require.True(t, destroyed)
	requireTeardownPending(t, mr, claimTestZone, sceneID, false)
	require.Equal(t, "0", mustGet(t, mr, nodeSceneCountKey(claimTestZone, "node-a")))
	requireNotNodeScene(t, mr, "node-a", sceneID)
	require.Equal(t, before+1, teardownJobCount(t, teardownKindInstance, "completed"))
}

// 只认领、不做副作用(等同认领方在认领之后立刻退出):租期内扫描不碰,过了租期接手做完,计 recovered。
func TestSceneTeardown_SweepWaitsForTheLease(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const sceneID uint64 = 89621
	seedClaimScene(t, mr, sceneID, "node-a", "", 0)
	claim, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.True(t, claim.Claimed)
	requireTeardownPending(t, mr, claimTestZone, sceneID, true)

	require.Zero(t, sweepSceneTeardownJobs(ctx, sc, claimTestZone, time.Now()), "租期内留给认领方")
	requireTeardownPending(t, mr, claimTestZone, sceneID, true)

	before := teardownJobCount(t, teardownKindInstance, "recovered")
	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	requireTeardownPending(t, mr, claimTestZone, sceneID, false)
	require.Equal(t, before+1, teardownJobCount(t, teardownKindInstance, "recovered"))
}

// force 认领已执行、回包丢失:计数在认领脚本里就扣了;快照里的剩余成员由扫描从节点成员集合摘掉。
// 旧代码:调用方拿到错误,成员永远留在节点集合里,节点人数永久偏高。
func TestSceneTeardown_SweepFinishesALostForceClaim(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
	ctx := context.Background()
	const sceneID uint64 = 89622
	const node = "node-a"
	seedClaimScene(t, mr, sceneID, node, "", 2)
	seedNodeMembers(mr, claimTestZone, node, 2)
	require.NoError(t, mr.Set(nodeSceneCountKey(claimTestZone, node), "1"))

	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, sceneID, "explicit")
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Error(t, err)
	require.False(t, destroyed)
	require.Equal(t, "0", mustGet(t, mr, nodeSceneCountKey(claimTestZone, node)), "计数在认领脚本里就扣了")
	require.True(t, mr.Exists(nodeMembersKey(claimTestZone, node)), "前提:剩余成员还没摘")

	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	require.False(t, mr.Exists(nodeMembersKey(claimTestZone, node)), "扫描按快照摘掉剩余成员")
	require.Equal(t, "0", mustGet(t, mr, nodeSceneCountKey(claimTestZone, node)), "扫描不再扣计数")
	requireTeardownPending(t, mr, claimTestZone, sceneID, false)
}

// 空闲回收清扫一个映射已不在的残留(带 agones_gs),回包丢失:扫描还这份名额,只还一次。
// 旧代码:名额永久不还;11-01 的本进程接手又因为属主为空被 force 分支当成落空。
func TestSceneTeardown_SweepReleasesTheRoomOfALostResidueSweep(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstance}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
	ctx := context.Background()
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-left", podIP: "10.0.0.8", state: "Allocated", capacity: 4})
	withAgones(t, sc, alloc, true)
	const sceneID uint64 = 89623
	seedAgonesRoomInZone(t, sc, alloc, claimTestZone, sceneID, "gs-left")
	_, err := mr.ZAdd(activeInstancesKey(claimTestZone), 1, strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)

	destroyed, err := destroyInstanceInternal(ctx, sc, claimTestZone, sceneID, false, "", "idle", nil)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Error(t, err)
	require.False(t, destroyed)
	_, releases, _ := alloc.stats()
	require.Zero(t, releases, "前提:回包丢失时名额还没还")

	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	_, releases, _ = alloc.stats()
	require.Equal(t, 1, releases, "残留的名额还且只还一次")
	require.EqualValues(t, 0, alloc.roomsOf("gs-left"))

	require.Zero(t, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()), "任务已删,不会再做")
	_, releases, _ = alloc.stats()
	require.Equal(t, 1, releases)
}

// 认领方与扫描同时做同一个任务(认领方慢过了租期):各步骤重跑无害,名额删的始终是快照里那一条,只还一次;
// 任务只删一次。2026-11-10 之前名额是计数,靠任务上「先置位、至多一次」挡住第二次归还,置位之后进程退出就漏一份。
func TestSceneTeardown_ConcurrentRunsReleaseTheRoomOnce(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-two", podIP: "10.0.0.9", state: "Allocated", count: 1, capacity: 4})
	withAgones(t, sc, alloc, true)
	const sceneID uint64 = 89624
	room := agonesRoom{GameServer: "gs-two", Entry: newAgonesRoomEntry(sceneID)}
	alloc.addEntryForTest("gs-two", room.Entry)
	seedClaimScene(t, mr, sceneID, "node-a", room.record(), 0)

	claim, err := ClaimIdleSceneTeardown(sc, claimTestZone, sceneID)
	require.NoError(t, err)
	require.True(t, claim.Claimed)
	job := sceneTeardownJob{ZoneID: claimTestZone, SceneID: sceneID, Kind: teardownKindInstance, Reason: "idle", Claim: claim}

	require.NoError(t, runSceneTeardown(ctx, sc, job, nil))
	require.NoError(t, runSceneTeardown(ctx, sc, job, nil))
	require.EqualValues(t, 1, alloc.roomsOf("gs-two"), "删的是这个场景那一条,别的名额不动")
	require.NotContains(t, alloc.entriesOf("gs-two"), room.Entry)

	require.True(t, completeSceneTeardown(ctx, sc, job, nil, false))
	require.False(t, completeSceneTeardown(ctx, sc, job, nil, true), "任务已删,后到的一方不再记完成")
	require.EqualValues(t, 1, alloc.roomsOf("gs-two"))
	for _, removed := range alloc.removed {
		require.Equal(t, room.record(), removed, "每次归还都只删快照里那一条")
	}
}

// 归还失败(K8s API 不可用)时任务留着,扫描重做到成功。2026-11-10 之前归还是「至多一次」,这一份就永远漏了。
func TestSceneTeardown_FailedRoomReleaseIsRetriedBySweep(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-f", podIP: "10.0.0.12", state: "Allocated", capacity: 4})
	withAgones(t, sc, alloc, true)
	const sceneID uint64 = 89637
	room := agonesRoom{GameServer: "gs-f", Entry: newAgonesRoomEntry(sceneID)}
	alloc.addEntryForTest("gs-f", room.Entry)
	seedClaimScene(t, mr, sceneID, "node-a", room.record(), 0)
	deferredBefore := teardownJobCount(t, teardownKindInstance, "deferred")

	alloc.removeErr = errors.New("apiserver down")
	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, sceneID, "explicit")
	require.NoError(t, err)
	require.True(t, destroyed, "认领已提交,销毁已经发生")
	require.EqualValues(t, 1, alloc.roomsOf("gs-f"), "前提:这次没还掉")
	requireTeardownPending(t, mr, claimTestZone, sceneID, true)
	require.Equal(t, deferredBefore+1, teardownJobCount(t, teardownKindInstance, "deferred"))

	alloc.removeErr = nil
	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	require.EqualValues(t, 0, alloc.roomsOf("gs-f"), "扫描重做时还掉")
	requireTeardownPending(t, mr, claimTestZone, sceneID, false)
}

// 镜像的 force 认领回包丢失:扫描按快照里的镜像源把它从源的子集合摘掉,指标 kind 记 mirror。
// 旧代码:没人摘,源的 channelHasMirrors 恒真、永远缩不了容。
func TestSceneTeardown_SweepUnlinksALostMirrorFromItsSource(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
	ctx := context.Background()
	const (
		mirror  uint64 = 89625
		source  uint64 = 89626
		sibling uint64 = 89627
	)
	siblingStr := strconv.FormatUint(sibling, 10)
	seedClaimScene(t, mr, mirror, "node-a", "", 0)
	require.NoError(t, mr.Set(sceneMirrorFlagKey(claimTestZone, mirror), "1"))
	require.NoError(t, mr.Set(sceneSourceKey(claimTestZone, mirror), strconv.FormatUint(source, 10)))
	_, err := mr.SAdd(sceneMirrorsKey(claimTestZone, source), strconv.FormatUint(mirror, 10), siblingStr)
	require.NoError(t, err)
	mirrorDestroys := func() float64 {
		return counterValueForTest(t, "scene_manager_instance_destroyed_total", map[string]string{
			"zone_id": strconv.FormatUint(uint64(claimTestZone), 10), "kind": "mirror", "reason": "explicit",
		})
	}
	before := mirrorDestroys()

	_, err = destroyInstanceForce(ctx, sc, claimTestZone, mirror, "explicit")
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Error(t, err)
	require.False(t, mr.Exists(sceneSourceKey(claimTestZone, mirror)), "前提:脚本已执行,镜像源键已删")

	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	members, err := mr.Members(sceneMirrorsKey(claimTestZone, source))
	require.NoError(t, err)
	require.Equal(t, []string{siblingStr}, members, "镜像源取自认领快照")
	require.Equal(t, before+1, mirrorDestroys(), "kind 取自认领快照,完成只记一次")
}

// 认领之后有一步没做成(级联重读子集合失败):销毁照常回 true,任务留着计 deferred;扫描重做时级联补完,任务删掉。
// 旧代码:级联失败只留下源的子集合索引,源已经没了,没有任何路径会再级联,镜像等空闲超时。
func TestSceneTeardown_FailedStepKeepsTheJobUntilItSucceeds(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const (
		parent uint64 = 89628
		child  uint64 = 89629
	)
	seedClaimScene(t, mr, parent, "node-a", "", 0)
	seedClaimScene(t, mr, child, "node-a", "", 0)
	require.NoError(t, mr.Set(sceneMirrorFlagKey(claimTestZone, child), "1"))
	require.NoError(t, mr.Set(sceneSourceKey(claimTestZone, child), strconv.FormatUint(parent, 10)))
	_, err := mr.SAdd(sceneMirrorsKey(claimTestZone, parent), strconv.FormatUint(child, 10))
	require.NoError(t, err)
	deferredBefore := teardownJobCount(t, teardownKindInstance, "deferred")
	// 第一次 SMEMBERS 是认领前的预读,放过;第二次是认领之后级联的重读,让它失败。
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "SMEMBERS", key: sceneMirrorsKey(claimTestZone, parent), skip: 1, times: 1})

	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, parent, "explicit")
	require.EqualValues(t, 1, fired.Load())
	require.NoError(t, err)
	require.True(t, destroyed, "认领已提交,销毁已经发生")
	requireSceneOwnerInZone(t, sc, claimTestZone, child, "", "按预读快照级联")
	requireTeardownPending(t, mr, claimTestZone, parent, true)
	require.Equal(t, deferredBefore+1, teardownJobCount(t, teardownKindInstance, "deferred"))

	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	requireTeardownPending(t, mr, claimTestZone, parent, false)
	require.False(t, mr.Exists(sceneMirrorsKey(claimTestZone, parent)), "补完的级联删掉了子集合索引")
}

// 上一次拆除没做完时,同一个 scene_id 又被登记(号源出错):新场景认领不到(Pending,不重读、不报错),
// 旧任务做完之后才能认领。旧代码:新认领照常提交,旧场景剩下的副作用与新场景混在一起。
func TestSceneTeardown_PendingJobBlocksAReusedSceneID(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const sceneID uint64 = 89630
	seedClaimScene(t, mr, sceneID, "node-a", "", 0)
	first, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.True(t, first.Claimed)
	require.NoError(t, mr.Set(sceneNodeKey(claimTestZone, sceneID), "node-b"))

	second, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.False(t, second.Claimed)
	require.True(t, second.Pending)
	require.Equal(t, "node-b", mustGet(t, mr, sceneNodeKey(claimTestZone, sceneID)), "新场景一个键都不动")

	job := sceneTeardownJob{ZoneID: claimTestZone, SceneID: sceneID, Kind: teardownKindInstance, Reason: "explicit", Claim: first}
	require.True(t, completeSceneTeardown(ctx, sc, job, nil, false))
	third, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.True(t, third.Claimed, "旧任务做完之后照常认领")
	require.Equal(t, "node-b", third.NodeID)
}

// 建实例回滚的认领回包丢失:计数在回滚脚本里就扣了,扫描还登记时记下的名额。
// 旧代码(11-01 之后):调用方拿到错误只记日志,名额由 Agones 对账告警发现。
func TestSceneTeardown_SweepReleasesTheRoomOfALostRollback(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
	ctx := context.Background()
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-r", podIP: "10.0.0.10", state: "Allocated", capacity: 4})
	withAgones(t, sc, alloc, true)
	const sceneID uint64 = 89631
	room := agonesRoom{GameServer: "gs-r", Entry: newAgonesRoomEntry(sceneID)}
	alloc.addEntryForTest("gs-r", room.Entry)
	seedClaimScene(t, mr, sceneID, "node-a", room.record(), 0)
	require.NoError(t, mr.Set(nodeSceneCountKey(claimTestZone, "node-a"), "1"))

	_, err := atomicRollbackInstanceAllocation(sc, claimTestZone, sceneID, "node-a")
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Error(t, err)
	require.Equal(t, "0", mustGet(t, mr, nodeSceneCountKey(claimTestZone, "node-a")))
	_, releases, _ := alloc.stats()
	require.Zero(t, releases)

	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	_, releases, _ = alloc.stats()
	require.Equal(t, 1, releases)
	require.EqualValues(t, 0, alloc.roomsOf("gs-r"))
	requireTeardownPending(t, mr, claimTestZone, sceneID, false)
}

// 排空收尾的认领回包丢失:频道已在同一步摘出排空集合,排空标记与名额由扫描处理,完成时记 scale_in/drained。
// 旧代码:频道已不在排空集合里,下一拍看不到它,标记等过期、名额永久不还。
func TestSceneTeardown_SweepFinishesALostWorldDrain(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
	ctx := context.Background()
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-w", podIP: "10.0.0.11", state: "Allocated", capacity: 4})
	withAgones(t, sc, alloc, true)
	const (
		channel uint64 = 89632
		confID  uint64 = 89633
	)
	channelStr := strconv.FormatUint(channel, 10)
	require.NoError(t, mr.Set(sceneNodeKey(claimTestZone, channel), "node-a"))
	seedAgonesRoomInZone(t, sc, alloc, claimTestZone, channel, "gs-w")
	require.NoError(t, mr.Set(sceneDrainingKey(claimTestZone, channel), "1"))
	_, err := mr.SAdd(worldDrainingSetKey(claimTestZone, confID), channelStr)
	require.NoError(t, err)
	drainedBefore := autoscaleOutcomeCount(t, claimTestZone, "scale_in", "drained")

	finishDrainedChannel(ctx, sc, claimTestZone, confID, channel, "node-a")
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	requireSceneOwnerInZone(t, sc, claimTestZone, channel, "", "前提:认领已执行")
	in, err := mr.IsMember(worldDrainingSetKey(claimTestZone, confID), channelStr)
	if err == nil {
		require.False(t, in, "排空集合成员资格与映射同一步删")
	}
	require.True(t, mr.Exists(sceneDrainingKey(claimTestZone, channel)), "前提:排空标记还没删")

	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	require.False(t, mr.Exists(sceneDrainingKey(claimTestZone, channel)))
	_, releases, _ := alloc.stats()
	require.Equal(t, 1, releases)
	require.EqualValues(t, 0, alloc.roomsOf("gs-w"))
	require.Equal(t, drainedBefore+1, autoscaleOutcomeCount(t, claimTestZone, "scale_in", "drained"))
}

// 不认识的任务种类(比如滚动升级窗口里新版本写下的):扫描不做、不删,留给认识它的版本。
func TestSceneTeardown_UnknownKindStaysPending(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89634
	mr.HSet(sceneTeardownJobKey(claimTestZone, sceneID), "token", "t-1", "kind", "from_a_newer_version", "reason", "x",
		"reply", `["claimed","","","",""]`)
	_, err := mr.ZAdd(sceneTeardownPendingKey(claimTestZone), 1, strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)

	require.Zero(t, sweepSceneTeardownJobs(context.Background(), sc, claimTestZone, pastLease()))
	requireTeardownPending(t, mr, claimTestZone, sceneID, true)
}

// 待办里有条目、任务键却不在(人工删键之类):扫描摘掉条目,不再每拍空转。
func TestSceneTeardown_DanglingPendingEntryIsDropped(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89635
	_, err := mr.ZAdd(sceneTeardownPendingKey(claimTestZone), 1, strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)

	require.Zero(t, sweepSceneTeardownJobs(context.Background(), sc, claimTestZone, pastLease()))
	requireTeardownPending(t, mr, claimTestZone, sceneID, false)
}

// 积压告警的依据:最老一条待办的年龄,没有待办为 0。
func TestSceneTeardown_OldestPendingGaugeTracksTheBacklog(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const sceneID uint64 = 89636
	seedClaimScene(t, mr, sceneID, "node-a", "", 0)
	claim, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.True(t, claim.Claimed)
	claimedAt := mustTeardownClaimedAt(t, mr, sceneID)

	require.Zero(t, sweepSceneTeardownJobs(ctx, sc, claimTestZone, time.Unix(claimedAt+30, 0)))
	require.EqualValues(t, 30, teardownOldestPendingForTest(t))

	require.Equal(t, 1, sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease()))
	sweepSceneTeardownJobs(ctx, sc, claimTestZone, pastLease())
	require.Zero(t, teardownOldestPendingForTest(t), "没有待办时为 0")
}

// mustTeardownClaimedAt 读待办集合里这条任务的分数(认领时刻,Unix 秒)。
func mustTeardownClaimedAt(t *testing.T, mr *miniredis.Miniredis, sceneID uint64) int64 {
	t.Helper()
	score, err := mr.ZScore(sceneTeardownPendingKey(claimTestZone), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	return int64(score)
}
