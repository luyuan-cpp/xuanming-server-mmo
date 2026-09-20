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
	friendpb "proto/friend"
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

// call 走真拦截器,handler 里顺手把"逻辑层看到的身份"抄下来 ——
// 断言的是业务能拿到什么,而不是拦截器内部实现。
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

// 规则 1:无会话 = 内部调用(robot 直连播种 / 运维工具),放行,且不能被当成客户端。
// fromClient=false 这一条是关键:逻辑层据此回 ErrInvalidParameter,而不是去信请求体。
func TestInternalCallWithoutSessionPassesThrough(t *testing.T) {
	result := call(nil, friendpb.ClientPlayerFriend_AddFriend_FullMethodName)

	require.NoError(t, result.err)
	assert.True(t, result.handlerCalled)
	assert.False(t, result.fromClient, "没有会话的调用是内部调用,不能被当成客户端")
	assert.Zero(t, result.playerID)
}

func TestClientSessionCarriesAuthoritativePlayerID(t *testing.T) {
	md := metadata.Pairs(MetadataKey, encodeSession(t, &base.SessionDetails{SessionId: 7, PlayerId: 42}))

	result := call(md, friendpb.ClientPlayerFriend_AddFriend_FullMethodName)

	require.NoError(t, result.err)
	assert.True(t, result.handlerCalled)
	assert.True(t, result.fromClient)
	assert.Equal(t, uint64(42), result.playerID)
}

// 规则 2:带了会话却解不开 → Unauthenticated,绝不退化成"当内部调用处理"。
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
			assertRejected(t, call(md, friendpb.ClientPlayerFriend_AddFriend_FullMethodName), codes.Unauthenticated)
		})
	}
}

// 规则 3:S2C 推送方法不在白名单 → PermissionDenied,且 handler 不被调用。
//
// 这条守的是"客户端伪造好友事件"这条攻击路径:NotifyFriendEvent 为了让生成器出 Unity / robot
// 下行 handler 必须挂在 ClientPlayerFriend 上,于是它的方法名对客户端是可拨的。
// 服务端不实现它(继承 Unimplemented)只是"今天恰好拨不通",拒绝必须发生在会话层。
func TestClientCannotCallNotifyFriendEvent(t *testing.T) {
	logtest.Discard(t)
	md := metadata.Pairs(MetadataKey, encodeSession(t, &base.SessionDetails{SessionId: 7, PlayerId: 42}))

	assertRejected(t, call(md, friendpb.ClientPlayerFriend_NotifyFriendEvent_FullMethodName), codes.PermissionDenied)
}

// TestClientMethodsMatchServiceDescriptor 让新增 RPC 必须显式表态:
// ClientPlayerFriend 上除 NotifyFriendEvent 之外的方法必须全部登记进白名单,
// NotifyFriendEvent 必须不在,白名单里也不许有 ServiceDesc 上不存在的条目。
//
// 少了这条,以后加一个 RPC 忘了登记 = 该功能对客户端静默 PermissionDenied(线上表现为"点了没反应"),
// 或者反过来,加一个 S2C 推送顺手抄进白名单 = 伪造入口。两种都只有遍历 ServiceDesc 才发现得了。
func TestClientMethodsMatchServiceDescriptor(t *testing.T) {
	desc := friendpb.ClientPlayerFriend_ServiceDesc
	const s2cMethod = friendpb.ClientPlayerFriend_NotifyFriendEvent_FullMethodName

	s2cSeen := false
	for _, method := range desc.Methods {
		fullMethod := "/" + desc.ServiceName + "/" + method.MethodName
		_, allowed := ClientMethods[fullMethod]
		if fullMethod == s2cMethod {
			s2cSeen = true
			assert.False(t, allowed, "%s 是 S2C 推送方法,绝不能对客户端开放", fullMethod)
			continue
		}
		assert.True(t, allowed, "%s 是客户端协议方法,必须登记进 ClientMethods", fullMethod)
	}

	// 推送方法被改名 / 挪走时,上面的循环会静默全绿(它只是不再命中那个 if),
	// 所以单独钉一下"它确实还在这个服务上"。
	assert.True(t, s2cSeen, "ClientPlayerFriend 上找不到 %s —— proto 改了就得同步改本测试的预期", s2cMethod)

	assert.Len(t, ClientMethods, len(desc.Methods)-1,
		"ClientMethods 只能恰好是 ClientPlayerFriend 的方法减去 NotifyFriendEvent")
}
