package clientplayerloginlogic

// gate 为活着的连接续租登录态(RenewSession)的用例(docs/design/gate-online-identity.md §7.3、§10.4)。
//
// 缺口:登录态与设备集是绝对寿命(Node.SessionExpireMin),在线超过寿命的连接不再计入账号的设备数上限,
// 同一账号错开登录就能同时在线超过上限的连接。续租之后,连接活着多久就被计数多久。
// 复用登录提交用例的夹具(生产的玩家域构造器,守卫生效)。本包用例改写 config.AppConfig,不能加 t.Parallel()。

import (
	"context"
	"testing"
	"time"

	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/sessionfence"
	pbbase "proto/common/base"
	login_proto "proto/login"
	"shared/generated/pb/table"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (e *loginCommitEnv) renew(t *testing.T, details *pbbase.SessionDetails) *login_proto.RenewSessionResponse {
	t.Helper()
	ctx := ctxkeys.WithSessionDetails(context.Background(), details)
	resp, err := NewRenewSessionLogic(ctx, e.svcCtx).RenewSession(&login_proto.RenewSessionRequest{})
	require.NoError(t, err)
	return resp
}

func TestRenewedConnectionsStillCountAgainstTheDeviceLimit(t *testing.T) {
	e := newLoginCommitEnv(t) // 设备数上限 3,登录态寿命 30 分钟
	online := []*pbbase.SessionDetails{loginCommitDetails(1), loginCommitDetails(2), loginCommitDetails(3)}
	for _, details := range online {
		require.Zero(t, e.login(t, details).GetErrorMessage().GetId())
	}
	// 三条连接一直在线,gate 每 10 分钟续一次,共 40 分钟:已经超过登录态最初的 30 分钟寿命。
	for round := 0; round < 4; round++ {
		e.mr.FastForward(10 * time.Minute)
		for _, details := range online {
			resp := e.renew(t, details)
			require.True(t, resp.GetRenewed())
			require.Equal(t, uint32(600), resp.GetRenewAfterSeconds(), "间隔是登录态寿命的三分之一,由 login 告诉 gate")
		}
	}

	resp := e.login(t, loginCommitDetails(4))

	require.Equal(t, uint32(table.LoginError_kTooManyDevices), resp.GetErrorMessage().GetId(),
		"在线的三条连接仍被计数,第四条连接超限")
}

func TestWithoutRenewalConnectionsStopCountingAfterTheLifetime(t *testing.T) {
	// 对照:没有人续租(gate 进程崩溃、Disconnect 没送到)时,登录态按寿命回收,这些连接不再占设备数。
	// 续租之前,一直在线的连接也是这个结局,所以第四条连接能登进来。
	e := newLoginCommitEnv(t)
	for incarnation := uint64(1); incarnation <= 3; incarnation++ {
		require.Zero(t, e.login(t, loginCommitDetails(incarnation)).GetErrorMessage().GetId())
	}
	e.mr.FastForward(30*time.Minute + time.Second)

	resp := e.login(t, loginCommitDetails(4))

	require.Zero(t, resp.GetErrorMessage().GetId())
}

func TestRenewalAfterALongOutageCountsConnectionsInGameAgain(t *testing.T) {
	// login 或登录域 Redis 长时间不可用,续租连续失败超过登录态寿命:三条在游戏里的连接的登录态都过期了。
	// 恢复后的第一轮续租把它们重建并重新计数,第四条连接照样超限,不必等这三条连接重新登录。
	e := newLoginCommitEnv(t)
	online := []*pbbase.SessionDetails{loginCommitDetails(1), loginCommitDetails(2), loginCommitDetails(3)}
	ctx := context.Background()
	for i, details := range online {
		require.Zero(t, e.login(t, details).GetErrorMessage().GetId())
		// 进游戏:后台链在绑定点把角色绑到这条连接上(这里直接走栅栏的入口与绑定两步)。
		fence, playerID := loginCommitRef(t, details).Fence(), uint64(880100+i)
		outcome, gen, err := sessionfence.CheckEnterGame(ctx, e.svcCtx.LoginRedis, fence, playerID, sessionFenceTTL)
		require.NoError(t, err)
		require.Equal(t, sessionfence.OutcomeOK, outcome)
		outcome, err = sessionfence.BindPlayer(ctx, e.svcCtx.LoginRedis, fence, playerID, gen)
		require.NoError(t, err)
		require.Equal(t, sessionfence.OutcomeOK, outcome)
	}
	e.mr.FastForward(30*time.Minute + time.Second)
	require.Empty(t, e.deviceMembers(t), "夹具前提:故障期间登录态与设备集都过期了")

	for _, details := range online {
		require.True(t, e.renew(t, details).GetRenewed())
	}
	require.Len(t, e.deviceMembers(t), 3, "在游戏里的三条连接重新登记进设备集")

	resp := e.login(t, loginCommitDetails(4))

	require.Equal(t, uint32(table.LoginError_kTooManyDevices), resp.GetErrorMessage().GetId(),
		"重建之后三条连接重新计数,第四条连接超限")
}

func TestRenewAfterDisconnectReportsNothingToRenew(t *testing.T) {
	// 续租在途时连接断了:Disconnect 先处理,迟到的续租不得把登录态续活,也不把连接登记回设备集。
	e := newLoginCommitEnv(t)
	details := loginCommitDetails(1)
	require.Zero(t, e.login(t, details).GetErrorMessage().GetId())
	e.disconnect(t, details)

	resp := e.renew(t, details)

	require.False(t, resp.GetRenewed(), "没有可续的,gate 据此停止为这条连接续租")
	require.Zero(t, resp.GetRenewAfterSeconds())
	require.Empty(t, e.deviceMembers(t))
	require.False(t, e.mr.Exists(loginCommitRef(t, details).Key()))
}

func TestRenewWithoutConnectionIdentityReportsNothingToRenew(t *testing.T) {
	e := newLoginCommitEnv(t)

	// 会话头缺 gate_instance_id / 会话代次:这种连接过不了 Login,在 login 不可能有记录。
	resp := e.renew(t, &pbbase.SessionDetails{SessionId: 7300})
	require.False(t, resp.GetRenewed())

	// 根本没有经验签的会话身份。
	resp, err := NewRenewSessionLogic(context.Background(), e.svcCtx).RenewSession(&login_proto.RenewSessionRequest{})
	require.NoError(t, err)
	require.False(t, resp.GetRenewed())
	require.Empty(t, e.mr.Keys(), "两种情况都不写任何键")
}

func TestRenewRedisFailureReturnsUnavailableSoGateRetries(t *testing.T) {
	// Redis 故障不能回「没有可续的」:那会让 gate 停止续租。回 gRPC 错误,gate 按调用失败退避后重发。
	e := newLoginCommitEnv(t)
	details := loginCommitDetails(1)
	require.Zero(t, e.login(t, details).GetErrorMessage().GetId())
	e.mr.SetError("ERR injected redis failure")
	t.Cleanup(func() { e.mr.SetError("") })

	ctx := ctxkeys.WithSessionDetails(context.Background(), details)
	resp, err := NewRenewSessionLogic(ctx, e.svcCtx).RenewSession(&login_proto.RenewSessionRequest{})

	require.Nil(t, resp)
	require.Equal(t, codes.Unavailable, status.Code(err))
}
