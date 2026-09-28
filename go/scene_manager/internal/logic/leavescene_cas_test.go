package logic

// GO-4 残余:LeaveScene 改成「比对原文」的条件删除,删 location 与减人数在同一段 Lua 里提交
// (leavescenelogic.go luaDeletePlayerLocationIfUnchanged)。

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
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

// seedLeaveCasFixture:location 写 raw,场景人数与节点 10 的人数都设为 count,scene:{id}:node 指向 "10"。
func seedLeaveCasFixture(t *testing.T, mr *miniredis.Miniredis, playerID, sceneID uint64, raw, count string) {
	t.Helper()
	require.NoError(t, mr.Set(getPlayerLocationKey(playerID), raw))
	require.NoError(t, mr.Set(fmt.Sprintf(InstancePlayerCountKey, sceneID), count))
	require.NoError(t, mr.Set(nodePlayerCountKey(testZoneId, "10"), count))
	require.NoError(t, mr.Set(sceneNodeKey(sceneID), "10"))
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
	instKey := fmt.Sprintf(InstancePlayerCountKey, sceneID)
	nodeKey := nodePlayerCountKey(testZoneId, "10")
	seedLeaveCasFixture(t, mr, playerID, sceneID, l1, "2")

	outcome, err := deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeDeleted, outcome)
	assert.False(t, mr.Exists(locKey))
	assert.Equal(t, "1", leaveCasValue(t, mr, instKey))
	assert.Equal(t, "1", leaveCasValue(t, mr, nodeKey))

	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeAlreadyGone, outcome)
	assert.Equal(t, "1", leaveCasValue(t, mr, instKey), "键已不在:不是本次删的,不再减")
	assert.Equal(t, "1", leaveCasValue(t, mr, nodeKey))

	require.NoError(t, mr.Set(locKey, l2))
	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeSuperseded, outcome)
	assert.Equal(t, l2, leaveCasValue(t, mr, locKey), "同一场景里换了节点 / 铸了新 epoch 的新落点必须原样保留")
	assert.Equal(t, "1", leaveCasValue(t, mr, instKey))
	assert.Equal(t, "1", leaveCasValue(t, mr, nodeKey))

	// scene:{id}:node 读不到:只减场景计数,节点人数键不传给脚本,也就不会被 DECR 新建出来。
	mr.Del(sceneNodeKey(sceneID))
	mr.Del(nodeKey)
	require.NoError(t, mr.Set(locKey, l1))
	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeDeleted, outcome)
	assert.Equal(t, "0", leaveCasValue(t, mr, instKey))
	assert.False(t, mr.Exists(nodeKey), "节点人数键不得被新建")
}

// go-redis 读超时后会把同一条 EVAL 原样重发:首发已删并减过人数,重发只看到「键已不在」,人数只减一次。
func TestDeletePlayerLocationIfUnchanged_ReplayDecrementsExactlyOnce(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8102)
		sceneID  = uint64(9202)
	)
	l1 := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100})
	instKey := fmt.Sprintf(InstancePlayerCountKey, sceneID)
	nodeKey := nodePlayerCountKey(testZoneId, "10")
	seedLeaveCasFixture(t, mr, playerID, sceneID, l1, "2")

	outcome, err := deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeDeleted, outcome)
	assert.Equal(t, "1", leaveCasValue(t, mr, instKey))
	assert.Equal(t, "1", leaveCasValue(t, mr, nodeKey))

	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeAlreadyGone, outcome)
	assert.Equal(t, "1", leaveCasValue(t, mr, instKey), "重发不得再减")
	assert.Equal(t, "1", leaveCasValue(t, mr, nodeKey))

	// 人数已经是 0 时再删一次:钳在 0,不出现负数(与 DecrInstancePlayerCount 一致)。
	seedLeaveCasFixture(t, mr, playerID, sceneID, l1, "0")
	outcome, err = deletePlayerLocationIfUnchanged(sc, playerID, l1, testZoneId, sceneID)
	require.NoError(t, err)
	assert.Equal(t, leaveOutcomeDeleted, outcome)
	assert.Equal(t, "0", leaveCasValue(t, mr, instKey))
	assert.Equal(t, "0", leaveCasValue(t, mr, nodeKey))
}

// GET 与删除之间有并发 EnterScene 写入了同一场景的新落点(换了节点、铸了新 epoch):不得删掉它。
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

	// 钩子在第一次碰到 location 键的 EVAL / EVALSHA / DEL 时把 location 改写成 L2。
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
		if !slices.Contains(args, locKey) || !fired.CompareAndSwap(false, true) {
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
	assert.Equal(t, "2", leaveCasValue(t, mr, fmt.Sprintf(InstancePlayerCountKey, sceneID)), "没删就不减")
	assert.Equal(t, "2", leaveCasValue(t, mr, nodePlayerCountKey(testZoneId, "10")))
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
	assert.Equal(t, "2", leaveCasValue(t, mr, fmt.Sprintf(InstancePlayerCountKey, sceneID)))
	assert.Equal(t, "2", leaveCasValue(t, mr, nodePlayerCountKey(testZoneId, "10")))
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
	assert.Equal(t, "2", leaveCasValue(t, mr, fmt.Sprintf(InstancePlayerCountKey, sceneID)))
	assert.Equal(t, "2", leaveCasValue(t, mr, nodePlayerCountKey(testZoneId, "10")))
}
