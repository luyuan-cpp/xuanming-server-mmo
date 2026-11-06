package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"friend/internal/data"
	base "proto/common/base"
	friendpb "proto/friend"
	"shared/authoritysql"
	"shared/callerauth/callerauthtest"
	"shared/drainproof"
	"shared/rolefence"
	"shared/roleidentity"
	"shared/serverbase"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// signedAs 让 gate 签 signed、业务层解出 claimed:两者相同即正常在线请求,不同即被改写过的声明。
// signed 为 nil 时只带会话 metadata、没有验签原件,模拟绕过 gate 签名的自报身份。
func signedAs(t *testing.T, signed, claimed *base.SessionDetails) context.Context {
	t.Helper()
	if signed == nil {
		return WithDetails(callerauthtest.Unverified(t, claimed), claimed)
	}
	return WithDetails(callerauthtest.Incoming(t, friendpb.ClientPlayerFriend_AddFriend_FullMethodName, signed), claimed)
}

func TestOriginalIdentityPreservesGateRevisionAndSignedAccount(t *testing.T) {
	detail := callerauthtest.OnlineSession("原账号", 11, 7)
	ctx := signedAs(t, detail, detail)
	detail.OwnerRevision = 99
	called := false
	guard := OriginalIdentityInterceptor(time.Second, func(ctx context.Context) error {
		original, ok := roleidentity.Original(ctx)
		require.True(t, ok)
		require.Equal(t, "原账号", original.Account)
		require.EqualValues(t, 7, original.OwnerRevision)
		require.EqualValues(t, 11, original.Role.PlayerId)
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		return nil
	})
	_, err := guard(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
		called = true
		original, ok := roleidentity.Original(ctx)
		require.True(t, ok)
		require.EqualValues(t, 7, original.OwnerRevision)
		return nil, nil
	})
	require.NoError(t, err)
	require.True(t, called)
}

// 规格 §3.6:身份检查只读。经真的发布准入跑一遍,角色变了、判不了都不打成 Hold。
func TestOriginalIdentityFailureDoesNotHoldDrain(t *testing.T) {
	for name, checkErr := range map[string]error{
		"role-changed":   data.ErrOriginalRole,
		"gate-frozen":    authoritysql.ErrWriterFrozen,
		"directory-down": errors.Join(rolefence.ErrUnavailable, errors.New("data_service down")),
		"storage":        errors.New("dial tcp: connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			guard := OriginalIdentityInterceptor(time.Second, func(context.Context) error { return checkErr })
			gate := drainproof.New()
			detail := callerauthtest.OnlineSession("old", 11, 7)
			_, err := serverbase.RootAdmission(gate)(signedAs(t, detail, detail), nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
				return guard(ctx, req, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { t.Fatal("没核过身份进入业务"); return nil, nil })
			})
			require.Error(t, err)
			gate.CloseAdmission()
			wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, gate.Wait(wait))
		})
	}
}

func TestOriginalIdentityMissingLegacyOrStaleNeverReachesBusiness(t *testing.T) {
	online := callerauthtest.OnlineSession("old", 11, 7)
	changed := func(mutate func(*base.SessionDetails)) *base.SessionDetails {
		d := proto.Clone(online).(*base.SessionDetails)
		mutate(d)
		return d
	}
	zeroRevision := changed(func(d *base.SessionDetails) { d.OwnerRevision = 0 })
	noAccount := changed(func(d *base.SessionDetails) { d.AuthenticatedAccount = "" })
	for _, test := range []struct {
		name            string
		signed, claimed *base.SessionDetails
		checkErr        error
		code            codes.Code
	}{
		{"zero", zeroRevision, zeroRevision, nil, codes.Unauthenticated},
		// 没有 gate 实例的旧会话签不了名,只能是自报身份。
		{"legacy", nil, changed(func(d *base.SessionDetails) { d.GateInstanceId = "" }), nil, codes.Unauthenticated},
		{"unsigned", nil, online, nil, codes.Unauthenticated},
		// gate 签了名但会话未绑账号:不属于在线角色。
		{"missing-account", noAccount, noAccount, nil, codes.Unauthenticated},
		// 业务层解出的声明与签名原件不符:换了归属、账号、gate 或连接代次。
		{"revision-changed", online, changed(func(d *base.SessionDetails) { d.OwnerRevision++ }), nil, codes.Unauthenticated},
		{"account-changed", online, changed(func(d *base.SessionDetails) { d.AuthenticatedAccount = "别人" }), nil, codes.Unauthenticated},
		{"gate-changed", online, changed(func(d *base.SessionDetails) { d.GateInstanceId = "gate-after-restart" }), nil, codes.Unauthenticated},
		{"incarnation-changed", online, changed(func(d *base.SessionDetails) { d.SessionIncarnation++ }), nil, codes.Unauthenticated},
		{"transferred-or-frozen", online, online, data.ErrOriginalRole, codes.PermissionDenied},
		// 判不了不是拒绝:客户端要能重试。回 Aborted 不回 Unavailable,这一层在 serverbase 外面,Unavailable 会被 RootAdmission 打成 Hold。
		{"writer-gate-frozen", online, online, fmt.Errorf("begin: %w", authoritysql.ErrWriterFrozen), codes.Aborted},
		{"directory-unavailable", online, online, errors.Join(rolefence.ErrUnavailable, errors.New("data_service down")), codes.Aborted},
		{"storage-failure", online, online, errors.New("dial tcp: connection refused"), codes.Aborted},
	} {
		t.Run(test.name, func(t *testing.T) {
			guard := OriginalIdentityInterceptor(time.Second, func(context.Context) error { return test.checkErr })
			_, err := guard(signedAs(t, test.signed, test.claimed), nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { t.Fatal("失效身份进入业务"); return nil, nil })
			require.Equal(t, test.code, status.Code(err))
		})
	}
}
