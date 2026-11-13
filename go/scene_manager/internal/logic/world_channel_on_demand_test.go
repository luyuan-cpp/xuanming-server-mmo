package logic

// 进场时按需铺大世界频道(world_channel_on_demand.go;docs/design/control-plane-zone-scoping-20261105.md §6 第 1 条)。
//
// 修复前红的取法:resolveSceneForEnter 里 reserveWorldChannelCreatingOnDemand 换回 ReserveBestWorldChannelForEnter,
// 前三条进场用例回 ErrNoAvailableNode;预检那条在 hasWorldHostingRegistration 里删掉 Draining 判断后红。

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

const (
	// onDemandTestConf 是本文件用例里的默认大世界(World 表第一行,用 SetWorldConfIdsForTest 顶上)。
	onDemandTestConf = uint64(3381)
	// onDemandIsolatedZone 是只给本文件用的 zone:用例要往进程级的 etcd 镜像里登记节点,别的 zone 的残留不能干扰判定。
	onDemandIsolatedZone = uint32(461)
)

// installWarmupSleepForTest 替换「等领导者」的退避等待,用例不真的等。
func installWarmupSleepForTest(t *testing.T, sleep func(context.Context, time.Duration) bool) {
	t.Helper()
	prev := worldChannelWarmupSleep
	worldChannelWarmupSleep = sleep
	t.Cleanup(func() { worldChannelWarmupSleep = prev })
}

// mirrorRegistrationForTest 往本副本的 etcd 镜像里登记一条大世界节点注册(draining = 自报计划排空)。
func mirrorRegistrationForTest(t *testing.T, zoneID uint32, nodeID string, draining bool) {
	t.Helper()
	key := fmt.Sprintf("SceneNodeService.rpc/zone/%d/on-demand-test/node/%s", zoneID, nodeID)
	t.Cleanup(SetKnownNodeForTest(key, nodeID, "", zoneID, constants.SceneNodeTypeMainWorld))
	knownNodesMu.Lock()
	entry := knownNodes[key]
	entry.reg.Draining = draining
	knownNodes[key] = entry
	knownNodesMu.Unlock()
}

func enterDefaultWorldForTest(t *testing.T, sc *svc.ServiceContext, playerID uint64, zoneID uint32) *scene_manager.EnterSceneResponse {
	t.Helper()
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, ZoneId: zoneID, GateZoneId: zoneID, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	return resp
}

// requireLandedOnOnlyChannel:默认大世界恰好一个频道,玩家落在它上面(节点 10),路由事件推给了 gate。
func requireLandedOnOnlyChannel(t *testing.T, sc *svc.ServiceContext, zoneID uint32, playerID uint64, captured *[]kafka.Message) {
	t.Helper()
	channels, err := sc.Redis.Smembers(worldChannelsKey(zoneID, onDemandTestConf))
	require.NoError(t, err)
	require.Len(t, channels, 1, "默认大世界一个频道(WorldChannelCount=1)")
	loc, err := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, err)
	require.NotNil(t, loc)
	assert.Equal(t, channels[0], strconv.FormatUint(loc.SceneId, 10), "落在这个频道上")
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, zoneID, loc.ZoneId)
	require.Len(t, *captured, 1, "路由事件推给 gate")
}

// 首登进一个刚起的 zone:节点已在负载集(领导者同步过),世界频道还没铺 → 当场补铺,落地。
func TestEnterScene_FirstLoginCreatesDefaultWorldChannelOnDemand(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	fake := withReachableSceneNode(t, sc, "10")
	t.Cleanup(SetWorldConfIdsForTest([]uint64{onDemandTestConf}))
	mr.ZAdd(testLoadKey(), 0, "10")
	seedNodeMembers(mr, testZoneId, "10", 0)
	createsBefore := fake.createCalls.Load()

	resp := enterDefaultWorldForTest(t, sc, 6181, testZoneId)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)
	requireLandedOnOnlyChannel(t, sc, testZoneId, 6181, captured)
	assert.Equal(t, createsBefore+1, fake.createCalls.Load(), "一个频道只建一次")
}

// 跨 zone 传送的第二条腿:源 scene 已经销毁了实体,目标 zone 刚起、频道还没铺,也要落得了地。
func TestEnterScene_TravelSecondLegCreatesDefaultWorldChannelOnDemand(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.ZoneId = 2 // 第二条腿来自 zone 2 的 gate,只会到 zone 2 的 scene_manager(§3.3)
	captured := capturingKafkaWriter(sc)
	withReachableSceneNode(t, sc, "10")
	t.Cleanup(SetWorldConfIdsForTest([]uint64{onDemandTestConf}))
	mr.ZAdd(nodeLoadKey(2), 0, "10")
	seedNodeMembers(mr, 2, "10", 0)

	const playerID = uint64(6182)
	// 第一条腿留下的等待落点:zone 2、无节点、epoch 1,没指定地图(第二条腿按默认大世界解析)。
	placed, err := placePlayerLocation(sc, playerID, 0, "", 2, placementGuard{observedEpoch: 0, mint: true})
	require.NoError(t, err)
	require.Equal(t, uint64(1), placed.epoch)

	resp := enterDefaultWorldForTest(t, sc, playerID, 2)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)
	requireLandedOnOnlyChannel(t, sc, 2, playerID, captured)
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "落点照常铸造下一代")
}

// 领导者还没完成第一次同步:节点在本副本的 etcd 镜像里,负载集里还没有 → 在期限内等它,不拿镜像自己挑节点。
func TestEnterScene_OnDemandWaitsForLeaderToAdmitRegisteredNode(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.ZoneId = onDemandIsolatedZone
	captured := capturingKafkaWriter(sc)
	withReachableSceneNode(t, sc, "10")
	t.Cleanup(SetWorldConfIdsForTest([]uint64{onDemandTestConf}))
	mirrorRegistrationForTest(t, onDemandIsolatedZone, "10", false)
	seedNodeMembers(mr, onDemandIsolatedZone, "10", 0)
	sleeps := 0
	installWarmupSleepForTest(t, func(context.Context, time.Duration) bool {
		sleeps++
		mr.ZAdd(nodeLoadKey(onDemandIsolatedZone), 0, "10") // 领导者同步完,把节点放进负载集
		return true
	})

	resp := enterDefaultWorldForTest(t, sc, 6183, onDemandIsolatedZone)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)
	assert.Equal(t, 1, sleeps, "等到领导者放进来就不再等")
	requireLandedOnOnlyChannel(t, sc, onDemandIsolatedZone, 6183, captured)
}

// 不等的情形:镜像里没有可用的大世界节点(没有注册 / 唯一的注册在计划排空),立刻按原口径失败;
// 镜像里有、领导者到期限也没放进负载集,放弃。都不发 CreateScene。
func TestEnterScene_OnDemandGivesUpWithoutUsableNode(t *testing.T) {
	cases := []struct {
		name        string
		mirror      bool
		draining    bool
		expectSleep bool
	}{
		{name: "no_registration"},
		{name: "only_draining_registration", mirror: true, draining: true},
		{name: "leader_never_admits_it", mirror: true, expectSleep: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, _ := newTestSvcCtxWithWorldScenes(t)
			sc.Config.ZoneId = onDemandIsolatedZone
			captured := capturingKafkaWriter(sc)
			fake := withReachableSceneNode(t, sc)
			t.Cleanup(SetWorldConfIdsForTest([]uint64{onDemandTestConf}))
			if tc.mirror {
				mirrorRegistrationForTest(t, onDemandIsolatedZone, "10", tc.draining)
			}
			sleeps := 0
			installWarmupSleepForTest(t, func(context.Context, time.Duration) bool {
				sleeps++
				return false // 期限到了
			})
			createsBefore := fake.createCalls.Load()

			resp := enterDefaultWorldForTest(t, sc, 6184, onDemandIsolatedZone)
			assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
			assert.Equal(t, tc.expectSleep, sleeps > 0, "只有镜像里有可用注册才等领导者")
			assert.Equal(t, createsBefore, fake.createCalls.Load(), "没有可用节点就不补铺")
			assert.Empty(t, *captured)
			channels, err := sc.Redis.Scard(worldChannelsKey(onDemandIsolatedZone, onDemandTestConf))
			require.NoError(t, err)
			assert.Zero(t, channels)
		})
	}
}

// 频道在、只是一个都用不了(属主不在负载集、也没有活节点接手):补铺帮不上,不补,照原口径失败。
func TestEnterScene_OnDemandSkippedWhenChannelsExistButAreUnusable(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.ZoneId = onDemandIsolatedZone
	capturingKafkaWriter(sc)
	fake := withReachableSceneNode(t, sc)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{onDemandTestConf}))
	mr.SAdd(worldChannelsKey(onDemandIsolatedZone, onDemandTestConf), "7481")
	seedSceneOnNode(mr, onDemandIsolatedZone, 7481, "99", "0")
	mr.ZRem(nodeLoadKey(onDemandIsolatedZone), "99") // 属主已死,负载集里没有任何节点
	installWarmupSleepForTest(t, func(context.Context, time.Duration) bool {
		t.Error("频道集合不空,不进补铺,也就不等")
		return false
	})
	createsBefore := fake.createCalls.Load()

	resp := enterDefaultWorldForTest(t, sc, 6185, onDemandIsolatedZone)
	assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
	assert.Equal(t, createsBefore, fake.createCalls.Load())
}

// 等的这段时间里领导者自己把频道铺好了:直接用它的,不再对频道发一遍 CreateScene。
func TestEnterScene_OnDemandUsesChannelLaidByLeaderWhileWaiting(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.ZoneId = onDemandIsolatedZone
	captured := capturingKafkaWriter(sc)
	fake := withReachableSceneNode(t, sc, "10")
	t.Cleanup(SetWorldConfIdsForTest([]uint64{onDemandTestConf}))
	mirrorRegistrationForTest(t, onDemandIsolatedZone, "10", false)
	seedNodeMembers(mr, onDemandIsolatedZone, "10", 0)
	installWarmupSleepForTest(t, func(context.Context, time.Duration) bool {
		// 领导者同步完、铺好了频道:节点进负载集,频道 7482 在节点 10 上。
		seedSceneOnNode(mr, onDemandIsolatedZone, 7482, "10", "0")
		mr.SAdd(worldChannelsKey(onDemandIsolatedZone, onDemandTestConf), "7482")
		return true
	})
	createsBefore := fake.createCalls.Load()

	resp := enterDefaultWorldForTest(t, sc, 6186, onDemandIsolatedZone)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)
	requireLandedOnOnlyChannel(t, sc, onDemandIsolatedZone, 6186, captured)
	assert.Equal(t, createsBefore, fake.createCalls.Load(), "领导者铺好了就不再补铺")
}

// onDemandWaitersForTest:key 那一次在跑的补铺有几个等待者;没有在跑的回 -1。
func onDemandWaitersForTest(key worldChannelOnDemandKey) int {
	worldChannelOnDemandMu.Lock()
	defer worldChannelOnDemandMu.Unlock()
	if flight, running := worldChannelOnDemandFlights[key]; running {
		return flight.waiters
	}
	return -1
}

// 同一进程里同时到的请求只补铺一次;结束就删,之后再来的请求自己再跑一次(上一次可能没铺成)。
func TestJoinWorldChannelOnDemand_ConcurrentCallersShareOneRun(t *testing.T) {
	key := worldChannelOnDemandKey{zoneID: onDemandIsolatedZone, confID: 3389}
	release := make(chan struct{})
	started := make(chan struct{})
	var runs atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		joinWorldChannelOnDemand(context.Background(), key, func() {
			runs.Add(1)
			close(started)
			<-release
		})
	}()
	<-started

	const waiters = 8
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			joinWorldChannelOnDemand(context.Background(), key, func() { runs.Add(1) })
		}()
	}
	require.Eventually(t, func() bool { return onDemandWaitersForTest(key) == waiters }, 5*time.Second, time.Millisecond,
		"等待者都挂到在跑的那一次上")
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), runs.Load(), "同时到的请求只补铺一次")
	assert.Equal(t, -1, onDemandWaitersForTest(key), "结束就删,不累积")

	joinWorldChannelOnDemand(context.Background(), key, func() { runs.Add(1) })
	assert.Equal(t, int32(2), runs.Load(), "结束之后再来的请求自己跑")
}

// 等待的一方按自己的期限放弃,不影响在跑的那一次,也不另起一次。
func TestJoinWorldChannelOnDemand_WaiterGivesUpOnItsOwnDeadline(t *testing.T) {
	key := worldChannelOnDemandKey{zoneID: onDemandIsolatedZone, confID: 3390}
	release := make(chan struct{})
	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		joinWorldChannelOnDemand(context.Background(), key, func() {
			close(started)
			<-release
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	joinWorldChannelOnDemand(ctx, key, func() { t.Error("在跑的那一次还没结束,等待者不能自己再跑") })
	close(release)
	<-finished
}

// 第一条腿的地图预检与选点同口径:计划排空中的节点、身份歧义的节点都不算「能落地」(hasWorldHostingRegistration)。
func TestTravelMapPrecheckUsesEtcd_DrainingAndAmbiguousNodesDoNotOpen(t *testing.T) {
	sc := foreignTestSvcCtx(t, 1) // 按类型分池
	t.Cleanup(SetWorldConfIdsForTest([]uint64{1001}))
	draining := registryEntry(2, "20", constants.SceneNodeTypeMainWorld, "u20")
	draining.reg.Draining = true
	registry := map[uint32][]nodeEntry{2: {draining}}
	fakeZoneRegistry(t, registry, nil)
	prev := travelMapOpenOverride
	travelMapOpenOverride = nil
	t.Cleanup(func() { travelMapOpenOverride = prev })

	open, err := foreignZoneOpensWorldMap(context.Background(), sc, 2, 1001)
	require.NoError(t, err)
	assert.False(t, open, "唯一的大世界节点在计划排空:第二条腿选不到它")

	registry[2] = []nodeEntry{
		registryEntry(2, "21", constants.SceneNodeTypeMainWorld, "u21a"),
		registryEntry(2, "21", constants.SceneNodeTypeMainWorld, "u21b"),
	}
	open, err = foreignZoneOpensWorldMap(context.Background(), sc, 2, 1001)
	require.NoError(t, err)
	assert.False(t, open, "同一 node_id 挂着两条注册:选点拒绝这个身份")

	registry[2] = append(registry[2], registryEntry(2, "22", constants.SceneNodeTypeMainWorld, "u22"))
	open, err = foreignZoneOpensWorldMap(context.Background(), sc, 2, 1001)
	require.NoError(t, err)
	assert.True(t, open, "有一台没在排空、身份唯一的大世界节点就算开着")

	sc.Config.StrictNodeTypeSeparation = false
	registry[2] = []nodeEntry{draining}
	open, err = foreignZoneOpensWorldMap(context.Background(), sc, 2, 1001)
	require.NoError(t, err)
	assert.False(t, open, "不分池时也不算排空中的节点")
}
