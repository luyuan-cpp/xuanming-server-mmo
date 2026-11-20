package clientplayerloginlogic

// Disconnect 只在栅栏关上、每个要收尾的玩家都挂上断线租约之后才回成功;任何一步失败回 Unavailable,自己不重试。
// gate 持有这条通知,login 确认之前退避重发同一条(cpp/nodes/gate/handler/rpc/login_session_renewal.h),所以这里要钉住两件事:
//   - 失败时回 Unavailable,且只调了一次 SetDisconnecting(重试只有 gate 一层,docs/design/infra-reconnect-overview.md §1.3);
//   - 重发(含「上次其实成了、只是回包丢了」)照常收尾,不重复产生副作用:栅栏 Close 重放返回同一个绑定的玩家,
//     清登录态删过再删是空操作,SetDisconnecting 重放的是同一个请求(player_locator 那边对已是 DISCONNECTING 的会话不延长租约,
//     见 go/player_locator/internal/logic/setdisconnecting_replay_test.go)。
// 修改前:SetDisconnecting 失败时 login 自己重试 4 次,随后照回成功;栅栏 Close 失败时照常收尾并回成功。gate 收到成功就不会再发,
// 玩家停在 ONLINE。夹具沿用 session_fence_wiring_test.go 的 newFenceWiring 与 leavegame_fence_failure_test.go 的 armFenceFault。

import (
	"context"
	"testing"

	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	pbbase "proto/common/base"
	login_proto "proto/login"
	plpb "proto/player_locator"
	"shared/redisslot"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const disconnectRetryAccount = "acct-disconnect-retry"

// flakyDisconnectLocator 让接下来 failures 次 SetDisconnecting 报 Unavailable(不记录),之后交给 fenceLocator 照常记录。
type flakyDisconnectLocator struct {
	*fenceLocator
	failures int
	calls    int
}

func (f *flakyDisconnectLocator) SetDisconnecting(ctx context.Context, in *plpb.SetDisconnectingRequest, opts ...grpc.CallOption) (*pbbase.Empty, error) {
	f.calls++
	if f.failures > 0 {
		f.failures--
		return nil, status.Error(codes.Unavailable, "injected: player_locator unavailable")
	}
	return f.fenceLocator.SetDisconnecting(ctx, in, opts...)
}

// bindAndLogIn 让这条连接登录(登录态 + 设备集成员)并在栅栏里绑定 fenceWiringPlayerID,返回本连接的登录态身份。
func bindAndLogIn(t *testing.T, ctx context.Context, rdb *redis.Client) loginsession.Ref {
	t.Helper()
	_, gen, err := sessionfence.CheckEnterGame(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, sessionFenceTTL)
	require.NoError(t, err)
	outcome, err := sessionfence.BindPlayer(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeOK, outcome)
	details, ok := ctxkeys.GetSessionDetails(ctx)
	require.True(t, ok)
	ref, ok := loginsession.RefOf(details)
	require.True(t, ok)
	require.NoError(t, rdb.Set(ctx, ref.Key(), disconnectRetryAccount, 0).Err())
	require.NoError(t, rdb.SAdd(ctx, redisslot.AccountDeviceSetKey(disconnectRetryAccount), ref.DeviceMember()).Err())
	return ref
}

func requireLoggedOut(t *testing.T, ctx context.Context, rdb *redis.Client, ref loginsession.Ref) {
	t.Helper()
	n, err := rdb.Exists(ctx, ref.Key()).Result()
	require.NoError(t, err)
	require.EqualValues(t, 0, n, "登录态必须删掉")
	member, err := rdb.SIsMember(ctx, redisslot.AccountDeviceSetKey(disconnectRetryAccount), ref.DeviceMember()).Result()
	require.NoError(t, err)
	require.False(t, member, "设备集成员必须移出")
}

func requireFenceClosed(t *testing.T, ctx context.Context, rdb *redis.Client) {
	t.Helper()
	outcome, err := sessionfence.Recheck(ctx, rdb, fenceWiringKey, 0)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeClosed, outcome)
}

func TestDisconnect_SetDisconnectingFailureReturnsUnavailableAndTheResendSettles(t *testing.T) {
	// 会话头带 player_id = 0(后台链已绑定、BindSession 还没到 gate):栅栏是这个玩家唯一的线索,重发时 Close 必须还能交出它。
	svcCtx, rdb, locator := newFenceWiring(t)
	flaky := &flakyDisconnectLocator{fenceLocator: locator, failures: 1}
	svcCtx.PlayerLocatorClient = flaky
	ctx := fenceWiringCtx(0)
	ref := bindAndLogIn(t, ctx, rdb)

	resp, err := NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.Equal(t, codes.Unavailable, status.Code(err), "断线租约没挂上就不能回成功,gate 收到成功就不会再发")
	require.Nil(t, resp)
	require.Equal(t, 1, flaky.calls, "login 自己不重试:重试只有 gate 一层")
	require.Empty(t, locator.disconnecting)
	requireFenceClosed(t, ctx, rdb)
	requireLoggedOut(t, ctx, rdb, ref)

	// gate 原样重发同一条通知。
	_, err = NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.NoError(t, err)
	require.Equal(t, 2, flaky.calls)
	require.Equal(t, []uint64{fenceWiringPlayerID}, locator.disconnectingPlayers(),
		"重发时 Close 重放交出同一个绑定的玩家,它挂上断线租约")
	requireSetDisconnectingInstances(t, locator, fenceWiringGate)
}

func TestDisconnect_FenceCloseFailureReturnsUnavailableWithoutCleanup(t *testing.T) {
	svcCtx, rdb, locator := newFenceWiring(t)
	ctx := fenceWiringCtx(fenceWiringPlayerID)
	ref := bindAndLogIn(t, ctx, rdb)

	fault := armFenceFault(rdb, false)
	resp, err := NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.False(t, fault.armed, "scenario never reached the fence close")
	require.Equal(t, codes.Unavailable, status.Code(err), "栅栏没关上就不能回成功")
	require.Nil(t, resp)
	require.Empty(t, locator.disconnecting, "栅栏没关上就什么都不收尾")
	n, err := rdb.Exists(ctx, ref.Key()).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "登录态留给重发清")
	outcome, err := sessionfence.Recheck(ctx, rdb, fenceWiringKey, 0)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeOK, outcome, "失败的 Close 什么都没写")

	_, err = NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.NoError(t, err)
	require.Equal(t, []uint64{fenceWiringPlayerID}, locator.disconnectingPlayers())
	requireFenceClosed(t, ctx, rdb)
	requireLoggedOut(t, ctx, rdb, ref)
}

func TestDisconnect_ResendAfterALostFenceReplyStillSettlesTheBoundPlayer(t *testing.T) {
	// Close 已在 Redis 上执行、回包丢了:这次回错误;重发时 Close 重放,仍交出同一个绑定的玩家。会话头带 player_id = 0。
	svcCtx, rdb, locator := newFenceWiring(t)
	ctx := fenceWiringCtx(0)
	bindAndLogIn(t, ctx, rdb)

	fault := armFenceFault(rdb, true)
	_, err := NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.False(t, fault.armed, "scenario never reached the fence close")
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Empty(t, locator.disconnecting)
	requireFenceClosed(t, ctx, rdb)

	_, err = NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.NoError(t, err)
	require.Equal(t, []uint64{fenceWiringPlayerID}, locator.disconnectingPlayers(),
		"重放的 Close 必须交出第一次执行时绑定的玩家,否则它永久停在 ONLINE")
}

func TestDisconnect_ResendAfterSuccessRepeatsNothing(t *testing.T) {
	// 第一次其实成了、gate 没收到回包(超时 / 断连):重发照回成功,不改写任何状态,送到 player_locator 的是同一个请求。
	svcCtx, rdb, locator := newFenceWiring(t)
	ctx := fenceWiringCtx(fenceWiringPlayerID)
	ref := bindAndLogIn(t, ctx, rdb)

	_, err := NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.NoError(t, err)
	_, err = NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.NoError(t, err)

	require.Len(t, locator.disconnecting, 2)
	require.True(t, proto.Equal(locator.disconnecting[0], locator.disconnecting[1]),
		"重放的 SetDisconnecting 必须是同一个请求(同一玩家、会话号、gate 实例),player_locator 对它幂等")
	requireFenceClosed(t, ctx, rdb)
	requireLoggedOut(t, ctx, rdb, ref)
}
