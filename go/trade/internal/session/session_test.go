package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	base "proto/common/base"
	rfpb "proto/role_fence"
	tradepb "proto/trade"
	"shared/generated/clientrpc"
)

// 没有会话 = 内部调用(robot 直连播种):取不到客户端身份。
func TestNoSessionIsNotAClient(t *testing.T) {
	_, ok := ClientPlayerID(context.Background())
	assert.False(t, ok, "没有会话的调用是内部调用,不能被当成客户端")
	assert.Nil(t, Details(context.Background()))
}

func TestClientSessionCarriesAuthoritativePlayerID(t *testing.T) {
	ctx := WithDetails(context.Background(), &base.SessionDetails{SessionId: 7, PlayerId: 42})

	player, ok := ClientPlayerID(ctx)
	require.True(t, ok)
	assert.Equal(t, uint64(42), player)
	detail := Details(ctx)
	require.NotNil(t, detail)
	detail.PlayerId = 1
	again, _ := ClientPlayerID(ctx)
	assert.Equal(t, uint64(42), again, "Details 返回副本,改它不影响 ctx 里的会话")
}

func TestSessionWithoutPlayerIsNotAClient(t *testing.T) {
	_, ok := ClientPlayerID(WithDetails(context.Background(), &base.SessionDetails{SessionId: 7}))
	assert.False(t, ok)
}

// 客户端能调哪些 trade 方法由生成的客户端方法集合决定(与 gate 上行白名单同源),新增 RPC 必须在 proto 上表态:
// ClientPlayerJubaozhai 的方法全部可调,TradeAdmin 与 RoleFenceParticipant 的方法全部不可调。
func TestGeneratedClientSetMatchesTradeServiceDescriptors(t *testing.T) {
	clientService := tradepb.ClientPlayerJubaozhai_ServiceDesc
	for _, method := range clientService.Methods {
		fullMethod := "/" + clientService.ServiceName + "/" + method.MethodName
		assert.True(t, clientrpc.Callable(fullMethod), "%s 是客户端协议方法,必须在生成的客户端方法集合里", fullMethod)
	}
	for _, desc := range []struct {
		name    string
		methods []string
	}{
		{tradepb.TradeAdmin_ServiceDesc.ServiceName, methodNames(tradepb.TradeAdmin_ServiceDesc.Methods)},
		{rfpb.RoleFenceParticipant_ServiceDesc.ServiceName, methodNames(rfpb.RoleFenceParticipant_ServiceDesc.Methods)},
	} {
		for _, method := range desc.methods {
			fullMethod := "/" + desc.name + "/" + method
			assert.False(t, clientrpc.Callable(fullMethod), "%s 是内部方法,绝不能对客户端开放", fullMethod)
		}
	}
}

func methodNames(methods []grpc.MethodDesc) []string {
	names := make([]string, 0, len(methods))
	for _, method := range methods {
		names = append(names, method.MethodName)
	}
	return names
}
