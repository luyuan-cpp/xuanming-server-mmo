package session

import (
	"context"
	"errors"
	"testing"
	"time"

	base "proto/common/base"
	"shared/roleidentity"

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
		{"transferred-or-frozen", 7, "gate", nil, errors.New("stale"), codes.PermissionDenied},
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
