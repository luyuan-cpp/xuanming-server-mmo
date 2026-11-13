package logic

// SetSession 的「记录变了」拒绝回 gRPC ABORTED(setsessionlogic.go sessionChangedError)。
//
// login 靠这个码区分「确定没写、重读重判即可」与其余失败(go/login/internal/logic/pkg/sessionmanager 的
// ErrSessionChanged)。gate 排空迁移与跨 zone 重定向时,客户端先关旧连接、再在新连接上 EnterGame:旧 gate 的断线上报
// 推进了会话版本,新登录链按旧版本构造的顶号写入就落在这里(docs/design/gate-drain-session-migration.md §5)。
// 码给错的后果是玩家在无感迁移途中被打回登录界面,所以两个方向都钉住:该回 ABORTED 的回,不该回的不回。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "proto/player_locator"
)

// 旧连接的断线上报抢在顶号写入之前:版本对不上,回 ABORTED,记录保持断线上报写下的样子。
func TestSetSession_RecordAdvancedByDisconnectIsAborted(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16101

	first := makeTestSession(playerID)
	first.LoginGeneration = 100
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: first})
	require.NoError(t, err)

	// login 读到 ONLINE 的 first,据此构造顶号写入(版本 = first + 1)。
	replace := takeoverSession(first, 200, 101)
	// 写入之前,旧连接的断线上报先到:会话转 DISCONNECTING,版本推进一格。
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: first.SessionId, LeaseTtlSeconds: 30,
	})
	require.NoError(t, err)
	disconnecting := readSessionBytes(t, sc, playerID)

	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: replace})

	require.Error(t, err)
	assert.Equal(t, codes.Aborted, status.Code(err), "record changed since the caller read it: must be ABORTED so login re-reads and re-decides")
	assert.Equal(t, disconnecting, readSessionBytes(t, sc, playerID), "a rejected write must not change the record")
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, playerID).State)
}

// 重读之后按新版本再写就能提交:ABORTED 只表示「重做」,不是这名玩家再也登不上。
func TestSetSession_RewriteOnTheFreshRecordCommits(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16102

	first := makeTestSession(playerID)
	first.LoginGeneration = 100
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: first})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: first.SessionId, LeaseTtlSeconds: 30,
	})
	require.NoError(t, err)

	fresh := readSession(t, sc, playerID)
	rewrite := takeoverSession(fresh, 200, 101)
	rewrite.State = pb.PlayerSessionState_SESSION_STATE_ONLINE
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: rewrite})

	require.NoError(t, err)
	stored := readSession(t, sc, playerID)
	assert.Equal(t, uint32(200), stored.SessionId)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, stored.State)
}

// 版本对得上、代次不前进:不是记录变了,是调用方的代次不合法(失锁的旧登录链)。不回 ABORTED,login 不重读重判。
func TestSetSession_NonAdvancingLoginGenerationIsNotAborted(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID uint64 = 16103

	first := makeTestSession(playerID)
	first.LoginGeneration = 100
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: first})
	require.NoError(t, err)

	stale := takeoverSession(first, 300, 100)
	_, err = NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: stale})

	require.Error(t, err)
	assert.NotEqual(t, codes.Aborted, status.Code(err), "a non-advancing login generation is a stale login chain, not a concurrent change")
}
