package clientplayerloginlogic

// 票据绑定账号(2026-11-26,docs/design/login-queue-2026-05.md §5「票据绑定账号」):gate 上的 Login 认证出的账号,
// 必须是这条连接的 gate 票发给的那个账号。走真实 Login(),环境复用 legacy_gate_killswitch_login_test.go。
// 没绑账号的票(SessionDetails 里没有摘要)照常放行,由本包其余走 gate 的 Login 用例覆盖。

import (
	"context"
	"testing"

	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/loginsession"
	pbbase "proto/common/base"
	loginpb "proto/login"
	"shared/generated/pb/table"

	"github.com/stretchr/testify/require"
)

// gateCtxWithTicketFor 模拟 gate 转发来的请求:这条连接验过的 gate 票是发给 ticketAccount 的。
func gateCtxWithTicketFor(ticketAccount string) context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &pbbase.SessionDetails{
		SessionId: killSwitchSessionID, GateInstanceId: "gate-uuid-killswitch", SessionIncarnation: 9,
		TicketAccountDigest: loginqueue.TicketAccountDigest(ticketAccount),
	})
}

// 拿别的账号的票登录被拒,什么都不写;同一个账号拿自己的票照常绑定会话。
// 去掉 Login 里的 TicketIssuedTo 判断,本用例红在第一个断言上(别人的票也绑上了会话)。
func TestGateLoginRejectsATicketIssuedToAnotherAccount(t *testing.T) {
	const account = "ticket-owner"
	env := newKillSwitchEnv(t, false, account)

	httpResp, err := NewLoginLogic(context.Background(), env.svcCtx).Login(&loginpb.LoginRequest{
		AuthType: env.thirdPartyType, AuthToken: "oauth-code",
	})
	require.NoError(t, err)
	require.NotEmpty(t, httpResp.GetAccessToken(), "HTTP 入口不带会话,不比对票据")
	bind := &loginpb.LoginRequest{AuthType: "access_token", AuthToken: httpResp.GetAccessToken()}

	borrowed := gateCtxWithTicketFor("someone-else")
	resp, err := NewLoginLogic(borrowed, env.svcCtx).Login(bind)
	require.NoError(t, err)
	require.Equal(t, uint32(table.LoginError_kLoginUnknownError), resp.GetErrorMessage().GetId(),
		"票是发给别的账号的:排队按账号放人,借来的票不能用")
	require.Empty(t, resp.GetPlayers())
	requireNoSessionWritten(t, env, borrowed, account)

	own := gateCtxWithTicketFor(account)
	resp, err = NewLoginLogic(own, env.svcCtx).Login(bind)
	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId(), "自己的票照常绑定会话")
	bound, err := loginsession.GetAccount(own, env.rdb, killSwitchSessionRef(t, own))
	require.NoError(t, err)
	require.Equal(t, account, bound)
}
