package loginsession

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	base "proto/common/base"
)

type loginReader struct {
	key   string
	value string
	calls int
}

func (r *loginReader) Get(_ context.Context, key string) *redis.StringCmd {
	r.key = key
	r.calls++
	return redis.NewStringResult(r.value, nil)
}
func TestConnectionAccountUsesOriginalGateIncarnationAndPreservesAccountBytes(t *testing.T) {
	r := &loginReader{value: " Seller 中文 "}
	in := &base.SessionDetails{SessionId: 7, GateInstanceId: "original-gate", SessionIncarnation: 19, PlayerId: 11}
	account, err := Account(context.Background(), r, in)
	require.NoError(t, err)
	require.Equal(t, " Seller 中文 ", account)
	require.Equal(t, "login_session:{original-gate:19}", r.key)
	in.SessionId = 8
	_, err = Account(context.Background(), r, in)
	require.NoError(t, err)
	require.Equal(t, "login_session:{original-gate:19}", r.key)
}
func TestLegacyOrMissingConnectionCannotBorrowSessionIDKey(t *testing.T) {
	for _, in := range []*base.SessionDetails{nil, {SessionId: 7}, {SessionId: 7, GateInstanceId: "g"}, {SessionId: 7, SessionIncarnation: 1}} {
		r := &loginReader{value: "victim"}
		_, err := Account(context.Background(), r, in)
		require.Error(t, err)
		require.Zero(t, r.calls)
	}
}
