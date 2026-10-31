package logic

// 删 location 绑定「持有者已释放」的 player_locator 一侧(docs/design/location-release-binding.md §3.4)。
// scene_manager 拿不到释放证据时 LeaveScene 回 FailedPrecondition:这里要保留清理任务重试、不按故障记录;MarkOffline 入队
// 首拍延后;登出清理的载荷不带 gate(连接仍在线,不得被 LeaseExpired 踢掉)。
// SceneManager 假实现复用 scene_manager_client_fake_test.go 的全包唯一一份;辅助标识符加 leaveRelease 前缀。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	"shared/redisslot"
)

// leaveReleaseSetup 装上 SceneManager 假实现,写一条带 gate 的 ONLINE 会话(模拟停在选角界面、连接还在的登出)。
// gate 实例号与 MarkOffline 请求都留空,避开 e 的实例号判定。
func leaveReleaseSetup(t *testing.T, playerID uint64) (*svc.ServiceContext, *fakeSceneManagerClient, *pb.PlayerSession) {
	t.Helper()
	sc, _ := newTestSvcCtx(t)
	fake := &fakeSceneManagerClient{}
	sc.SceneManagerClient = fake
	session := makeTestSession(playerID)
	session.GateId = "7"
	session.GateInstanceId = ""
	_, err := NewSetSessionLogic(context.Background(), sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	return sc, fake, session
}

func leaveReleasePendingStatus() error {
	return status.Error(codes.FailedPrecondition, "holder_release_pending: player=1 scene=50 node=10 owner_epoch=3 verdict=pending waited=0s")
}

func TestNotifySceneManagerLeave_ClassifiesHolderReleasePending(t *testing.T) {
	const playerID = uint64(17101)
	sc, fake, session := leaveReleaseSetup(t, playerID)

	fake.setLeaveErr(leaveReleasePendingStatus())
	err := notifySceneManagerLeave(context.Background(), sc, session)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errHolderReleasePending), "FailedPrecondition 必须识别为等持有者释放")
	assert.True(t, isOnlyHolderReleasePending(err))

	fake.setLeaveErr(status.Error(codes.Unavailable, "scene manager down"))
	err = notifySceneManagerLeave(context.Background(), sc, session)
	require.Error(t, err)
	assert.False(t, errors.Is(err, errHolderReleasePending), "传输失败仍按故障处理")
}

func TestIsOnlyHolderReleasePending(t *testing.T) {
	pending := fmt.Errorf("%w: detail", errHolderReleasePending)
	assert.True(t, isOnlyHolderReleasePending(pending))
	assert.True(t, isOnlyHolderReleasePending(errors.Join(nil, pending)), "清理副作用只剩这一条时仍算")
	assert.False(t, isOnlyHolderReleasePending(errors.Join(errors.New("publish LeaseExpired failed"), pending)),
		"夹着别的失败(如 LeaseExpired 投递)就按故障记录")
	assert.False(t, isOnlyHolderReleasePending(errors.New("scene manager down")))
}

// 登出时持有节点还没释放:会话照删,清理入队,首拍延后 holderReleaseFirstRetryDelay;入队载荷不带 gate。
func TestMarkOffline_HolderReleasePendingEnqueuesDelayedCleanupWithoutGate(t *testing.T) {
	const playerID = uint64(17102)
	sc, fake, session := leaveReleaseSetup(t, playerID)
	ctx := context.Background()
	fake.setLeaveErr(leaveReleasePendingStatus())

	start := time.Now()
	_, err := NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: playerID, ExpectedSessionId: session.SessionId, ExpectedSessionVersion: session.SessionVersion,
	})
	require.NoError(t, err, "等持有者释放不是 MarkOffline 的失败")
	require.Len(t, fake.leaveCalls(), 1)
	exists, err := sc.PlayerRedis.Exists(ctx, sessionKey(playerID)).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "会话照删")

	// ready 分数延后 2s:1s 之后还领不到,3s 之后能领到。
	score, err := sc.PlayerRedis.ZScore(ctx, leaseKeysForPlayer(playerID).ready, strconv.FormatUint(playerID, 10)).Result()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, int64(score), start.Add(holderReleaseFirstRetryDelay).Unix()-1)
	early, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), start.Add(holderReleaseFirstRetryDelay-time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	assert.Empty(t, early, "首拍要延后,给持有节点落盘写标记的时间")
	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), start.Add(holderReleaseFirstRetryDelay+time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	queued := &pb.PlayerSession{}
	require.NoError(t, proto.Unmarshal(claims[0].payload, queued))
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, queued.State)
	assert.Empty(t, queued.GateId, "登出清理不得给 gate 发 LeaseExpired(连接仍在线)")
	assert.Empty(t, queued.GateInstanceId)
	assert.Equal(t, session.SessionId, queued.SessionId)
}

// 其它 LeaveScene 失败照旧立即入队;载荷同样不带 gate(登出的连接在任何失败下都不该被踢)。
func TestMarkOffline_LeaveSceneFailureEnqueuesImmediatelyWithoutGate(t *testing.T) {
	const playerID = uint64(17103)
	sc, fake, session := leaveReleaseSetup(t, playerID)
	ctx := context.Background()
	fake.setLeaveErr(status.Error(codes.Unavailable, "scene manager down"))

	start := time.Now()
	_, err := NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: playerID, ExpectedSessionId: session.SessionId, ExpectedSessionVersion: session.SessionVersion,
	})
	require.NoError(t, err)

	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), start.Add(time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1, "故障类失败不延后")
	queued := &pb.PlayerSession{}
	require.NoError(t, proto.Unmarshal(claims[0].payload, queued))
	assert.Empty(t, queued.GateId)
}

// 清理任务重试时 scene_manager 仍回"等持有者释放":claim 保留在 processing、不 ack;之后拿到证据删掉就 ack。
func TestLeaseExpiry_HolderReleasePendingRetainsClaimUntilReleased(t *testing.T) {
	const playerID = uint64(17104)
	sc, fake, session := leaveReleaseSetup(t, playerID)
	ctx := context.Background()
	fake.setLeaveErr(leaveReleasePendingStatus())
	start := time.Now()
	_, err := NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: playerID, ExpectedSessionId: session.SessionId, ExpectedSessionVersion: session.SessionVersion,
	})
	require.NoError(t, err)

	claims, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), start.Add(holderReleaseFirstRetryDelay+time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	handleLeaseExpiry(ctx, sc, claims[0])
	require.Len(t, fake.leaveCalls(), 2)
	processing, err := sc.PlayerRedis.ZRange(ctx, leaseKeysForPlayer(playerID).processing, 0, -1).Result()
	require.NoError(t, err)
	assert.Contains(t, processing, strconv.FormatUint(playerID, 10), "等持有者释放时 claim 必须保留重试")

	// 清理在途期间首次登录被既有屏障挡住(不改 claim / 会话 Lua):这是设计 §6 第 1 条写明的代价。
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: makeTestSession(playerID)})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSessionCleanupPending))

	// claim 超时回 ready 后再领,这次 scene_manager 拿到了释放证据、删掉 location → ack。
	fake.setLeaveErr(nil)
	claims, err = claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(playerID), start.Add(time.Minute), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	handleLeaseExpiry(ctx, sc, claims[0])
	processing, err = sc.PlayerRedis.ZRange(ctx, leaseKeysForPlayer(playerID).processing, 0, -1).Result()
	require.NoError(t, err)
	assert.NotContains(t, processing, strconv.FormatUint(playerID, 10))
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: makeTestSession(playerID)})
	require.NoError(t, err, "清理 ack 之后首次登录放行")
}
