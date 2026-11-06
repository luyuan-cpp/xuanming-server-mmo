package main

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"match/internal/pkg/ctxkeys"
	base "proto/common/base"
	dspb "proto/data_service"
	matchpb "proto/match"
	"shared/callerauth/callerauthtest"
	"shared/roleidentity"
)

// joinQueueMethod 是测试链路模拟的客户端方法;签名绑定方法名,验签原件只对这个方法成立。
const joinQueueMethod = matchpb.MatchService_JoinQueue_FullMethodName

// originalRoleSession 是已绑定角色的在线会话:原账号、原修订与 gate 连接身份齐全。
func originalRoleSession() *base.SessionDetails {
	return callerauthtest.OnlineSession("原账号", 11, 9)
}

// originalRoleChain 按生产顺序串起会话解码与原角色捕获;ctx 里有没有验签原件由调用方决定。
// 拦截器不持有任何存储句柄:原件捕获查不了当前 SQL 或 login Redis,入口无从给旧会话补权。
func originalRoleChain(ctx context.Context, handler grpc.UnaryHandler) (any, error) {
	info := &grpc.UnaryServerInfo{FullMethod: joinQueueMethod}
	return sessionInterceptor(ctx, &matchpb.JoinQueueRequest{}, info, func(decoded context.Context, request any) (any, error) {
		return originalRoleInterceptor(decoded, request, info, handler)
	})
}

// withClaimedSession 保留 ctx 里的验签原件,只把透传的会话 metadata 换成另一份声明,模拟业务解码结果与原件不符。
func withClaimedSession(t *testing.T, ctx context.Context, claimed *base.SessionDetails) context.Context {
	t.Helper()
	raw, err := proto.Marshal(claimed)
	require.NoError(t, err)
	md, _ := metadata.FromIncomingContext(ctx)
	md = md.Copy()
	md.Set(sessionMetadataKey, base64.StdEncoding.EncodeToString(raw))
	return metadata.NewIncomingContext(ctx, md)
}

func TestOriginalRoleInterceptorCapturesSignedAccountAndWireRevisionAfterSessionDecode(t *testing.T) {
	detail := originalRoleSession()
	calls := 0
	result, err := originalRoleChain(callerauthtest.Incoming(t, joinQueueMethod, detail), func(ctx context.Context, _ any) (any, error) {
		calls++
		decoded, ok := ctxkeys.GetSessionDetails(ctx)
		require.True(t, ok)
		require.True(t, proto.Equal(detail, decoded), "原会话字段与未知字段必须完整保留")
		captured, ok := roleidentity.Original(ctx)
		require.True(t, ok)
		require.Equal(t, "原账号", captured.Account)
		require.EqualValues(t, 9, captured.OwnerRevision)
		require.EqualValues(t, 11, captured.Role.PlayerId)
		copy, ok := roleidentity.Original(ctx)
		require.True(t, ok)
		copy.Account = "不得改写上下文"
		again, ok := roleidentity.Original(ctx)
		require.True(t, ok)
		require.Equal(t, "原账号", again.Account)
		return captured, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, "原账号", result.(*dspb.AccountRoleOwnership).Account)
}

// 未签名的自报身份、签了名但缺修订或缺账号、解码不出会话,一律 PermissionDenied 且不进 handler。
func TestOriginalRoleInterceptorRejectsUnsignedOrIncompleteSession(t *testing.T) {
	for _, mode := range []string{"unsigned", "unsigned-no-gate-uuid", "unsigned-no-incarnation", "zero-revision", "no-account", "bad-session-bytes", "bad-session-protobuf"} {
		t.Run(mode, func(t *testing.T) {
			detail := originalRoleSession()
			var ctx context.Context
			switch mode {
			case "unsigned":
				ctx = callerauthtest.Unverified(t, detail)
			case "unsigned-no-gate-uuid":
				// 缺 gate 实例或连接代次的会话过不了验签拦截器,到这里只能是没有原件的自报身份。
				detail.GateInstanceId = ""
				ctx = callerauthtest.Unverified(t, detail)
			case "unsigned-no-incarnation":
				detail.SessionIncarnation = 0
				ctx = callerauthtest.Unverified(t, detail)
			case "zero-revision":
				detail.OwnerRevision = 0
				ctx = callerauthtest.Incoming(t, joinQueueMethod, detail)
			case "no-account":
				// gate 只在角色绑定后才写账号;即使签了名,缺账号的会话也不能授予在线角色身份。
				detail.AuthenticatedAccount = ""
				ctx = callerauthtest.Incoming(t, joinQueueMethod, detail)
			case "bad-session-bytes":
				ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionMetadataKey, "%%%"))
			case "bad-session-protobuf":
				ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionMetadataKey, base64.StdEncoding.EncodeToString([]byte{0xff})))
			}
			calls := 0
			_, err := originalRoleChain(ctx, func(context.Context, any) (any, error) { calls++; return nil, nil })
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			require.Zero(t, calls)
		})
	}
}

// 透传的会话声明与验签原件不符时拒绝:不能借另一条连接、另一个账号或另一次归属的签名进匹配。
func TestOriginalRoleInterceptorRejectsClaimsThatDifferFromSignedOriginal(t *testing.T) {
	for name, mutate := range map[string]func(*base.SessionDetails){
		"新修订":      func(d *base.SessionDetails) { d.OwnerRevision++ },
		"换角色":      func(d *base.SessionDetails) { d.PlayerId++ },
		"换账号":      func(d *base.SessionDetails) { d.AuthenticatedAccount = "别人" },
		"重启后的gate": func(d *base.SessionDetails) { d.GateInstanceId = "gate-after-restart" },
		"上一连接代次":   func(d *base.SessionDetails) { d.SessionIncarnation-- },
	} {
		t.Run(name, func(t *testing.T) {
			signed := originalRoleSession()
			claimed := proto.Clone(signed).(*base.SessionDetails)
			mutate(claimed)
			ctx := withClaimedSession(t, callerauthtest.Incoming(t, joinQueueMethod, signed), claimed)
			calls := 0
			_, err := originalRoleChain(ctx, func(context.Context, any) (any, error) { calls++; return nil, nil })
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			require.Zero(t, calls)
		})
	}
}

func TestOriginalRoleInterceptorInternalCallCannotManufactureClientActor(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: matchpb.MatchInternal_CancelActivityBattle_FullMethodName}
	calls := 0
	_, err := sessionInterceptor(context.Background(), &matchpb.CancelActivityBattleRequest{}, info, func(ctx context.Context, request any) (any, error) {
		return originalRoleInterceptor(ctx, request, info, func(bound context.Context, _ any) (any, error) {
			calls++
			_, ok := roleidentity.Original(bound)
			require.False(t, ok)
			return nil, nil
		})
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}
