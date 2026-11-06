package session

import (
	"context"
	"errors"
	"testing"

	base "proto/common/base"
	pb "proto/guild"
	"shared/callerauth/callerauthtest"
	"shared/generated/clientrpc"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var errRoleChangedForTest = errors.New("当前已是买家修订8")

// 生产链上入口准入在前、归属预检在后(guild.go 的 buildUnaryInterceptors),这里按同样的先后串起来测。
func TestOriginalGuildSessionRejectsMissingAccountZeroRevisionAndChangedOwner(t *testing.T) {
	// 账号原样带首尾空格:原账号取 gate 签名原件本身,不做任何规范化。
	seller := callerauthtest.OnlineSession(" seller ", 11, 7)
	changed := func(mutate func(*base.SessionDetails)) *base.SessionDetails {
		d := proto.Clone(seller).(*base.SessionDetails)
		mutate(d)
		return d
	}
	info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_CreateGuild_FullMethodName}
	for _, test := range []struct {
		name     string
		signed   *base.SessionDetails // 为 nil:没有 gate 签名原件,只有自报的会话
		claimed  *base.SessionDetails // signed 为 nil 时自报的会话
		checkErr error
		code     codes.Code
	}{
		{"valid-original", seller, nil, nil, codes.OK},
		{"unsigned", nil, seller, nil, codes.Unauthenticated},
		// gate 签了名但会话不完整:未绑账号或缺修订都不属于在线角色,入口准入就拒,不进归属复核。
		{"missing-account", changed(func(d *base.SessionDetails) { d.AuthenticatedAccount = "" }), nil, nil, codes.PermissionDenied},
		{"zero-revision", changed(func(d *base.SessionDetails) { d.OwnerRevision = 0 }), nil, nil, codes.PermissionDenied},
		{"transferred", seller, nil, errRoleChangedForTest, codes.PermissionDenied},
		{"inconclusive", seller, nil, errors.New("目录读不到"), codes.Aborted},
	} {
		t.Run(test.name, func(t *testing.T) {
			checked := false
			called := false
			guard := OriginalIdentityInterceptor(func(ctx context.Context) error {
				checked = true
				original, ok := roleidentity.Original(ctx)
				require.True(t, ok)
				require.Equal(t, test.signed.GetAuthenticatedAccount(), original.Account)
				require.Equal(t, test.signed.GetOwnerRevision(), original.OwnerRevision)
				return test.checkErr
			}, func(err error) bool { return errors.Is(err, errRoleChangedForTest) })
			entry, err := roleidentity.EntryInterceptor(clientrpc.Callable)
			require.NoError(t, err)

			var incoming context.Context
			if test.signed != nil {
				incoming = callerauthtest.Incoming(t, info.FullMethod, test.signed)
			} else {
				incoming = callerauthtest.Unverified(t, test.claimed)
			}
			_, err = entry(incoming, nil, info, func(ctx context.Context, req any) (any, error) {
				return guard(ctx, req, info, func(ctx context.Context, req any) (any, error) { called = true; return nil, nil })
			})
			require.Equal(t, test.code, status.Code(err))
			require.Equal(t, test.code == codes.OK, called)
			// 入口准入就拒掉的身份(未签名、不完整)不进归属复核。
			require.Equal(t, test.code == codes.OK || test.checkErr != nil, checked)
		})
	}
}

// 不带会话的调用(内部调用、匿名只读例外)ctx 里没有原件,不做归属预检。
func TestOriginalGuildPrecheckSkipsCallsWithoutOriginal(t *testing.T) {
	checked, called := false, false
	guard := OriginalIdentityInterceptor(func(context.Context) error { checked = true; return nil }, nil)
	_, err := guard(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: pb.GuildService_GetPlayerGuild_FullMethodName},
		func(context.Context, any) (any, error) { called = true; return nil, nil })
	require.NoError(t, err)
	require.True(t, called)
	require.False(t, checked)
}
