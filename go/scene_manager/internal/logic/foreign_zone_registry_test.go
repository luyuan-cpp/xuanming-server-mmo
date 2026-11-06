package logic

import (
	"context"
	"errors"
	"testing"
	"time"

	"scene_manager/internal/config"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// installTestZoneFakes 给本包单测装上「别的 zone」的替身(docs/design/control-plane-zone-scoping-20261105.md §3.5、§4.3):
//   - 别的 zone 的 etcd 注册表 = 测试往 knownNodes 里登记的那个 zone 的条目(与收窄前全服镜像给出的事实相同);
//   - 跨 zone 传送的地图预检 = 测试库里目标 zone 的频道集合是否非空(与收窄前的判定相同);
//   - 首次缺席观察清零。
//
// 生产判定本身(foreignOwnerDead / foreignZoneGone / foreignZoneOpensWorldMap)由本文件下面的用例直接测。
func installTestZoneFakes(t *testing.T, rds *redis.Redis) {
	t.Helper()
	prevFetch, prevOpen := zoneRegistryFetchOverride, travelMapOpenOverride
	zoneRegistryFetchOverride = func(_ context.Context, zoneID uint32) ([]nodeEntry, error) {
		knownNodesMu.RLock()
		defer knownNodesMu.RUnlock()
		var entries []nodeEntry
		for _, entry := range knownNodes {
			if entry.reg.ZoneId == zoneID {
				entries = append(entries, entry)
			}
		}
		return entries, nil
	}
	travelMapOpenOverride = func(zoneID uint32, confID uint64) (bool, error) {
		n, err := rds.Scard(worldChannelsKey(zoneID, confID))
		return n > 0, err
	}
	resetForeignAbsenceForTest()
	t.Cleanup(func() {
		zoneRegistryFetchOverride, travelMapOpenOverride = prevFetch, prevOpen
		resetForeignAbsenceForTest()
	})
}

func resetForeignAbsenceForTest() {
	for _, o := range []*absenceObservations{foreignNodeAbsence, foreignZoneAbsence} {
		o.mu.Lock()
		o.at = make(map[string]time.Time)
		o.now = time.Now
		o.mu.Unlock()
	}
}

// backdateForeignAbsenceForTest 让本副本「早在 ago 之前就看到它不在册」,用于验证屏障已过的接管。
func backdateForeignAbsenceForTest(o *absenceObservations, key string, ago time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.at[key] = time.Now().Add(-ago)
}

func foreignTestSvcCtx(t *testing.T, ownZone uint32) *svc.ServiceContext {
	t.Helper()
	c := config.Config{ZoneId: ownZone, StrictNodeTypeSeparation: true}
	return &svc.ServiceContext{Config: c}
}

func fakeZoneRegistry(t *testing.T, registry map[uint32][]nodeEntry, err error) {
	t.Helper()
	prev := zoneRegistryFetchOverride
	zoneRegistryFetchOverride = func(_ context.Context, zoneID uint32) ([]nodeEntry, error) {
		if err != nil {
			return nil, err
		}
		return registry[zoneID], nil
	}
	resetForeignAbsenceForTest()
	t.Cleanup(func() {
		zoneRegistryFetchOverride = prev
		resetForeignAbsenceForTest()
	})
}

func registryEntry(zoneID uint32, nodeID string, sceneNodeType uint32, uuid string) nodeEntry {
	entry := nodeEntry{nodeID: nodeID}
	entry.reg.ZoneId = zoneID
	entry.reg.SceneNodeType = sceneNodeType
	entry.reg.NodeUuid = uuid
	return entry
}

func TestOrchestratedZonesIsOwnZoneOnly(t *testing.T) {
	sc := foreignTestSvcCtx(t, 7)
	assert.Equal(t, []uint32{7}, orchestratedZones(sc))
	assert.True(t, ownsControlZone(sc, 7))
	assert.False(t, ownsControlZone(sc, 8), "同组别的 zone 也不归本进程")
	assert.False(t, ownsControlZone(sc, 0), "zone 未知不算本 zone")
}

func TestLeaderLockKeyPerZone(t *testing.T) {
	assert.Equal(t, "scene_manager:leader:lock:zone:101", LeaderLockKey(101))
	assert.NotEqual(t, LeaderLockKey(101), LeaderLockKey(102))
}

func TestSceneNodeZonePrefixDoesNotMatchOtherZones(t *testing.T) {
	assert.Equal(t, "SceneNodeService.rpc/zone/7/", sceneNodeZonePrefix(7))
	assert.NotContains(t, "SceneNodeService.rpc/zone/70/node_type/20/node_id/1", sceneNodeZonePrefix(7))
}

func TestForeignOwnerDead_PresentHolderIsNotDead(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	fakeZoneRegistry(t, map[uint32][]nodeEntry{2: {registryEntry(2, "20", constants.SceneNodeTypeMainWorld, "u20")}}, nil)
	assert.False(t, foreignOwnerDead(context.Background(), sc, 2, "20", barrierSiteDeadOwnerTakeover))
}

func TestForeignOwnerDead_FirstAbsenceWaitsOneBarrier(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	fakeZoneRegistry(t, map[uint32][]nodeEntry{2: {registryEntry(2, "21", constants.SceneNodeTypeMainWorld, "u21")}}, nil)
	// 第一次看到它不在册:记下时刻、按屏障拒绝(可重试),不接管。
	assert.False(t, foreignOwnerDead(context.Background(), sc, 2, "20", barrierSiteDeadOwnerTakeover))
	// 一直不在册、满一个屏障之后才算死亡证据。
	backdateForeignAbsenceForTest(foreignNodeAbsence, nodeGoneObservedKey(2, "20"), sceneReentryBarrier(sc)+time.Second)
	assert.True(t, foreignOwnerDead(context.Background(), sc, 2, "20", barrierSiteDeadOwnerTakeover))
}

func TestForeignOwnerDead_ReappearanceResetsTheObservation(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	registry := map[uint32][]nodeEntry{2: {}}
	fakeZoneRegistry(t, registry, nil)
	assert.False(t, foreignOwnerDead(context.Background(), sc, 2, "20", barrierSiteDeadOwnerTakeover))
	backdateForeignAbsenceForTest(foreignNodeAbsence, nodeGoneObservedKey(2, "20"), sceneReentryBarrier(sc)+time.Second)
	// 它回来了:观察作废。
	registry[2] = []nodeEntry{registryEntry(2, "20", constants.SceneNodeTypeMainWorld, "u20b")}
	assert.False(t, foreignOwnerDead(context.Background(), sc, 2, "20", barrierSiteDeadOwnerTakeover))
	// 再次消失要重新等满屏障。
	registry[2] = nil
	assert.False(t, foreignOwnerDead(context.Background(), sc, 2, "20", barrierSiteDeadOwnerTakeover))
}

func TestForeignOwnerDead_RegistryReadFailureIsFailClosed(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	fakeZoneRegistry(t, nil, errors.New("etcd unreachable"))
	backdateForeignAbsenceForTest(foreignNodeAbsence, nodeGoneObservedKey(2, "20"), time.Hour)
	assert.False(t, foreignOwnerDead(context.Background(), sc, 2, "20", barrierSiteDeadOwnerTakeover))
}

func TestForeignOwnerDead_NeverJudgesOwnZone(t *testing.T) {
	sc := foreignTestSvcCtx(t, 2)
	fakeZoneRegistry(t, map[uint32][]nodeEntry{}, nil)
	backdateForeignAbsenceForTest(foreignNodeAbsence, nodeGoneObservedKey(2, "20"), time.Hour)
	assert.False(t, foreignOwnerDead(context.Background(), sc, 2, "20", barrierSiteDeadOwnerTakeover),
		"本 zone 走 watch 镜像与 death_at,不走这里")
}

func TestForeignZoneGone_EmptyZoneAfterBarrier(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	registry := map[uint32][]nodeEntry{3: {registryEntry(3, "30", constants.SceneNodeTypeInstance, "u30")}}
	fakeZoneRegistry(t, registry, nil)
	assert.False(t, foreignZoneGone(context.Background(), sc, 3, barrierSiteStaleLocation), "还有节点在册")
	registry[3] = nil
	assert.False(t, foreignZoneGone(context.Background(), sc, 3, barrierSiteStaleLocation), "第一次看到空要等屏障")
	backdateForeignAbsenceForTest(foreignZoneAbsence, "3", sceneReentryBarrier(sc)+time.Second)
	assert.True(t, foreignZoneGone(context.Background(), sc, 3, barrierSiteStaleLocation))
}

func TestForeignRegisteredInstance_AmbiguousIdentityIsNotEvidence(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	fakeZoneRegistry(t, map[uint32][]nodeEntry{2: {
		registryEntry(2, "20", constants.SceneNodeTypeMainWorld, "a"),
		registryEntry(2, "20", constants.SceneNodeTypeMainWorld, "b"),
	}}, nil)
	_, entries, ok := foreignRegisteredInstance(context.Background(), sc, 2, "20")
	assert.False(t, ok)
	assert.True(t, entriesRegisterInstance(entries, "a"))
	assert.False(t, entriesRegisterInstance(entries, "c"))
}

func TestTravelMapPrecheckUsesEtcd_WorldHostingNodeOpensWorldMaps(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{1001}))
	registry := map[uint32][]nodeEntry{2: {registryEntry(2, "21", constants.SceneNodeTypeInstance, "u21")}}
	fakeZoneRegistry(t, registry, nil)
	prev := travelMapOpenOverride
	travelMapOpenOverride = nil
	t.Cleanup(func() { travelMapOpenOverride = prev })

	open, err := foreignZoneOpensWorldMap(context.Background(), sc, 2, 1001)
	require.NoError(t, err)
	assert.False(t, open, "只有副本节点:按类型分池时大世界没有宿主")

	registry[2] = append(registry[2], registryEntry(2, "20", constants.SceneNodeTypeMainWorld, "u20"))
	open, err = foreignZoneOpensWorldMap(context.Background(), sc, 2, 1001)
	require.NoError(t, err)
	assert.True(t, open)

	open, err = foreignZoneOpensWorldMap(context.Background(), sc, 2, 9999)
	require.NoError(t, err)
	assert.False(t, open, "不在 World 表里的 conf(副本 / 镜像 / 乱填)一律算没开")
}

func TestTravelMapPrecheckUsesEtcd_ReadFailureIsReportedForFailOpen(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{1001}))
	fakeZoneRegistry(t, nil, errors.New("etcd unreachable"))
	prev := travelMapOpenOverride
	travelMapOpenOverride = nil
	t.Cleanup(func() { travelMapOpenOverride = prev })

	_, err := foreignZoneOpensWorldMap(context.Background(), sc, 2, 1001)
	assert.Error(t, err, "调用方按原口径放行,这里必须把读失败报出来")
}

func TestTravelMapPrecheckUsesEtcd_UnsplitPoolCountsAnyNode(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1)
	sc.Config.StrictNodeTypeSeparation = false
	t.Cleanup(SetWorldConfIdsForTest([]uint64{1001}))
	fakeZoneRegistry(t, map[uint32][]nodeEntry{2: {registryEntry(2, "21", constants.SceneNodeTypeInstance, "u21")}}, nil)
	prev := travelMapOpenOverride
	travelMapOpenOverride = nil
	t.Cleanup(func() { travelMapOpenOverride = prev })

	open, err := foreignZoneOpensWorldMap(context.Background(), sc, 2, 1001)
	require.NoError(t, err)
	assert.True(t, open)
}

func TestOwnsControlZoneSkipsForeignMembership(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "10")
	// 本进程是 zone 1;往 zone 2 的成员集合写入一律跳过(由 zone 2 自己的对账收敛)。
	require.NoError(t, addSceneMember(sc, 42, 900, 2, "20"))
	assert.False(t, mr.Exists(instanceMembersKey(900)))
	assert.False(t, mr.Exists(nodeMembersKey(2, "20")))
	// 本 zone 照常写。
	require.NoError(t, addSceneMember(sc, 42, 901, testZoneId, "10"))
	ok, err := mr.SIsMember(instanceMembersKey(901), "42")
	require.NoError(t, err)
	assert.True(t, ok)
	// 只给场景号:scene:{id}:zone 不是本 zone 就不动。
	mr.Set(sceneZoneKey(902), "2")
	mr.SAdd(instanceMembersKey(902), "42")
	require.NoError(t, removeScenePlayer(sc, 42, 902, 0))
	ok, err = mr.SIsMember(instanceMembersKey(902), "42")
	require.NoError(t, err)
	assert.True(t, ok, "别的 zone 的场景成员不由本进程摘")
}
