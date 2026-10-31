package session

import (
	"context"
	"errors"
	"testing"

	base "proto/common/base"
	"shared/roleidentity"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type loginAccountFixture struct {
	account string
	err     error
	keys    []string
}

func (f *loginAccountFixture) Get(ctx context.Context, key string) *redis.StringCmd {
	f.keys = append(f.keys, key)
	out := redis.NewStringCmd(ctx)
	out.SetVal(f.account)
	out.SetErr(f.err)
	return out
}

func TestOriginalGuildSessionRejectsMissingAccountZeroRevisionAndChangedOwner(t *testing.T) {
	for _, test := range []struct {
		name, account     string
		revision          uint64
		readErr, checkErr error
		code              codes.Code
	}{
		{"valid-original", " seller ", 7, nil, nil, codes.OK},
		{"expired-session", "", 7, redis.Nil, nil, codes.Unauthenticated},
		{"zero-revision", "seller", 0, nil, nil, codes.Unauthenticated},
		{"transferred", "seller", 7, nil, errors.New("当前已是买家修订8"), codes.PermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &loginAccountFixture{account: test.account, err: test.readErr}
			checked := false
			called := false
			guard := OriginalIdentityInterceptor(reader, func(ctx context.Context) error {
				checked = true
				original, ok := roleidentity.Original(ctx)
				require.True(t, ok)
				require.Equal(t, test.account, original.Account)
				require.Equal(t, test.revision, original.OwnerRevision)
				return test.checkErr
			})
			ctx := WithDetails(context.Background(), &base.SessionDetails{SessionId: 1, PlayerId: 11, GateInstanceId: "原gate", SessionIncarnation: 8, OwnerRevision: test.revision})
			_, err := guard(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) { called = true; return nil, nil })
			require.Equal(t, test.code, status.Code(err))
			require.Equal(t, test.code == codes.OK, called)
			if test.revision == 0 {
				require.Empty(t, reader.keys)
				require.False(t, checked)
			} else {
				require.Equal(t, []string{"login_session:{原gate:8}"}, reader.keys)
			}
		})
	}
}
