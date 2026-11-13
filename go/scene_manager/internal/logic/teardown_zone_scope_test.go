package logic

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

// 只给场景号的入口与销毁路径只读写本 zone 的控制域键(docs/design/control-plane-zone-scoping-20261105.md §4.3:zone z 的
// 控制域键只由 zone z 的 scene_manager 读写)的回归。
//
// 2026-11-12 起控制域键都带 zone 的 tag(docs/design/redis-cluster-all-domains-20261110.md §3):场景键是
// scene:{ctl:<z>}:<id>:*,同一个场景号在别的 zone 是另一组键。只拿得到场景号的入口按本进程的 zone 拼键(§3.3),
// 别的 zone 的场景读成「不存在」。原来靠 scene:<id>:zone 认出别的 zone(认领脚本回 foreign、销毁回
// errSceneInOtherZone、级联与拆除先读子镜像 / 源的 zone),那把键与这些分支一起删了。各用例核对两件事:
//   - 「不存在」与原来「是别的 zone」走同一个结局,不销毁、不拆除任何东西;
//   - 别的 zone 的键一把都不读、不写(用例按 tag 取出别的 zone 的全部键,前后逐字比对)。

// foreignTestZone 是与 claimTestZone 共用一个 Redis 的另一个 zone。
const foreignTestZone = claimTestZone + 1

// zoneKeysForTest 回 zone 的全部控制域键与值(按 tag 取,列表与集合取成员):用来核对别的 zone 一个字节都没动。
func zoneKeysForTest(t *testing.T, mr *miniredis.Miniredis, zoneID uint32) map[string]string {
	t.Helper()
	tag := sceneControlTag(zoneID)
	out := make(map[string]string)
	for _, key := range mr.Keys() {
		if !strings.Contains(key, tag) {
			continue
		}
		switch mr.Type(key) {
		case "string":
			v, err := mr.Get(key)
			require.NoError(t, err)
			out[key] = v
		case "set":
			members, err := mr.Members(key)
			require.NoError(t, err)
			out[key] = strings.Join(members, ",")
		case "zset":
			members, err := mr.ZMembers(key)
			require.NoError(t, err)
			pairs := make([]string, 0, len(members))
			for _, member := range members {
				score, err := mr.ZScore(key, member)
				require.NoError(t, err)
				pairs = append(pairs, member+"="+strconv.FormatFloat(score, 'f', -1, 64))
			}
			out[key] = strings.Join(pairs, ",")
		case "hash":
			fields, err := mr.HKeys(key)
			require.NoError(t, err)
			pairs := make([]string, 0, len(fields))
			for _, field := range fields {
				pairs = append(pairs, field+"="+mr.HGet(key, field))
			}
			out[key] = strings.Join(pairs, ",")
		default:
			out[key] = mr.Type(key)
		}
	}
	return out
}

// mirrorColocateCountForTest 读 scene_manager_mirror_colocate_total 的一个序列。
func mirrorColocateCountForTest(t *testing.T, zoneID uint32, outcome, reason string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_mirror_colocate_total", map[string]string{
		"zone_id": strconv.FormatUint(uint64(zoneID), 10), "outcome": outcome, "reason": reason,
	})
}

// 同一个场景号只登记在别的 zone:本 zone 的认领读不到映射,force 认领(谁是属主都拆)与建实例回滚(按期望属主)都落空,
// 一个键都不动。原来认领脚本读 scene:<id>:zone、回 foreign。
func TestSceneClaim_SceneInAnotherZoneIsNotClaimed(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		sceneID uint64 = 89801
		node           = "node-f"
	)
	seedClaimSceneInZone(t, mr, foreignTestZone, sceneID, node, "gs-f", 1)
	require.NoError(t, mr.Set(nodeSceneCountKey(foreignTestZone, node), "1"))
	_, err := mr.SAdd(nodeScenesKey(foreignTestZone, node), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	before := mr.Dump()

	force, err := AtomicDestroyForce(sc, claimTestZone, sceneID, "", "explicit")
	require.NoError(t, err)
	require.False(t, force.Claimed, "force 认领:本 zone 没有映射")

	rollback, err := atomicRollbackInstanceAllocation(sc, claimTestZone, sceneID, node)
	require.NoError(t, err)
	require.False(t, rollback.Claimed, "建实例回滚:期望属主在本 zone 落空")

	require.Equal(t, before, mr.Dump(), "一个键都不动")
}

// 销毁入口拿到只登记在别的 zone 的场景:与「映射已不存在」同一个结局 —— 幂等成功、不销毁,别的 zone 的键不动。
// 原来回 errSceneInOtherZone。
func TestDestroyInstance_SceneInAnotherZoneReadsAsMissing(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	ctx := context.Background()
	const sceneID uint64 = 89803
	seedClaimSceneInZone(t, mr, foreignTestZone, sceneID, "node-f", "", 0)
	before := mr.Dump()

	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, sceneID, "explicit")
	require.NoError(t, err)
	require.False(t, destroyed)
	require.Equal(t, before, mr.Dump(), "一个键都不动")
}

// 源销毁时级联源的子集合。子集合是本 zone 的键;列着的子镜像不在本 zone 的键里(只登记在别的 zone)时,认领读不到它、
// 什么都不拆,别的 zone 的映射、active 条目、成员、标记与反向索引一个字节都不动,也不替它写拆除任务;本 zone 的子镜像照常
// 级联销毁,子集合做完照删,源的拆除任务做完。原来先读子镜像的 scene:<id>:zone、是别的 zone 就跳过。
func TestCascadeDestroyMirrors_ChildOfAnotherZoneIsNotTouched(t *testing.T) {
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
	require.NoError(t, mr.Set(sceneMirrorFlagKey(claimTestZone, own), "1"))
	require.NoError(t, mr.Set(sceneSourceKey(claimTestZone, own), parentStr))
	seedClaimSceneInZone(t, mr, foreignTestZone, foreign, "node-f", "", 1)
	require.NoError(t, mr.Set(sceneMirrorFlagKey(foreignTestZone, foreign), "1"))
	require.NoError(t, mr.Set(sceneSourceKey(foreignTestZone, foreign), parentStr))
	_, err := mr.SAdd(sceneMirrorsKey(claimTestZone, parent), strconv.FormatUint(own, 10), strconv.FormatUint(foreign, 10))
	require.NoError(t, err)
	foreignBefore := zoneKeysForTest(t, mr, foreignTestZone)

	destroyed, err := destroyInstanceForce(ctx, sc, claimTestZone, parent, "explicit")
	require.NoError(t, err)
	require.True(t, destroyed)

	requireSceneOwnerInZone(t, sc, claimTestZone, own, "", "本 zone 的子镜像随源级联销毁")
	requireTeardownPending(t, mr, claimTestZone, own, false)
	require.False(t, mr.Exists(sceneTeardownJobKey(claimTestZone, foreign)), "不替不在本 zone 的子镜像写拆除任务")

	require.Equal(t, foreignBefore, zoneKeysForTest(t, mr, foreignTestZone), "别的 zone 的键一个字节都不动")
	requireActiveInstance(t, sc, foreignTestZone, foreign, true)

	require.False(t, mr.Exists(sceneMirrorsKey(claimTestZone, parent)), "源的子集合是本 zone 的键,级联做完照删")
	requireTeardownPending(t, mr, claimTestZone, parent, false)
}

// 源频道迁移时同理:不在本 zone 的镜像认领落空、不拆,按已处理从源的子集合(本 zone 的键)里摘掉;本 zone 的镜像照常级联。
func TestCascadeMirrorsOnSourceMigration_MirrorOfAnotherZoneIsNotTouched(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		source  uint64 = 89807
		own     uint64 = 89808
		foreign uint64 = 89809
	)
	ownStr, foreignStr := strconv.FormatUint(own, 10), strconv.FormatUint(foreign, 10)
	seedClaimScene(t, mr, own, "node-a", "", 0)
	seedClaimSceneInZone(t, mr, foreignTestZone, foreign, "node-f", "", 0)
	_, err := mr.SAdd(sceneMirrorsKey(claimTestZone, source), ownStr, foreignStr)
	require.NoError(t, err)
	foreignBefore := zoneKeysForTest(t, mr, foreignTestZone)

	cascadeMirrorsOnSourceMigration(context.Background(), sc, claimTestZone, source, []string{ownStr, foreignStr}, "node-a", "node-b")

	requireSceneOwnerInZone(t, sc, claimTestZone, own, "", "本 zone 的镜像随源迁移级联销毁")
	require.Equal(t, foreignBefore, zoneKeysForTest(t, mr, foreignTestZone), "别的 zone 的键一个字节都不动")
	requireActiveInstance(t, sc, foreignTestZone, foreign, true)
	require.False(t, mr.Exists(sceneTeardownJobKey(claimTestZone, foreign)), "不替不在本 zone 的镜像写拆除任务")
	require.False(t, mr.Exists(sceneMirrorsKey(claimTestZone, source)), "两个都处理完,源的子集合摘空")
}

// 拆除时从镜像源的子集合摘自己:子集合在本 zone 的键里,源在不在本 zone 都照摘(源已销毁或从来不在本 zone 时集合不存在,
// SREM 什么都不动);同一个源号在别的 zone 的子集合是别的 zone 的键,一个成员都不摘。原来先读源的 scene:<id>:zone,
// 是别的 zone 就不动、值损坏就回错误。
func TestUnlinkFromSourceMirrors_OnlyTouchesTheOwnZoneSet(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		mirror  uint64 = 89810
		source  uint64 = 89811
		sibling uint64 = 89815
	)
	mirrorStr, siblingStr := strconv.FormatUint(mirror, 10), strconv.FormatUint(sibling, 10)
	for _, zone := range []uint32{claimTestZone, foreignTestZone} {
		_, err := mr.SAdd(sceneMirrorsKey(zone, source), mirrorStr, siblingStr)
		require.NoError(t, err)
	}
	membersOf := func(zone uint32) []string {
		t.Helper()
		members, err := mr.Members(sceneMirrorsKey(zone, source))
		require.NoError(t, err)
		return members
	}

	require.NoError(t, unlinkFromSourceMirrors(sc, claimTestZone, mirror, strconv.FormatUint(source, 10)))
	require.Equal(t, []string{siblingStr}, membersOf(claimTestZone), "本 zone 的子集合摘掉自己")
	require.ElementsMatch(t, []string{mirrorStr, siblingStr}, membersOf(foreignTestZone), "别的 zone 的子集合不动")

	require.NoError(t, unlinkFromSourceMirrors(sc, claimTestZone, mirror, "89899"), "源从来不在本 zone:集合不存在,什么都不动")
	require.NoError(t, unlinkFromSourceMirrors(sc, claimTestZone, mirror, ""), "不是镜像:没有要摘的")
}

// 显式 DestroyScene 只拆本进程 zone 的场景:请求别的 zone 时在任何读写之前拒绝。请求本 zone(或 zone_id 没填,按本进程的
// zone)而场景只登记在别的 zone 时,本 zone 的键里读不到它:与映射已不存在同一个结局,幂等成功、一个键都不动。zone_id 没填
// 照常拆本 zone 的场景。原来后两种回 errSceneInOtherZone。
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
	require.NoError(t, destroy(&scene_manager.DestroySceneRequest{SceneId: foreignScene, ZoneId: testZoneId}),
		"请求本 zone、场景只登记在别的 zone:读成不存在,幂等成功")
	require.NoError(t, destroy(&scene_manager.DestroySceneRequest{SceneId: foreignScene}), "zone_id 没填按本进程的 zone")
	require.Equal(t, before, mr.Dump(), "三次都一个键都不动")

	require.NoError(t, destroy(&scene_manager.DestroySceneRequest{SceneId: ownScene}))
	requireSceneOwner(t, sc, ownScene, "", "zone_id 没填按本进程的 zone 拆")
	requireActiveInstance(t, sc, testZoneId, ownScene, false)
	requireTeardownPending(t, mr, testZoneId, ownScene, false)
}

// CreateScene 的 zone_id = 0(调用方没填)按本进程的 zone 登记,与 DestroyScene 同一口径(§3.3)。原来以 zone 0 登记:
// 场景键、active 集合与节点计数都写到 zone 0,而控制域里不该有 zone 0 的键。
func TestCreateScene_ZoneZeroRegistersInOwnZone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	registerTypedNode(mr, testZoneId, "Y", constants.SceneNodeTypeInstance, 0)

	resp, err := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)
	require.Zero(t, resp.ErrorCode)
	require.Equal(t, "Y", mustGet(t, mr, sceneNodeKey(testZoneId, resp.SceneId)), "登记在本进程的 zone")
	requireActiveInstance(t, sc, testZoneId, resp.SceneId, true)
	zeroTag := sceneControlTag(0)
	for _, key := range mr.Keys() {
		assert.NotContains(t, key, zeroTag, "不得出现 zone 0 的控制域键")
	}
}

// ownZoneOfScene 是「只给场景号,它在哪个 zone」的唯一答法(EnterScene 第 1 步与位置记录、LeaveScene 的释放判定用它):
// 只看本 zone 的属主键。别的 zone 的场景、不存在、读失败都回 0(zone 未知),与原来 scene:<id>:zone 没有记录同一个结局。
func TestOwnZoneOfScene_OnlyAnswersForTheOwnZone(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		own     uint64 = 89820
		foreign uint64 = 89821
		missing uint64 = 89822
	)
	require.NoError(t, mr.Set(sceneNodeKey(testZoneId, own), "node-a"))
	require.NoError(t, mr.Set(sceneNodeKey(testZoneId+1, foreign), "node-f"))

	assert.Equal(t, testZoneId, ownZoneOfScene(sc, own))
	assert.Zero(t, ownZoneOfScene(sc, foreign), "别的 zone 的场景读成不存在")
	assert.Zero(t, ownZoneOfScene(sc, missing))
	mr.SetError("ERR injected")
	assert.Zero(t, ownZoneOfScene(sc, own), "读失败按 zone 未知")
	mr.SetError("")
}

// EnterScene 只给场景号(没填 zone_id)、场景却只登记在别的 zone:不再按 scene:<id>:zone 把玩家重定向到场景所在的 zone,
// 按「zone 未知」定去向 —— 没有位置记录时就是 gate 所在的 zone;在本 zone 查无此场景,回 ErrNoAvailableNode,
// 不判活、不改派、不重定向,别的 zone 的键一个字节都不动。
func TestEnterScene_SceneIDWithoutZoneOfAnotherZoneIsNotFollowed(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID     = uint64(5191)
		foreignScene = uint64(2291)
	)
	otherZone := testZoneId + 1
	seedSceneOnNode(mr, otherZone, foreignScene, "10", "3")
	otherBefore := zoneKeysForTest(t, mr, otherZone)

	logic := NewEnterSceneLogic(context.Background(), sc)
	var redirectZones []uint32
	logic.assignGateForZone = func(_ context.Context, _ *svc.ServiceContext, zone uint32, _ uint64) (*scene_manager.RedirectToGateInfo, error) {
		redirectZones = append(redirectZones, zone)
		return nil, errors.New("must not redirect")
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: foreignScene,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode, "本 zone 查无此场景")
	assert.Nil(t, resp.Redirect)
	assert.Empty(t, redirectZones, "不往场景所在的 zone 重定向")
	assert.Empty(t, *captured, "不发路由")
	assert.Equal(t, otherBefore, zoneKeysForTest(t, mr, otherZone), "别的 zone 的键一个字节都不动")
	assert.False(t, mr.Exists(sceneNodeKey(testZoneId, foreignScene)), "本 zone 不凭空写出属主")
}
