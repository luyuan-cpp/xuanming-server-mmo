package logic

import (
	"context"
	"strconv"
	"testing"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 新场景登记的回归(缺口清单第 1 条残余,scene_registration.go)。
// 旧代码 allocateScene 与建实例、铺世界频道的其余写入是十来条独立命令,下面各用例注释写了旧代码的表现。

const registrationTestNode = "node-a"

func testInstanceRegistration(sceneID uint64) instanceRegistration {
	return instanceRegistration{
		sceneRegistration: sceneRegistration{SceneID: sceneID, ZoneID: claimTestZone, NodeID: registrationTestNode},
		CreatedAt:         1,
	}
}

// evalTestInstanceRegistration 用给定 token 跑一次实例登记脚本,模拟 go-redis 重发或晚到的原 EVAL。
func evalTestInstanceRegistration(t *testing.T, sc *svc.ServiceContext, reg instanceRegistration, token string) registrationOutcome {
	t.Helper()
	keys, extraArgs := instanceRegistrationParams(reg)
	raw, err := evalSceneRegistration(sc, luaRegisterInstance, keys, reg.sceneRegistration, token, extraArgs...)
	require.NoError(t, err)
	return parseRegistrationReply(raw)
}

func evalTestAbortRegistration(t *testing.T, sc *svc.ServiceContext, reg instanceRegistration, token string) registrationOutcome {
	t.Helper()
	raw, err := evalAbortSceneRegistration(sc, reg.sceneRegistration, token)
	require.NoError(t, err)
	return parseRegistrationReply(raw)
}

// 镜像实例的全部状态一步写好。旧代码在任意两条命令之间崩溃,都会留下映射在、却不在 active 集合里的实例。
func TestRegisterInstance_WritesEverythingInOneStep(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		sceneID uint64 = 89701
		source  uint64 = 89702
	)
	member := strconv.FormatUint(sceneID, 10)
	reg := testInstanceRegistration(sceneID)
	reg.AgonesRoom = agonesRoom{GameServer: "gs-a", Entry: newAgonesRoomEntry(sceneID)}
	reg.CreatedAt = 1234
	reg.Mirror = true
	reg.SourceSceneID = source
	require.NoError(t, holdAgonesRoom(sc, claimTestZone, reg.AgonesRoom.Entry))

	require.Equal(t, registrationRegistered, registerInstance(sc, reg))

	require.Equal(t, registrationTestNode, mustGet(t, mr, sceneNodeKey(claimTestZone, sceneID)))
	require.False(t, mr.Exists("scene:"+member+":zone"), "场景所在的 zone 是键的 tag,不再另写 scene:<id>:zone")
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)))
	require.Equal(t, reg.AgonesRoom.record(), mustGet(t, mr, sceneAgonesGsKey(claimTestZone, sceneID)))
	require.Empty(t, agonesHoldsOf(t, sc, claimTestZone), "登记在同一步消费了名额的在途占用")
	require.Equal(t, "1", mustGet(t, mr, sceneMirrorFlagKey(claimTestZone, sceneID)))
	require.Equal(t, strconv.FormatUint(source, 10), mustGet(t, mr, sceneSourceKey(claimTestZone, sceneID)))
	nodeScenes, err := mr.Members(nodeScenesKey(claimTestZone, registrationTestNode))
	require.NoError(t, err)
	require.Equal(t, []string{member}, nodeScenes)
	mirrors, err := mr.Members(sceneMirrorsKey(claimTestZone, source))
	require.NoError(t, err)
	require.Equal(t, []string{member}, mirrors)
	score, err := mr.ZScore(activeInstancesKey(claimTestZone), member)
	require.NoError(t, err)
	require.Equal(t, float64(1234), score, "空闲时钟从创建时刻起算")
	require.Positive(t, mr.TTL(sceneRegistrationRecordKey(claimTestZone, sceneID)), "登记记录要带 TTL")
}

// 普通实例不写镜像标记、镜像源和 agones_gs。
func TestRegisterInstance_PlainInstanceWritesNoMirrorOrRoomKeys(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89703

	require.Equal(t, registrationRegistered, registerInstance(sc, testInstanceRegistration(sceneID)))

	requireActiveInstance(t, sc, claimTestZone, sceneID, true)
	for _, key := range []string{sceneMirrorFlagKey(claimTestZone, sceneID), sceneSourceKey(claimTestZone, sceneID), sceneAgonesGsKey(claimTestZone, sceneID)} {
		require.False(t, mr.Exists(key), key)
	}
}

// 世界频道一步进频道集合,不进 active 集合。旧代码映射写成、SADD 失败时只记日志 continue,频道成了谁都找不到的孤儿,
// 节点计数却已加一。
func TestRegisterWorldChannel_WritesEverythingInOneStep(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		sceneID uint64 = 89704
		confID  uint64 = 3300
	)
	member := strconv.FormatUint(sceneID, 10)
	reg := sceneRegistration{SceneID: sceneID, ZoneID: claimTestZone, NodeID: registrationTestNode,
		AgonesRoom: agonesRoom{GameServer: "gs-w", Entry: newAgonesRoomEntry(sceneID)}}
	require.NoError(t, holdAgonesRoom(sc, claimTestZone, reg.AgonesRoom.Entry))

	require.Equal(t, registrationRegistered, registerWorldChannel(sc, reg, confID, 1))

	require.Equal(t, registrationTestNode, mustGet(t, mr, sceneNodeKey(claimTestZone, sceneID)))
	require.False(t, mr.Exists("scene:"+member+":zone"), "场景所在的 zone 是键的 tag,不再另写 scene:<id>:zone")
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)))
	require.Equal(t, reg.AgonesRoom.record(), mustGet(t, mr, sceneAgonesGsKey(claimTestZone, sceneID)))
	require.Empty(t, agonesHoldsOf(t, sc, claimTestZone))
	channels, err := mr.Members(worldChannelsKey(claimTestZone, confID))
	require.NoError(t, err)
	require.Equal(t, []string{member}, channels)
	nodeScenes, err := mr.Members(nodeScenesKey(claimTestZone, registrationTestNode))
	require.NoError(t, err)
	require.Equal(t, []string{member}, nodeScenes)
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// 登记已执行、回包丢失:作废查询用同一 token 拿回 'registered',照常往下走,计数只加一次。
func TestRegisterInstance_ReplyLostResolvesToRegistered(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaRegisterInstance}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
	const sceneID uint64 = 89705
	reg := testInstanceRegistration(sceneID)
	reg.AgonesRoom = agonesRoom{GameServer: "gs-a", Entry: newAgonesRoomEntry(sceneID)}
	require.NoError(t, holdAgonesRoom(sc, claimTestZone, reg.AgonesRoom.Entry))

	require.Equal(t, registrationRegistered, registerInstance(sc, reg))
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)), "计数只加一次")
	require.Equal(t, reg.AgonesRoom.record(), mustGet(t, mr, sceneAgonesGsKey(claimTestZone, sceneID)), "作废查询读记录拿回已登记,不用再消费在途占用")
	requireActiveInstance(t, sc, claimTestZone, sceneID, true)
}

// 登记脚本没执行:作废写下记录、回 aborted,一个场景键都没有。
func TestRegisterInstance_ScriptNotRunIsAborted(t *testing.T) {
	hook := &failFirstEvalHook{script: luaRegisterInstance}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
	const sceneID uint64 = 89706

	require.Equal(t, registrationAborted, registerInstance(sc, testInstanceRegistration(sceneID)))
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.False(t, mr.Exists(sceneNodeKey(claimTestZone, sceneID)))
	require.False(t, mr.Exists(nodeSceneCountKey(claimTestZone, registrationTestNode)))
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// 作废之后才到的同一 token 的登记(原来那次 EVAL 还在路上)什么都不写。只读映射判断不了这种交错。
func TestAbortSceneRegistration_FencesLateRegistration(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89707
	reg := testInstanceRegistration(sceneID)

	require.Equal(t, registrationAborted, evalTestAbortRegistration(t, sc, reg, "token-1"))
	require.Equal(t, registrationAborted, evalTestInstanceRegistration(t, sc, reg, "token-1"), "作废之后到的登记不落地")
	require.False(t, mr.Exists(sceneNodeKey(claimTestZone, sceneID)))
	require.False(t, mr.Exists(nodeSceneCountKey(claimTestZone, registrationTestNode)))
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// 同一 token 重发:回原结果,一个键都不再写,计数只加一次。
func TestRegisterInstance_SameTokenReplaysWithoutWriting(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89710
	reg := testInstanceRegistration(sceneID)

	require.Equal(t, registrationRegistered, evalTestInstanceRegistration(t, sc, reg, "token-1"))
	require.Equal(t, registrationRegistered, evalTestInstanceRegistration(t, sc, reg, "token-1"))
	require.Equal(t, registrationRegistered, evalTestAbortRegistration(t, sc, reg, "token-1"), "作废查询拿回已登记")
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)), "计数只加一次")
}

// 登记落地之后、重发或作废查询到达之前,场景被销毁(映射没了)或改派到别的节点:不能再回已登记,否则调用方
// 会向原节点发 CreateScene,在节点上留下一个没有 Redis 状态的实体。销毁认领用自己的记录键,不会替登记改写结局。
func TestRegisterInstance_ReplayAfterDestroyOrMoveIsConflict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner string // 重放时的属主映射;空串表示映射已被销毁
	}{
		{name: "destroyed"},
		{name: "moved", owner: "node-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newIntegrationSvcCtx(t)
			const sceneID uint64 = 89711
			reg := testInstanceRegistration(sceneID)
			require.Equal(t, registrationRegistered, evalTestInstanceRegistration(t, sc, reg, "token-1"))
			mr.Del(sceneNodeKey(claimTestZone, sceneID))
			if tc.owner != "" {
				require.NoError(t, mr.Set(sceneNodeKey(claimTestZone, sceneID), tc.owner))
			}

			require.Equal(t, registrationConflict, evalTestInstanceRegistration(t, sc, reg, "token-1"), "重发不得回已登记")
			require.Equal(t, registrationConflict, evalTestAbortRegistration(t, sc, reg, "token-1"), "作废查询不得回已登记")
			require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)), "重放一个键都不写")
			if tc.owner == "" {
				require.False(t, mr.Exists(sceneNodeKey(claimTestZone, sceneID)), "不得在已销毁的场景上复活映射")
			} else {
				require.Equal(t, tc.owner, mustGet(t, mr, sceneNodeKey(claimTestZone, sceneID)), "不得改回原节点")
			}
		})
	}
}

// 新铸的号已有映射:一个键都不写。旧代码裸 SET,两个场景共用一个 id,路由互相覆盖。
func TestRegisterInstance_ExistingMappingIsConflict(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89708
	require.NoError(t, mr.Set(sceneNodeKey(claimTestZone, sceneID), "node-old"))

	require.Equal(t, registrationConflict, registerInstance(sc, testInstanceRegistration(sceneID)))
	require.Equal(t, "node-old", mustGet(t, mr, sceneNodeKey(claimTestZone, sceneID)))
	require.False(t, mr.Exists(nodeSceneCountKey(claimTestZone, registrationTestNode)))
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// 上一个用这个号的场景还有没拆完的拆除任务:不登记,否则旧任务补发的 DestroyScene 会打到新场景上。
func TestRegisterInstance_PendingTeardownIsConflict(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89712
	require.NoError(t, mr.Set(sceneTeardownJobKey(claimTestZone, sceneID), "pending"))

	require.Equal(t, registrationConflict, registerInstance(sc, testInstanceRegistration(sceneID)))
	require.False(t, mr.Exists(sceneNodeKey(claimTestZone, sceneID)))
	require.False(t, mr.Exists(nodeSceneCountKey(claimTestZone, registrationTestNode)))
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// 别的 token 的登记记录同样是冲突:同一个号已经被另一次登记用过(映射可能已随销毁删掉),这次一个键都不写。
func TestRegisterWorldChannel_OtherTokenRecordIsConflict(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		sceneID uint64 = 89709
		confID  uint64 = 3300
	)
	require.NoError(t, mr.Set(sceneRegistrationRecordKey(claimTestZone, sceneID), `{"token":"other-token","reply":["registered"]}`))
	reg := sceneRegistration{SceneID: sceneID, ZoneID: claimTestZone, NodeID: registrationTestNode}

	require.Equal(t, registrationConflict, registerWorldChannel(sc, reg, confID, 1))
	require.False(t, mr.Exists(sceneNodeKey(claimTestZone, sceneID)))
	require.False(t, mr.Exists(worldChannelsKey(claimTestZone, confID)))
	require.False(t, mr.Exists(nodeSceneCountKey(claimTestZone, registrationTestNode)))
}

// ── 世界频道登记在同一步里比在役数与期望数(2026-11-12,infra-reconnect-overview.md §4.1 R7) ─────────────────

// 在役频道数已到期望数:登记一个键都不写,回 over_desired,在途占用也不消费。
// 修复前红:登记脚本只 SADD、不比期望数。补建锁丢了(Redis 主从切换)或补建拖过了锁的 TTL 时,两个补建者按同一份
// 「缺 1 个」各建一条,频道数超过期望数,多出来的那条没有任何路径收回。
func TestRegisterWorldChannel_RefusesBeyondDesiredCount(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		first  uint64 = 89720
		second uint64 = 89721
		confID uint64 = 3300
	)
	mr.HSet(worldDesiredChannelsKey(claimTestZone), strconv.FormatUint(confID, 10), "1")
	require.Equal(t, registrationRegistered,
		registerWorldChannel(sc, sceneRegistration{SceneID: first, ZoneID: claimTestZone, NodeID: registrationTestNode}, confID, 1))

	late := sceneRegistration{SceneID: second, ZoneID: claimTestZone, NodeID: registrationTestNode,
		AgonesRoom: agonesRoom{GameServer: "gs-w", Entry: newAgonesRoomEntry(second)}}
	require.NoError(t, holdAgonesRoom(sc, claimTestZone, late.AgonesRoom.Entry))
	require.Equal(t, registrationOverDesired, registerWorldChannel(sc, late, confID, 1))

	channels, err := mr.Members(worldChannelsKey(claimTestZone, confID))
	require.NoError(t, err)
	require.Equal(t, []string{strconv.FormatUint(first, 10)}, channels, "频道数停在期望数")
	for _, key := range []string{sceneNodeKey(claimTestZone, second), sceneAgonesGsKey(claimTestZone, second), sceneRegistrationRecordKey(claimTestZone, second)} {
		require.False(t, mr.Exists(key), key)
	}
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)), "节点计数只算登记成功的那一条")
	require.Equal(t, []string{late.AgonesRoom.Entry}, agonesHoldsOf(t, sc, claimTestZone),
		"在途占用没被消费,调用方放弃名额时一并删")
}

// 同一 token 重发:第一次已登记,在役数已经把自己算进去。重发先拿回登记记录里的结局,不会被在役数判断改判成 over_desired
// (改判的话调用方会放弃一个已经登记在案的名额)。
func TestRegisterWorldChannel_ReplayAfterFillingTheMapStillReportsRegistered(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const (
		sceneID uint64 = 89722
		confID  uint64 = 3300
	)
	reg := sceneRegistration{SceneID: sceneID, ZoneID: claimTestZone, NodeID: registrationTestNode}
	eval := func() registrationOutcome {
		raw, err := evalSceneRegistration(sc, luaRegisterWorldChannel, worldChannelRegistrationKeys(reg, confID), reg, "token-1",
			strconv.FormatUint(confID, 10), 1)
		require.NoError(t, err)
		return parseRegistrationReply(raw)
	}

	require.Equal(t, registrationRegistered, eval())
	require.Equal(t, registrationRegistered, eval(), "重发拿回同一个结局")
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)), "重发不重复计数")
}

// 期望数以哈希里的权威值为准(自动伸缩改的就是它);字段不存在或不是正整数时用调用方读到的值
// (DesiredWorldChannelCount 这时回的是配置种子)。每种情况连登记 4 条,落地的条数就是生效的期望数。
func TestRegisterWorldChannel_DesiredCountSource(t *testing.T) {
	const confID uint64 = 3300
	cases := []struct {
		name          string
		stored        string // 空串 = 字段不存在
		callerDesired int
		wantChannels  int
	}{
		{name: "字段不存在用调用方的值", stored: "", callerDesired: 1, wantChannels: 1},
		{name: "字段非法用调用方的值", stored: "abc", callerDesired: 2, wantChannels: 2},
		{name: "字段为 0 用调用方的值", stored: "0", callerDesired: 1, wantChannels: 1},
		{name: "权威值比调用方读到的大", stored: "2", callerDesired: 1, wantChannels: 2},
		{name: "权威值比调用方读到的小", stored: "1", callerDesired: 3, wantChannels: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newIntegrationSvcCtx(t)
			if tc.stored != "" {
				mr.HSet(worldDesiredChannelsKey(claimTestZone), strconv.FormatUint(confID, 10), tc.stored)
			}
			registered := 0
			for i := range 4 {
				reg := sceneRegistration{SceneID: 89730 + uint64(i), ZoneID: claimTestZone, NodeID: registrationTestNode}
				switch outcome := registerWorldChannel(sc, reg, confID, tc.callerDesired); outcome {
				case registrationRegistered:
					registered++
				case registrationOverDesired:
				default:
					t.Fatalf("unexpected registration outcome %s", outcome)
				}
			}
			require.Equal(t, tc.wantChannels, registered)
			channels, err := mr.Members(worldChannelsKey(claimTestZone, confID))
			require.NoError(t, err)
			require.Len(t, channels, tc.wantChannels)
		})
	}
}

// 补建一侧:补建锁没挡住并发时,另一个补建者在本轮数完频道集合之后、登记之前抢先登记了一条。本轮按「缺 1 个」去建的
// 那条被登记脚本拒绝,频道数停在期望数。
// 修复前红:本轮照样登记,这张图有 2 条频道而期望数是 1。
func TestInitWorldScenes_LateInitializerDoesNotExceedDesiredCount(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		confID     uint64 = 1001
		rivalScene        = "990001"
	)
	ctx := context.Background()
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	channelsKey := worldChannelsKey(testZoneId, confID)

	// 本轮第一条带频道集合键的 EVAL 就是新频道的登记:在它执行之前,让对手的登记落地。
	fired := identHookOnceBeforeEval(t, mr, channelsKey, func() {
		if _, err := mr.SAdd(channelsKey, rivalScene); err != nil {
			t.Errorf("simulate the rival initializer's registration: %v", err)
		}
		if err := mr.Set(sceneNodeKey(testZoneId, 990001), "10"); err != nil {
			t.Errorf("simulate the rival initializer's owner mapping: %v", err)
		}
	})
	initWorldScenesForZone(ctx, sc, testZoneId, []uint64{confID}, true)
	require.True(t, fired.Load(), "前提不成立:本轮没有走到登记")

	channels, err := mr.Members(channelsKey)
	require.NoError(t, err)
	require.Equal(t, []string{rivalScene}, channels, "只有对手登记的那一条,本轮没有多建")
	require.False(t, mr.Exists(nodeSceneCountKey(testZoneId, "10")), "被拒的登记不加节点计数")
}

// aborted、conflict、fenced、over_desired 都放弃这次占的名额条目:记录没指向它,删的只是自己那一条。unknown 不放弃:
// 登记可能已经落地,记录指向这一条,删了就少记;条目与在途占用留给对账在过期之后按记录判。
// 2026-11-10 之前名额是计数,conflict 分不清两种来历、怕双还,只有 aborted 还。
func TestRegistrationOutcome_ReleasesReservationUnlessItMayHaveLanded(t *testing.T) {
	require.True(t, registrationAborted.releasesReservation())
	require.True(t, registrationConflict.releasesReservation())
	require.True(t, registrationFenced.releasesReservation())
	require.True(t, registrationOverDesired.releasesReservation())
	require.False(t, registrationUnknown.releasesReservation())
	require.False(t, registrationRegistered.releasesReservation())
}

// 名额的在途占用已被对账取消(拖过了截止时刻):登记一个键都不写,回 fenced。否则记录会指向一条已经被删的条目,
// 这个场景不占名额,节点被超卖。没有在途占用这道闸时,对账只能靠宽限期猜「这条还在不在途」,猜错就是这一种少记。
func TestRegisterInstance_CanceledRoomHoldIsFenced(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89713
	reg := testInstanceRegistration(sceneID)
	reg.AgonesRoom = agonesRoom{GameServer: "gs-a", Entry: newAgonesRoomEntry(sceneID)}
	// 刻意不写在途占用:等同于对账已经取消了它。

	require.Equal(t, registrationFenced, registerInstance(sc, reg))
	require.Equal(t, registrationFenced, evalTestInstanceRegistration(t, sc, reg, "token-1"), "重发照样被挡")
	for _, key := range []string{sceneNodeKey(claimTestZone, sceneID), sceneAgonesGsKey(claimTestZone, sceneID), sceneRegistrationRecordKey(claimTestZone, sceneID)} {
		require.False(t, mr.Exists(key), key)
	}
	require.False(t, mr.Exists(nodeSceneCountKey(claimTestZone, registrationTestNode)))
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// newAgonesInstanceCtx 铺一个 Agones 模式、只有一个实例节点 10(gs-1)的 zone。opts(钩子)要在首次为这个地址
// 建客户端时挂上,所以用新起的 miniredis,不能在别的 helper 建好之后再换,见 newIntegrationSvcCtxWithRedisOptions。
func newAgonesInstanceCtx(t *testing.T, opts ...redis.Hook) (*fakeAllocator, *CreateSceneLogic) {
	t.Helper()
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, opts...)
	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Ready", capacity: 4})
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)
	return alloc, NewCreateSceneLogic(context.Background(), sc)
}

func createTestInstance(t *testing.T, l *CreateSceneLogic) *scene_manager.CreateSceneResponse {
	t.Helper()
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType(constants.SceneTypeInstance),
	})
	require.NoError(t, err)
	return resp
}

// 登记没落地:建实例回错,刚占的名额还回去,不留实例。旧代码 SET 映射失败也还名额,但 ZADD、agones_gs
// 这些后续写失败只记日志,照样返回成功。
func TestCreateScene_AgonesRegistrationAbortedReleasesRoom(t *testing.T) {
	hook := &failFirstEvalHook{script: luaRegisterInstance}
	alloc, l := newAgonesInstanceCtx(t, hook)

	resp := createTestInstance(t, l)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Equal(t, constants.ErrRedis, resp.ErrorCode)
	assert.EqualValues(t, 0, alloc.roomsOf("gs-1"), "登记确定没落地,名额要还")
	assert.Empty(t, agonesHoldsOf(t, l.svcCtx, testZoneId), "在途占用一起删")
	members, _ := l.svcCtx.Redis.ZrangeWithScores(activeInstancesKey(testZoneId), 0, -1)
	assert.Empty(t, members)
}

// 登记落地、回包丢失:照常建成,名额只占一份。
func TestCreateScene_AgonesRegistrationReplyLostStillCreates(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaRegisterInstance}
	alloc, l := newAgonesInstanceCtx(t, hook)

	resp := createTestInstance(t, l)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.EqualValues(t, 1, alloc.roomsOf("gs-1"))
	requireAgonesRecordOn(t, l.svcCtx, alloc, resp.SceneId, "gs-1")
	assert.Empty(t, agonesHoldsOf(t, l.svcCtx, testZoneId))
}
