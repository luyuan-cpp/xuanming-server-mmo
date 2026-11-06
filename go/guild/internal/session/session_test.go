package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "proto/guild"
	"shared/callerauth"
	"shared/callerauth/callerauthtest"
	"shared/generated/clientrpc"
	"shared/roleidentity"
)

type callResult struct {
	handlerCalled bool
	playerID      uint64
	fromClient    bool
	err           error
}

// call 让一次调用过 guild 装配的同一个入口准入(GetPlayerGuild 为匿名只读例外)。
func call(t *testing.T, ctx context.Context, method string) callResult {
	t.Helper()
	entry, err := roleidentity.EntryInterceptor(clientrpc.Callable, pb.GuildService_GetPlayerGuild_FullMethodName)
	require.NoError(t, err)
	var result callResult
	_, result.err = entry(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(ctx context.Context, _ any) (any, error) {
			result.handlerCalled = true
			result.playerID, result.fromClient = ClientPlayerID(ctx)
			return "ok", nil
		})
	return result
}

func signedClient(t *testing.T, method string) context.Context {
	t.Helper()
	return callerauthtest.Incoming(t, method, callerauthtest.OnlineSession("guild-account", 42, 7))
}

func assertRejected(t *testing.T, result callResult, want codes.Code) {
	t.Helper()
	assert.False(t, result.handlerCalled, "被拒绝的调用不能进入业务 handler")
	st, ok := status.FromError(result.err)
	require.True(t, ok, "应返回 gRPC status,得到 %v", result.err)
	assert.Equal(t, want, st.Code())
}

func TestInternalCallWithoutSessionPassesThrough(t *testing.T) {
	result := call(t, context.Background(), pb.GuildService_UpdateGuildScore_FullMethodName)

	require.NoError(t, result.err)
	assert.True(t, result.handlerCalled)
	assert.False(t, result.fromClient, "没有会话的调用是内部调用,不能被当成客户端")
}

func TestClientSessionCarriesAuthoritativePlayerID(t *testing.T) {
	result := call(t, signedClient(t, pb.GuildService_CreateGuild_FullMethodName), pb.GuildService_CreateGuild_FullMethodName)

	require.NoError(t, result.err)
	assert.True(t, result.handlerCalled)
	assert.True(t, result.fromClient)
	assert.Equal(t, uint64(42), result.playerID)
}

func TestClientCannotCallInternalMethod(t *testing.T) {
	assertRejected(t, call(t, signedClient(t, pb.GuildService_UpdateGuildScore_FullMethodName), pb.GuildService_UpdateGuildScore_FullMethodName),
		codes.PermissionDenied)
}

// 会话头没有验签原件(坏头、自报)一律 Unauthenticated,不能退化成「当内部调用处理」。
func TestBrokenSessionMetadataFailsClosed(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"not base64": metadata.NewIncomingContext(context.Background(), metadata.Pairs(callerauth.MetaSessionDetail, "%%%not-base64%%%")),
		"unsigned":   callerauthtest.Unverified(t, callerauthtest.OnlineSession("guild-account", 42, 7)),
	} {
		t.Run(name, func(t *testing.T) {
			assertRejected(t, call(t, ctx, pb.GuildService_CreateGuild_FullMethodName), codes.Unauthenticated)
		})
	}
}

// 客户端能调哪些 GuildService 方法由生成的客户端方法集合决定(与 gate 上行白名单同源),新增 RPC 必须在 proto 上表态:
// 只有 UpdateGuildScore(SERVER_ONLY)与 NotifyGuildChanged(PUSH)不可调,其余全部可调。
func TestGeneratedClientSetCoversEveryGuildRPCExceptScoreWritesAndPushes(t *testing.T) {
	internalOnly := map[string]bool{"UpdateGuildScore": true, "NotifyGuildChanged": true}
	for _, method := range pb.GuildService_ServiceDesc.Methods {
		fullMethod := "/" + pb.GuildService_ServiceDesc.ServiceName + "/" + method.MethodName
		assert.Equal(t, !internalOnly[method.MethodName], clientrpc.Callable(fullMethod), "%s 的客户端准入与预期不符", fullMethod)
	}
}

// 推送占位 RPC 只为分配 message id 存在,将来新增的每一个都不得对客户端开放。按前缀机械断言,不依赖有人记得改名单。
func TestNotifyPlaceholdersAreNeverClientCallable(t *testing.T) {
	for _, method := range pb.GuildService_ServiceDesc.Methods {
		if strings.HasPrefix(method.MethodName, "Notify") {
			assert.False(t, clientrpc.Callable("/"+pb.GuildService_ServiceDesc.ServiceName+"/"+method.MethodName),
				"%s 是推送占位,不得对客户端开放", method.MethodName)
		}
	}
}

// TestActivityMethodsAreClientCallable 钉死 B6a 登记的五个活动 RPC 走客户端准入,且拿到的是会话里的 player_id。
func TestActivityMethodsAreClientCallable(t *testing.T) {
	for _, method := range []string{
		pb.GuildService_GetGuildActivities_FullMethodName,
		pb.GuildService_LightGuildLantern_FullMethodName,
		pb.GuildService_ClaimGuildReunion_FullMethodName,
		pb.GuildService_StartGuildTrial_FullMethodName,
		pb.GuildService_RespondGuildTrialInvite_FullMethodName,
	} {
		result := call(t, signedClient(t, method), method)

		require.NoError(t, result.err, method)
		assert.True(t, result.handlerCalled, method)
		assert.True(t, result.fromClient, method)
		assert.Equal(t, uint64(42), result.playerID, method)
	}
}

// TestGuildInternalMethodsAreNeverClientCallable(G10,07 §7.4.4):GuildInternal 是服务间内部服务(回档前检查),
// 它的方法全部不在客户端方法集合里;带会话的调用一律 PermissionDenied、进不了 handler;不带会话的按内部调用放行。
func TestGuildInternalMethodsAreNeverClientCallable(t *testing.T) {
	service := pb.GuildInternal_ServiceDesc
	require.NotEmpty(t, service.Methods)
	for _, method := range service.Methods {
		fullMethod := "/" + service.ServiceName + "/" + method.MethodName
		assert.False(t, clientrpc.Callable(fullMethod), "%s 是内部方法,绝不能对客户端开放", fullMethod)

		assertRejected(t, call(t, signedClient(t, fullMethod), fullMethod), codes.PermissionDenied)

		internal := call(t, context.Background(), fullMethod)
		require.NoError(t, internal.err, fullMethod)
		assert.True(t, internal.handlerCalled, "%s 不带会话 = 内部调用,应放行", fullMethod)
		assert.False(t, internal.fromClient, fullMethod)
	}
	assert.Equal(t, "/guildpb.GuildInternal/ListAppliedAssetOpsSince", pb.GuildInternal_ListAppliedAssetOpsSince_FullMethodName,
		"方法全名是路由表与 data_service 客户端共同依赖的契约")
}

// 匿名只读例外只有 GetPlayerGuild(mail 领附件读所在帮):不带会话也放行,但它眼里没有客户端身份。
func TestOnlyGetPlayerGuildIsAnonymousRead(t *testing.T) {
	read := call(t, context.Background(), pb.GuildService_GetPlayerGuild_FullMethodName)
	require.NoError(t, read.err)
	assert.True(t, read.handlerCalled)
	assert.False(t, read.fromClient)

	assertRejected(t, call(t, context.Background(), pb.GuildService_CreateGuild_FullMethodName), codes.PermissionDenied)
}
