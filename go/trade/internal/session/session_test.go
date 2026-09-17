package session

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	tradepb "proto/trade"
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

// 无会话 = 内部调用(robot 直连播种),放行且不被当成客户端。
func TestInternalCallWithoutSessionPassesThrough(t *testing.T) {
	result := call(nil, tradepb.TradeAdmin_SeedListing_FullMethodName)

	require.NoError(t, result.err)
	assert.True(t, result.handlerCalled)
	assert.False(t, result.fromClient, "没有会话的调用是内部调用,不能被当成客户端")
}

func TestClientSessionCarriesAuthoritativePlayerID(t *testing.T) {
	md := metadata.Pairs(MetadataKey, encodeSession(t, &base.SessionDetails{SessionId: 7, PlayerId: 42}))

	result := call(md, tradepb.ClientPlayerJubaozhai_BrowseListings_FullMethodName)

	require.NoError(t, result.err)
	assert.True(t, result.handlerCalled)
	assert.True(t, result.fromClient)
	assert.Equal(t, uint64(42), result.playerID)
}

// 带会话调内部方法 SeedListing → PermissionDenied,handler 不被调用。
func TestClientCannotCallTradeAdmin(t *testing.T) {
	logtest.Discard(t)
	md := metadata.Pairs(MetadataKey, encodeSession(t, &base.SessionDetails{SessionId: 7, PlayerId: 42}))

	assertRejected(t, call(md, tradepb.TradeAdmin_SeedListing_FullMethodName), codes.PermissionDenied)
}

func TestBrokenSessionMetadataFailsClosed(t *testing.T) {
	logtest.Discard(t)
	cases := map[string]string{
		"not base64":         "%%%not-base64%%%",
		"not SessionDetails": base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff}),
		"missing player_id":  encodeSession(t, &base.SessionDetails{SessionId: 7}),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			md := metadata.Pairs(MetadataKey, value)
			assertRejected(t, call(md, tradepb.ClientPlayerJubaozhai_BrowseListings_FullMethodName), codes.Unauthenticated)
		})
	}
}

// TestClientMethodsMatchServiceDescriptors 让新增 RPC 必须显式表态:
// 客户端服务的方法必须全部在白名单里,TradeAdmin 的方法必须全部不在,白名单里也不许有别的东西。
func TestClientMethodsMatchServiceDescriptors(t *testing.T) {
	clientService := tradepb.ClientPlayerJubaozhai_ServiceDesc
	for _, method := range clientService.Methods {
		fullMethod := "/" + clientService.ServiceName + "/" + method.MethodName
		_, allowed := ClientMethods[fullMethod]
		assert.True(t, allowed, "%s 是客户端协议方法,必须登记进 ClientMethods", fullMethod)
	}

	adminService := tradepb.TradeAdmin_ServiceDesc
	for _, method := range adminService.Methods {
		fullMethod := "/" + adminService.ServiceName + "/" + method.MethodName
		_, allowed := ClientMethods[fullMethod]
		assert.False(t, allowed, "%s 是内部方法,绝不能对客户端开放", fullMethod)
	}

	assert.Len(t, ClientMethods, len(clientService.Methods),
		"ClientMethods 只能恰好包含 ClientPlayerJubaozhai 的方法")
}
