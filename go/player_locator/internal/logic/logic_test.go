package logic

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/config"
	"player_locator/internal/svc"
	pb "proto/player_locator"
	smpb "proto/scene_manager"
)

// ---------------------------------------------------------------------------
// Test helper
// ---------------------------------------------------------------------------

func newTestSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	return &svc.ServiceContext{
		Config: config.Config{
			Lease: config.LeaseConf{DefaultTTLSeconds: 30},
		},
		RedisClient: rdb,
	}, mr
}

// ---------------------------------------------------------------------------
// Location tests
// ---------------------------------------------------------------------------

func TestSetAndGetLocation(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	setLogic := NewSetLocationLogic(ctx, sc)
	_, err := setLogic.SetLocation(&pb.PlayerLocation{
		Uid:      1001,
		ServerId: "scene-node-1",
		SceneId:  42,
	})
	require.NoError(t, err)

	getLogic := NewGetLocationLogic(ctx, sc)
	loc, err := getLogic.GetLocation(&pb.PlayerId{Uid: 1001})
	require.NoError(t, err)

	assert.Equal(t, int64(1001), loc.Uid)
	assert.True(t, loc.Online)
	assert.Equal(t, "scene-node-1", loc.ServerId)
	assert.Equal(t, int32(42), loc.SceneId)
	assert.True(t, loc.Ts > 0, "timestamp should be set")
}

func TestGetLocation_NotFound(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	logic := NewGetLocationLogic(ctx, sc)
	loc, err := logic.GetLocation(&pb.PlayerId{Uid: 9999})
	require.NoError(t, err)

	assert.Equal(t, int64(9999), loc.Uid)
	assert.False(t, loc.Online)
}

func TestMarkOffline(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	// Set location first
	setLogic := NewSetLocationLogic(ctx, sc)
	_, err := setLogic.SetLocation(&pb.PlayerLocation{Uid: 2001, ServerId: "node-1"})
	require.NoError(t, err)

	// Verify it exists
	getLogic := NewGetLocationLogic(ctx, sc)
	loc, _ := getLogic.GetLocation(&pb.PlayerId{Uid: 2001})
	assert.True(t, loc.Online)

	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{
		Session: makeTestSession(2001),
	})
	require.NoError(t, err)

	// Mark offline
	offlineLogic := NewMarkOfflineLogic(ctx, sc)
	_, err = offlineLogic.MarkOffline(&pb.MarkOfflineRequest{
		PlayerId:               2001,
		ExpectedSessionId:      100,
		ExpectedSessionVersion: 1,
	})
	require.NoError(t, err)

	// Verify it's gone
	loc, _ = getLogic.GetLocation(&pb.PlayerId{Uid: 2001})
	assert.False(t, loc.Online)
}

func TestMarkOffline_DelayedOldLeaveGameDoesNotDeleteReplacement(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	playerID := uint64(2002)

	_, err := NewSetLocationLogic(ctx, sc).SetLocation(&pb.PlayerLocation{
		Uid:      int64(playerID),
		ServerId: "scene-new",
		SceneId:  88,
	})
	require.NoError(t, err)

	oldSession := makeTestSession(playerID)
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: oldSession})
	require.NoError(t, err)

	newSession := proto.Clone(oldSession).(*pb.PlayerSession)
	newSession.SessionId = 200
	newSession.SessionVersion = 2
	newSession.GateId = "gate-new"
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: newSession})
	require.NoError(t, err)

	_, err = NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId:               playerID,
		ExpectedSessionId:      oldSession.SessionId,
		ExpectedSessionVersion: oldSession.SessionVersion,
	})
	require.NoError(t, err)

	got, err := NewGetSessionLogic(ctx, sc).GetSession(&pb.GetSessionRequest{PlayerId: playerID})
	require.NoError(t, err)
	require.True(t, got.Found)
	assert.Equal(t, uint32(200), got.Session.SessionId)
	assert.Equal(t, uint32(2), got.Session.SessionVersion)

	loc, err := NewGetLocationLogic(ctx, sc).GetLocation(&pb.PlayerId{Uid: int64(playerID)})
	require.NoError(t, err)
	assert.True(t, loc.Online, "旧 LeaveGame 也不能删除新会话对应的位置")
}

// ---------------------------------------------------------------------------
// Session tests
// ---------------------------------------------------------------------------

func makeTestSession(playerID uint64) *pb.PlayerSession {
	return &pb.PlayerSession{
		PlayerId:       playerID,
		SessionId:      100,
		GateId:         "gate-1",
		GateInstanceId: "inst-aaa",
		SceneNodeId:    "scene-1",
		SceneId:        50,
		TokenId:        "tok-abc",
		SessionVersion: 1,
		State:          pb.PlayerSessionState_SESSION_STATE_ONLINE,
		LastActiveTs:   time.Now().UnixMilli(),
		Account:        "test_account",
	}
}

func TestSetAndGetSession(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	session := makeTestSession(3001)

	setLogic := NewSetSessionLogic(ctx, sc)
	_, err := setLogic.SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)

	getLogic := NewGetSessionLogic(ctx, sc)
	resp, err := getLogic.GetSession(&pb.GetSessionRequest{PlayerId: 3001})
	require.NoError(t, err)

	assert.True(t, resp.Found)
	assert.Equal(t, uint64(3001), resp.Session.PlayerId)
	assert.Equal(t, uint32(100), resp.Session.SessionId)
	assert.Equal(t, "gate-1", resp.Session.GateId)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, resp.Session.State)
}

func TestGetSession_NotFound(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	logic := NewGetSessionLogic(ctx, sc)
	resp, err := logic.GetSession(&pb.GetSessionRequest{PlayerId: 9999})
	require.NoError(t, err)

	assert.False(t, resp.Found)
}

func TestSetSession_NilSessionReturnsError(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	logic := NewSetSessionLogic(ctx, sc)
	_, err := logic.SetSession(&pb.SetSessionRequest{Session: nil})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Disconnect → Reconnect lifecycle
// ---------------------------------------------------------------------------

func TestSetDisconnecting_HappyPath(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()

	// Create session
	session := makeTestSession(4001)
	setLogic := NewSetSessionLogic(ctx, sc)
	_, err := setLogic.SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)

	// Disconnect with matching sessionId
	dcLogic := NewSetDisconnectingLogic(ctx, sc)
	_, err = dcLogic.SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId:        4001,
		SessionId:       100,
		LeaseTtlSeconds: 10,
	})
	require.NoError(t, err)

	// Verify session state changed to DISCONNECTING
	getLogic := NewGetSessionLogic(ctx, sc)
	resp, err := getLogic.GetSession(&pb.GetSessionRequest{PlayerId: 4001})
	require.NoError(t, err)
	assert.True(t, resp.Found)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, resp.Session.State)
	assert.Equal(t, uint32(2), resp.Session.SessionVersion)

	// Verify lease ZSET entry exists
	members, _ := mr.ZMembers(LeaseZSetKey)
	assert.Contains(t, members, "4001")
}

func TestSetDisconnecting_SessionMismatch_Ignored(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	// Create session with sessionId=100
	session := makeTestSession(5001)
	setLogic := NewSetSessionLogic(ctx, sc)
	_, err := setLogic.SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)

	// Disconnect with wrong sessionId=999
	dcLogic := NewSetDisconnectingLogic(ctx, sc)
	_, err = dcLogic.SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId:  5001,
		SessionId: 999,
	})
	require.NoError(t, err)

	// Session should still be ONLINE (not changed)
	getLogic := NewGetSessionLogic(ctx, sc)
	resp, _ := getLogic.GetSession(&pb.GetSessionRequest{PlayerId: 5001})
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, resp.Session.State)
}

func TestSetDisconnecting_NoSession_Noop(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	dcLogic := NewSetDisconnectingLogic(ctx, sc)
	_, err := dcLogic.SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId:  8888,
		SessionId: 1,
	})
	assert.NoError(t, err) // should succeed (no-op)
}

func TestReconnect_HappyPath(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()

	// Create session and disconnect
	session := makeTestSession(6001)
	setLogic := NewSetSessionLogic(ctx, sc)
	_, _ = setLogic.SetSession(&pb.SetSessionRequest{Session: session})

	dcLogic := NewSetDisconnectingLogic(ctx, sc)
	_, _ = dcLogic.SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId:        6001,
		SessionId:       100,
		LeaseTtlSeconds: 30,
	})

	// Reconnect
	rcLogic := NewReconnectLogic(ctx, sc)
	resp, err := rcLogic.Reconnect(&pb.ReconnectRequest{
		PlayerId:       6001,
		NewSessionId:   200,
		GateId:         "gate-2",
		GateInstanceId: "inst-bbb",
		TokenId:        "tok-new",
	})
	require.NoError(t, err)

	assert.True(t, resp.Success)
	assert.Equal(t, uint32(200), resp.Session.SessionId)
	assert.Equal(t, "gate-2", resp.Session.GateId)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, resp.Session.State)
	assert.Equal(t, uint32(3), resp.Session.SessionVersion) // disconnect + reconnect each advance once

	// Lease ZSET entry should be removed
	members, _ := mr.ZMembers(LeaseZSetKey)
	assert.NotContains(t, members, "6001")
}

func TestReconnect_NoSession(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()

	logic := NewReconnectLogic(ctx, sc)
	resp, err := logic.Reconnect(&pb.ReconnectRequest{
		PlayerId:     7001,
		NewSessionId: 300,
	})
	require.NoError(t, err)
	assert.False(t, resp.Success)
	assert.Contains(t, resp.ErrorMessage, "no session")
}

// ---------------------------------------------------------------------------
// Full lifecycle: Online → Disconnect → Reconnect → MarkOffline
// ---------------------------------------------------------------------------

func TestFullSessionLifecycle(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()

	playerID := uint64(10001)

	// 1. Set location (player enters game)
	setLocLogic := NewSetLocationLogic(ctx, sc)
	_, err := setLocLogic.SetLocation(&pb.PlayerLocation{
		Uid:      int64(playerID),
		ServerId: "scene-1",
		SceneId:  100,
	})
	require.NoError(t, err)

	// 2. Set session (gate binds session)
	setSessionLogic := NewSetSessionLogic(ctx, sc)
	_, err = setSessionLogic.SetSession(&pb.SetSessionRequest{
		Session: &pb.PlayerSession{
			PlayerId:       playerID,
			SessionId:      500,
			GateId:         "gate-1",
			GateInstanceId: "inst-111",
			SessionVersion: 1,
			State:          pb.PlayerSessionState_SESSION_STATE_ONLINE,
		},
	})
	require.NoError(t, err)

	// 3. Player disconnects
	dcLogic := NewSetDisconnectingLogic(ctx, sc)
	_, err = dcLogic.SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId:        playerID,
		SessionId:       500,
		LeaseTtlSeconds: 30,
	})
	require.NoError(t, err)

	// Verify disconnecting state
	getSession := NewGetSessionLogic(ctx, sc)
	resp, _ := getSession.GetSession(&pb.GetSessionRequest{PlayerId: playerID})
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, resp.Session.State)

	members, _ := mr.ZMembers(LeaseZSetKey)
	assert.Contains(t, members, "10001")

	// 4. Player reconnects
	rcLogic := NewReconnectLogic(ctx, sc)
	rcResp, err := rcLogic.Reconnect(&pb.ReconnectRequest{
		PlayerId:       playerID,
		NewSessionId:   600,
		GateId:         "gate-2",
		GateInstanceId: "inst-222",
	})
	require.NoError(t, err)
	assert.True(t, rcResp.Success)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, rcResp.Session.State)

	// Lease entry should be gone
	members, _ = mr.ZMembers(LeaseZSetKey)
	assert.NotContains(t, members, "10001")

	// 5. Location still exists
	getLoc := NewGetLocationLogic(ctx, sc)
	loc, _ := getLoc.GetLocation(&pb.PlayerId{Uid: int64(playerID)})
	assert.True(t, loc.Online)

	// 6. Player goes offline for real
	offLogic := NewMarkOfflineLogic(ctx, sc)
	_, _ = offLogic.MarkOffline(&pb.MarkOfflineRequest{
		PlayerId:               playerID,
		ExpectedSessionId:      rcResp.Session.SessionId,
		ExpectedSessionVersion: rcResp.Session.SessionVersion,
	})

	loc, _ = getLoc.GetLocation(&pb.PlayerId{Uid: int64(playerID)})
	assert.False(t, loc.Online)
}

// ---------------------------------------------------------------------------
// Key format tests
// ---------------------------------------------------------------------------

func TestKeyFormats(t *testing.T) {
	assert.Equal(t, "player:location:42", locationKey(42))
	assert.Equal(t, "player:session:99", sessionKey(99))
}

// ---------------------------------------------------------------------------
// SetDisconnecting default TTL fallback
// ---------------------------------------------------------------------------

func TestSetDisconnecting_DefaultTTL(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()

	session := makeTestSession(11001)
	setLogic := NewSetSessionLogic(ctx, sc)
	_, _ = setLogic.SetSession(&pb.SetSessionRequest{Session: session})

	// Disconnect with LeaseTtlSeconds=0, should use config default (30s)
	dcLogic := NewSetDisconnectingLogic(ctx, sc)
	_, err := dcLogic.SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId:        11001,
		SessionId:       100,
		LeaseTtlSeconds: 0,
	})
	require.NoError(t, err)

	// 会话载荷没有独立 TTL；只有 monitor 完成外部副作用并 ack 后才显式删除。
	// 这样批量积压或 monitor 停机不会先吃掉 Gate/Scene 通知所需的载荷。
	ttl, err := sc.RedisClient.TTL(ctx, sessionKey(11001)).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), ttl)

	// 租约本身仍按 30s 到期(宽限期只延长载荷寿命,不推迟清理时机)。
	scores, err := mr.ZMembers(LeaseZSetKey)
	require.NoError(t, err)
	require.Contains(t, scores, "11001")

	// Verify session is stored correctly despite TTL
	data, _ := mr.Get(sessionKey(11001))
	s := &pb.PlayerSession{}
	require.NoError(t, proto.Unmarshal([]byte(data), s))
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, s.State)
}

func TestLeaseClaimTimeout_RequeuesWithoutLosingPayload(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	playerID := uint64(12001)

	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: makeTestSession(playerID)})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: 100, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)

	now := time.Now().Add(2 * time.Second)
	claims, err := claimExpiredLeases(ctx, sc, now, 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.NotEmpty(t, claims[0].payload)
	firstToken := claims[0].token

	// 模拟 worker 在处理前崩溃：不 ack，直接越过 processing deadline。
	claims, err = claimExpiredLeases(ctx, sc, now.Add(6*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.NotEqual(t, firstToken, claims[0].token)
	assert.Equal(t, playerID, claims[0].playerID)
	assert.NotEmpty(t, claims[0].payload)

	processing, err := mr.ZMembers(LeaseProcessingZSetKey)
	require.NoError(t, err)
	assert.Contains(t, processing, "12001")
}

func TestLeaseClaimHeartbeat_PreventsPrematureRecovery(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	playerID := uint64(12501)
	base := time.Now()

	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: makeTestSession(playerID)})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: 100, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)
	claims, err := claimExpiredLeases(ctx, sc, base.Add(2*time.Second), 10, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	// 在原 processing deadline 前心跳，把仍属本 token 的 claim 延后 30 秒。
	renewed, err := renewLeaseClaims(ctx, sc, claims, base.Add(50*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, renewed)

	// 已越过原 deadline(base+32s)，但尚未越过心跳后的 deadline，不得被新 worker 回收。
	recovered, err := claimExpiredLeases(ctx, sc, base.Add(33*time.Second), 10, 30*time.Second)
	require.NoError(t, err)
	assert.Empty(t, recovered)

	// 心跳真正失效后仍可恢复，保证 worker 崩溃不会永久卡住任务。
	recovered, err = claimExpiredLeases(ctx, sc, base.Add(51*time.Second), 10, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.NotEqual(t, claims[0].token, recovered[0].token)
}

func TestReconnect_CancelsAlreadyClaimedExpiry(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	playerID := uint64(13001)

	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: makeTestSession(playerID)})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: 100, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)
	claims, err := claimExpiredLeases(ctx, sc, time.Now().Add(2*time.Second), 10, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	reconnected, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId: playerID, NewSessionId: 201, GateId: "gate-2", GateInstanceId: "inst-2",
	})
	require.NoError(t, err)
	require.True(t, reconnected.Success)

	processingCount, err := sc.RedisClient.ZCard(ctx, LeaseProcessingZSetKey).Result()
	require.NoError(t, err)
	assert.Zero(t, processingCount)

	// 旧 worker 即便继续执行，也已经失去 claim token，不能删除新 ONLINE 会话。
	result, err := commitLeaseExpiry(ctx, sc, claims[0])
	require.NoError(t, err)
	assert.Equal(t, 0, result)
	stored, err := NewGetSessionLogic(ctx, sc).GetSession(&pb.GetSessionRequest{PlayerId: playerID})
	require.NoError(t, err)
	require.True(t, stored.Found)
	assert.Equal(t, uint32(201), stored.Session.SessionId)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, stored.Session.State)
}

func TestSetSession_RejectsMissingSessionWhileExpiryCleanupPending(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	playerID := uint64(13501)

	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: makeTestSession(playerID)})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: 100, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)
	claims, err := claimExpiredLeases(ctx, sc, time.Now().Add(2*time.Second), 10, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	// monitor 已获得清理权并删掉旧 session/location，但模拟 Gate 或 Scene 副作用
	// 失败：processing claim 尚未 ack，必须保留以便超时重试。
	result, err := commitLeaseExpiry(ctx, sc, claims[0])
	require.NoError(t, err)
	require.Equal(t, 1, result)
	assert.False(t, mr.Exists(sessionKey(playerID)))

	replacement := makeTestSession(playerID)
	replacement.SessionId = 201
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: replacement})
	require.ErrorIs(t, err, ErrSessionCleanupPending)
	assert.False(t, mr.Exists(sessionKey(playerID)), "cleanup pending 时不得建立新会话")
	processing, err := mr.ZMembers(LeaseProcessingZSetKey)
	require.NoError(t, err)
	assert.Contains(t, processing, "13501", "失败的新登录不得撤销旧清理 claim")

	// 外部副作用完成并 ack 后，新登录才能建立会话。
	require.NoError(t, ackLeaseClaim(ctx, sc, claims[0]))
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: replacement})
	require.NoError(t, err)
	stored, err := NewGetSessionLogic(ctx, sc).GetSession(&pb.GetSessionRequest{PlayerId: playerID})
	require.NoError(t, err)
	require.True(t, stored.Found)
	assert.Equal(t, uint32(201), stored.Session.SessionId)
}

func TestResolveSceneManagerSceneID_FallsBackToAuthorityKey(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 14001
	data, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 9876, NodeId: "scene-9"})
	require.NoError(t, err)
	require.NoError(t, sc.RedisClient.Set(ctx, sceneManagerLocationKey(playerID), data, 0).Err())

	sceneID, err := resolveSceneManagerSceneID(ctx, sc, playerID)
	require.NoError(t, err)
	assert.Equal(t, uint64(9876), sceneID)
}

func TestLeaseExpiry_MissingSceneManagerRetainsReceiptForRetry(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 14501
	base := time.Now()

	session := makeTestSession(playerID)
	// 隔离本用例：Gate 为空表示没有 Gate 副作用，只验证 SceneManager 缺失。
	session.GateId = ""
	// 首次登录链路的 session.SceneId 可能仍为 0，此时必须回查 SceneManager 的
	// 权威位置；有位置却没有 client 仍然不能当作成功。
	session.SceneId = 0
	location, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 50, NodeId: "scene-1"})
	require.NoError(t, err)
	require.NoError(t, sc.RedisClient.Set(ctx, sceneManagerLocationKey(playerID), location, 0).Err())
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)

	claims, err := claimExpiredLeases(ctx, sc, base.Add(2*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	first := claims[0]

	// commit 已取得清理权并删除旧 session；SceneManager client 缺失必须使
	// 外部副作用失败，不能 ACK processing receipt。
	handleLeaseExpiry(ctx, sc, first)
	assert.False(t, mr.Exists(sessionKey(playerID)))
	processing, err := mr.ZMembers(LeaseProcessingZSetKey)
	require.NoError(t, err)
	assert.Contains(t, processing, "14501")
	token, err := sc.RedisClient.HGet(ctx, leaseClaimTokenHashKey, "14501").Result()
	require.NoError(t, err)
	assert.Equal(t, first.token, token)
	payload, err := sc.RedisClient.HGet(ctx, leaseClaimPayloadHashKey, "14501").Bytes()
	require.NoError(t, err)
	assert.Equal(t, first.payload, payload)

	// worker/配置恢复后，deadline 回收应换 token 并带回同一份 payload，
	// 从而能够重新执行 SceneManager.LeaveScene。
	reclaimed, err := claimExpiredLeases(ctx, sc, base.Add(8*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	assert.NotEqual(t, first.token, reclaimed[0].token)
	assert.Equal(t, first.payload, reclaimed[0].payload)
}

func TestNotifySceneManagerLeave_NoAuthorityLocationNeedsNoClient(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	err := notifySceneManagerLeave(context.Background(), sc, &pb.PlayerSession{PlayerId: 14751})
	require.NoError(t, err)
}

func TestLeaseExpiry_PlayerIDMismatchRetainsReceiptForRetry(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 15001
	base := time.Now()

	session := makeTestSession(playerID)
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)

	// 模拟 key member 与 protobuf 内身份发生数据损坏。不能信任 payload 去通知
	// 另一个玩家，也不能直接 ACK 后永久丢掉清理责任。
	data, err := sc.RedisClient.Get(ctx, sessionKey(playerID)).Bytes()
	require.NoError(t, err)
	corrupt := &pb.PlayerSession{}
	require.NoError(t, proto.Unmarshal(data, corrupt))
	corrupt.PlayerId = 99999
	data, err = proto.Marshal(corrupt)
	require.NoError(t, err)
	require.NoError(t, sc.RedisClient.Set(ctx, sessionKey(playerID), data, 0).Err())

	claims, err := claimExpiredLeases(ctx, sc, base.Add(2*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	first := claims[0]
	handleLeaseExpiry(ctx, sc, first)

	assert.True(t, mr.Exists(sessionKey(playerID)), "身份不匹配时不得删除或改写不可信 session")
	processing, err := mr.ZMembers(LeaseProcessingZSetKey)
	require.NoError(t, err)
	assert.Contains(t, processing, "15001")
	token, err := sc.RedisClient.HGet(ctx, leaseClaimTokenHashKey, "15001").Result()
	require.NoError(t, err)
	assert.Equal(t, first.token, token)

	reclaimed, err := claimExpiredLeases(ctx, sc, base.Add(8*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	assert.NotEqual(t, first.token, reclaimed[0].token)
	assert.Equal(t, first.payload, reclaimed[0].payload)
}
