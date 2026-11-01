package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"friend/internal/data"
	base "proto/common/base"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/rolefence"
	"shared/roleidentity"
	"shared/serverbase"

	redv9 "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type identityReader struct {
	account string
	err     error
	key     string
}

func (r *identityReader) Get(_ context.Context, key string) *redv9.StringCmd {
	r.key = key
	return redv9.NewStringResult(r.account, r.err)
}

func TestOriginalIdentityPreservesGateRevisionAndLoginAccount(t *testing.T) {
	reader := &identityReader{account: "原账号"}
	detail := &base.SessionDetails{PlayerId: 11, SessionId: 1, GateInstanceId: "原gate", SessionIncarnation: 8, OwnerRevision: 7}
	ctx := WithDetails(context.Background(), detail)
	detail.OwnerRevision = 99
	called := false
	guard := OriginalIdentityInterceptor(reader, time.Second, func(ctx context.Context) error {
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
	require.Equal(t, "login_session:{原gate:8}", reader.key)
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
			guard := OriginalIdentityInterceptor(&identityReader{account: "old"}, time.Second, func(context.Context) error { return checkErr })
			gate := drainproof.New()
			detail := &base.SessionDetails{PlayerId: 11, SessionId: 1, GateInstanceId: "gate", SessionIncarnation: 8, OwnerRevision: 7}
			_, err := serverbase.RootAdmission(gate)(WithDetails(context.Background(), detail), nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
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
	for _, test := range []struct {
		name               string
		revision           uint64
		gate               string
		loginErr, checkErr error
		code               codes.Code
	}{
		{"zero", 0, "gate", nil, nil, codes.Unauthenticated},
		{"legacy", 7, "", nil, nil, codes.Unauthenticated},
		{"missing-login", 7, "gate", redv9.Nil, nil, codes.Unauthenticated},
		{"transferred-or-frozen", 7, "gate", nil, data.ErrOriginalRole, codes.PermissionDenied},
		// 判不了不是拒绝:客户端要能重试。回 Aborted 不回 Unavailable,这一层在 serverbase 外面,Unavailable 会被 RootAdmission 打成 Hold。
		{"writer-gate-frozen", 7, "gate", nil, fmt.Errorf("begin: %w", authoritysql.ErrWriterFrozen), codes.Aborted},
		{"directory-unavailable", 7, "gate", nil, errors.Join(rolefence.ErrUnavailable, errors.New("data_service down")), codes.Aborted},
		{"storage-failure", 7, "gate", nil, errors.New("dial tcp: connection refused"), codes.Aborted},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &identityReader{account: "old", err: test.loginErr}
			guard := OriginalIdentityInterceptor(reader, time.Second, func(context.Context) error { return test.checkErr })
			ctx := WithDetails(context.Background(), &base.SessionDetails{PlayerId: 11, SessionId: 1, GateInstanceId: test.gate, SessionIncarnation: 8, OwnerRevision: test.revision})
			_, err := guard(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { t.Fatal("失效身份进入业务"); return nil, nil })
			require.Equal(t, test.code, status.Code(err))
		})
	}
}
