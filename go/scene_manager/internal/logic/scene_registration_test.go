package logic

import (
	"context"
	"strconv"
	"testing"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
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
	reg.AgonesGs = "gs-a"
	reg.CreatedAt = 1234
	reg.Mirror = true
	reg.SourceSceneID = source

	require.Equal(t, registrationRegistered, registerInstance(sc, reg))

	require.Equal(t, registrationTestNode, mustGet(t, mr, sceneNodeKey(sceneID)))
	require.Equal(t, strconv.FormatUint(uint64(claimTestZone), 10), mustGet(t, mr, sceneZoneKey(sceneID)))
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)))
	require.Equal(t, "gs-a", mustGet(t, mr, sceneAgonesGsKey(sceneID)))
	require.Equal(t, "1", mustGet(t, mr, sceneMirrorFlagKey(sceneID)))
	require.Equal(t, strconv.FormatUint(source, 10), mustGet(t, mr, sceneSourceKey(sceneID)))
	nodeScenes, err := mr.Members(nodeScenesKey(claimTestZone, registrationTestNode))
	require.NoError(t, err)
	require.Equal(t, []string{member}, nodeScenes)
	mirrors, err := mr.Members(sceneMirrorsKey(source))
	require.NoError(t, err)
	require.Equal(t, []string{member}, mirrors)
	score, err := mr.ZScore(activeInstancesKey(claimTestZone), member)
	require.NoError(t, err)
	require.Equal(t, float64(1234), score, "空闲时钟从创建时刻起算")
	require.Positive(t, mr.TTL(sceneRegistrationRecordKey(sceneID)), "登记记录要带 TTL")
}

// 普通实例不写镜像标记、镜像源和 agones_gs。
func TestRegisterInstance_PlainInstanceWritesNoMirrorOrRoomKeys(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89703

	require.Equal(t, registrationRegistered, registerInstance(sc, testInstanceRegistration(sceneID)))

	requireActiveInstance(t, sc, claimTestZone, sceneID, true)
	for _, key := range []string{sceneMirrorFlagKey(sceneID), sceneSourceKey(sceneID), sceneAgonesGsKey(sceneID)} {
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
	reg := sceneRegistration{SceneID: sceneID, ZoneID: claimTestZone, NodeID: registrationTestNode, AgonesGs: "gs-w"}

	require.Equal(t, registrationRegistered, registerWorldChannel(sc, reg, confID))

	require.Equal(t, registrationTestNode, mustGet(t, mr, sceneNodeKey(sceneID)))
	require.Equal(t, strconv.FormatUint(uint64(claimTestZone), 10), mustGet(t, mr, sceneZoneKey(sceneID)))
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)))
	require.Equal(t, "gs-w", mustGet(t, mr, sceneAgonesGsKey(sceneID)))
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
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	const sceneID uint64 = 89705
	reg := testInstanceRegistration(sceneID)
	reg.AgonesGs = "gs-a"

	require.Equal(t, registrationRegistered, registerInstance(sc, reg))
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)), "计数只加一次")
	require.Equal(t, "gs-a", mustGet(t, mr, sceneAgonesGsKey(sceneID)))
	requireActiveInstance(t, sc, claimTestZone, sceneID, true)
}

// 登记脚本没执行:作废写下记录、回 aborted,一个场景键都没有。
func TestRegisterInstance_ScriptNotRunIsAborted(t *testing.T) {
	hook := &failFirstEvalHook{script: luaRegisterInstance}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	const sceneID uint64 = 89706

	require.Equal(t, registrationAborted, registerInstance(sc, testInstanceRegistration(sceneID)))
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.False(t, mr.Exists(sceneNodeKey(sceneID)))
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
	require.False(t, mr.Exists(sceneNodeKey(sceneID)))
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
			mr.Del(sceneNodeKey(sceneID))
			if tc.owner != "" {
				require.NoError(t, mr.Set(sceneNodeKey(sceneID), tc.owner))
			}

			require.Equal(t, registrationConflict, evalTestInstanceRegistration(t, sc, reg, "token-1"), "重发不得回已登记")
			require.Equal(t, registrationConflict, evalTestAbortRegistration(t, sc, reg, "token-1"), "作废查询不得回已登记")
			require.Equal(t, "1", mustGet(t, mr, nodeSceneCountKey(claimTestZone, registrationTestNode)), "重放一个键都不写")
			if tc.owner == "" {
				require.False(t, mr.Exists(sceneNodeKey(sceneID)), "不得在已销毁的场景上复活映射")
			} else {
				require.Equal(t, tc.owner, mustGet(t, mr, sceneNodeKey(sceneID)), "不得改回原节点")
			}
		})
	}
}

// 新铸的号已有映射:一个键都不写。旧代码裸 SET,两个场景共用一个 id,路由互相覆盖。
func TestRegisterInstance_ExistingMappingIsConflict(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89708
	require.NoError(t, mr.Set(sceneNodeKey(sceneID), "node-old"))

	require.Equal(t, registrationConflict, registerInstance(sc, testInstanceRegistration(sceneID)))
	require.Equal(t, "node-old", mustGet(t, mr, sceneNodeKey(sceneID)))
	require.False(t, mr.Exists(nodeSceneCountKey(claimTestZone, registrationTestNode)))
	requireActiveInstance(t, sc, claimTestZone, sceneID, false)
}

// 上一个用这个号的场景还有没拆完的拆除任务:不登记,否则旧任务补发的 DestroyScene 会打到新场景上。
func TestRegisterInstance_PendingTeardownIsConflict(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID uint64 = 89712
	require.NoError(t, mr.Set(sceneTeardownJobKey(sceneID), "pending"))

	require.Equal(t, registrationConflict, registerInstance(sc, testInstanceRegistration(sceneID)))
	require.False(t, mr.Exists(sceneNodeKey(sceneID)))
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
	require.NoError(t, mr.Set(sceneRegistrationRecordKey(sceneID), `{"token":"other-token","reply":["registered"]}`))
	reg := sceneRegistration{SceneID: sceneID, ZoneID: claimTestZone, NodeID: registrationTestNode}

	require.Equal(t, registrationConflict, registerWorldChannel(sc, reg, confID))
	require.False(t, mr.Exists(sceneNodeKey(sceneID)))
	require.False(t, mr.Exists(worldChannelsKey(claimTestZone, confID)))
	require.False(t, mr.Exists(nodeSceneCountKey(claimTestZone, registrationTestNode)))
}

// 只有 aborted 能还名额。conflict 和 unknown 时登记可能已经落地,名额由空闲回收或销毁方按 agones_gs 还。
func TestRegistrationOutcome_OnlyAbortedReleasesReservation(t *testing.T) {
	require.True(t, registrationAborted.releasesReservation())
	require.False(t, registrationConflict.releasesReservation())
	require.False(t, registrationUnknown.releasesReservation())
	require.False(t, registrationRegistered.releasesReservation())
}

// newAgonesInstanceCtx 铺一个 Agones 模式、只有一个实例节点 10(gs-1)的 zone。opts(钩子)要在首次为这个地址
// 建客户端时挂上,所以用新起的 miniredis,不能在别的 helper 建好之后再换,见 newIntegrationSvcCtxWithRedisOptions。
func newAgonesInstanceCtx(t *testing.T, opts ...redis.Option) (*fakeAllocator, *CreateSceneLogic) {
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
	alloc, l := newAgonesInstanceCtx(t, redis.WithHook(hook))

	resp := createTestInstance(t, l)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Equal(t, constants.ErrRedis, resp.ErrorCode)
	assert.EqualValues(t, 0, alloc.roomsOf("gs-1"), "登记确定没落地,名额要还")
	members, _ := l.svcCtx.Redis.ZrangeWithScores(activeInstancesKey(testZoneId), 0, -1)
	assert.Empty(t, members)
}

// 登记落地、回包丢失:照常建成,名额只占一份。
func TestCreateScene_AgonesRegistrationReplyLostStillCreates(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaRegisterInstance}
	alloc, l := newAgonesInstanceCtx(t, redis.WithHook(hook))

	resp := createTestInstance(t, l)
	require.True(t, hook.fired.Load(), "注入必须真的发生")
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.EqualValues(t, 1, alloc.roomsOf("gs-1"))
	gs, _ := l.svcCtx.Redis.Get(sceneAgonesGsKey(resp.SceneId))
	assert.Equal(t, "gs-1", gs)
}
