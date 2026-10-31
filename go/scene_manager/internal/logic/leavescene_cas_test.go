package logic

// GO-4 残余:LeaveScene 改成「比对原文」的条件删除(leavescenelogic.go luaDeletePlayerLocationIfUnchanged),
// 摘成员是删除之后的第二步(scene_membership.go,设计 §5.10)。

import (
	"context"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"shared/ownerepoch"
)

// leaveCasLocationRaw 用与生产相同的编码(proto.Marshal)构造 location 原文。
func leaveCasLocationRaw(t *testing.T, loc *scene_manager.PlayerLocation) string {
	t.Helper()
	b, err := gproto.Marshal(loc)
	require.NoError(t, err)
	return string(b)
}

// leaveCasValue 直接读 miniredis(不经钩子);键不存在读作 ""。
func leaveCasValue(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	if !mr.Exists(key) {
		return ""
	}
	v, err := mr.Get(key)
	require.NoError(t, err)
	return v
}

// seedLeaveCasFixture:location 写 raw,场景与节点 10 的成员集合都铺成 count 人(count 非 0 时含这名玩家),
// scene:{id}:node 指向 "10"。
//
// raw 能解析且记着非 0 的 owner_epoch 时,另外摆上当前代的释放证据(owner_epoch 键 + handoff 标记 "E:100"):
// LeaveScene 删 location 之前要求持有者已释放(docs/design/location-release-binding.md §3.2),而用这个夹具的用例
// 测的是删除机制与离场身份本身,不是释放判定。要测释放判定的用例(leave_release_test.go)自己撤掉这两把键。
func seedLeaveCasFixture(t *testing.T, mr *miniredis.Miniredis, playerID, sceneID uint64, raw, count string) {
	t.Helper()
	require.NoError(t, mr.Set(getPlayerLocationKey(playerID), raw))
	var members []uint64
	if n := mustAtoi(count); n > 0 {
		members = append(members, playerID)
	}
	seedSceneMembers(mr, sceneID, mustAtoi(count), members...)
	seedNodeMembers(mr, testZoneId, "10", mustAtoi(count), members...)
	require.NoError(t, mr.Set(sceneNodeKey(sceneID), "10"))
	loc := &scene_manager.PlayerLocation{}
	if err := gproto.Unmarshal([]byte(raw), loc); err == nil && loc.GetOwnerEpoch() != 0 {
		epoch := strconv.FormatUint(loc.GetOwnerEpoch(), 10)
		require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), epoch))
		require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), epoch+":100"))
	}
}

func TestDeletePlayerLocationIfUnchanged_Outcomes(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8101)
		sceneID  = uint64(9201)
	)
	l1 := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100})
	l2 := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "20", ZoneId: testZoneId, OwnerEpoch: 2, UpdateTime: 200})
	locKey := getPlayerLocationKey(playerID)
	seedLeaveCasFixture(t, mr, playerID, sceneID, l1, "2")

	outcome, err := deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeDeleted, outcome)
	assert.False(t, mr.Exists(locKey))
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))

	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeAlreadyGone, outcome)
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID), "成员已摘,再来一次不多减")
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))

	require.NoError(t, mr.Set(locKey, l2))
	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeSuperseded, outcome)
	assert.Equal(t, l2, leaveCasValue(t, mr, locKey), "同一场景里换了节点 / 铸了新 epoch 的新落点必须原样保留")
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))

	// scene:{id}:node 读不到:只摘场景成员,节点集合键不传给脚本,也就不会被建出来。
	mr.Del(sceneNodeKey(sceneID))
	mr.Del(nodeMembersKey(testZoneId, "10"))
	seedSceneMembers(mr, sceneID, 2, playerID)
	require.NoError(t, mr.Set(locKey, l1))
	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeDeleted, outcome)
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
	assert.False(t, mr.Exists(nodeMembersKey(testZoneId, "10")), "节点集合键不得被新建")
}

// go-redis 读超时后会把同一条 EVAL 原样重发:首发已删,重发只看到「键已不在」;SREM 幂等,人数只少一个。
func TestDeletePlayerLocationIfUnchanged_ReplayRemovesMemberOnce(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8102)
		sceneID  = uint64(9202)
	)
	l1 := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100})
	seedLeaveCasFixture(t, mr, playerID, sceneID, l1, "2")

	outcome, err := deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeDeleted, outcome)
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))

	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeAlreadyGone, outcome)
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID), "重发不得再减")
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))

	// 成员集合已经空了再删一次:SREM 什么都不做,也不把键建出来。
	seedLeaveCasFixture(t, mr, playerID, sceneID, l1, "0")
	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeDeleted, outcome)
	assert.False(t, mr.Exists(instanceMembersKey(sceneID)))
	assert.False(t, mr.Exists(nodeMembersKey(testZoneId, "10")))
}

// GET 与删除之间有并发 EnterScene 写入了同一场景的新落点(换了节点、铸了新 epoch):不得删掉它。
// 成员身份跟着 location 走:场景还是这个场景,成员保住;旧节点 10 不再持有这名玩家,摘掉。
func TestLeaveScene_KeepsLocationRewrittenBetweenReadAndDelete(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8103)
		sceneID  = uint64(9101)
	)
	l1 := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100})
	l2 := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "20", ZoneId: testZoneId, OwnerEpoch: 2, UpdateTime: 200})
	locKey := getPlayerLocationKey(playerID)
	seedLeaveCasFixture(t, mr, playerID, sceneID, l1, "2")

	// 钩子跳过原子快照读取，在第一次写 location 的 EVAL / EVALSHA / DEL 前改写成 L2。
	// 先用 CompareAndSwap 消费「只触发一次」的标志,再 mr.Set:Lua 里的 redis.call('DEL') 会带着
	// miniredis 的锁重入本钩子,那时标志已被消费,走不到 mr.Set(否则死锁)。同一套钩子对旧代码
	// (顶层裸 DEL)也能触发,可以直接拿来红跑。
	var fired atomic.Bool
	var rewriteErr atomic.Value
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		switch cmd {
		case "EVAL", "EVALSHA", "DEL":
		default:
			return false
		}
		if identReadOnlySnapshotEval(cmd, args) || !slices.Contains(args, locKey) || !fired.CompareAndSwap(false, true) {
			return false
		}
		if err := mr.Set(locKey, l2); err != nil {
			rewriteErr.Store(err)
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: playerID,
		SceneId:  sceneID,
	})
	require.NoError(t, err)
	require.True(t, fired.Load(), "钩子没触发,用例空转")
	require.Nil(t, rewriteErr.Load())

	assert.Equal(t, l2, leaveCasValue(t, mr, locKey), "并发写入的新落点必须保留")
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID), "新落点还在这个场景")
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"), "新落点已不在节点 10")
}

// Redis 失败必须回错误(player_locator 据此保留 claim 重试),而不是回成功让 location 永久残留。
// 本用例用 mr.SetError,不能与 SetPreHook 同时用(两者共用同一个钩子槽位)。
func TestLeaveScene_RedisFailureIsReturnedForRetry(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8104)
		sceneID  = uint64(9104)
	)
	l1 := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100})
	seedLeaveCasFixture(t, mr, playerID, sceneID, l1, "2")

	mr.SetError("ERR injected")
	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: playerID,
		SceneId:  sceneID,
	})
	mr.SetError("")
	require.Error(t, err)

	assert.Equal(t, l1, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "2", nodeMemberCount(mr, testZoneId, "10"))
}

// 写坏的 location 不是瞬时故障:回成功(不能变成永远重试的毒 claim),也不删、不动人数。
func TestLeaveScene_CorruptLocationIsNotDeletedAndNotRetried(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8105)
		sceneID  = uint64(9105)
		corrupt  = "not-a-player-location-protobuf"
	)
	seedLeaveCasFixture(t, mr, playerID, sceneID, corrupt, "2")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: playerID,
		SceneId:  sceneID,
	})
	require.NoError(t, err)

	assert.Equal(t, corrupt, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "2", nodeMemberCount(mr, testZoneId, "10"))
}
