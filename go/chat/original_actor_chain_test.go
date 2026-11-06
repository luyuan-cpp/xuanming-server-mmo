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

func TestOriginalChatActorIsCapturedAfterWireSessionDecode(t *testing.T) {
	detail := callerauthtest.OnlineSession("original-account", 11, 7)
	// 真签名并过验签拦截器:ctx 同时带交给本服务解码的会话 metadata 与 gate 签名原件。
	ctx := callerauthtest.Incoming(t, chatpb.ClientPlayerChat_SendChat_FullMethodName, detail)
	chain := buildUnaryInterceptors(killswitch.New(killswitch.Config{}), session.OriginalIdentityInterceptor())
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
