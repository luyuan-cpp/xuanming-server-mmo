package logic

// Redis 读错 ≠ 键不存在(2026-10-22/23,见 read_unknown.go)。
//
// go-zero 的 Get 把 redis.Nil 吞成 ("", nil),其它错误回 ("", err);以前 `x, _ :=` 的写法把读错当成
// 「不存在」,于是频道被判陈旧去改派、映射照删、Agones 名额不还、镜像索引照删。这里的用例大多只让
// **一条**指定命令失败一次(或指定的第 N 次),断言那一轮没按读错的结果行事(归属、映射、索引、名额原样),
// 多数还断言指标 site 加一、故障过去后下一轮照常推进。例外写在各用例注释里:提交后重读失败的用例按设计
// 会写(退回快照级联);属主在读与原子删除之间被改写的用例不注入读错,而是在 EVAL 之前改写属主。
//
// 这些用例引用了新代码的符号,不能原样在旧代码上跑;注释里写了旧代码在同一输入下会做什么。

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

// redisFaultRule 让 miniredis 对「命令名 + 第一个参数(键)」完全匹配的调用,先放过 skip 次,
// 再回 times 次 ERR(模拟超时 / 断连这类「不知道」),之后一律放行。
type redisFaultRule struct {
	cmd   string // 大写命令名:miniredis 交给钩子的就是大写
	key   string
	skip  int
	times int
}

// injectRedisFaultsForTest 装上故障钩子,返回实际注入的失败次数(用来断言故障确实打到了,避免空转通过)。
// 约束同 installNodeDetachFaultsForTest:
//   - 钩子会被 Lua 里的 redis.call 带锁重入,所以只读写原子计数,绝不调 mr.*;Lua 里对同一个键的
//     访问也计入 skip / times,用例按「第几次访问」设计规则;
//   - 每个 svcCtx 注入的失败远少于 5 次(go-zero 熔断);
//   - 与 mr.SetError / 其它 SetPreHook 共用钩子槽位,不能混用。
func injectRedisFaultsForTest(t *testing.T, mr *miniredis.Miniredis, rules ...redisFaultRule) *atomic.Int32 {
	t.Helper()
	seen := make([]atomic.Int32, len(rules))
	var fired atomic.Int32
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if len(args) == 0 {
			return false
		}
		for i := range rules {
			if cmd != rules[i].cmd || args[0] != rules[i].key {
				continue
			}
			n := int(seen[i].Add(1))
			if n <= rules[i].skip || n > rules[i].skip+rules[i].times {
				return false
			}
			fired.Add(1)
			c.WriteError("ERR injected read failure")
			return true
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	return &fired
}

// readUnknownCount 读 scene_manager_ownership_read_unknown_total{zone_id, site}。
// counterValueForTest 定义在 node_detach_deferred_test.go(标签子集匹配,没发射过读作 0);计数器是包级全局的,
// 用例一律比较前后差值。
func readUnknownCount(t *testing.T, zoneID uint32, site string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_ownership_read_unknown_total",
		map[string]string{"zone_id": strconv.FormatUint(uint64(zoneID), 10), "site": site})
}

// autoscaleOutcomeCount 读 scene_manager_world_autoscale_total{zone_id, action, outcome}。
func autoscaleOutcomeCount(t *testing.T, zoneID uint32, action, outcome string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_world_autoscale_total",
		map[string]string{"zone_id": strconv.FormatUint(uint64(zoneID), 10), "action": action, "outcome": outcome})
}

func requireSceneOwner(t *testing.T, sc *svc.ServiceContext, sceneID uint64, want string, msgAndArgs ...interface{}) {
	t.Helper()
	got, err := sc.Redis.Get(sceneNodeKey(sceneID))
	require.NoError(t, err)
	require.Equal(t, want, got, msgAndArgs...)
}

// ---------------------------------------------------------------------------
// world_init.go:选频道与懒改派
// ---------------------------------------------------------------------------

// readUnknownWorldConf:懒改派的哈希键是 confId*1000+下标,1001000 在排序后的 {"10","20"} 上落 "20"
// (与 world_init_unreachable_test.go 的 worldInitTestConfIDs 同一前提,用例里仍复核一次)。
const readUnknownWorldConf uint64 = 1001

// readUnknownPlayer 是预占用例里的玩家。
const readUnknownPlayer uint64 = 6801

// seedLiveWorldChannelForReadUnknown:频道 501 属于活节点 "10","20" 也活着(懒改派的目标)。
func seedLiveWorldChannelForReadUnknown(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
	seedWorldChannel(sc, testZoneId, readUnknownWorldConf, 501, "10")
	require.Equal(t, "20", assignNodeByHash(readUnknownWorldConf*1000, []string{"10", "20"}),
		"前提不成立:懒改派目标不是 20,用例会空转")
	return sc, mr
}

// 属主读错不是「陈旧」:旧代码把 501 判陈旧,懒改派时重读成功拿到活属主 "10",CAS 10→20 落地,
// 在 20 上再建一份同 scene_id 的实体。现在这一轮回可重试错误,归属不动。
func TestGetBestWorldChannel_OwnerReadErrorIsNotStale(t *testing.T) {
	sc, mr := seedLiveWorldChannelForReadUnknown(t)
	ctx := context.Background()
	before := readUnknownCount(t, testZoneId, readUnknownSiteWorldPick)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneNodeKey(501), times: 1})

	sid, nid, err := GetBestWorldChannel(ctx, sc, readUnknownWorldConf, testZoneId)
	require.EqualValues(t, 1, fired.Load())
	require.Error(t, err, "属主读不到时不能回「没有频道」,更不能改派")
	assert.Zero(t, sid)
	assert.Empty(t, nid)
	requireSceneOwner(t, sc, 501, "10", "读错时归属必须原样")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteWorldPick))

	// 故障过去:照常选中活属主上的 501。
	sid, nid, err = GetBestWorldChannel(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, uint64(501), sid)
	assert.Equal(t, "10", nid)
}

// 同上,EnterScene 预占路径:读错的频道不进懒改派,也不被预占人数。
func TestReserveBestWorldChannelForEnter_OwnerReadErrorIsNotStale(t *testing.T) {
	sc, mr := seedLiveWorldChannelForReadUnknown(t)
	ctx := context.Background()
	before := readUnknownCount(t, testZoneId, readUnknownSiteWorldPick)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneNodeKey(501), times: 1})

	sid, _, err := ReserveBestWorldChannelForEnter(ctx, sc, readUnknownWorldConf, testZoneId, readUnknownPlayer)
	require.EqualValues(t, 1, fired.Load())
	require.Error(t, err)
	assert.Zero(t, sid)
	requireSceneOwner(t, sc, 501, "10", "读错时归属必须原样")
	assert.Equal(t, "0", sceneMemberCount(mr, 501), "没选出频道就不能预占")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteWorldPick))

	sid, nid, err := ReserveBestWorldChannelForEnter(ctx, sc, readUnknownWorldConf, testZoneId, readUnknownPlayer)
	require.NoError(t, err)
	assert.Equal(t, uint64(501), sid)
	assert.Equal(t, "10", nid)
}

// lazyReassignStaleWorldChannel 动手之前重读属主,按读到的结果分开处理;拒绝的情形一次 CreateScene 都不发。
// 节点 "10" 已死(不在负载集),"20" 活着,目标一律是 "20"。频道 502 在役(在 world_channels 集合里),
// 「不在役」子用例除外。
func TestLazyReassignStaleWorldChannel_RechecksOwnerBeforeWrite(t *testing.T) {
	newCtx := func(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
		t.Helper()
		clearKnownNodesForTest()
		t.Cleanup(clearKnownNodesForTest)
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
		_, err := mr.SAdd(worldChannelsKey(testZoneId, readUnknownWorldConf), "502")
		require.NoError(t, err)
		return sc, mr
	}
	reassign := func(sc *svc.ServiceContext) bool {
		return lazyReassignStaleWorldChannel(context.Background(), sc, testZoneId, readUnknownWorldConf, 502, "20")
	}

	t.Run("read error leaves ownership alone", func(t *testing.T) {
		sc, mr := newCtx(t)
		require.NoError(t, sc.Redis.Set(sceneNodeKey(502), "10"))
		createsBefore := sharedFakeSceneNode.createCalls.Load()
		before := readUnknownCount(t, testZoneId, readUnknownSiteWorldLazy)
		fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneNodeKey(502), times: 1})

		assert.False(t, reassign(sc))
		require.EqualValues(t, 1, fired.Load())
		requireSceneOwner(t, sc, 502, "10")
		assert.Equal(t, createsBefore, sharedFakeSceneNode.createCalls.Load(), "拒绝时不能先建实体")
		assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteWorldLazy))
	})

	t.Run("owner became alive since the stale check", func(t *testing.T) {
		sc, mr := newCtx(t)
		// 判陈旧之后被补建 / 被 rebalance 改派到了活节点:CAS 只比属主,放过去就会把它从活节点搬走。
		mr.ZAdd(nodeLoadKey(testZoneId), 0, "30")
		require.NoError(t, sc.Redis.Set(sceneNodeKey(502), "30"))
		createsBefore := sharedFakeSceneNode.createCalls.Load()

		assert.False(t, reassign(sc))
		requireSceneOwner(t, sc, 502, "30")
		assert.Equal(t, createsBefore, sharedFakeSceneNode.createCalls.Load(), "拒绝时不能先建实体")
	})

	t.Run("dead owner inside the reentry barrier", func(t *testing.T) {
		sc, _ := newCtx(t)
		require.NoError(t, sc.Redis.Set(sceneNodeKey(502), "10"))
		require.NoError(t, markNodeDeath(sc, testZoneId, "10"))
		createsBefore := sharedFakeSceneNode.createCalls.Load()

		assert.False(t, reassign(sc))
		requireSceneOwner(t, sc, 502, "10")
		assert.Equal(t, createsBefore, sharedFakeSceneNode.createCalls.Load(), "屏障窗口里不能每个请求都白建一次")
	})

	t.Run("dead owner without a recent death mark", func(t *testing.T) {
		sc, _ := newCtx(t)
		require.NoError(t, sc.Redis.Set(sceneNodeKey(502), "10"))

		assert.True(t, reassign(sc))
		requireSceneOwner(t, sc, 502, "20")
	})

	t.Run("no owner at all", func(t *testing.T) {
		sc, _ := newCtx(t)

		assert.True(t, reassign(sc))
		requireSceneOwner(t, sc, 502, "20")
	})

	// 调用方读到频道列表之后,排空收尾 / 孤儿清理已经原子地拆掉了 502(删映射、摘出在役集合)。
	// 只比属主的 CAS(期望空串)会把它复活成一个哪个集合都不列的频道;在役核对必须挡住,并回收预建的实体。
	t.Run("torn down after the caller listed it", func(t *testing.T) {
		sc, mr := newCtx(t)
		mr.SRem(worldChannelsKey(testZoneId, readUnknownWorldConf), "502")
		createsBefore := sharedFakeSceneNode.createCalls.Load()
		destroysBefore := sharedFakeSceneNode.destroyCalls.Load()

		assert.False(t, reassign(sc))
		requireSceneOwner(t, sc, 502, "", "已拆掉的频道不能被复活")
		assert.Equal(t, createsBefore+1, sharedFakeSceneNode.createCalls.Load())
		assert.Equal(t, destroysBefore+1, sharedFakeSceneNode.destroyCalls.Load(), "落空时要回收在 \"20\" 上预建的实体")
		count, err := sc.Redis.Get(nodeSceneCountKey(testZoneId, "20"))
		require.NoError(t, err)
		assert.Empty(t, count, "落空时新节点不能记账")
	})

	t.Run("CAS lost after the entity was pre-created", func(t *testing.T) {
		sc, mr := newCtx(t)
		mr.ZAdd(nodeLoadKey(testZoneId), 0, "30")
		require.NoError(t, sc.Redis.Set(sceneNodeKey(502), "10"))
		// 复核通过、实体已在 "20" 上预建之后,别的写者抢先把归属改到 "30"。
		// 钩子跑在 miniredis 的服务端协程上:只能用 t.Errorf,不能用 require。
		fired := identHookOnceBeforeEval(t, mr, sceneNodeKey(502), func() {
			if err := mr.Set(sceneNodeKey(502), "30"); err != nil {
				t.Errorf("simulate concurrent reassign: %v", err)
			}
		})
		destroysBefore := sharedFakeSceneNode.destroyCalls.Load()

		assert.False(t, reassign(sc))
		require.True(t, fired.Load(), "前提不成立:改派的 CAS 没有带上 scene:502:node")
		requireSceneOwner(t, sc, 502, "30")
		assert.Equal(t, destroysBefore+1, sharedFakeSceneNode.destroyCalls.Load(), "落空时要回收在 \"20\" 上预建的实体")
	})
}

// 读不到频道集合就不知道缺几个:旧代码当成 0,把整组频道再建一遍。
func TestInitWorldScenes_ChannelSetReadErrorDoesNotDuplicateChannels(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	confIDs := []uint64{readUnknownWorldConf}

	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	channels, err := GetAllWorldChannels(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	require.Len(t, channels, 1)

	before := readUnknownCount(t, testZoneId, readUnknownSiteWorldInit)
	fired := injectRedisFaultsForTest(t, mr,
		redisFaultRule{cmd: "SMEMBERS", key: worldChannelsKey(testZoneId, readUnknownWorldConf), times: 1})
	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	require.EqualValues(t, 1, fired.Load())

	after, err := GetAllWorldChannels(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, channels, after, "读错时不能补建频道")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteWorldInit))

	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	after, err = GetAllWorldChannels(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, channels, after)
}

// 补发 CreateScene 的循环读不到频道属主:这个频道本轮不补发,归属不动,计入 world_init。
// 旧代码把读错打成「Missing node」,看日志会以为映射丢了。
func TestInitWorldScenes_EnsureLoopOwnerReadErrorSkipsChannel(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	confIDs := []uint64{readUnknownWorldConf}
	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	channels, err := GetAllWorldChannels(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	require.Len(t, channels, 1)

	createsBefore := sharedFakeSceneNode.createCalls.Load()
	before := readUnknownCount(t, testZoneId, readUnknownSiteWorldInit)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneNodeKey(channels[0]), times: 1})
	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	require.EqualValues(t, 1, fired.Load())
	assert.Equal(t, createsBefore, sharedFakeSceneNode.createCalls.Load(), "读不到属主就不知道该发给谁")
	requireSceneOwner(t, sc, channels[0], "10")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteWorldInit))
}

// 读不到期望频道数就不补建。配置种子是 3,自动缩容已把权威值缩到 1、只剩 1 个频道。
// 旧代码:HGET 失败、HSETNX 是空操作、第二次 HGET 也失败时回种子 3,补建出 2 个缩掉的频道。
// 规则给两次失败是为了让旧代码走到那条路;新代码第一次读失败就停,只吃掉一次。
func TestInitWorldScenes_DesiredCountReadErrorDoesNotRecreateScaledInChannels(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	confIDs := []uint64{readUnknownWorldConf}
	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true) // 种子 1,建 1 个
	sc.Config.WorldChannelCount = 3                            // 之后配置改成 3,但权威值已在 Redis 里是 1
	channels, err := GetAllWorldChannels(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, 1, mustDesiredWorldChannelCount(t, sc, testZoneId, readUnknownWorldConf))

	before := readUnknownCount(t, testZoneId, readUnknownSiteWorldInit)
	fired := injectRedisFaultsForTest(t, mr,
		redisFaultRule{cmd: "HGET", key: worldDesiredChannelsKey(testZoneId), times: 2})
	initWorldScenesForZone(ctx, sc, testZoneId, confIDs, true)
	require.EqualValues(t, 1, fired.Load(), "读期望数失败后应当立刻停下,不再重读")
	mr.Server().SetPreHook(nil)

	after, err := GetAllWorldChannels(ctx, sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, channels, after, "读错时不能按种子值补建")
	assert.Equal(t, 1, mustDesiredWorldChannelCount(t, sc, testZoneId, readUnknownWorldConf), "权威值不能被改写")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteWorldInit))
}

// 懒改派要把 Agones 名额跟着转到新节点(reassignSceneNode 的契约交给调用方的副作用)。
// 旧代码:懒改派只改归属、发 CreateScene,agones_gs 仍指向老 GameServer,新节点的 rooms 不记账。
func TestGetBestWorldChannel_LazyReassignTransfersAgonesRoom(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	// "10" 已死(不在负载集、没有近期 death_at),"20" 活着:频道 503 陈旧,懒改派到 "20"。
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
	seedWorldChannel(sc, testZoneId, readUnknownWorldConf, 503, "10")
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(503), "gs-old"))

	sid, nid, err := GetBestWorldChannel(context.Background(), sc, readUnknownWorldConf, testZoneId)
	require.NoError(t, err)
	require.Equal(t, uint64(503), sid)
	require.Equal(t, "20", nid)
	requireSceneOwner(t, sc, 503, "20")
	gs, err := sc.Redis.Get(sceneAgonesGsKey(503))
	require.NoError(t, err)
	assert.Equal(t, "gs-new", gs, "agones_gs 要跟着频道转到新节点的 GameServer")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-new"), "新节点必须记账")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-old"), "老 GameServer 的名额要还回去")
}

// 读不到节点类型不能当「未分类」放进每个池子:只跑副本的 "10" 会被派到世界频道。这一轮当没有可用节点。
func TestGetNodesForPurpose_TypeReadErrorReturnsNoNodes(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 1)
	before := readUnknownCount(t, testZoneId, readUnknownSiteNodeSelection)
	fired := injectRedisFaultsForTest(t, mr,
		redisFaultRule{cmd: "GET", key: nodeSceneNodeTypeKey(testZoneId, "10"), times: 1})

	assert.Empty(t, getNodesForPurpose(sc, testZoneId, constants.NodePurposeWorld))
	require.EqualValues(t, 1, fired.Load())
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteNodeSelection))

	assert.Equal(t, []string{"20"}, getNodesForPurpose(sc, testZoneId, constants.NodePurposeWorld), "故障过去后照常按类型过滤")
}

// ---------------------------------------------------------------------------
// world_autoscale.go:伸缩决策、排空推进、排空收尾
// ---------------------------------------------------------------------------

// 决策要读的三样任一读不到,这张图本轮不伸缩。101 满员、102 很闲:正常这一轮会排空 102。
// 旧代码:101 的人数读错当 0,101 被挑成缩容对象(满员频道被排空);排空标记 / 冷却标记读错时照常排空。
func TestAutoscale_DecisionReadErrorSkipsTheRound(t *testing.T) {
	cases := []struct {
		name string
		rule redisFaultRule
	}{
		{"draining flag", redisFaultRule{cmd: "EXISTS", key: sceneDrainingKey(101), times: 1}},
		{"player count", redisFaultRule{cmd: "SCARD", key: instanceMembersKey(101), times: 1}},
		{"cooldown flag", redisFaultRule{cmd: "EXISTS", key: worldAutoscaleCooldownKey(testZoneId, autoscaleTestConfID), times: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newAutoscaleCtx(t)
			ctx := context.Background()
			sc.Redis.Zadd(testLoadKey(), 0, "10")
			seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})

			before := readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDecide)
			fired := injectRedisFaultsForTest(t, mr, tc.rule)
			out, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
			require.EqualValues(t, 1, fired.Load())
			assert.Zero(t, out)
			assert.Zero(t, in)
			members, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"101", "102"}, members, "读错的这一轮一个频道都不能摘")
			assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDecide))

			// 故障过去:照常排空最闲的 102。
			_, in = AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
			assert.Equal(t, 1, in)
			draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
			require.NoError(t, err)
			assert.Equal(t, []string{"102"}, draining)
		})
	}
}

// beginDrainingForReadUnknown 让 102 进入排空态(人还没走完),返回 svcCtx。节点 "10" 活着,
// 名下 scene_count=2、反向索引含 102,与 TestAutoscale_DrainedChannelIsCleanedUp 同一铺法。
func beginDrainingForReadUnknown(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})
	sc.Redis.Sadd(nodeScenesKey(testZoneId, "10"), "102")
	sc.Redis.Set(nodeSceneCountKey(testZoneId, "10"), "2")
	_, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	require.Equal(t, 1, in)
	require.NoError(t, requireDraining(sc, 102))
	return sc, mr
}

func requireStillDraining(t *testing.T, sc *svc.ServiceContext, sceneID uint64) {
	t.Helper()
	requireSceneOwner(t, sc, sceneID, "10", "读错的这一拍不能收尾")
	draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	require.Contains(t, draining, strconv.FormatUint(sceneID, 10), "频道要留在排空集合里等下一拍")
	count, err := sc.Redis.Get(nodeSceneCountKey(testZoneId, "10"))
	require.NoError(t, err)
	require.Equal(t, "2", count, "scene_count 不能先减")
}

func requireDrainFinished(t *testing.T, sc *svc.ServiceContext, sceneID uint64) {
	t.Helper()
	requireSceneOwner(t, sc, sceneID, "")
	draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	require.NotContains(t, draining, strconv.FormatUint(sceneID, 10))
	count, err := sc.Redis.Get(nodeSceneCountKey(testZoneId, "10"))
	require.NoError(t, err)
	require.Equal(t, "1", count)
}

// 排空推进读属主 / 人数出错:旧代码属主读错当「映射已没」直接收尾(跳过 C++ DestroyScene);
// 人数读错当 0,在还有 12 个人时收尾。
func TestAutoscale_DrainStepReadErrorKeepsDrainingState(t *testing.T) {
	t.Run("owner", func(t *testing.T) {
		sc, mr := beginDrainingForReadUnknown(t)
		ctx := context.Background()
		seedSceneMembersVia(sc, 102, 0)
		before := readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDrain)
		fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneNodeKey(102), times: 1})

		sweepDrainingWorldChannels(ctx, sc, testZoneId)
		require.EqualValues(t, 1, fired.Load())
		requireStillDraining(t, sc, 102)
		assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDrain))

		sweepDrainingWorldChannels(ctx, sc, testZoneId)
		requireDrainFinished(t, sc, 102)
	})

	t.Run("player count", func(t *testing.T) {
		sc, mr := beginDrainingForReadUnknown(t)
		ctx := context.Background()
		seedSceneMembersVia(sc, 102, 12)
		before := readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDrain)
		fired := injectRedisFaultsForTest(t, mr,
			redisFaultRule{cmd: "SCARD", key: instanceMembersKey(102), times: 1})

		sweepDrainingWorldChannels(ctx, sc, testZoneId)
		require.EqualValues(t, 1, fired.Load())
		requireStillDraining(t, sc, 102)
		assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDrain))

		// 人还在:下一拍照样不收尾。
		sweepDrainingWorldChannels(ctx, sc, testZoneId)
		requireStillDraining(t, sc, 102)
	})
}

// 排空收尾的脚本把场景里剩的成员交回来,由调用方从节点集合里摘掉。节点已死时不等人走完就收尾,这里就有残留。
func TestAutoscale_FinishDrainRemovesResidualMembersFromNodeSet(t *testing.T) {
	sc, mr := beginDrainingForReadUnknown(t)
	const player uint64 = 7001
	seedSceneMembersVia(sc, 102, 1, player)
	seedNodeMembersVia(sc, testZoneId, "10", 1, player)
	_, err := mr.ZRem(testLoadKey(), "10")
	require.NoError(t, err)

	sweepDrainingWorldChannels(context.Background(), sc, testZoneId)
	requireDrainFinished(t, sc, 102)
	assert.Equal(t, "0", sceneMemberCount(mr, 102))
	assert.Equal(t, "0", nodeMemberCount(mr, testZoneId, "10"), "节点集合不能留着已销毁场景的成员")
}

// 排空收尾的 Agones 名额改由原子删除脚本在删之前读出(不再单独 GET)。脚本里读名额失败 = 脚本整体报错、
// 一个键都没删:本拍不收尾,名额映射原样,计 world_autoscale_total{scale_in,error}。
// 旧代码:单独 GET 名额读错当「没有名额」,不还名额却照删 agones_gs 键,名额永久占着。
func TestAutoscale_FinishDrainAtomicDestroyFailureKeepsRoomMapping(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-a", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(102), "gs-a"))
	_, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	require.Equal(t, 1, in)
	sc.Redis.Set(sceneNodeKey(102), "10")
	seedSceneMembersVia(sc, 102, 0)

	before := autoscaleOutcomeCount(t, testZoneId, "scale_in", "error")
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneAgonesGsKey(102), times: 1})
	sweepDrainingWorldChannels(ctx, sc, testZoneId)
	require.EqualValues(t, 1, fired.Load())
	requireSceneOwner(t, sc, 102, "10", "脚本报错的这一拍一个键都不能删")
	gs, err := sc.Redis.Get(sceneAgonesGsKey(102))
	require.NoError(t, err)
	assert.Equal(t, "gs-a", gs, "agones_gs 映射是归还名额的唯一依据,不能删")
	assert.EqualValues(t, 2, alloc.roomsOf("gs-a"))
	assert.Equal(t, before+1, autoscaleOutcomeCount(t, testZoneId, "scale_in", "error"))

	sweepDrainingWorldChannels(ctx, sc, testZoneId)
	requireSceneOwner(t, sc, 102, "")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-a"), "故障过去后名额照常归还,而且只还一次")
}

// 收尾读到属主 "10" 之后、原子删除之前,频道被别的写者改到了 "20"(例如任意副本上的 resolveScene 死节点
// 自愈)。原子删除带期望属主 "10",落空:一个键都不动,频道留在排空集合,"10" 的计数不扣。
// 旧代码:逐条 DEL,把 "20" 上那份实体变成没有映射的孤儿,还扣 "10" 的计数。
func TestAutoscale_FinishDrainSkipsWhenOwnerChangedBeforeDestroy(t *testing.T) {
	sc, mr := beginDrainingForReadUnknown(t)
	ctx := context.Background()
	seedSceneMembersVia(sc, 102, 0)
	// 钩子跑在 miniredis 的服务端协程上:只能用 t.Errorf,不能用 require(FailNow 必须在用例协程里调)。
	fired := identHookOnceBeforeEval(t, mr, sceneNodeKey(102), func() {
		if err := mr.Set(sceneNodeKey(102), "20"); err != nil {
			t.Errorf("simulate concurrent reassign: %v", err)
		}
	})

	sweepDrainingWorldChannels(ctx, sc, testZoneId)
	require.True(t, fired.Load(), "前提不成立:原子删除没有带上 scene:102:node")
	requireSceneOwner(t, sc, 102, "20", "新属主的映射不能被删")
	draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	assert.Contains(t, draining, "102", "频道留在排空集合,下一拍按新属主重来")
	count, err := sc.Redis.Get(nodeSceneCountKey(testZoneId, "10"))
	require.NoError(t, err)
	assert.Equal(t, "2", count, "老属主的 scene_count 不能被扣")
}

// 扩容读不到权威期望数就不写。权威值 2、配置种子 4。
// 旧代码:两次 HGET 都失败时回种子 4,setDesiredWorldChannelCount(5) 把权威值冲掉。
func TestAutoscale_ScaleOutDesiredReadErrorDoesNotOverwriteAuthority(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 2500, 102: 2600}) // 两个都满,该扩容
	sc.Config.WorldChannelCount = 4

	before := readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDecide)
	fired := injectRedisFaultsForTest(t, mr,
		redisFaultRule{cmd: "HGET", key: worldDesiredChannelsKey(testZoneId), times: 2})
	out, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	require.EqualValues(t, 1, fired.Load(), "读期望数失败后应当立刻停下,不再重读")
	mr.Server().SetPreHook(nil)
	assert.Zero(t, out)
	assert.Zero(t, in)
	assert.Equal(t, 2, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID), "权威值不能被种子值冲掉")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDecide))
}

// 缩容在摘路由之前读期望数:读不到就什么都不写。
// 旧代码:先 SREM 摘路由,再读期望数失败回种子,写回种子 - 1。
func TestAutoscale_BeginDrainDesiredReadErrorWritesNothing(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40}) // 102 该被排空
	sc.Config.WorldChannelCount = 4

	before := readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDecide)
	fired := injectRedisFaultsForTest(t, mr,
		redisFaultRule{cmd: "HGET", key: worldDesiredChannelsKey(testZoneId), times: 2})
	_, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	require.EqualValues(t, 1, fired.Load(), "读期望数失败后应当立刻停下,不再重读")
	mr.Server().SetPreHook(nil)
	assert.Zero(t, in)
	members, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"101", "102"}, members, "读不到期望数时不能摘路由")
	draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	assert.Empty(t, draining)
	assert.Equal(t, 2, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleDecide))
}

// 排空收尾读镜像集合出错:旧代码不级联却照删索引,镜像成了再也找不到的孤儿。
func TestAutoscale_FinishDrainMirrorReadErrorKeepsMirrorIndex(t *testing.T) {
	sc, mr := beginDrainingForReadUnknown(t)
	ctx := context.Background()
	const mirrorID uint64 = 9100
	mirrorStr := strconv.FormatUint(mirrorID, 10)
	_, err := sc.Redis.Sadd(sceneMirrorsKey(102), mirrorStr)
	require.NoError(t, err)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(mirrorID), "10"))
	require.NoError(t, sc.Redis.Set(sceneZoneKey(mirrorID), strconv.FormatUint(uint64(testZoneId), 10)))
	require.NoError(t, sc.Redis.Set(sceneSourceKey(mirrorID), "102"))
	seedSceneMembersVia(sc, 102, 0)

	before := readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleFinish)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "SMEMBERS", key: sceneMirrorsKey(102), times: 1})
	sweepDrainingWorldChannels(ctx, sc, testZoneId)
	require.EqualValues(t, 1, fired.Load())
	requireStillDraining(t, sc, 102)
	mirrors, err := sc.Redis.Smembers(sceneMirrorsKey(102))
	require.NoError(t, err)
	assert.Equal(t, []string{mirrorStr}, mirrors, "读错时镜像索引不能删")
	requireSceneOwner(t, sc, mirrorID, "10")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteAutoscaleFinish))

	sweepDrainingWorldChannels(ctx, sc, testZoneId)
	requireSceneOwner(t, sc, 102, "")
	requireSceneOwner(t, sc, mirrorID, "", "故障过去后镜像照常级联销毁")
}

// ---------------------------------------------------------------------------
// instance_lifecycle.go:副本销毁的镜像级联
// ---------------------------------------------------------------------------

// createInstanceWithMirrorForReadUnknown 在节点 "10" 上建一个副本和一个以它为源的镜像。
func createInstanceWithMirrorForReadUnknown(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, uint64, uint64) {
	t.Helper()
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	mr.ZAdd(testLoadKey(), 0, "10")
	parent, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)
	mirror, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		MirrorConfigId: 9001,
		SourceSceneId:  parent.SceneId,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)
	members, err := sc.Redis.Smembers(sceneMirrorsKey(parent.SceneId))
	require.NoError(t, err)
	require.Equal(t, []string{strconv.FormatUint(mirror.SceneId, 10)}, members, "前提不成立:镜像没有登记到源的子集合")
	return sc, mr, parent.SceneId, mirror.SceneId
}

// 空闲销毁读镜像子集合出错:本轮放弃,什么都不改。
// 旧代码:原子脚本先把源的映射删了,随后读子集合失败 → 不级联、照删索引,镜像成孤儿。
func TestDestroyInstance_IdleMirrorReadErrorAbandonsRound(t *testing.T) {
	sc, mr, parent, mirror := createInstanceWithMirrorForReadUnknown(t)
	ctx := context.Background()
	before := readUnknownCount(t, testZoneId, readUnknownSiteInstanceDestroy)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "SMEMBERS", key: sceneMirrorsKey(parent), times: 1})

	destroyInstance(ctx, sc, testZoneId, parent)
	require.EqualValues(t, 1, fired.Load())
	requireSceneOwner(t, sc, parent, "10", "读不到子集合时不能提交销毁")
	requireSceneOwner(t, sc, mirror, "10")
	members, err := sc.Redis.Smembers(sceneMirrorsKey(parent))
	require.NoError(t, err)
	assert.Equal(t, []string{strconv.FormatUint(mirror, 10)}, members)
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteInstanceDestroy))

	destroyInstance(ctx, sc, testZoneId, parent)
	requireSceneOwner(t, sc, parent, "")
	requireSceneOwner(t, sc, mirror, "", "故障过去后照常级联")
}

// force 路径提交前读不到子集合同样本轮不销毁、回 (false, err):显式 DestroyScene 与死节点收尾把错误交给
// 上游,级联里的子镜像由 cascadeDestroyMirrors 保留源索引。
// 旧代码:唯一的一次读在提交之后,读失败 → 不级联、照删索引,镜像成孤儿。
func TestDestroyInstanceForce_MirrorPreReadErrorAbandonsRound(t *testing.T) {
	sc, mr, parent, mirror := createInstanceWithMirrorForReadUnknown(t)
	ctx := context.Background()
	before := readUnknownCount(t, testZoneId, readUnknownSiteInstanceDestroy)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "SMEMBERS", key: sceneMirrorsKey(parent), times: 1})

	destroyed, err := destroyInstanceForce(ctx, sc, testZoneId, parent, "explicit")
	require.EqualValues(t, 1, fired.Load())
	require.Error(t, err)
	assert.False(t, destroyed)
	requireSceneOwner(t, sc, parent, "10", "提交前读不到子集合就不能提交")
	requireSceneOwner(t, sc, mirror, "10")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteInstanceDestroy))

	destroyed, err = destroyInstanceForce(ctx, sc, testZoneId, parent, "explicit")
	require.NoError(t, err)
	assert.True(t, destroyed)
	requireSceneOwner(t, sc, parent, "")
	requireSceneOwner(t, sc, mirror, "", "故障过去后照常级联")
}

// 提交前读不到镜像源:本轮不销毁。两个销毁脚本都会删掉 scene:{id}:source,提交之后就再也不知道该从哪个
// 源的子集合里摘掉自己。
// 旧代码:源读错当「不是镜像」,镜像照常销毁,但源的子集合永久挂着它的死 id(channelHasMirrors 恒真)。
func TestDestroyInstance_SourceReadErrorAbandonsRound(t *testing.T) {
	sc, mr, parent, mirror := createInstanceWithMirrorForReadUnknown(t)
	ctx := context.Background()
	mirrorStr := strconv.FormatUint(mirror, 10)
	before := readUnknownCount(t, testZoneId, readUnknownSiteInstanceDestroy)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneSourceKey(mirror), times: 1})

	destroyInstance(ctx, sc, testZoneId, mirror)
	require.EqualValues(t, 1, fired.Load())
	requireSceneOwner(t, sc, mirror, "10", "读不到镜像源时不能提交销毁")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteInstanceDestroy))

	destroyInstance(ctx, sc, testZoneId, mirror)
	requireSceneOwner(t, sc, mirror, "")
	members, err := sc.Redis.Smembers(sceneMirrorsKey(parent))
	require.NoError(t, err)
	assert.NotContains(t, members, mirrorStr, "镜像销毁后必须从源的子集合里摘掉自己")
}

// 级联里某个子镜像本轮没销毁(读不到它的镜像源):源照常销毁,但源的子集合索引保留,子镜像仍可被发现;
// 之后子镜像空闲销毁时把自己摘掉。
func TestCascadeDestroyMirrors_ChildFailureKeepsSourceIndex(t *testing.T) {
	sc, mr, parent, mirror := createInstanceWithMirrorForReadUnknown(t)
	ctx := context.Background()
	mirrorStr := strconv.FormatUint(mirror, 10)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneSourceKey(mirror), times: 1})

	destroyed, err := destroyInstanceForce(ctx, sc, testZoneId, parent, "explicit")
	require.EqualValues(t, 1, fired.Load())
	require.NoError(t, err)
	assert.True(t, destroyed, "源自己的读都成功,源照常销毁")
	requireSceneOwner(t, sc, parent, "")
	requireSceneOwner(t, sc, mirror, "10", "子镜像本轮没销毁")
	members, err := sc.Redis.Smembers(sceneMirrorsKey(parent))
	require.NoError(t, err)
	assert.Equal(t, []string{mirrorStr}, members, "有子镜像没销毁时不能删源的子集合索引")

	destroyInstance(ctx, sc, testZoneId, mirror)
	requireSceneOwner(t, sc, mirror, "")
	members, err = sc.Redis.Smembers(sceneMirrorsKey(parent))
	require.NoError(t, err)
	assert.Empty(t, members, "子镜像销毁时把自己从源的子集合里摘掉")
}

// force 路径提交后重读失败:按预读快照级联,并且不删子集合索引(预读之后才挂上的镜像还要靠它找到)。
// "9999" 是一个只在索引里、没有映射的成员:级联对它是空操作,它留在索引里证明索引没被删。
// 这个用例按设计会写(提交已发生),计入的是 mirror_cascade 而不是 instance_destroy。
func TestDestroyInstanceForce_MirrorPostReadErrorCascadesSnapshotAndKeepsIndex(t *testing.T) {
	sc, mr, parent, mirror := createInstanceWithMirrorForReadUnknown(t)
	_, err := sc.Redis.Sadd(sceneMirrorsKey(parent), "9999")
	require.NoError(t, err)
	before := readUnknownCount(t, testZoneId, readUnknownSiteMirrorCascade)
	fired := injectRedisFaultsForTest(t, mr,
		redisFaultRule{cmd: "SMEMBERS", key: sceneMirrorsKey(parent), skip: 1, times: 1})

	destroyed, err := destroyInstanceForce(context.Background(), sc, testZoneId, parent, "explicit")
	require.EqualValues(t, 1, fired.Load())
	require.NoError(t, err)
	assert.True(t, destroyed)
	requireSceneOwner(t, sc, parent, "")
	requireSceneOwner(t, sc, mirror, "", "按预读快照级联")
	members, err := sc.Redis.Smembers(sceneMirrorsKey(parent))
	require.NoError(t, err)
	assert.Equal(t, []string{"9999"}, members, "重读失败时不能删子集合索引")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteMirrorCascade))
}

// ---------------------------------------------------------------------------
// world_rebalance.go:迁移规划与源迁移时的镜像级联
// ---------------------------------------------------------------------------

// 规划时读不到人数 / 老节点的负载集成员资格,就不把这个频道排进计划。
//   - opportunistic:频道在活节点 "10" 上、哈希目标是 "20"、有 300 人。旧代码人数读错当 0,把有人的频道
//     从活属主身上搬走(CAS 只比属主、不看人数)。
//   - urgent:"10" 还在负载集但角色已改成非 world,频道有 5 人。旧代码 ZSCORE 读错当「不在负载集」,
//     按节点已死排紧急迁移,同样搬走有人的频道。
func TestPlanWorldChannelRebalance_ReadErrorSkipsChannel(t *testing.T) {
	t.Run("player count", func(t *testing.T) {
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		sc.Config.MaxRebalanceMigrationsPerTick = 10
		registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
		registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)
		sceneID := findSceneIdHashingTo(t, []string{"10", "20"}, "20", 1000)
		seedWorldChannel(sc, testZoneId, readUnknownWorldConf, sceneID, "10")
		seedSceneMembersVia(sc, sceneID, 300)

		before := readUnknownCount(t, testZoneId, readUnknownSiteRebalancePlan)
		fired := injectRedisFaultsForTest(t, mr,
			redisFaultRule{cmd: "SCARD", key: instanceMembersKey(sceneID), times: 1})
		urgent, opportunistic, _ := PlanWorldChannelRebalance(sc, testZoneId, []uint64{readUnknownWorldConf})
		require.EqualValues(t, 1, fired.Load())
		assert.Empty(t, urgent)
		assert.Empty(t, opportunistic, "人数读不到的频道不能排进迁移计划")
		assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteRebalancePlan))
	})

	t.Run("load-set membership", func(t *testing.T) {
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		sc.Config.MaxRebalanceMigrationsPerTick = 10
		sc.Config.StrictNodeTypeSeparation = true
		registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)
		registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)
		seedWorldChannel(sc, testZoneId, readUnknownWorldConf, 556, "10")
		seedSceneMembersVia(sc, 556, 5)

		before := readUnknownCount(t, testZoneId, readUnknownSiteRebalancePlan)
		fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "ZSCORE", key: nodeLoadKey(testZoneId), times: 1})
		urgent, opportunistic, _ := PlanWorldChannelRebalance(sc, testZoneId, []uint64{readUnknownWorldConf})
		require.EqualValues(t, 1, fired.Load())
		assert.Empty(t, urgent, "读不到老节点是否还在负载集,就不能把有人的频道当节点已死迁走")
		assert.Empty(t, opportunistic)
		assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteRebalancePlan))
	})
}

// 源频道迁移时级联销毁共置的镜像:某个镜像本轮没销毁(读不到它的镜像源)时不删源的子集合索引。
// 旧代码:级联完无条件删索引,没销毁的镜像从此不可发现,channelHasMirrors 误报「没有镜像」。
func TestCascadeMirrorsOnSourceMigration_ChildFailureKeepsIndex(t *testing.T) {
	sc, mr, parent, mirror := createInstanceWithMirrorForReadUnknown(t)
	mirrorStr := strconv.FormatUint(mirror, 10)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneSourceKey(mirror), times: 1})

	cascadeMirrorsOnSourceMigration(context.Background(), sc, testZoneId, parent, []string{mirrorStr}, "10", "20")
	require.EqualValues(t, 1, fired.Load())
	requireSceneOwner(t, sc, mirror, "10", "读不到镜像源的镜像本轮不销毁")
	members, err := sc.Redis.Smembers(sceneMirrorsKey(parent))
	require.NoError(t, err)
	assert.Equal(t, []string{mirrorStr}, members, "有镜像没销毁时不能删源的子集合索引")
}

// 迁移改归属之前读不到源频道的镜像集合:这次迁移作废,归属不动,回收在新节点上预建的实体。
// 旧代码读错当「没有镜像」继续迁移,共置在老节点上的镜像不级联,留在那里等空闲超时。
func TestMigrateWorldChannel_MirrorReadErrorInvalidatesMigration(t *testing.T) {
	sc, mr := seedLiveWorldChannelForReadUnknown(t)
	createsBefore := sharedFakeSceneNode.createCalls.Load()
	destroysBefore := sharedFakeSceneNode.destroyCalls.Load()
	before := readUnknownCount(t, testZoneId, readUnknownSiteRebalancePlan)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "SMEMBERS", key: sceneMirrorsKey(501), times: 1})

	assert.False(t, migrateWorldChannel(context.Background(), sc, testZoneId, readUnknownWorldConf, 501, "10", "20", reasonBetterHome))
	require.EqualValues(t, 1, fired.Load())
	requireSceneOwner(t, sc, 501, "10", "读错时归属必须原样")
	assert.Equal(t, createsBefore+1, sharedFakeSceneNode.createCalls.Load())
	assert.Equal(t, destroysBefore+1, sharedFakeSceneNode.destroyCalls.Load(), "作废时要回收在 \"20\" 上预建的实体")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteRebalancePlan))
}

// ---------------------------------------------------------------------------
// createscenelogic.go:CreateScene 的世界频道路径与镜像去重
// ---------------------------------------------------------------------------

// CreateScene 选世界频道时读不到属主,回 ErrRedis 让上游重试,不走按需补建。
// 旧代码丢掉 error,走按需补建(zone 锁上忙等、对整组频道发 CreateScene)后再查一遍。
func TestCreateScene_MainWorldOwnerReadErrorReturnsErrRedis(t *testing.T) {
	sc, mr := seedLiveWorldChannelForReadUnknown(t)
	createCallsBefore := sharedFakeSceneNode.createCalls.Load()
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneNodeKey(501), times: 1})

	resp, err := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: readUnknownWorldConf,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_MAIN_WORLD,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, fired.Load())
	assert.Equal(t, constants.ErrRedis, resp.ErrorCode)
	assert.Zero(t, resp.SceneId)
	assert.Equal(t, createCallsBefore, sharedFakeSceneNode.createCalls.Load(), "不能因为一次读错走按需补建")
	requireSceneOwner(t, sc, 501, "10")
}

// 镜像去重读已有镜像的属主出错:不复用、也不把它当陈旧条目从源的子集合里摘掉,照常新建一个。
// 旧代码:读错当「陈旧」,把活着的镜像从索引里摘掉,源销毁时再也级联不到它。
func TestCreateScene_MirrorDedupReadErrorKeepsLiveMirrorIndexed(t *testing.T) {
	sc, mr, parent, mirror := createInstanceWithMirrorForReadUnknown(t)
	sc.Config.MirrorDedupBySource = true
	mirrorStr := strconv.FormatUint(mirror, 10)
	before := readUnknownCount(t, testZoneId, readUnknownSiteMirrorDedup)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneNodeKey(mirror), times: 1})

	resp, err := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		MirrorConfigId: 9001,
		SourceSceneId:  parent,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, fired.Load())
	require.Zero(t, resp.ErrorCode)
	assert.NotEqual(t, mirror, resp.SceneId, "读不到属主时不复用")
	members, err := sc.Redis.Smembers(sceneMirrorsKey(parent))
	require.NoError(t, err)
	assert.Contains(t, members, mirrorStr, "活着的镜像不能被当成陈旧条目摘掉")
	assert.Contains(t, members, strconv.FormatUint(resp.SceneId, 10))
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteMirrorDedup))
}

// ---------------------------------------------------------------------------
// orphan_cleanup.go:被删地图的频道清理
// ---------------------------------------------------------------------------

// readUnknownOrphanConf 不在 World 表里(newAutoscaleCtx 只登记了 autoscaleTestConfID)。
const readUnknownOrphanConf uint64 = 779

// 读不到排空集合:整组留到下一次。旧代码当「没有排空中的频道」,清完普通频道后删掉排空集合,
// 排空中的 902 从此再也找不到。
func TestOrphanCleanup_DrainingSetReadErrorDefersGroup(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, readUnknownOrphanConf, map[uint64]int64{901: 0})
	_, err := sc.Redis.Sadd(worldDrainingSetKey(testZoneId, readUnknownOrphanConf), "902")
	require.NoError(t, err)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(902), "10"))

	before := readUnknownCount(t, testZoneId, readUnknownSiteOrphanCleanup)
	fired := injectRedisFaultsForTest(t, mr,
		redisFaultRule{cmd: "SMEMBERS", key: worldDrainingSetKey(testZoneId, readUnknownOrphanConf), times: 1})
	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc))
	require.EqualValues(t, 1, fired.Load())
	requireSceneOwner(t, sc, 901, "10")
	requireSceneOwner(t, sc, 902, "10")
	draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, readUnknownOrphanConf))
	require.NoError(t, err)
	assert.Equal(t, []string{"902"}, draining, "读错时排空集合不能删")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteOrphanCleanup))

	assert.Equal(t, 1, CleanupOrphanWorldChannels(ctx, sc))
	requireSceneOwner(t, sc, 901, "")
	requireSceneOwner(t, sc, 902, "")
}

// 属主读不到不能绕过再入屏障。"10" 刚判死(不在负载集、death_at 就在现在):屏障应当挡住整组。
// 旧代码:屏障检查那次读失败当「没有属主」放行,删除循环重读成功后照删映射。
func TestOrphanCleanup_OwnerReadErrorDoesNotBypassReentryBarrier(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	seedChannels(t, sc, testZoneId, readUnknownOrphanConf, map[uint64]int64{911: 0})
	require.NoError(t, markNodeDeath(sc, testZoneId, "10"))

	before := readUnknownCount(t, testZoneId, readUnknownSiteOrphanCleanup)
	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneNodeKey(911), times: 1})
	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc))
	require.EqualValues(t, 1, fired.Load())
	requireSceneOwner(t, sc, 911, "10", "老属主刚判死,读错也不能清")
	assert.Equal(t, before+1, readUnknownCount(t, testZoneId, readUnknownSiteOrphanCleanup))

	// 读得到时由屏障挡住:证明上面拦下它的是 fail-closed,而不是巧合。
	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc))
	requireSceneOwner(t, sc, 911, "10")
}

// Agones 名额改由原子删除脚本在删之前读出。脚本里读名额失败 = 脚本整体报错、一个键都没删:这个频道
// 留在集合里,整组留到下一次 fullSync。旧代码:单独 GET 名额读错当「没有名额」,不还名额却照删 agones_gs 键。
func TestOrphanCleanup_AtomicDestroyFailureKeepsRoomMapping(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-a", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	seedChannels(t, sc, testZoneId, readUnknownOrphanConf, map[uint64]int64{921: 0})
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(921), "gs-a"))

	fired := injectRedisFaultsForTest(t, mr, redisFaultRule{cmd: "GET", key: sceneAgonesGsKey(921), times: 1})
	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc))
	require.EqualValues(t, 1, fired.Load())
	requireSceneOwner(t, sc, 921, "10", "脚本报错时还没动这个场景")
	gs, err := sc.Redis.Get(sceneAgonesGsKey(921))
	require.NoError(t, err)
	assert.Equal(t, "gs-a", gs)
	assert.EqualValues(t, 2, alloc.roomsOf("gs-a"))
	members, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, readUnknownOrphanConf))
	require.NoError(t, err)
	assert.Equal(t, []string{"921"}, members, "没拆掉的频道要留在集合里")

	assert.Equal(t, 1, CleanupOrphanWorldChannels(ctx, sc))
	requireSceneOwner(t, sc, 921, "")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-a"), "故障过去后名额照常归还,而且只还一次")
}

// 孤儿清理不看人数,场景里可能还有人:脚本交回的成员要从节点集合里摘掉。
func TestOrphanCleanup_RemovesResidualMembersFromNodeSet(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, readUnknownOrphanConf, map[uint64]int64{951: 0})
	const player uint64 = 7002
	seedSceneMembersVia(sc, 951, 1, player)
	seedNodeMembersVia(sc, testZoneId, "10", 1, player)

	assert.Equal(t, 1, CleanupOrphanWorldChannels(context.Background(), sc))
	requireSceneOwner(t, sc, 951, "")
	assert.Equal(t, "0", sceneMemberCount(mr, 951))
	assert.Equal(t, "0", nodeMemberCount(mr, testZoneId, "10"), "节点集合不能留着已销毁场景的成员")
}

// 第一段读到属主 "10" 之后、原子删除之前,属主被改写成 "20"(任意副本上的懒改派)。带期望属主的原子删除
// 落空:映射不动、频道留在集合里;已拆掉的频道已从集合摘掉,集合里列着的频道映射都还在。
// 旧代码:逐条 DEL,把新属主的映射删掉。
func TestOrphanCleanup_SkipsSceneWhoseOwnerChangedBeforeDestroy(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	sc.Redis.Zadd(testLoadKey(), 0, "20")
	seedChannels(t, sc, testZoneId, readUnknownOrphanConf, map[uint64]int64{931: 0, 932: 0})
	// 钩子跑在 miniredis 的服务端协程上:只能用 t.Errorf,不能用 require。
	fired := identHookOnceBeforeEval(t, mr, sceneNodeKey(931), func() {
		if err := mr.Set(sceneNodeKey(931), "20"); err != nil {
			t.Errorf("simulate concurrent reassign: %v", err)
		}
	})

	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc))
	require.True(t, fired.Load(), "前提不成立:原子删除没有带上 scene:931:node")
	requireSceneOwner(t, sc, 931, "20", "新属主的映射不能被删")
	requireSceneOwner(t, sc, 932, "", "同组其它频道照常拆")
	members, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, readUnknownOrphanConf))
	require.NoError(t, err)
	assert.Equal(t, []string{"931"}, members, "拆掉的频道立刻摘出集合,没拆的留着")
	desired, err := sc.Redis.Hget(worldDesiredChannelsKey(testZoneId), strconv.FormatUint(readUnknownOrphanConf, 10))
	require.NoError(t, err, "组没拆完,发现索引(期望数字段)必须留着")
	assert.NotEmpty(t, desired)
}

// 第一段读到「没有属主」,原子删除之前却被写入了属主 "20"(任意副本上的懒改派)。原子删除按「映射必须仍不存在」
// 落空:不能把新属主的映射抹掉 —— 调用方按空属主做副作用,新节点上的实体与计数会没人收。
// 旧代码(以及把空串当「不比对」的写法):照删映射。
func TestOrphanCleanup_SkipsSceneThatGainedAnOwnerBeforeDestroy(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "20")
	seedChannels(t, sc, testZoneId, readUnknownOrphanConf, map[uint64]int64{941: 0})
	_, err := sc.Redis.Del(sceneNodeKey(941)) // 第一段读到的是「映射不存在」
	require.NoError(t, err)
	// 钩子跑在 miniredis 的服务端协程上:只能用 t.Errorf,不能用 require。
	fired := identHookOnceBeforeEval(t, mr, sceneNodeKey(941), func() {
		if err := mr.Set(sceneNodeKey(941), "20"); err != nil {
			t.Errorf("simulate concurrent lazy reassign: %v", err)
		}
	})

	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc))
	require.True(t, fired.Load(), "前提不成立:原子删除没有带上 scene:941:node")
	requireSceneOwner(t, sc, 941, "20", "期间新写入的属主不能被抹掉")
	members, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, readUnknownOrphanConf))
	require.NoError(t, err)
	assert.Equal(t, []string{"941"}, members, "没拆的频道留在集合里,下一次 fullSync 按新属主再拆")
}
