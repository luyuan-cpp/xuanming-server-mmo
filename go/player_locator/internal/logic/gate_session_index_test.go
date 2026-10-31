package logic

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	"shared/redisslot"
)

// 写路径行为测试:只经公开的 Logic RPC 驱动,断言 gate 会话索引随会话写入原子维护,
// 即不变量 I1「会话 ONLINE 且 gate_instance_id 非空 ⇔ player_id 是它的 slot + 它的 gate 这把索引的成员」。
//
// 索引键形用字面量的正则钉住,不引用实现里的常量:这是跨版本契约,清库脚本、运维命令也按这个形状写。
// 确定性序列测试要驱动对账,放在 session_reconciler_test.go。

const (
	idxGateA = "inst-gate-a"
	idxGateB = "inst-gate-b"
	idxGateC = "inst-gate-c"
)

var (
	gateIndexKeyShape    = regexp.MustCompile(`^player:gate_index:\{[a-z]+\}:sessions:(.*)$`)
	gateRegistryKeyShape = regexp.MustCompile(`^player:gate_index:\{[a-z]+\}:gates$`)
)

// indexTestSession 构造一条 ONLINE 会话。SceneId 为 0 且不种场景位置,MarkOffline 的 LeaveScene
// 快路径因此直接返回(没有权威位置就不需要 SceneManager 客户端),测试只看会话与索引。
func indexTestSession(pid uint64, sessionID uint32, gate string, version uint32) *pb.PlayerSession {
	return &pb.PlayerSession{
		PlayerId:       pid,
		SessionId:      sessionID,
		GateId:         "1",
		GateInstanceId: gate,
		SessionVersion: version,
		State:          pb.PlayerSessionState_SESSION_STATE_ONLINE,
		Account:        "acc",
		RequestId:      fmt.Sprintf("req-%d-%d-%d", pid, sessionID, version),
	}
}

func callSetSession(sc *svc.ServiceContext, s *pb.PlayerSession) error {
	_, err := NewSetSessionLogic(context.Background(), sc).SetSession(&pb.SetSessionRequest{Session: s})
	return err
}

// indexMembers 返回 gate 在全部 slot 上的索引成员(排序后);一把索引键都没有时为 nil。
func indexMembers(t *testing.T, mr *miniredis.Miniredis, gate string) []string {
	t.Helper()
	var out []string
	for _, key := range mr.Keys() {
		m := gateIndexKeyShape.FindStringSubmatch(key)
		if m == nil || m[1] != gate {
			continue
		}
		members, err := mr.Members(key)
		require.NoError(t, err)
		out = append(out, members...)
	}
	sort.Strings(out)
	return out
}

// registryMembers 返回全部 slot 的登记集里出现过的 gate(去重、排序后);一个都没有时为 nil。
func registryMembers(t *testing.T, mr *miniredis.Miniredis) []string {
	t.Helper()
	seen := map[string]struct{}{}
	for _, key := range mr.Keys() {
		if !gateRegistryKeyShape.MatchString(key) {
			continue
		}
		members, err := mr.Members(key)
		require.NoError(t, err)
		for _, member := range members {
			seen[member] = struct{}{}
		}
	}
	var out []string
	for member := range seen {
		out = append(out, member)
	}
	sort.Strings(out)
	return out
}

func TestGateIndex_FirstLoginIndexesOnlineSession(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(7001, 100, idxGateA, 1)))

	assert.Equal(t, []string{"7001"}, indexMembers(t, mr, idxGateA))
	assert.Equal(t, []string{idxGateA}, registryMembers(t, mr))
	// 索引与登记集按玩家的 slot 分桶,和会话键同槽。
	slot := redisslot.PlayerSlot(7001)
	assert.True(t, mr.Exists(gateSessionIndexKey(slot, idxGateA)))
	assert.True(t, mr.Exists(gateRegistryKey(slot)))
}

func TestGateIndex_ReplaceLoginMovesMemberAcrossGates(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(7002, 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(7002, 200, idxGateB, 2)))

	assert.Empty(t, indexMembers(t, mr, idxGateA), "顶号换 gate 后旧 gate 的索引里不得残留")
	assert.Equal(t, []string{"7002"}, indexMembers(t, mr, idxGateB))
	assert.Equal(t, []string{idxGateB}, registryMembers(t, mr), "这个 slot 上 A 的索引空了,A 随之移出登记集")
}

// 同一 gate 上顶号:旧 gate == 新 gate。钉住脚本里「先 SADD 后 SREM」这种顺序写错会把成员删掉。
func TestGateIndex_ReplaceLoginOnSameGateKeepsMember(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(7003, 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(7003, 200, idxGateA, 2)))

	assert.Equal(t, []string{"7003"}, indexMembers(t, mr, idxGateA))
	assert.Equal(t, []string{idxGateA}, registryMembers(t, mr))
}

func TestGateIndex_SetDisconnectingRemovesMember(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(7004, 100, idxGateA, 1)))

	_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: 7004, SessionId: 100, GateInstanceId: idxGateA,
	})
	require.NoError(t, err)

	assert.Empty(t, indexMembers(t, mr, idxGateA))
	assert.Empty(t, registryMembers(t, mr), "这把索引空了,gate 移出本 slot 的登记集")
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 7004).State)
	assert.Contains(t, leaseMembers(t, mr), "7004", "原有行为:断线进租约 ZSET")
}

// 同 slot 同 gate 的另一名玩家还在:这把索引没空,gate 留在登记集里(I2)。
func TestGateIndex_RegistryKeepsGateWhileIndexNotEmpty(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	pids := pidsInSameSlot(7011, 2)
	require.NoError(t, callSetSession(sc, indexTestSession(pids[0], 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(pids[1], 100, idxGateA, 1)))

	_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: pids[0], SessionId: 100, GateInstanceId: idxGateA,
	})
	require.NoError(t, err)

	assert.Equal(t, []string{fmt.Sprint(pids[1])}, indexMembers(t, mr, idxGateA))
	assert.Equal(t, []string{idxGateA}, registryMembers(t, mr))
}

// 不同 slot 的玩家在同一个 gate 上:各进各的索引键与登记集,互不牵连。
func TestGateIndex_PlayersOnDifferentSlotsUseSeparateKeys(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const first, second = uint64(21001), uint64(21002)
	require.NotEqual(t, redisslot.PlayerSlot(first), redisslot.PlayerSlot(second))
	require.NoError(t, callSetSession(sc, indexTestSession(first, 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(second, 100, idxGateA, 1)))

	assert.Equal(t, []string{"21001", "21002"}, indexMembers(t, mr, idxGateA))
	_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: first, SessionId: 100, GateInstanceId: idxGateA,
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"21002"}, indexMembers(t, mr, idxGateA))
	assert.False(t, mr.Exists(gateRegistryKey(redisslot.PlayerSlot(first))))
	assert.True(t, mr.Exists(gateRegistryKey(redisslot.PlayerSlot(second))))
	assert.Equal(t, []string{"21001"}, leaseMembers(t, mr))
	assert.True(t, mr.Exists(leaseKeysForPlayer(first).ready))
	assert.False(t, mr.Exists(leaseKeysForPlayer(second).ready), "租约只进断线玩家自己的 slot")
}

func TestGateIndex_ReconnectIndexesNewGate(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	require.NoError(t, callSetSession(sc, indexTestSession(7005, 100, idxGateA, 1)))
	_, err := NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{PlayerId: 7005, SessionId: 100})
	require.NoError(t, err)

	resp, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId: 7005, NewSessionId: 200, GateId: "2", GateInstanceId: idxGateB, RequestId: "rc-1",
	})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.ErrorMessage)

	assert.Empty(t, indexMembers(t, mr, idxGateA))
	assert.Equal(t, []string{"7005"}, indexMembers(t, mr, idxGateB))
}

// gate_instance_id 为空(旧版 login 或早期记录)的会话不进任何索引;「这一侧没有索引」的哨兵在 ARGV 里,
// KEYS 里没有空串,不会落成名为 "" 的键,占位用的登记集也不会被写。
func TestGateIndex_EmptyGateInstanceNotIndexed(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(7006, 100, "", 1)))
	_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{PlayerId: 7006, SessionId: 100})
	require.NoError(t, err)

	assert.Empty(t, registryMembers(t, mr))
	assert.False(t, mr.Exists(gateRegistryKey(redisslot.PlayerSlot(7006))), "占位的登记集键不得被写出来")
	for _, key := range mr.Keys() {
		assert.False(t, gateIndexKeyShape.MatchString(key), "空 gate 不得建出任何索引键:%s", key)
	}
	assert.False(t, mr.Exists(""), "不得落成名为 \"\" 的键")
}

func TestGateIndex_StaleVersionLeavesIndexUntouched(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(7007, 100, idxGateA, 1)))

	// 版本没有恰好 +1:SetSession 判 stale 拒绝,索引一个字节都不动。
	require.Error(t, callSetSession(sc, indexTestSession(7007, 200, idxGateB, 5)))

	assert.Equal(t, []string{"7007"}, indexMembers(t, mr, idxGateA))
	assert.Nil(t, indexMembers(t, mr, idxGateB))
	assert.Equal(t, []string{idxGateA}, registryMembers(t, mr))
}

func TestGateIndex_ReplayIsIdempotent(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	s := indexTestSession(7008, 100, idxGateA, 1)
	require.NoError(t, callSetSession(sc, s))
	require.NoError(t, callSetSession(sc, s), "RPC 响应丢失后的同请求重放")

	assert.Equal(t, []string{"7008"}, indexMembers(t, mr, idxGateA))
}

// missing 模式下旧清理仍在 processing:SetSession fail-closed 返回 ErrSessionCleanupPending,不得建任何索引。
func TestGateIndex_CleanupPendingDoesNotIndex(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	_, err := mr.ZAdd(leaseKeysForPlayer(7009).processing, 9999999999, "7009")
	require.NoError(t, err)

	err = callSetSession(sc, indexTestSession(7009, 100, idxGateA, 1))
	require.ErrorIs(t, err, ErrSessionCleanupPending)

	assert.Nil(t, indexMembers(t, mr, idxGateA))
	assert.Empty(t, registryMembers(t, mr))
}

func TestGateIndex_MarkOfflineRemovesMember(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	require.NoError(t, callSetSession(sc, indexTestSession(7010, 100, idxGateA, 1)))

	// 带陈旧 expected 的 MarkOffline 是 no-op:会话与成员都保留。
	_, err := NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: 7010, ExpectedSessionId: 100, ExpectedSessionVersion: 9,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"7010"}, indexMembers(t, mr, idxGateA))

	_, err = NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: 7010, ExpectedSessionId: 100, ExpectedSessionVersion: 1, ExpectedGateInstanceId: idxGateA,
	})
	require.NoError(t, err)
	assert.False(t, mr.Exists(sessionKey(7010)))
	assert.Empty(t, indexMembers(t, mr, idxGateA))
	assert.Empty(t, registryMembers(t, mr), "这把索引空了,gate 移出本 slot 的登记集")
}
