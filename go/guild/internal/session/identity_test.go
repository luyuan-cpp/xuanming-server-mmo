package session

import (
	"context"
	"errors"
	"testing"

	base "proto/common/base"
	pb "proto/guild"
	"shared/callerauth/callerauthtest"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var errRoleChangedForTest = errors.New("当前已是买家修订8")

func TestOriginalGuildSessionRejectsMissingAccountZeroRevisionAndChangedOwner(t *testing.T) {
	// 账号原样带首尾空格:原账号取 gate 签名原件本身,不做任何规范化。
	seller := callerauthtest.OnlineSession(" seller ", 11, 7)
	changed := func(mutate func(*base.SessionDetails)) *base.SessionDetails {
		d := proto.Clone(seller).(*base.SessionDetails)
		mutate(d)
		return d
	}
	zeroRevision := changed(func(d *base.SessionDetails) { d.OwnerRevision = 0 })
	noAccount := changed(func(d *base.SessionDetails) { d.AuthenticatedAccount = "" })
	for _, test := range []struct {
		name            string
		signed, claimed *base.SessionDetails // signed 为 nil:没有 gate 签名原件,只有自报的会话
		checkErr        error
		code            codes.Code
	}{
		{"valid-original", seller, seller, nil, codes.OK},
		{"unsigned", nil, seller, nil, codes.Unauthenticated},
		// gate 签了名但会话不完整:未绑账号或缺修订都不属于在线角色。
		{"missing-account", noAccount, noAccount, nil, codes.Unauthenticated},
		{"zero-revision", zeroRevision, zeroRevision, nil, codes.Unauthenticated},
		// 业务层解出的声明与签名原件不符:换了归属、账号、gate 或连接代次。
		{"revision-changed", seller, changed(func(d *base.SessionDetails) { d.OwnerRevision++ }), nil, codes.Unauthenticated},
		{"account-changed", seller, changed(func(d *base.SessionDetails) { d.AuthenticatedAccount = "buyer" }), nil, codes.Unauthenticated},
		{"gate-changed", seller, changed(func(d *base.SessionDetails) { d.GateInstanceId = "gate-after-restart" }), nil, codes.Unauthenticated},
		{"incarnation-changed", seller, changed(func(d *base.SessionDetails) { d.SessionIncarnation++ }), nil, codes.Unauthenticated},
		{"transferred", seller, seller, errRoleChangedForTest, codes.PermissionDenied},
		{"inconclusive", seller, seller, errors.New("目录读不到"), codes.Aborted},
	} {
		t.Run(test.name, func(t *testing.T) {
			checked := false
			called := false
			guard := OriginalIdentityInterceptor(func(ctx context.Context) error {
				checked = true
				original, ok := roleidentity.Original(ctx)
				require.True(t, ok)
				require.Equal(t, test.claimed.GetAuthenticatedAccount(), original.Account)
				require.Equal(t, test.claimed.GetOwnerRevision(), original.OwnerRevision)
				return test.checkErr
			}, func(err error) bool { return errors.Is(err, errRoleChangedForTest) })
			incoming := callerauthtest.Unverified(t, test.claimed)
			if test.signed != nil {
				incoming = callerauthtest.Incoming(t, pb.GuildService_CreateGuild_FullMethodName, test.signed)
			}
			_, err := guard(WithDetails(incoming, test.claimed), nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) { called = true; return nil, nil })
			require.Equal(t, test.code, status.Code(err))
			require.Equal(t, test.code == codes.OK, called)
			// 未经签名原件证实的身份不进归属复核。
			require.Equal(t, test.code != codes.Unauthenticated, checked)
		})
	}
}
