package logic

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"proto/scene_manager"
)

// 销毁路径只读写本 zone 的控制域键(docs/design/control-plane-zone-scoping-20261105.md §4.3:zone z 的控制域键只由
// zone z 的 scene_manager 读写)的回归。同组几个 zone 共用一个控制域 Redis 时,别的 zone 的场景键在这里读得到、
// 也删得掉;各用例注释写了 2026-11-06 之前代码的表现。

// foreignTestZone 是与 claimTestZone 共用一个 Redis 的另一个 zone。
const foreignTestZone = claimTestZone + 1

// 认领脚本在同一步比对场景登记的 zone:不是认领方的 zone 就回 Foreign、一个键都不动。三种认领(force、空闲、
// 建实例回滚)都走这一段。zone 记录损坏时同样不认领(不猜)。
// 旧代码:脚本不看 zone,删掉别的 zone 的映射与成员,按本 zone 的 active 集合与节点计数记账。
func TestSceneClaim_SceneInAnotherZoneIsNotClaimed(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		sceneID   uint64 = 89801
		corruptID uint64 = 89802
		node             = "node-f"
	)
	seedClaimSceneInZone(t, mr, foreignTestZone, sceneID, node, "gs-f", 1)
	require.NoError(t, mr.Set(nodeSceneCountKey(foreignTestZone, node), "1"))
	_, err := mr.SAdd(nodeScenesKey(foreignTestZone, node), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	seedClaimScene(t, mr, corruptID, node, "", 0)
	require.NoError(t, mr.Set(sceneZoneKey(corruptID), "not-a-zone"))
	before := mr.Dump()

	force, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.True(t, force.Foreign, "force 认领")
	require.False(t, force.Claimed)

	idle, err := ClaimIdleSceneTeardown(sc, claimTestZone, sceneID)
	require.NoError(t, err)
	require.True(t, idle.Foreign, "空闲认领")
	require.False(t, idle.Claimed)

	rollback, err := atomicRollbackInstanceAllocation(sc, claimTestZone, sceneID, node)
	require.NoError(t, err)
	require.True(t, rollback.Foreign, "建实例回滚")
	require.False(t, rollback.Claimed)

	corrupt, err := AtomicDestroyForce(sc, claimTestZone, corruptID, "", "explicit")
	require.NoError(t, err)
	require.True(t, corrupt.Foreign, "zone 记录损坏时不认领")

	require.Equal(t, before, mr.Dump(), "一个键都不动")
}

// 销毁入口拿到别的 zone 的场景:回 errSceneInOtherZone,不当成「已被别人销毁」静默成功。
// 旧代码:照常销毁,回 (true, nil)。
func TestDestroyInstance_SceneInAnotherZoneIsAnError(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const sceneID uint64 = 89803
	seedClaimSceneInZone(t, mr, foreignTestZone, sceneID, "node-f", "", 0)
	before := mr.Dump()

	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, sceneID, "explicit")
	require.ErrorIs(t, err, errSceneInOtherZone)
	require.False(t, destroyed)

	destroyed, err = destroyInstanceInternal(ctx, sc, claimTestZone, sceneID, false, "", "idle")
	require.ErrorIs(t, err, errSceneInOtherZone)
	require.False(t, destroyed)

	require.Equal(t, before, mr.Dump(), "一个键都不动")
}

// 源销毁时只级联本 zone 的子镜像。登记在别的 zone 的子镜像原样留给那个 zone(它的映射、active 条目、成员都不动,
// 也不替它写拆除任务);源的子集合是本 zone 的键,级联做完照删,源的拆除任务做完。
// 旧代码:按子镜像自己的 zone 去 force 销毁,删掉别的 zone 的映射、active 条目与节点计数。
func TestCascadeDestroyMirrors_LeavesAChildOfAnotherZone(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const (
		parent  uint64 = 89804
		own     uint64 = 89805
		foreign uint64 = 89806
	)
	parentStr := strconv.FormatUint(parent, 10)
	seedClaimScene(t, mr, parent, "node-a", "", 0)
	seedClaimScene(t, mr, own, "node-a", "", 0)
	seedClaimSceneInZone(t, mr, foreignTestZone, foreign, "node-f", "", 1)
	for _, child := range []uint64{own, foreign} {
		require.NoError(t, mr.Set(sceneMirrorFlagKey(child), "1"))
		require.NoError(t, mr.Set(sceneSourceKey(child), parentStr))
	}
	_, err := mr.SAdd(sceneMirrorsKey(parent), strconv.FormatUint(own, 10), strconv.FormatUint(foreign, 10))
	require.NoError(t, err)

	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, parent, "explicit")
	require.NoError(t, err)
	require.True(t, destroyed)

	requireSceneOwner(t, sc, own, "", "本 zone 的子镜像随源级联销毁")
	requireTeardownPending(t, mr, claimTestZone, own, false)

	requireSceneOwner(t, sc, foreign, "node-f", "别的 zone 的子镜像不动")
	require.Equal(t, strconv.FormatUint(uint64(foreignTestZone), 10), mustGet(t, mr, sceneZoneKey(foreign)))
	require.Equal(t, "1", mustGet(t, mr, sceneMirrorFlagKey(foreign)))
	require.Equal(t, parentStr, mustGet(t, mr, sceneSourceKey(foreign)))
	requireActiveInstance(t, sc, foreignTestZone, foreign, true)
	require.True(t, mr.Exists(instanceMembersKey(foreign)), "别的 zone 的成员不动")
	require.False(t, mr.Exists(sceneTeardownJobKey(foreign)), "不替别的 zone 写拆除任务")

	require.False(t, mr.Exists(sceneMirrorsKey(parent)), "源的子集合是本 zone 的键,级联做完照删")
	requireTeardownPending(t, mr, claimTestZone, parent, false)
}

// 源频道迁移时同理:别的 zone 的镜像不拆,只从源的子集合(本 zone 的键)里摘掉;本 zone 的镜像照常级联。
// 旧代码:按镜像自己的 zone 去 force 销毁。
func TestCascadeMirrorsOnSourceMigration_LeavesAMirrorOfAnotherZone(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		source  uint64 = 89807
		own     uint64 = 89808
		foreign uint64 = 89809
	)
	ownStr, foreignStr := strconv.FormatUint(own, 10), strconv.FormatUint(foreign, 10)
	seedClaimScene(t, mr, own, "node-a", "", 0)
	seedClaimSceneInZone(t, mr, foreignTestZone, foreign, "node-f", "", 0)
	_, err := mr.SAdd(sceneMirrorsKey(source), ownStr, foreignStr)
	require.NoError(t, err)

	cascadeMirrorsOnSourceMigration(context.Background(), sc, claimTestZone, source, []string{ownStr, foreignStr}, "node-a", "node-b")

	requireSceneOwner(t, sc, own, "", "本 zone 的镜像随源迁移级联销毁")
	requireSceneOwner(t, sc, foreign, "node-f", "别的 zone 的镜像不动")
	requireActiveInstance(t, sc, foreignTestZone, foreign, true)
	require.False(t, mr.Exists(sceneTeardownJobKey(foreign)), "不替别的 zone 写拆除任务")
	require.False(t, mr.Exists(sceneMirrorsKey(source)), "两个都处理完,源的子集合摘空")
}

// 拆除时从镜像源的子集合摘自己:源登记在本 zone,或本实例里没有源的 zone 记录(源已销毁,或在别的组的实例里)时摘;
// 源登记在别的 zone 时不碰它的键;源的 zone 记录损坏时回错误(拆除任务留着重做),不猜。
// 旧代码:不看源在哪个 zone,直接 SREM。
func TestUnlinkFromSourceMirrors_OnlyTouchesASourceOfTheSameZone(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		mirror        uint64 = 89810
		ownSource     uint64 = 89811
		foreignSource uint64 = 89812
		goneSource    uint64 = 89813
		corruptSource uint64 = 89814
		sibling       uint64 = 89815
	)
	mirrorStr, siblingStr := strconv.FormatUint(mirror, 10), strconv.FormatUint(sibling, 10)
	require.NoError(t, mr.Set(sceneZoneKey(ownSource), strconv.FormatUint(uint64(claimTestZone), 10)))
	require.NoError(t, mr.Set(sceneZoneKey(foreignSource), strconv.FormatUint(uint64(foreignTestZone), 10)))
	require.NoError(t, mr.Set(sceneZoneKey(corruptSource), "not-a-zone"))
	for _, source := range []uint64{ownSource, foreignSource, goneSource, corruptSource} {
		_, err := mr.SAdd(sceneMirrorsKey(source), mirrorStr, siblingStr)
		require.NoError(t, err)
	}
	membersOf := func(source uint64) []string {
		t.Helper()
		members, err := mr.Members(sceneMirrorsKey(source))
		require.NoError(t, err)
		return members
	}
	unlink := func(source uint64) error {
		return unlinkFromSourceMirrors(sc, claimTestZone, mirror, strconv.FormatUint(source, 10))
	}

	require.NoError(t, unlink(ownSource))
	require.Equal(t, []string{siblingStr}, membersOf(ownSource), "本 zone 的源")

	require.NoError(t, unlink(goneSource))
	require.Equal(t, []string{siblingStr}, membersOf(goneSource),
		"没有 zone 记录(源已销毁,或在别的组的实例里):这份子集合只有本 zone 写(createInstance)")

	require.NoError(t, unlink(foreignSource))
	require.ElementsMatch(t, []string{mirrorStr, siblingStr}, membersOf(foreignSource), "别的 zone 的源的子集合不动")

	require.Error(t, unlink(corruptSource))
	require.ElementsMatch(t, []string{mirrorStr, siblingStr}, membersOf(corruptSource), "zone 记录损坏时不动")

	require.NoError(t, unlinkFromSourceMirrors(sc, claimTestZone, mirror, ""), "不是镜像:没有要摘的")
}

// 显式 DestroyScene 只拆本进程 zone 的场景:请求别的 zone 时在任何读写之前拒绝;请求本 zone(或 zone_id 没填,
// 按本进程的 zone)而场景登记在别的 zone 时,认领脚本不动它、回 errSceneInOtherZone。zone_id 没填照常拆本 zone 的场景。
// 旧代码:zone_id 原样往下传,别的 zone 的场景照拆;没填时按 zone 0 认领,active 条目与节点计数记到 zone 0 的键上。
func TestDestroyScene_OnlyDestroysScenesOfItsOwnZone(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const (
		foreignScene uint64 = 89816
		ownScene     uint64 = 89817
	)
	otherZone := testZoneId + 1
	seedClaimSceneInZone(t, mr, otherZone, foreignScene, "node-f", "", 0)
	seedClaimSceneInZone(t, mr, testZoneId, ownScene, "node-a", "", 0)
	destroy := func(req *scene_manager.DestroySceneRequest) error {
		_, err := NewDestroySceneLogic(ctx, sc).DestroyScene(req)
		return err
	}
	before := mr.Dump()

	require.Error(t, destroy(&scene_manager.DestroySceneRequest{SceneId: foreignScene, ZoneId: otherZone}), "请求别的 zone")
	require.ErrorIs(t, destroy(&scene_manager.DestroySceneRequest{SceneId: foreignScene, ZoneId: testZoneId}), errSceneInOtherZone,
		"请求本 zone、场景登记在别的 zone")
	require.ErrorIs(t, destroy(&scene_manager.DestroySceneRequest{SceneId: foreignScene}), errSceneInOtherZone,
		"zone_id 没填按本进程的 zone")
	require.Equal(t, before, mr.Dump(), "三次拒绝一个键都不动")

	require.NoError(t, destroy(&scene_manager.DestroySceneRequest{SceneId: ownScene}))
	requireSceneOwner(t, sc, ownScene, "", "zone_id 没填按本进程的 zone 拆")
	requireActiveInstance(t, sc, testZoneId, ownScene, false)
	requireTeardownPending(t, mr, testZoneId, ownScene, false)
}
