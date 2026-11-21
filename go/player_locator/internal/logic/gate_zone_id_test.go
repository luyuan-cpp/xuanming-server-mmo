package logic

// 会话记录的 gate_zone_id 持久化与覆盖语义(docs/design/login-queue-2026-05.md §8 / §14)。
// gate_zone_id 标识当前连接所在的区(不是角色建角区):SetSession 与 Reconnect 都按本次请求写入,
// 不沿用上一条会话记录的旧区号,避免跨区跳转或旧版 login 写 0 时留下过期区号。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "proto/player_locator"
)

func TestSetSession_PersistsAndReplacesGateZoneID(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 19001

	first := makeTestSession(playerID)
	first.GateZoneId = 7
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: first})
	require.NoError(t, err)
	assert.Equal(t, uint32(7), readSession(t, sc, playerID).GetGateZoneId())

	// 顶号换到另一个区:GateZoneId 必须覆盖为新区号。
	second := makeTestSession(playerID)
	second.SessionId = first.SessionId + 1
	second.SessionVersion = first.SessionVersion + 1
	second.GateZoneId = 8
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: second})
	require.NoError(t, err)
	assert.Equal(t, uint32(8), readSession(t, sc, playerID).GetGateZoneId())

	// 旧版 login 发来 0 时不得偷偷沿用旧区号 8。
	legacy := makeTestSession(playerID)
	legacy.SessionId = second.SessionId + 1
	legacy.SessionVersion = second.SessionVersion + 1
	legacy.GateZoneId = 0
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: legacy})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), readSession(t, sc, playerID).GetGateZoneId())
}

func TestReconnect_UpdatesGateZoneID(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 19002

	initial := makeTestSession(playerID)
	initial.GateZoneId = 7
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: initial})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: initial.SessionId, LeaseTtlSeconds: 30,
	})
	require.NoError(t, err)

	resp, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId:       playerID,
		NewSessionId:   initial.SessionId + 1,
		GateId:         "gate-zone-8",
		GateInstanceId: "inst-8",
		GateZoneId:     8,
		RequestId:      "reconnect-z8",
	})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.ErrorMessage)
	assert.Equal(t, uint32(8), resp.Session.GetGateZoneId())
	assert.Equal(t, uint32(8), readSession(t, sc, playerID).GetGateZoneId())

	// 再断线后由旧版 login(GateZoneId=0)重连:写 0,不沿用 8。
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: initial.SessionId + 1, LeaseTtlSeconds: 30,
	})
	require.NoError(t, err)
	resp, err = NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId:       playerID,
		NewSessionId:   initial.SessionId + 2,
		GateId:         "gate-legacy",
		GateInstanceId: "inst-legacy",
		GateZoneId:     0,
		RequestId:      "reconnect-legacy",
	})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.ErrorMessage)
	assert.Equal(t, uint32(0), resp.Session.GetGateZoneId())
	assert.Equal(t, uint32(0), readSession(t, sc, playerID).GetGateZoneId())
}
