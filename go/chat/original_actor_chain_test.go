package main

import (
	"context"
	"testing"

	"chat/internal/session"
	chatpb "proto/chat"
	"shared/callerauth/callerauthtest"
	"shared/killswitch"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// 整条链走完:handler 取到的会话就是 gate 签名的验签原件,原角色归属已捕获,提交点可复核。
func TestOriginalChatActorIsCapturedByOnlineEntry(t *testing.T) {
	detail := callerauthtest.OnlineSession("original-account", 11, 7)
	ctx := callerauthtest.Incoming(t, chatpb.ClientPlayerChat_SendChat_FullMethodName, detail)
	chain := buildUnaryInterceptors(killswitch.New(killswitch.Config{}), chatEntry(t))
	called := false
	handler := chainUnary(chain, &grpc.UnaryServerInfo{FullMethod: chatpb.ClientPlayerChat_SendChat_FullMethodName}, func(ctx context.Context, _ any) (any, error) {
		called = true
		decoded, ok := session.GetSessionDetails(ctx)
		require.True(t, ok)
		require.True(t, proto.Equal(detail, decoded))
		original, ok := roleidentity.Original(ctx)
		require.True(t, ok)
		require.Equal(t, "original-account", original.Account)
		require.EqualValues(t, 7, original.OwnerRevision)
		require.EqualValues(t, 11, original.Role.PlayerId)
		return &chatpb.SendChatResponse{}, nil
	})
	_, err := handler(ctx, nil)
	require.NoError(t, err)
	require.True(t, called)
}
