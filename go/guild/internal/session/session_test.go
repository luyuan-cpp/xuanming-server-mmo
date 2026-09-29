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

// TestActivityMethodsAreClientCallable 钉死 B6a 登记的五个活动 RPC 走客户端准入,且拿到的是会话里的 player_id。
// TestClientMethodsCoverEveryRPCExceptScoreWrites 只守"每个方法要么进白名单、要么进 internalOnly";
// 这里守另一侧的具体决定:历练两个在 B6a 虽是桩,也必须已对客户端开放 —— B6b 去桩时不该再有人去动安全白名单。
func TestActivityMethodsAreClientCallable(t *testing.T) {
	md := metadata.Pairs(MetadataKey, encodeSession(t, &base.SessionDetails{SessionId: 7, PlayerId: 42}))
	for _, method := range []string{
		pb.GuildService_GetGuildActivities_FullMethodName,
		pb.GuildService_LightGuildLantern_FullMethodName,
		pb.GuildService_ClaimGuildReunion_FullMethodName,
		pb.GuildService_StartGuildTrial_FullMethodName,
		pb.GuildService_RespondGuildTrialInvite_FullMethodName,
	} {
		result := call(md, method)

		require.NoError(t, result.err, method)
		assert.True(t, result.handlerCalled, method)
		assert.True(t, result.fromClient, method)
		assert.Equal(t, uint64(42), result.playerID, method)
	}
}

// TestGuildInternalMethodsAreNeverClientCallable(G10,07 §7.4.4):GuildInternal 是服务间内部服务(回档前检查),
// 它的方法全部不在 ClientMethods 里;带会话 metadata 的调用一律 PermissionDenied、进不了 handler;
// 不带会话的按内部调用放行。session.go 不需要为它改任何东西 —— "白名单外一律拒"本身就是这道闸,
// 这里钉住的是"没人把它顺手登记进白名单"。
func TestGuildInternalMethodsAreNeverClientCallable(t *testing.T) {
	service := pb.GuildInternal_ServiceDesc
	require.NotEmpty(t, service.Methods)
	md := metadata.Pairs(MetadataKey, encodeSession(t, &base.SessionDetails{SessionId: 7, PlayerId: 42}))
	for _, method := range service.Methods {
		fullMethod := "/" + service.ServiceName + "/" + method.MethodName
		_, allowed := ClientMethods[fullMethod]
		assert.False(t, allowed, "%s 是内部方法,绝不能对客户端开放", fullMethod)

		assertRejected(t, call(md, fullMethod), codes.PermissionDenied)

		internal := call(nil, fullMethod)
		require.NoError(t, internal.err, fullMethod)
		assert.True(t, internal.handlerCalled, "%s 不带会话 = 内部调用,应放行", fullMethod)
		assert.False(t, internal.fromClient, fullMethod)
	}
	assert.Equal(t, "/guildpb.GuildInternal/ListAppliedAssetOpsSince", pb.GuildInternal_ListAppliedAssetOpsSince_FullMethodName,
		"方法全名是路由表与 data_service 客户端共同依赖的契约")
}
