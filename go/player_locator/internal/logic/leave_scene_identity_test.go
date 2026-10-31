package logic

// b:player_locator 发给 SceneManager.LeaveScene 的请求必须带**被删那条会话记录的原值**
// (session_id, login_generation),scene_manager 据此判断 location 是否已属于更新的登录
// (docs/design/scene-enter-chain-residuals.md §3.2、§3.7)。
//
// 会话记录与请求都不带 gate 实例号(留空),避开 e 的实例号判定(§7 组间关系)。
// 辅助标识符统一加 leaveIdent 前缀;SceneManager 假实现复用 scene_manager_client_fake_test.go 的全包唯一一份。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	"shared/redisslot"
)

// leaveIdentSetup 装上 SceneManager 假实现,写一条 ONLINE 会话(会话号 100、版本 1、SceneId 50、带代次),
// GateId 留空:没有 gate 侧副作用,用例只看 LeaveScene。
func leaveIdentSetup(t *testing.T, playerID uint64, generation uint64) (*svc.ServiceContext, *fakeSceneManagerClient, *pb.PlayerSession) {
	t.Helper()
	sc, _ := newTestSvcCtx(t)
	fake := &fakeSceneManagerClient{}
	sc.SceneManagerClient = fake
	session := makeTestSession(playerID)
	session.GateId = ""
	session.GateInstanceId = ""
	session.LoginGeneration = generation
	_, err := NewSetSessionLogic(context.Background(), sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	return sc, fake, session
}

func TestMarkOffline_LeaveSceneCarriesDeletedSessionIdentity(t *testing.T) {
	const (
		playerID   = uint64(17001)
		generation = uint64(1757000004242)
	)
	sc, fake, session := leaveIdentSetup(t, playerID, generation)

	_, err := NewMarkOfflineLogic(context.Background(), sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: playerID, ExpectedSessionId: session.SessionId, ExpectedSessionVersion: session.SessionVersion,
	})
	require.NoError(t, err)

	calls := fake.leaveCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, playerID, calls[0].PlayerId)
	assert.Equal(t, session.SceneId, calls[0].SceneId)
	assert.Equal(t, session.SessionId, calls[0].SessionId, "带被删会话记录的会话号")
	assert.Equal(t, generation, calls[0].LoginGeneration, "带被删会话记录的登录代次")
}

func TestLeaseExpiry_LeaveSceneCarriesSessionIdentity(t *testing.T) {
	const (
		playerID   = uint64(17002)
		generation = uint64(1757000005151)
	)
	sc, fake, session := leaveIdentSetup(t, playerID, generation)
	ctx := context.Background()
	base := time.Now()

	_, err := NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)
	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), base.Add(2*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	handleLeaseExpiry(ctx, sc, claims[0])

	calls := fake.leaveCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, session.SceneId, calls[0].SceneId)
	assert.Equal(t, session.SessionId, calls[0].SessionId)
	assert.Equal(t, generation, calls[0].LoginGeneration, "断线只推进 session_version,不改代次;到期清理带的仍是这次登录的代次")
	processing, err := sc.PlayerRedis.ZRange(ctx, leaseKeysForPlayer(playerID).processing, 0, -1).Result()
	require.NoError(t, err)
	assert.NotContains(t, processing, "17002", "副作用全部成功后 claim 应已 ack")
}

// MarkOffline 的同步 LeaveScene 失败后入租约队列:入队的快照经 proto.Clone 保留代次,LeaseMonitor 重试时
// 发出的 LeaveScene 仍带被删会话的身份。
func TestMarkOffline_EnqueuedRetryPreservesGeneration(t *testing.T) {
	const (
		playerID   = uint64(17003)
		generation = uint64(1757000006161)
	)
	sc, fake, session := leaveIdentSetup(t, playerID, generation)
	ctx := context.Background()
	fake.setLeaveErr(errors.New("scene manager unavailable"))

	_, err := NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: playerID, ExpectedSessionId: session.SessionId, ExpectedSessionVersion: session.SessionVersion,
	})
	require.NoError(t, err)
	require.Len(t, fake.leaveCalls(), 1)

	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), time.Now().Add(2*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	queued := &pb.PlayerSession{}
	require.NoError(t, proto.Unmarshal(claims[0].payload, queued))
	assert.Equal(t, generation, queued.LoginGeneration, "入队快照必须保留代次")
	assert.Equal(t, session.SessionId, queued.SessionId)

	fake.setLeaveErr(nil)
	handleLeaseExpiry(ctx, sc, claims[0])

	calls := fake.leaveCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, session.SessionId, calls[1].SessionId)
	assert.Equal(t, generation, calls[1].LoginGeneration, "重试的 LeaveScene 仍带被删会话的代次")
	assert.Equal(t, session.SceneId, calls[1].SceneId)
}
