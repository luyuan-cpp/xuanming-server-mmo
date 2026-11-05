package clientplayerloginlogic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"login/internal/logic/pkg/ctxkeys"
	base "proto/common/base"
	loginpb "proto/login"
	smpb "proto/scene_manager"
	loginreader "shared/loginsession"
)

type acceptedIdentityScene struct {
	smpb.SceneManagerClient
	calls int
}

func (s *acceptedIdentityScene) EnterScene(context.Context, *smpb.EnterSceneRequest, ...grpc.CallOption) (*smpb.EnterSceneResponse, error) {
	s.calls++
	return &smpb.EnterSceneResponse{}, nil
}

// 经过完整异步入场链，再使用匹配服等服务的公共登录态读取接口验证身份。
// 不能只测 applyLoadedPlayerSession：旧清理发生在它成功返回之后。
func TestEnterGameRetainsOnlineIdentityUntilConnectionLeaves(t *testing.T) {
	for _, action := range []string{"disconnect", "leave"} {
		t.Run(action, func(t *testing.T) {
			h := newEnterWiringHarness(t, []*base.AccountSimplePlayer{{PlayerId: identityPlayerID, ClassId: 3, Name: "云中君"}}, nil)
			h.l.svcCtx.PlayerLocatorClient = &kickLocator{events: &kickEventLog{}}
			binder := &bindRecorder{}
			scene := &acceptedIdentityScene{}
			h.l.binder = binder
			h.l.svcCtx.SceneManagerClient = scene
			resp, err := h.l.EnterGame(&loginpb.EnterGameRequest{PlayerId: identityPlayerID, RequestId: "online-identity"})
			require.NoError(t, err)
			require.Nil(t, resp.GetErrorMessage())
			h.waitChain(t)
			require.Equal(t, 1, binder.calls)
			require.Equal(t, 1, scene.calls)
			details, ok := ctxkeys.GetSessionDetails(h.l.ctx)
			require.True(t, ok)
			require.Equal(t, enterWiringAccount, binder.last.AuthenticatedAccount)
			require.Equal(t, details.SessionIncarnation, binder.last.SessionIncarnation)
			require.NotZero(t, binder.last.OwnerRevision, "绑定原身份必须携带实际归属修订")
			account, err := loginreader.Account(context.Background(), h.rdb, details)
			require.NoError(t, err, "进场后匹配等在线服务仍须能核验本条登录连接")
			require.Equal(t, enterWiringAccount, account)

			h.l.svcCtx.PlayerLocatorClient = &fenceLocator{}
			if action == "disconnect" {
				_, err = NewDisconnectLogic(h.l.ctx, h.l.svcCtx).Disconnect(&loginpb.LoginNodeDisconnectRequest{SessionId: enterWiringSessionID})
			} else {
				_, err = NewLeaveGameLogic(h.l.ctx, h.l.svcCtx).LeaveGame(&loginpb.LeaveGameRequest{})
			}
			require.NoError(t, err)
			_, err = loginreader.Account(context.Background(), h.rdb, details)
			require.Error(t, err, "离场或断线后的旧身份必须失效")
		})
	}
}
