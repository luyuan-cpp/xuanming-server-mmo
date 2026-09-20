package session

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	pb "proto/guild"
)

type callResult struct {
	handlerCalled bool
	playerID      uint64
	fromClient    bool
	err           error
}

func encodeSession(t *testing.T, detail *base.SessionDetails) string {
	t.Helper()
	raw, err := proto.Marshal(detail)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

func call(md metadata.MD, method string) callResult {
	ctx := context.Background()
	if md != nil {
		ctx = metadata.NewIncomingContext(ctx, md)
	}
	var result callResult
	_, result.err = UnaryServerInterceptor(ClientMethods)(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: method},
		func(ctx context.Context, _ any) (any, error) {
			result.handlerCalled = true
			result.playerID, result.fromClient = ClientPlayerID(ctx)
			return "ok", nil
		})
	return result
}

func assertRejected(t *testing.T, result callResult, want codes.Code) {
	t.Helper()
	assert.False(t, result.handlerCalled, "被拒绝的调用不能进入业务 handler")
	st, ok := status.FromError(result.err)
	require.True(t, ok, "应返回 gRPC status,得到 %v", result.err)
	assert.Equal(t, want, st.Code())
}

func TestInternalCallWithoutSessionPassesThrough(t *testing.T) {
	result := call(nil, pb.GuildService_UpdateGuildScore_FullMethodName)

	require.NoError(t, result.err)
	assert.True(t, result.handlerCalled)
	assert.False(t, result.fromClient, "没有会话的调用是内部调用,不能被当成客户端")
}

func TestClientSessionCarriesAuthoritativePlayerID(t *testing.T) {
	md := metadata.Pairs(MetadataKey, encodeSession(t, &base.SessionDetails{SessionId: 7, PlayerId: 42}))

	result := call(md, pb.GuildService_CreateGuild_FullMethodName)

	require.NoError(t, result.err)
	assert.True(t, result.handlerCalled)
	assert.True(t, result.fromClient)
	assert.Equal(t, uint64(42), result.playerID)
}

func TestClientCannotCallInternalMethod(t *testing.T) {
	md := metadata.Pairs(MetadataKey, encodeSession(t, &base.SessionDetails{SessionId: 7, PlayerId: 42}))

	assertRejected(t, call(md, pb.GuildService_UpdateGuildScore_FullMethodName), codes.PermissionDenied)
}

func TestBrokenSessionMetadataFailsClosed(t *testing.T) {
	cases := map[string]string{
		"not base64":         "%%%not-base64%%%",
		"not SessionDetails": base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff}),
		"missing player_id":  encodeSession(t, &base.SessionDetails{SessionId: 7}),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			md := metadata.Pairs(MetadataKey, value)
			assertRejected(t, call(md, pb.GuildService_CreateGuild_FullMethodName), codes.Unauthenticated)
		})
	}
}

// TestClientMethodsCoverEveryRPCExceptScoreWrites 让新增 RPC 必须显式表态:
// 忘了登记会在这里红,而不是悄悄变成"客户端调不到"或"客户端能调到内部方法"。
// 名单里的每一项都要么进 ClientMethods,要么进 internalOnly——没有第三种状态。
func TestClientMethodsCoverEveryRPCExceptScoreWrites(t *testing.T) {
	internalOnly := map[string]bool{"UpdateGuildScore": true, "NotifyGuildChanged": true}
	for _, method := range pb.GuildService_ServiceDesc.Methods {
		fullMethod := "/" + pb.GuildService_ServiceDesc.ServiceName + "/" + method.MethodName
		_, allowed := ClientMethods[fullMethod]
		assert.Equal(t, !internalOnly[method.MethodName], allowed, "%s 的客户端准入与预期不符", fullMethod)
	}
}

// TestNotifyPlaceholdersAreNeverClientCallable 比上面那条更强:上面靠一张手写的
// internalOnly 名单,而 Notify* 是一整类——推送占位 RPC 只为分配 message id 存在,
// 将来新增的每一个都不得对客户端开放。这里按前缀机械断言,不依赖有人记得改名单。
func TestNotifyPlaceholdersAreNeverClientCallable(t *testing.T) {
	for _, method := range pb.GuildService_ServiceDesc.Methods {
		if strings.HasPrefix(method.MethodName, "Notify") {
			_, allowed := ClientMethods["/"+pb.GuildService_ServiceDesc.ServiceName+"/"+method.MethodName]
			assert.False(t, allowed, "%s 是推送占位,不得进客户端白名单", method.MethodName)
		}
	}
}
