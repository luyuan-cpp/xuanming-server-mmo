package logic

// gate 持有断线通知、login 确认之前原样重发(cpp/nodes/gate/handler/rpc/login_session_renewal.h),login 每收到一次就调一次
// SetDisconnecting,所以同一次断线的 SetDisconnecting 会被重放(上一次其实成了、回包丢了;或同一条通知里另一个玩家失败了)。
// 重放必须是空操作:不改写会话记录(版本不再推进),也不把第一次调用定下的租约到期时刻往后推。

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "proto/player_locator"
)

func TestSetDisconnecting_ReplayKeepsTheFirstLeaseDeadline(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID = uint64(4101)
	session := makeTestSession(playerID)
	require.NoError(t, callSetSession(sc, session))

	first := &pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 10, GateInstanceId: session.GateInstanceId,
	}
	_, err := NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(first)
	require.NoError(t, err)
	committed := readSessionBytes(t, sc, playerID)
	member := strconv.FormatUint(playerID, 10)
	deadline, err := sc.PlayerRedis.ZScore(ctx, leaseKeysForPlayer(playerID).ready, member).Result()
	require.NoError(t, err)

	// 重放时给一个长得多的租约:若重放会重新挂租约,到期时刻必然变大,不受两次调用是否落在同一秒影响。
	replay := &pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 3600, GateInstanceId: session.GateInstanceId,
	}
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(replay)
	require.NoError(t, err)

	assert.Equal(t, committed, readSessionBytes(t, sc, playerID), "重放不得改写会话记录")
	again, err := sc.PlayerRedis.ZScore(ctx, leaseKeysForPlayer(playerID).ready, member).Result()
	require.NoError(t, err)
	assert.Equal(t, deadline, again, "重放不得延长断线租约")
}

// ── 按会话代次认断线(docs/design/gate-online-identity.md §10.4「断线通知」) ─────────────────────────────
//
// 重发让旧连接的断线可以迟到任意久。同一 gate 进程里会话号每 131072 次建连回绕一圈,只比会话号与 gate 实例时,
// 玩家恰好在同号新连接上重新登录、旧连接的通知这时才送达,就会把新会话转成 DISCONNECTING,30 秒后被踢。
// 规则同实例号:请求与记录的代次都非 0 且不等即忽略、回成功;任一方为 0 不比较。

func TestSetDisconnecting_StaleSessionIncarnationIgnored(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const playerID = uint64(4102)
	old := makeTestSession(playerID)
	old.SessionIncarnation = 1
	require.NoError(t, callSetSession(sc, old))
	// 顶号重登落在同号新连接上:会话号、gate 实例都与旧连接相同,只有代次不同。
	replaced := makeTestSession(playerID)
	replaced.SessionVersion = old.SessionVersion + 1
	replaced.SessionIncarnation = 2
	require.NoError(t, callSetSession(sc, replaced))
	require.Equal(t, uint64(2), readSession(t, sc, playerID).SessionIncarnation, "SetSession 写请求里的代次,不继承旧记录的")
	before := readSessionBytes(t, sc, playerID)

	_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: old.SessionId, LeaseTtlSeconds: 30,
		GateInstanceId: old.GateInstanceId, SessionIncarnation: old.SessionIncarnation,
	})
	require.NoError(t, err, "陈旧断线按空操作回成功,gate 不再重发")

	assert.Equal(t, before, readSessionBytes(t, sc, playerID), "新连接的会话必须逐字节不变(仍 ONLINE、版本不变)")
	assert.NotContains(t, leaseMembers(t, mr), strconv.FormatUint(playerID, 10), "不得给新连接上的会话挂断线租约")
}

func TestSetDisconnecting_SessionIncarnationMatchOrUnknownPlacesLease(t *testing.T) {
	cases := map[string]struct{ record, request uint64 }{
		"same connection":                   {record: 2, request: 2},
		"legacy login sends no incarnation": {record: 2, request: 0},
		"legacy record carries none":        {record: 0, request: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sc, mr := newTestSvcCtx(t)
			const playerID = uint64(4103)
			session := makeTestSession(playerID)
			session.SessionIncarnation = tc.record
			require.NoError(t, callSetSession(sc, session))

			_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{
				PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 30,
				GateInstanceId: session.GateInstanceId, SessionIncarnation: tc.request,
			})
			require.NoError(t, err)

			got := readSession(t, sc, playerID)
			assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, got.State)
			assert.Equal(t, session.SessionVersion+1, got.SessionVersion)
			assert.Contains(t, leaseMembers(t, mr), strconv.FormatUint(playerID, 10), "必须挂上断线租约")
		})
	}
}

// 短线重连必须把记录里的代次换成新连接的:留着旧连接的值,新连接自己的断线会被当成陈旧忽略,玩家永久停在 ONLINE。
func TestReconnect_RecordsTheNewConnectionsIncarnation(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID = uint64(4104)
	session := makeTestSession(playerID)
	session.SessionIncarnation = 1
	require.NoError(t, callSetSession(sc, session))
	disconnect := func(incarnation uint64) {
		t.Helper()
		_, err := NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
			PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 30,
			GateInstanceId: session.GateInstanceId, SessionIncarnation: incarnation,
		})
		require.NoError(t, err)
	}
	disconnect(1)

	// 同一 gate 进程里会话号回绕到同一个号,玩家在这条新连接(代次 2)上重连。
	resp, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId: playerID, NewSessionId: session.SessionId, GateId: session.GateId,
		GateInstanceId: session.GateInstanceId, RequestId: "req-reconnect-4104", SessionIncarnation: 2,
	})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.ErrorMessage)
	require.Equal(t, uint64(2), readSession(t, sc, playerID).SessionIncarnation)

	disconnect(1) // 旧连接那条通知的回包丢了,gate 又发了一次
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, readSession(t, sc, playerID).State,
		"旧连接迟到的断线不得动新连接的会话")
	disconnect(2)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, playerID).State,
		"新连接自己的断线照常转 DISCONNECTING")
}
