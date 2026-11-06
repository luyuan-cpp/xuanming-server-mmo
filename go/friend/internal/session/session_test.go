package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	base "proto/common/base"
	friendpb "proto/friend"
	rfpb "proto/role_fence"
	"shared/generated/clientrpc"
)

// 没有会话 = 内部调用:取不到客户端身份。
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
	_, ok = ClientPlayerID(WithDetails(context.Background(), &base.SessionDetails{SessionId: 7}))
	assert.False(t, ok, "player_id 为 0 的会话不是在线角色")
}

// 客户端能调哪些 friend 方法由生成的客户端方法集合决定(与 gate 上行白名单同源),新增 RPC 必须在 proto 上表态:
// ClientPlayerFriend 的方法除 S2C 推送 NotifyFriendEvent 外全部可调;推送与 RoleFenceParticipant 全部不可调。
// 漏标 CLIENT_RPC_PUSH 的推送 = 伪造事件的入口,只有遍历 ServiceDesc 才发现得了。
func TestGeneratedClientSetMatchesFriendServiceDescriptor(t *testing.T) {
	desc := friendpb.ClientPlayerFriend_ServiceDesc
	sawPush := false
	for _, method := range desc.Methods {
		fullMethod := "/" + desc.ServiceName + "/" + method.MethodName
		if strings.HasPrefix(method.MethodName, "Notify") {
			sawPush = true
			assert.False(t, clientrpc.Callable(fullMethod), "%s 是 S2C 推送,客户端不得发起", fullMethod)
			continue
		}
		assert.True(t, clientrpc.Callable(fullMethod), "%s 是客户端协议方法,必须在生成的客户端方法集合里", fullMethod)
	}
	assert.True(t, sawPush, "NotifyFriendEvent 应在 ClientPlayerFriend 的 ServiceDesc 上")
	assert.False(t, clientrpc.Callable(friendpb.ClientPlayerFriend_NotifyFriendEvent_FullMethodName))

	fence := rfpb.RoleFenceParticipant_ServiceDesc
	for _, method := range fence.Methods {
		fullMethod := "/" + fence.ServiceName + "/" + method.MethodName
		assert.False(t, clientrpc.Callable(fullMethod), "%s 是内部方法,绝不能对客户端开放", fullMethod)
	}
}
