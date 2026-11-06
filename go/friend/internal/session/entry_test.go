package session

import (
	"context"
	"testing"

	base "proto/common/base"
	friendpb "proto/friend"
	rfpb "proto/role_fence"
	"shared/callerauth/callerauthtest"
	"shared/generated/clientrpc"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// friend 的客户端入口只有共享的 roleidentity.EntryInterceptor(friend.go 拦截器链第 ③ 位)。
// 原角色归属不在入口预检:写方法在业务事务里锁 role_fence 复核原件,读方法经 roleReader 复核,
// 核不过回 ErrRoleNotVerified(docs/design/gate-online-identity.md §9.3,业务侧用例在 internal/logic)。
// 这里只钉住入口在 friend 方法集上的行为,以及取值函数读到的是验签原件。

var addFriend = &grpc.UnaryServerInfo{FullMethod: friendpb.ClientPlayerFriend_AddFriend_FullMethodName}

func friendEntry(t *testing.T) grpc.UnaryServerInterceptor {
	t.Helper()
	entry, err := roleidentity.EntryInterceptor(clientrpc.Callable)
	require.NoError(t, err)
	return entry
}

func TestEntryCapturesSignedAccountAndGateRevision(t *testing.T) {
	detail := callerauthtest.OnlineSession("原账号", 11, 7)
	ctx := callerauthtest.Incoming(t, addFriend.FullMethod, detail)
	detail.OwnerRevision = 99 // 签名之后再改对象,不能影响已验签的原件
	called := false
	_, err := friendEntry(t)(ctx, nil, addFriend, func(ctx context.Context, _ any) (any, error) {
		called = true
		original, ok := roleidentity.Original(ctx)
		require.True(t, ok)
		require.Equal(t, "原账号", original.Account)
		require.EqualValues(t, 7, original.OwnerRevision)
		require.EqualValues(t, 11, original.Role.PlayerId)
		player, ok := ClientPlayerID(ctx)
		require.True(t, ok)
		require.EqualValues(t, 11, player)
		return nil, nil
	})
	require.NoError(t, err)
	require.True(t, called)
}

func TestMissingOrIncompleteIdentityNeverReachesBusiness(t *testing.T) {
	online := callerauthtest.OnlineSession("old", 11, 7)
	changed := func(mutate func(*base.SessionDetails)) *base.SessionDetails {
		d := proto.Clone(online).(*base.SessionDetails)
		mutate(d)
		return d
	}
	for _, test := range []struct {
		name string
		ctx  func(t *testing.T) context.Context
		code codes.Code
	}{
		// gate 签了名但会话不完整(离场后账号与修订已清空、未绑账号):入口准入拒绝。
		{"zero-revision", func(t *testing.T) context.Context {
			return callerauthtest.Incoming(t, addFriend.FullMethod, changed(func(d *base.SessionDetails) { d.OwnerRevision = 0 }))
		}, codes.PermissionDenied},
		{"missing-account", func(t *testing.T) context.Context {
			return callerauthtest.Incoming(t, addFriend.FullMethod, changed(func(d *base.SessionDetails) { d.AuthenticatedAccount = "" }))
		}, codes.PermissionDenied},
		// 没有验签原件的自报身份(含没有 gate 实例、签不了名的旧会话)。
		{"legacy", func(t *testing.T) context.Context {
			return callerauthtest.Unverified(t, changed(func(d *base.SessionDetails) { d.GateInstanceId = "" }))
		}, codes.Unauthenticated},
		{"unsigned", func(t *testing.T) context.Context { return callerauthtest.Unverified(t, online) }, codes.Unauthenticated},
		// 不带会话调客户端方法只可能是内部进程写错了:拒绝,不能凭空造出客户端身份。
		{"no-session", func(*testing.T) context.Context { return context.Background() }, codes.PermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := friendEntry(t)(test.ctx(t), nil, addFriend, func(context.Context, any) (any, error) {
				t.Fatal("失效身份进入业务")
				return nil, nil
			})
			require.Equal(t, test.code, status.Code(err))
		})
	}
}

// 协调者调 RoleFenceParticipant 不带会话:放行,交给方法自己的 ServiceCallAuth;带会话则是客户端在调内部方法,拒绝。
func TestRoleFenceParticipantIsInternalOnly(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName}
	called := false
	_, err := friendEntry(t)(context.Background(), nil, info, func(ctx context.Context, _ any) (any, error) {
		called = true
		_, ok := ClientPlayerID(ctx)
		require.False(t, ok, "内部调用不是客户端")
		return nil, nil
	})
	require.NoError(t, err)
	require.True(t, called)

	signed := callerauthtest.Incoming(t, info.FullMethod, callerauthtest.OnlineSession("原账号", 11, 7))
	_, err = friendEntry(t)(signed, nil, info, func(context.Context, any) (any, error) {
		t.Fatal("客户端会话调到了内部方法")
		return nil, nil
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}
