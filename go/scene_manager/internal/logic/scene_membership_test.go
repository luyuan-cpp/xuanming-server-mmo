package logic

// 成员集合的测试夹具与行为用例(scene_membership.go,docs/design/redis-player-store-cluster.md §5.10)。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

const sceneMemberFillerPrefix = "seed-"

// sceneMemberFill 组 n 个成员:players 在内,不足的用占位成员补齐。占位成员不是数字,不会撞上真实玩家。
func sceneMemberFill(n int, players []uint64) []string {
	members := make([]string, 0, max(n, len(players)))
	for _, player := range players {
		members = append(members, strconv.FormatUint(player, 10))
	}
	for i := len(members); i < n; i++ {
		members = append(members, sceneMemberFillerPrefix+strconv.Itoa(i))
	}
	return members
}

func resetMembers(mr *miniredis.Miniredis, key string, n int, players []uint64) {
	mr.Del(key)
	members := sceneMemberFill(n, players)
	if len(members) == 0 {
		return
	}
	if _, err := mr.SetAdd(key, members...); err != nil {
		panic(err)
	}
}

// seedSceneMembers 把场景的成员集合铺成 n 人,players 在内。
func seedSceneMembers(mr *miniredis.Miniredis, sceneID uint64, n int, players ...uint64) {
	resetMembers(mr, instanceMembersKey(sceneID), n, players)
}

// seedNodeMembers 把节点的成员集合铺成 n 人,players 在内。
func seedNodeMembers(mr *miniredis.Miniredis, zoneID uint32, nodeID string, n int, players ...uint64) {
	resetMembers(mr, nodeMembersKey(zoneID, nodeID), n, players)
}

// seedSceneMembersVia / seedNodeMembersVia 与上面相同,给只拿着 svcCtx 的用例用。
func seedSceneMembersVia(sc *svc.ServiceContext, sceneID uint64, n int, players ...uint64) {
	resetMembersVia(sc, instanceMembersKey(sceneID), n, players)
}

func seedNodeMembersVia(sc *svc.ServiceContext, zoneID uint32, nodeID string, n int, players ...uint64) {
	resetMembersVia(sc, nodeMembersKey(zoneID, nodeID), n, players)
}

func resetMembersVia(sc *svc.ServiceContext, key string, n int, players []uint64) {
	if _, err := sc.Redis.Del(key); err != nil {
		panic(err)
	}
	members := sceneMemberFill(n, players)
	if len(members) == 0 {
		return
	}
	values := make([]any, len(members))
	for i, member := range members {
		values[i] = member
	}
	if _, err := sc.Redis.Sadd(key, values...); err != nil {
		panic(err)
	}
}

// adoptMember 把一个占位成员换成真实玩家,人数不变:location 指向这里的玩家本来就算在这个人数里。
// 没有占位成员时什么都不做,铺好的人数说了算。
func adoptMember(mr *miniredis.Miniredis, key string, player uint64) {
	members, err := mr.SMembers(key)
	if errors.Is(err, miniredis.ErrKeyNotFound) {
		return
	}
	if err != nil {
		panic(err)
	}
	for _, member := range members {
		if !strings.HasPrefix(member, sceneMemberFillerPrefix) {
			continue
		}
		if _, err := mr.SRem(key, member); err != nil {
			panic(err)
		}
		if _, err := mr.SetAdd(key, strconv.FormatUint(player, 10)); err != nil {
			panic(err)
		}
		return
	}
}

// adoptSeedMemberVia 与 adoptMember 相同,给只拿着 svcCtx 的用例用。
func adoptSeedMemberVia(sc *svc.ServiceContext, key string, player uint64) {
	members, err := sc.Redis.Smembers(key)
	if err != nil {
		panic(err)
	}
	for _, member := range members {
		if !strings.HasPrefix(member, sceneMemberFillerPrefix) {
			continue
		}
		if _, err := sc.Redis.Srem(key, member); err != nil {
			panic(err)
		}
		if _, err := sc.Redis.Sadd(key, strconv.FormatUint(player, 10)); err != nil {
			panic(err)
		}
		return
	}
}

func adoptSceneMember(mr *miniredis.Miniredis, sceneID, player uint64) {
	adoptMember(mr, instanceMembersKey(sceneID), player)
}

func adoptNodeMember(mr *miniredis.Miniredis, zoneID uint32, nodeID string, player uint64) {
	adoptMember(mr, nodeMembersKey(zoneID, nodeID), player)
}

func memberCountText(mr *miniredis.Miniredis, key string) string {
	members, err := mr.SMembers(key)
	if errors.Is(err, miniredis.ErrKeyNotFound) {
		return "0"
	}
	if err != nil {
		panic(err)
	}
	return strconv.Itoa(len(members))
}

// sceneMemberCount / nodeMemberCount 直接读 miniredis(不经钩子),返回人数的十进制文本,键不存在是 "0"。
func sceneMemberCount(mr *miniredis.Miniredis, sceneID uint64) string {
	return memberCountText(mr, instanceMembersKey(sceneID))
}

func nodeMemberCount(mr *miniredis.Miniredis, zoneID uint32, nodeID string) string {
	return memberCountText(mr, nodeMembersKey(zoneID, nodeID))
}

// sceneCountText / nodeCountText 与上面相同,经业务句柄读。
func sceneCountText(sc *svc.ServiceContext, sceneID uint64) string {
	n, err := sc.Redis.Scard(instanceMembersKey(sceneID))
	if err != nil {
		panic(err)
	}
	return strconv.FormatInt(n, 10)
}

func nodeCountText(sc *svc.ServiceContext, zoneID uint32, nodeID string) string {
	n, err := sc.Redis.Scard(nodeMembersKey(zoneID, nodeID))
	if err != nil {
		panic(err)
	}
	return strconv.FormatInt(n, 10)
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

// 增减幂等:重复 SADD 不多计,重复 SREM 不多减,这是离场两步能分开的前提。
func TestSceneMembers_AddAndRemoveAreIdempotent(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	const sceneID, a, b = uint64(42), uint64(1001), uint64(1002)

	require.NoError(t, addSceneMember(sc, a, sceneID, testZoneId, "node-A"))
	require.NoError(t, addSceneMember(sc, a, sceneID, testZoneId, "node-A"))
	require.NoError(t, addSceneMember(sc, b, sceneID, testZoneId, "node-A"))
	assert.Equal(t, "2", sceneCountText(sc, sceneID))
	assert.Equal(t, "2", nodeCountText(sc, testZoneId, "node-A"))

	require.NoError(t, removeSceneMember(sc, a, sceneID, testZoneId, "node-A"))
	require.NoError(t, removeSceneMember(sc, a, sceneID, testZoneId, "node-A"))
	assert.Equal(t, "1", sceneCountText(sc, sceneID))
	assert.Equal(t, "1", nodeCountText(sc, testZoneId, "node-A"))
}

// 从没加入过就摘:不出现负数,也不建键(计数器时代 DECR 会把键建出来,要钳 0)。
func TestSceneMembers_RemoveWithoutAddCreatesNothing(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-1")
	const sceneID, player = uint64(999), uint64(1001)

	require.NoError(t, removeSceneMember(sc, player, sceneID, testZoneId, "node-A"))

	assert.False(t, mr.Exists(instanceMembersKey(sceneID)))
	assert.False(t, mr.Exists(nodeMembersKey(testZoneId, "node-A")))
}

// 节点未知时只动场景集合:zone 为 0 或节点为空都不造出 node:zone:0:... 这样的键。
func TestSceneMembers_NodeSkippedWhenZoneOrNodeUnknown(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-1")
	const sceneID, player = uint64(43), uint64(1001)

	require.NoError(t, addSceneMember(sc, player, sceneID, 0, "node-A"))
	require.NoError(t, addSceneMember(sc, player, sceneID, testZoneId, ""))

	assert.Equal(t, "1", sceneCountText(sc, sceneID))
	assert.False(t, mr.Exists(nodeMembersKey(0, "node-A")))
	assert.False(t, mr.Exists(nodeMembersKey(testZoneId, "")))
}

// location 指向的场景或节点不摘。同节点换图摘旧场景时节点集合靠它保住,人数也不下探。
func TestRemoveSceneMember_KeepsWhatLocationStillPointsAt(t *testing.T) {
	const (
		playerID = uint64(1101)
		located  = uint64(7701)
		other    = uint64(7702)
	)
	seed := func(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
		t.Helper()
		sc, mr := newTestSvcCtx(t, "node-1")
		raw, err := gproto.Marshal(&scene_manager.PlayerLocation{SceneId: located, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1})
		require.NoError(t, err)
		require.NoError(t, mr.Set(getPlayerLocationKey(playerID), string(raw)))
		seedSceneMembers(mr, located, 2, playerID)
		seedSceneMembers(mr, other, 2, playerID)
		seedNodeMembers(mr, testZoneId, "10", 2, playerID)
		seedNodeMembers(mr, testZoneId, "20", 2, playerID)
		return sc, mr
	}

	t.Run("同节点换图摘旧场景:旧场景少一人,节点集合不变", func(t *testing.T) {
		sc, mr := seed(t)
		require.NoError(t, removeSceneMember(sc, playerID, other, testZoneId, "10"))
		assert.Equal(t, "1", sceneMemberCount(mr, other))
		assert.Equal(t, "2", nodeMemberCount(mr, testZoneId, "10"))
	})
	t.Run("同场景换节点摘旧节点:场景集合不变,旧节点少一人", func(t *testing.T) {
		sc, mr := seed(t)
		require.NoError(t, removeSceneMember(sc, playerID, located, testZoneId, "20"))
		assert.Equal(t, "2", sceneMemberCount(mr, located))
		assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "20"))
	})
	t.Run("都不匹配:两边各少一人", func(t *testing.T) {
		sc, mr := seed(t)
		require.NoError(t, removeSceneMember(sc, playerID, other, testZoneId, "20"))
		assert.Equal(t, "1", sceneMemberCount(mr, other))
		assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "20"))
	})
	t.Run("都匹配:成员身份都保住", func(t *testing.T) {
		sc, mr := seed(t)
		require.NoError(t, removeSceneMember(sc, playerID, located, testZoneId, "10"))
		assert.Equal(t, "2", sceneMemberCount(mr, located))
		assert.Equal(t, "2", nodeMemberCount(mr, testZoneId, "10"))
	})
	t.Run("location 记的 zone 不同:节点号相同也是两个进程,不加回", func(t *testing.T) {
		sc, mr := seed(t)
		seedNodeMembers(mr, 77, "10", 2, playerID)
		require.NoError(t, removeSceneMember(sc, playerID, other, 77, "10"))
		assert.Equal(t, "1", nodeMemberCount(mr, 77, "10"))
	})
	t.Run("location 已不在:只摘不加", func(t *testing.T) {
		sc, mr := seed(t)
		mr.Del(getPlayerLocationKey(playerID))
		require.NoError(t, removeSceneMember(sc, playerID, located, testZoneId, "10"))
		assert.Equal(t, "1", sceneMemberCount(mr, located))
		assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))
	})
}

// 预占碰巧选中玩家已在的频道,而这次进场被拒:释放预占不得把玩家移出他所在的频道。计数器做 +1 -1 净 0,集合靠 location 保住。
func TestEnterScene_ReleasingReservationKeepsExistingMember(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{}
	const (
		playerID = uint64(1102)
		channel  = uint64(7703)
		confID   = uint64(3703)
	)
	// 频道属主从 10 改到了 20(10 还活着、没写交接标记),玩家的 location 还记着 10:自动选线选中同一个频道,
	// 但这是跨节点落点,换手门回 18,预占要释放。
	mr.SAdd(worldChannelsKey(testZoneId, confID), strconv.FormatUint(channel, 10))
	seedSceneOnNode(mr, testZoneId, channel, "20", "2")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	seedNodeMembers(mr, testZoneId, "10", 1)
	seedNodeMembers(mr, testZoneId, "20", 3)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, channel, "10", testZoneId))
	adoptSceneMember(mr, channel, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneConfId: confID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "前提:跨节点落点没有标记,换手门回 18")

	assert.Equal(t, "2", sceneMemberCount(mr, channel), "玩家还在这个频道里")
	members, err := mr.SIsMember(instanceMembersKey(channel), strconv.FormatUint(playerID, 10))
	require.NoError(t, err)
	assert.True(t, members)
	assert.Equal(t, "3", nodeMemberCount(mr, testZoneId, "20"), "新节点的预占退回")
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"), "旧节点不动")
}

// 同节点换图:节点集合一直有这名玩家,旧场景少一人,新场景多一人。
func TestEnterScene_SameNodeSceneSwitchKeepsNodeMember(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{}
	const (
		playerID = uint64(1103)
		oldScene = uint64(7704)
		newScene = uint64(7705)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "2")
	seedSceneOnNode(mr, testZoneId, newScene, "10", "0")
	seedNodeMembers(mr, testZoneId, "10", 2)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	adoptSceneMember(mr, oldScene, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: newScene, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Zero(t, resp.ErrorCode)

	assert.Equal(t, "1", sceneMemberCount(mr, oldScene))
	assert.Equal(t, "1", sceneMemberCount(mr, newScene))
	assert.Equal(t, "2", nodeMemberCount(mr, testZoneId, "10"), "节点集合不因同节点换图变化")
}

// 离场两步之间失败:第一步已删 location、第二步没摘成。player_locator 重试时 location 已不在,重试照样把人摘掉,
// 实例人数不会永久多计(设计 §5.10 的核心场景)。
func TestLeaveScene_RetryDrivesMemberRemovalAfterLocationGone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID = uint64(1104)
		sceneID  = uint64(7706)
	)
	seedSceneOnNode(mr, testZoneId, sceneID, "10", "1")
	seedNodeMembers(mr, testZoneId, "10", 1)
	adoptSceneMember(mr, sceneID, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)
	// 第一步已经做完的状态:location 不在,成员还在。
	require.False(t, mr.Exists(getPlayerLocationKey(playerID)))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{PlayerId: playerID, SceneId: sceneID})
	require.NoError(t, err)
	assert.Equal(t, "0", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "0", nodeMemberCount(mr, testZoneId, "10"))

	// 再来一次:幂等,不报错。
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{PlayerId: playerID, SceneId: sceneID})
	require.NoError(t, err)
	assert.Equal(t, "0", sceneMemberCount(mr, sceneID))
}

// SREM 之前并发进场已落到这里:第一次读 location 还不指向这里,摘完再读就指向了,加回去。
func TestRemoveSceneMember_AddsBackWhenPlacementLandsBeforeRemoval(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-1")
	const (
		playerID = uint64(1109)
		sceneID  = uint64(7713)
	)
	seedSceneMembers(mr, sceneID, 2, playerID)
	seedNodeMembers(mr, testZoneId, "10", 2, playerID)
	raw, err := gproto.Marshal(&scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1})
	require.NoError(t, err)
	locKey := getPlayerLocationKey(playerID)
	// 钩子只在第一次 SREM 时写 location,等同并发进场插在第一次读之后、SREM 之前。Lua 里的 redis.call 带着 miniredis 的锁
	// 重入钩子,所以挂在外层的 EVAL 上,先消费一次性标志再写。
	var fired atomic.Bool
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if (cmd != "EVAL" && cmd != "EVALSHA") || len(args) == 0 || args[0] != luaRemoveSceneMember || !fired.CompareAndSwap(false, true) {
			return false
		}
		_ = mr.Set(locKey, string(raw))
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	require.NoError(t, removeSceneMember(sc, playerID, sceneID, testZoneId, "10"))

	require.True(t, fired.Load(), "钩子没触发,用例空转")
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "2", nodeMemberCount(mr, testZoneId, "10"))
	inScene, err := mr.SIsMember(instanceMembersKey(sceneID), strconv.FormatUint(playerID, 10))
	require.NoError(t, err)
	assert.True(t, inScene)
}

// 第一步之后玩家已经进了别的场景,旧离场的重试才到:旧场景的成员照样摘,新场景不动。
func TestLeaveScene_RetryAfterReenteringOtherSceneRemovesOnlyOldMember(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID = uint64(1105)
		oldScene = uint64(7707)
		newScene = uint64(7708)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "2")
	seedSceneOnNode(mr, testZoneId, newScene, "20", "1")
	adoptSceneMember(mr, oldScene, playerID)
	adoptSceneMember(mr, newScene, playerID)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, newScene, "20", testZoneId))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{PlayerId: playerID, SceneId: oldScene})
	require.NoError(t, err)

	assert.Equal(t, "1", sceneMemberCount(mr, oldScene))
	assert.Equal(t, "1", sceneMemberCount(mr, newScene))
	require.NotNil(t, identLocation(t, sc, playerID), "新落点原样保留")
}

// 第二步失败要报错,让 player_locator 保留清理任务重试;location 已删,下一次调用只补第二步。
func TestLeaveScene_MemberRemovalFailureIsReturnedForRetry(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(1106)
		sceneID  = uint64(7709)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100})
	seedLeaveCasFixture(t, mr, playerID, sceneID, raw, "1")
	// 成员集合键被写成 string:第二步的 SREM 回 WRONGTYPE,第一步不受影响。
	mr.Del(instanceMembersKey(sceneID))
	require.NoError(t, mr.Set(instanceMembersKey(sceneID), "corrupt"))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{PlayerId: playerID, SceneId: sceneID})
	require.Error(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)), "第一步已提交")

	mr.Del(instanceMembersKey(sceneID))
	seedSceneMembers(mr, sceneID, 2, playerID)
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{PlayerId: playerID, SceneId: sceneID})
	require.NoError(t, err)
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID), "重试把第二步补上")
}

// 预占脚本按 SCARD 选人最少的频道,同一玩家重复预占不累加。
func TestReserveBestWorldChannel_PicksLeastMembersAndIsIdempotent(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-1")
	const a, b, player = uint64(7710), uint64(7711), uint64(1107)
	mr.Set(sceneNodeKey(a), "10")
	mr.Set(sceneNodeKey(b), "20")
	seedSceneMembers(mr, a, 3)
	seedSceneMembers(mr, b, 1)
	candidates := []WorldChannelCandidate{{SceneID: a, NodeID: "10", ZoneID: testZoneId}, {SceneID: b, NodeID: "20", ZoneID: testZoneId}}

	sceneID, nodeID, count, err := ReserveBestWorldChannel(sc, candidates, player)
	require.NoError(t, err)
	assert.Equal(t, b, sceneID)
	assert.Equal(t, "20", nodeID)
	assert.EqualValues(t, 2, count)

	sceneID, _, count, err = ReserveBestWorldChannel(sc, candidates, player)
	require.NoError(t, err)
	assert.Equal(t, b, sceneID)
	assert.EqualValues(t, 2, count, "同一玩家重复预占不累加")
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "20"))
	assert.Equal(t, "0", nodeMemberCount(mr, testZoneId, "10"))
}

// 属主已变的候选不参与选择;全部陈旧时什么都不写。
func TestReserveBestWorldChannel_SkipsStaleCandidates(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-1")
	const a, player = uint64(7712), uint64(1108)
	mr.Set(sceneNodeKey(a), "20")

	sceneID, nodeID, _, err := ReserveBestWorldChannel(sc, []WorldChannelCandidate{{SceneID: a, NodeID: "10", ZoneID: testZoneId}}, player)
	require.NoError(t, err)
	assert.Zero(t, sceneID)
	assert.Empty(t, nodeID)
	assert.False(t, mr.Exists(instanceMembersKey(a)))
	assert.False(t, mr.Exists(nodeMembersKey(testZoneId, "10")))
}
