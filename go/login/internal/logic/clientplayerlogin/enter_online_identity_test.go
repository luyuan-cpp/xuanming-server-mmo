package clientplayerloginlogic

import (
	"context"
	"testing"

	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	base "proto/common/base"
	loginpb "proto/login"
	smpb "proto/scene_manager"
	"shared/callerauth/callerauthtest"
	"shared/generated/clientrpc"
	"shared/roleidentity"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type acceptedIdentityScene struct {
	smpb.SceneManagerClient
	calls int
}

func (s *acceptedIdentityScene) EnterScene(context.Context, *smpb.EnterSceneRequest, ...grpc.CallOption) (*smpb.EnterSceneResponse, error) {
	s.calls++
	return &smpb.EnterSceneResponse{}, nil
}

// 完整异步入场后，登录态保留到离场，并把原身份通过 BindSessionEvent 交给在线入口。
// 不能只测 applyLoadedPlayerSession：旧清理发生在它成功返回之后。
// 在线域不读登录态；gate 实际清空身份由 C++ Bind / ResetSceneRoute / Build 测试覆盖。
func TestEnterGameKeepsLoginSessionAndPublishesOriginalIdentity(t *testing.T) {
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
			require.NotNil(t, binder.last)
			require.Equal(t, identityPlayerID, binder.last.PlayerId)
			require.Equal(t, enterWiringAccount, binder.last.AuthenticatedAccount)
			require.Equal(t, details.SessionIncarnation, binder.last.SessionIncarnation)
			require.NotZero(t, binder.last.OwnerRevision, "绑定原身份必须携带实际归属修订")
			ref, ok := loginsession.RefOf(details)
			require.True(t, ok)
			account, err := loginsession.GetAccount(context.Background(), h.rdb, ref)
			require.NoError(t, err, "进场成功不能提前删除本连接的登录态")
			require.Equal(t, enterWiringAccount, account)

			// 仅在 gate / Go 接缝将已捕获的 Bind 字段放入会话头，使用现有真实签名、验签与入口准入。
			// 不模拟 gate 的绑定或清空实现，也不把 Redis 删除当成已签名请求的即时撤销证明。
			const onlineMethod = "/match.MatchService/JoinQueue"
			onlineDetails := proto.Clone(details).(*base.SessionDetails)
			onlineDetails.PlayerId = binder.last.PlayerId
			onlineDetails.AuthenticatedAccount = binder.last.AuthenticatedAccount
			onlineDetails.OwnerRevision = binder.last.OwnerRevision
			entry, err := roleidentity.EntryInterceptor(clientrpc.Callable)
			require.NoError(t, err)
			admitted := false
			_, err = entry(callerauthtest.Incoming(t, onlineMethod, onlineDetails), nil,
				&grpc.UnaryServerInfo{FullMethod: onlineMethod}, func(ctx context.Context, _ any) (any, error) {
					admitted = true
					original, ok := roleidentity.Original(ctx)
					require.True(t, ok)
					require.Equal(t, enterWiringAccount, original.Account)
					require.Equal(t, binder.last.OwnerRevision, original.OwnerRevision)
					require.Equal(t, identityPlayerID, original.Role.GetPlayerId())
					return nil, nil
				})
			require.NoError(t, err, "在线入口必须按验签原件捕获本次入场的原身份")
			require.True(t, admitted)
			admitted = false
			_, err = entry(callerauthtest.Unverified(t, onlineDetails), nil,
				&grpc.UnaryServerInfo{FullMethod: onlineMethod}, func(context.Context, any) (any, error) {
					admitted = true
					return nil, nil
				})
			require.Equal(t, codes.Unauthenticated, status.Code(err), "普通会话 metadata 不能冒充验签原件")
			require.False(t, admitted)

			h.l.svcCtx.PlayerLocatorClient = &fenceLocator{}
			if action == "disconnect" {
				_, err = NewDisconnectLogic(h.l.ctx, h.l.svcCtx).Disconnect(&loginpb.LoginNodeDisconnectRequest{SessionId: enterWiringSessionID})
			} else {
				_, err = NewLeaveGameLogic(h.l.ctx, h.l.svcCtx).LeaveGame(&loginpb.LeaveGameRequest{})
			}
			require.NoError(t, err)
			_, err = loginsession.GetAccount(context.Background(), h.rdb, ref)
			require.ErrorIs(t, err, redis.Nil, "离场或断线必须删除本连接的登录态")
		})
	}
}
