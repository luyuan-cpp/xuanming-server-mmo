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
	"shared/playerredis"
	"shared/redisslot"
)

// ---------------------------------------------------------------------------
// Test helper
// ---------------------------------------------------------------------------

func newTestSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	// 玩家域走生产的构造函数:挂着发送前守卫,键没带 tag 或跨 slot 的脚本在任何用例里都当场失败。
	player, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}})
	require.NoError(t, err)
	t.Cleanup(func() { player.Close() })

	return &svc.ServiceContext{
		Config: config.Config{
			Lease: config.LeaseConf{DefaultTTLSeconds: 30},
		},
		RedisClient: rdb,
		PlayerRedis: player,
	}, mr
}

// ---------------------------------------------------------------------------
// Location tests
// ---------------------------------------------------------------------------

// 两个历史位置端点都 fail-closed,读写都不碰 Redis:位置权威在 scene_manager。
func TestLegacyLocationEndpoints_DeprecatedRejected(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()

	_, err := NewSetLocationLogic(ctx, sc).SetLocation(&pb.PlayerLocation{
		Uid:      1001,
		ServerId: "scene-node-1",
		SceneId:  42,
	})
	require.Error(t, err)

	_, err = NewGetLocationLogic(ctx, sc).GetLocation(&pb.PlayerId{Uid: 1001})
	require.Error(t, err)

	assert.Empty(t, mr.Keys(), "拒绝必须发生在任何写入之前")
}

func TestMarkOffline(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()

	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{
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
	assert.False(t, mr.Exists(sessionKey(2001)))
}

func TestMarkOffline_DelayedOldLeaveGameDoesNotDeleteReplacement(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	playerID := uint64(2002)

	oldSession := makeTestSession(playerID)
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: oldSession})
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
	assert.Contains(t, leaseMembers(t, mr), "4001")
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
	assert.NotContains(t, leaseMembers(t, mr), "6001")
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

	// 1. Set session (gate binds session)
	setSessionLogic := NewSetSessionLogic(ctx, sc)
	_, err := setSessionLogic.SetSession(&pb.SetSessionRequest{
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

	// 2. Player disconnects
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

	assert.Contains(t, leaseMembers(t, mr), "10001")

	// 3. Player reconnects
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
	assert.NotContains(t, leaseMembers(t, mr), "10001")

	// 4. Player goes offline for real
	offLogic := NewMarkOfflineLogic(ctx, sc)
	_, _ = offLogic.MarkOffline(&pb.MarkOfflineRequest{
		PlayerId:               playerID,
		ExpectedSessionId:      rcResp.Session.SessionId,
		ExpectedSessionVersion: rcResp.Session.SessionVersion,
	})

	assert.False(t, mr.Exists(sessionKey(playerID)))
}

// ---------------------------------------------------------------------------
// Key format tests
// ---------------------------------------------------------------------------

func TestKeyFormats(t *testing.T) {
	assert.Equal(t, "player:{42}:location", sceneManagerLocationKey(42))
	assert.Equal(t, "player:session:{99}", sessionKey(99))
	assert.Equal(t, "player:afk_pass:{99}", afkPassKey(99))

	// 按 slot 分桶的键形:金样用 SlotTag(0) = afps(redisslot 的金样)。清库脚本、运维手册也按这个形状写。
	lease := leaseKeysForSlot(0)
	assert.Equal(t, "player:leases:{afps}", lease.ready)
	assert.Equal(t, "player:leases:{afps}:processing", lease.processing)
	assert.Equal(t, "player:leases:{afps}:claim_tokens", lease.claimTokens)
	assert.Equal(t, "player:leases:{afps}:claim_payloads", lease.claimPayloads)
	assert.Equal(t, "player:gate_index:{afps}:gates", gateRegistryKey(0))
	assert.Equal(t, "player:gate_index:{afps}:sessions:inst-1", gateSessionIndexKey(0, "inst-1"))
	assert.Equal(t, "player:locator_shard:gate_sweep:members:{afps}", shardMembersKey(shardRoleGateSweep))
	assert.Equal(t, "player:locator_shard:lease_monitor:segment:{afps}", shardSegmentKey(shardRoleLeaseMonitor, 0))

	// 一名玩家的租约键与索引键带它那个 slot 的 tag。
	tag := redisslot.PlayerIndexTag(99)
	assert.Equal(t, "player:leases:"+tag, leaseKeysForPlayer(99).ready)
	assert.Equal(t, "player:gate_index:"+tag+":gates", gateRegistryKey(redisslot.PlayerSlot(99)))
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
	ttl, err := sc.PlayerRedis.TTL(ctx, sessionKey(11001)).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), ttl)

	// 租约本身仍按 30s 到期(宽限期只延长载荷寿命,不推迟清理时机)。
	require.Contains(t, leaseMembers(t, mr), "11001")

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
	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), now, 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.NotEmpty(t, claims[0].payload)
	firstToken := claims[0].token

	// 模拟 worker 在处理前崩溃：不 ack，直接越过 processing deadline。
	claims, err = claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), now.Add(6*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.NotEqual(t, firstToken, claims[0].token)
	assert.Equal(t, playerID, claims[0].playerID)
	assert.NotEmpty(t, claims[0].payload)

	processing, err := mr.ZMembers(leaseKeysForPlayer(playerID).processing)
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
	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), base.Add(2*time.Second), 10, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	// 在原 processing deadline 前心跳，把仍属本 token 的 claim 延后 30 秒。
	renewed, err := renewLeaseClaims(ctx, sc, claims, base.Add(50*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, renewed)

	// 已越过原 deadline(base+32s)，但尚未越过心跳后的 deadline，不得被新 worker 回收。
	recovered, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), base.Add(33*time.Second), 10, 30*time.Second)
	require.NoError(t, err)
	assert.Empty(t, recovered)

	// 心跳真正失效后仍可恢复，保证 worker 崩溃不会永久卡住任务。
	recovered, err = claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), base.Add(51*time.Second), 10, 30*time.Second)
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
	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), time.Now().Add(2*time.Second), 10, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	reconnected, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId: playerID, NewSessionId: 201, GateId: "gate-2", GateInstanceId: "inst-2",
	})
	require.NoError(t, err)
	require.True(t, reconnected.Success)

	processingCount, err := sc.PlayerRedis.ZCard(ctx, leaseKeysForPlayer(playerID).processing).Result()
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
	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), time.Now().Add(2*time.Second), 10, 30*time.Second)
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
	processing, err := mr.ZMembers(leaseKeysForPlayer(playerID).processing)
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
	require.NoError(t, sc.PlayerRedis.Set(ctx, sceneManagerLocationKey(playerID), data, 0).Err())

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
	require.NoError(t, sc.PlayerRedis.Set(ctx, sceneManagerLocationKey(playerID), location, 0).Err())
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)

	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), base.Add(2*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	first := claims[0]

	// commit 已取得清理权并删除旧 session；SceneManager client 缺失必须使
	// 外部副作用失败，不能 ACK processing receipt。
	handleLeaseExpiry(ctx, sc, first)
	assert.False(t, mr.Exists(sessionKey(playerID)))
	processing, err := mr.ZMembers(leaseKeysForPlayer(playerID).processing)
	require.NoError(t, err)
	assert.Contains(t, processing, "14501")
	token, err := sc.PlayerRedis.HGet(ctx, leaseKeysForPlayer(playerID).claimTokens, "14501").Result()
	require.NoError(t, err)
	assert.Equal(t, first.token, token)
	payload, err := sc.PlayerRedis.HGet(ctx, leaseKeysForPlayer(playerID).claimPayloads, "14501").Bytes()
	require.NoError(t, err)
	assert.Equal(t, first.payload, payload)

	// worker/配置恢复后，deadline 回收应换 token 并带回同一份 payload，
	// 从而能够重新执行 SceneManager.LeaveScene。
	reclaimed, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), base.Add(8*time.Second), 10, 5*time.Second)
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
	data, err := sc.PlayerRedis.Get(ctx, sessionKey(playerID)).Bytes()
	require.NoError(t, err)
	corrupt := &pb.PlayerSession{}
	require.NoError(t, proto.Unmarshal(data, corrupt))
	corrupt.PlayerId = 99999
	data, err = proto.Marshal(corrupt)
	require.NoError(t, err)
	require.NoError(t, sc.PlayerRedis.Set(ctx, sessionKey(playerID), data, 0).Err())

	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), base.Add(2*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	first := claims[0]
	handleLeaseExpiry(ctx, sc, first)

	assert.True(t, mr.Exists(sessionKey(playerID)), "身份不匹配时不得删除或改写不可信 session")
	processing, err := mr.ZMembers(leaseKeysForPlayer(playerID).processing)
	require.NoError(t, err)
	assert.Contains(t, processing, "15001")
	token, err := sc.PlayerRedis.HGet(ctx, leaseKeysForPlayer(playerID).claimTokens, "15001").Result()
	require.NoError(t, err)
	assert.Equal(t, first.token, token)

	reclaimed, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), base.Add(8*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	assert.NotEqual(t, first.token, reclaimed[0].token)
	assert.Equal(t, first.payload, reclaimed[0].payload)
}

// ---------------------------------------------------------------------------
// 登录代次栅栏(docs/design/scene-enter-session-fence.md §3.4)
// ---------------------------------------------------------------------------

// readSessionBytes 读会话键原始字节。"被拒后记录不变"按整字节比较,与提交点的整字节 CAS 同一口径。
func readSessionBytes(t *testing.T, sc *svc.ServiceContext, playerID uint64) []byte {
	t.Helper()
	data, err := sc.PlayerRedis.Get(context.Background(), sessionKey(playerID)).Bytes()
	require.NoError(t, err)
	return data
}

func readSession(t *testing.T, sc *svc.ServiceContext, playerID uint64) *pb.PlayerSession {
	t.Helper()
	s := &pb.PlayerSession{}
	require.NoError(t, proto.Unmarshal(readSessionBytes(t, sc, playerID), s))
	return s
}

// takeoverSession 以 base 为当前记录构造一次顶号写入:换会话号、session_version 恰好 +1、带指定代次。
// 版本号总是合法的,被拒时原因只可能是登录代次。
func takeoverSession(base *pb.PlayerSession, sessionID uint32, generation uint64) *pb.PlayerSession {
	next := proto.Clone(base).(*pb.PlayerSession)
	next.SessionId = sessionID
	next.SessionVersion = base.SessionVersion + 1
	next.LoginGeneration = generation
	return next
}

// 代次严格递增才提交。相等 = 两条会话共用一个代次(违反 G2);更小 = 失锁的旧登录链在新链提交之后
// 才写(违反 G1)。两者都按 stale 拒,且记录逐字节不变。
func TestSetSession_LoginGenerationMustStrictlyIncrease(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16001

	first := makeTestSession(playerID)
	first.LoginGeneration = 100
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: first})
	require.NoError(t, err)

	newer := takeoverSession(first, 200, 101)
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: newer})
	require.NoError(t, err)
	assert.Equal(t, uint64(101), readSession(t, sc, playerID).LoginGeneration)
	committed := readSessionBytes(t, sc, playerID)

	for _, tc := range []struct {
		name       string
		generation uint64
	}{
		{name: "equal", generation: 101},
		{name: "smaller", generation: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := takeoverSession(newer, 300, tc.generation)
			_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: stale})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "stale session update")
			assert.Contains(t, err.Error(), "login_generation")
			assert.Equal(t, committed, readSessionBytes(t, sc, playerID), "被拒的写入不得改动记录")
		})
	}
}

// 顶号写入的响应丢失后 login 原样重放:请求代次与记录相等,必须先命中 proto.Equal 的重放分支
// 幂等成功,不能被代次栅栏判成 stale(否则合法登录因一次丢包失败)。
func TestSetSession_ReplayWithSameLoginGenerationIsIdempotent(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16101

	first := makeTestSession(playerID)
	first.LoginGeneration = 100
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: first})
	require.NoError(t, err)

	takeover := takeoverSession(first, 200, 101)
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: takeover})
	require.NoError(t, err)
	committed := readSessionBytes(t, sc, playerID)

	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: takeover})
	require.NoError(t, err)
	assert.Equal(t, committed, readSessionBytes(t, sc, playerID))
}

// 请求代次为 0(旧版 login)不校验、原样写 0,不继承记录里的旧代次:继承会让无代次的会话冒用上一次
// 登录的代次。记录为 0 时不与任何历史值比较,任意非 0 代次都能提交(升级后的第一次登录)。
func TestSetSession_ZeroLoginGenerationSkipsCheck(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16201

	first := makeTestSession(playerID)
	first.LoginGeneration = 100
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: first})
	require.NoError(t, err)

	legacy := takeoverSession(first, 200, 0)
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: legacy})
	require.NoError(t, err)
	stored := readSession(t, sc, playerID)
	assert.Equal(t, uint32(200), stored.SessionId)
	assert.Zero(t, stored.LoginGeneration, "请求无代次时记录如实为 0,不得继承旧值")

	upgraded := takeoverSession(legacy, 300, 1)
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: upgraded})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), readSession(t, sc, playerID).LoginGeneration)
}

// 断线不改代次;Reconnect 把本链铸出的代次写进记录并在应答里回带。同 (session_id, request_id) 的
// 重放请求代次与记录相等,必须先于代次栅栏命中重放分支,幂等成功且记录不变。
func TestReconnect_WritesLoginGenerationAndReplayIsIdempotent(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16301

	session := makeTestSession(playerID)
	session.LoginGeneration = 100
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 30,
	})
	require.NoError(t, err)
	disconnected := readSession(t, sc, playerID)
	require.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, disconnected.State)
	assert.Equal(t, uint64(100), disconnected.LoginGeneration, "断线只推进 session_version,不改登录代次")

	req := &pb.ReconnectRequest{
		PlayerId: playerID, NewSessionId: 200, GateId: "gate-2", GateInstanceId: "inst-2",
		RequestId: "req-reconnect-1", LoginGeneration: 101,
	}
	resp, err := NewReconnectLogic(ctx, sc).Reconnect(req)
	require.NoError(t, err)
	require.True(t, resp.Success, resp.ErrorMessage)
	assert.Equal(t, uint64(101), resp.Session.LoginGeneration)
	assert.Equal(t, uint64(101), readSession(t, sc, playerID).LoginGeneration)
	committed := readSessionBytes(t, sc, playerID)

	replay, err := NewReconnectLogic(ctx, sc).Reconnect(req)
	require.NoError(t, err)
	require.True(t, replay.Success, replay.ErrorMessage)
	assert.Equal(t, uint64(101), replay.Session.LoginGeneration)
	assert.Equal(t, committed, readSessionBytes(t, sc, playerID), "重放不得再次改写记录")
}

// 记录与请求代次都非 0 时要求严格递增:相等 / 更小按现有失败语义回 success=false,记录逐字节不变
// (仍是 DISCONNECTING、租约仍在);随后更大的代次照常重连。
func TestReconnect_RejectsNonIncreasingLoginGeneration(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16401

	session := makeTestSession(playerID)
	session.LoginGeneration = 100
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 30,
	})
	require.NoError(t, err)
	disconnected := readSessionBytes(t, sc, playerID)

	for _, tc := range []struct {
		name       string
		sessionID  uint32
		requestID  string
		generation uint64
	}{
		{name: "equal", sessionID: 200, requestID: "req-equal", generation: 100},
		{name: "smaller", sessionID: 201, requestID: "req-smaller", generation: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
				PlayerId: playerID, NewSessionId: tc.sessionID, GateId: "gate-2",
				RequestId: tc.requestID, LoginGeneration: tc.generation,
			})
			require.NoError(t, err)
			assert.False(t, resp.Success)
			assert.Contains(t, resp.ErrorMessage, "login generation")
			assert.Equal(t, disconnected, readSessionBytes(t, sc, playerID), "被拒的重连不得改动记录")
		})
	}
	assert.Contains(t, leaseMembers(t, mr), "16401", "被拒的重连不得撤销断线租约")

	resp, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId: playerID, NewSessionId: 300, GateId: "gate-2", RequestId: "req-newer", LoginGeneration: 101,
	})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.ErrorMessage)
	assert.Equal(t, uint64(101), readSession(t, sc, playerID).LoginGeneration)
}

// 任一方代次为 0 不校验:记录无代次(旧版 login 写入)时接受新代次;请求无代次时如实写 0,
// 不保留记录里的旧代次。
func TestReconnect_ZeroLoginGenerationSkipsCheck(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16501

	session := makeTestSession(playerID) // 代次 0:旧版 login 写入的会话
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	disconnect := func(sessionID uint32) {
		t.Helper()
		_, err := NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
			PlayerId: playerID, SessionId: sessionID, LeaseTtlSeconds: 30,
		})
		require.NoError(t, err)
	}

	disconnect(session.SessionId)
	resp, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId: playerID, NewSessionId: 200, GateId: "gate-2", RequestId: "req-upgraded", LoginGeneration: 7,
	})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.ErrorMessage)
	assert.Equal(t, uint64(7), readSession(t, sc, playerID).LoginGeneration)

	disconnect(200)
	resp, err = NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
		PlayerId: playerID, NewSessionId: 300, GateId: "gate-3", RequestId: "req-legacy",
	})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.ErrorMessage)
	assert.Zero(t, resp.Session.LoginGeneration)
	assert.Zero(t, readSession(t, sc, playerID).LoginGeneration, "请求无代次时记录如实为 0")
}
