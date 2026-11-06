package session

import (
	"context"
	"testing"

	chatpb "proto/chat"
	base "proto/common/base"
	"shared/callerauth/callerauthtest"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestOriginalChatActorCapturesOnlyOriginalGateAccountAndWireRevision(t *testing.T) {
	detail := callerauthtest.OnlineSession("original-account", 11, 7)
	ctx := WithSessionDetails(callerauthtest.Incoming(t, chatpb.ClientPlayerChat_SendChat_FullMethodName, detail), detail)
	called := false
	_, err := OriginalIdentityInterceptor()(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
		called = true
		original, ok := roleidentity.Original(ctx)
		require.True(t, ok)
		require.Equal(t, "original-account", original.Account)
		require.EqualValues(t, 7, original.OwnerRevision)
		require.EqualValues(t, 11, original.Role.PlayerId)
		return nil, nil
	})
	require.NoError(t, err)
	require.True(t, called)
}

func TestUnverifiedMismatchedOrIncompleteChatSessionNeverCreatesActor(t *testing.T) {
	signed := callerauthtest.OnlineSession("original-account", 11, 7)
	contexts := map[string]context.Context{
		"未签名的自报身份": WithSessionDetails(callerauthtest.Unverified(t, signed), signed),
	}
	// 业务层解出的声明与 gate 签名原件不符:换了连接、账号或归属。
	for name, mutate := range map[string]func(*base.SessionDetails){
		"新修订":      func(d *base.SessionDetails) { d.OwnerRevision++ },
		"换账号":      func(d *base.SessionDetails) { d.AuthenticatedAccount = "other-account" },
		"重启后的gate": func(d *base.SessionDetails) { d.GateInstanceId = "gate-after-restart" },
		"新连接代次":    func(d *base.SessionDetails) { d.SessionIncarnation++ },
	} {
		claimed := proto.Clone(signed).(*base.SessionDetails)
		mutate(claimed)
		contexts[name] = WithSessionDetails(callerauthtest.Incoming(t, chatpb.ClientPlayerChat_SendChat_FullMethodName, signed), claimed)
	}
	// gate 签了名但会话不完整:未绑账号或缺修订都不属于在线角色。
	for name, mutate := range map[string]func(*base.SessionDetails){
		"缺账号": func(d *base.SessionDetails) { d.AuthenticatedAccount = "" },
		"缺修订": func(d *base.SessionDetails) { d.OwnerRevision = 0 },
	} {
		incomplete := proto.Clone(signed).(*base.SessionDetails)
		mutate(incomplete)
		contexts[name] = WithSessionDetails(callerauthtest.Incoming(t, chatpb.ClientPlayerChat_SendChat_FullMethodName, incomplete), incomplete)
	}
	for name, ctx := range contexts {
		t.Run(name, func(t *testing.T) {
			called := false
			_, err := OriginalIdentityInterceptor()(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
				called = true
				_, ok := roleidentity.Original(ctx)
				require.False(t, ok, "不能用未签名声明或当前目录补原账号或修订")
				return nil, nil
			})
			require.NoError(t, err)
			require.True(t, called, "聊天入口验不过时照常放行,由业务按缺原件拒绝")
		})
	}
}
