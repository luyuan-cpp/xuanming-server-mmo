package svc

import (
	"context"
	"testing"

	"friend/internal/config"
	base "proto/common/base"
	"shared/loginsession"
	"shared/playerredis"

	"github.com/alicebob/miniredis/v2"
	redv9 "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestLoginSessionClientUsesOnlyExplicitGlobalDatabase(t *testing.T) {
	server := miniredis.RunT(t)
	server.Select(0)
	require.NoError(t, server.Set("login_session:{gate:8}", "wrong-db"))
	server.Select(3)
	require.NoError(t, server.Set("login_session:{gate:8}", "original"))
	c := config.Config{GlobalLoginSessions: true, LoginRedis: playerredis.LoginReaderConfig{Addrs: []string{server.Addr()}, DB: 3, PoolSizePerNode: 8}}
	client, err := NewLoginSessionClient(c)
	require.NoError(t, err)
	require.Equal(t, 8, client.(*redv9.Client).Options().MaxActiveConns)
	t.Cleanup(func() { _ = client.Close() })
	account, err := loginsession.Account(context.Background(), client, &base.SessionDetails{SessionId: 1, GateInstanceId: "gate", SessionIncarnation: 8})
	require.NoError(t, err)
	require.Equal(t, "original", account)
	c.GlobalLoginSessions = false
	_, err = NewLoginSessionClient(c)
	require.Error(t, err, "不允许回落到共享缓存Redis")
}
