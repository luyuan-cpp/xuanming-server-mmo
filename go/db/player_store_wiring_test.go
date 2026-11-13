package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/zrpc"

	"db/internal/config"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/playerstore"

	"shared/accountdirectory"
)

// go/db 不连目录,选库靠 data_service:没配地址或没配密钥就拒绝启动,起来了也存不了盘。
// 不在启动时读一次:data_service 可能还没起来,NonBlock 的客户端不等它。
func TestNewStorageResolverRequiresDataServiceRpcAndSecret(t *testing.T) {
	var c config.Config
	_, err := newStorageResolver(&c)
	require.ErrorContains(t, err, "DataServiceRpc")

	c.DataServiceRpc = zrpc.RpcClientConf{Endpoints: []string{"127.0.0.1:1"}, NonBlock: true, Timeout: 2000}
	t.Setenv(accountdirectory.RouteSecretEnv, "")
	_, err = newStorageResolver(&c)
	require.ErrorContains(t, err, accountdirectory.RouteSecretEnv)

	t.Setenv(accountdirectory.RouteSecretEnv, strings.Repeat("d", 32))
	resolve, err := newStorageResolver(&c)
	require.NoError(t, err)
	require.NotNil(t, resolve)
}

func TestNewPlayerStoreServerDoesNotOpenZoneDatabase(t *testing.T) {
	var c config.Config
	c.ZoneId = 1
	c.DataServiceRpc = zrpc.RpcClientConf{Endpoints: []string{"127.0.0.1:1"}, NonBlock: true}
	t.Setenv(accountdirectory.RouteSecretEnv, strings.Repeat("r", 32))
	t.Setenv(playerstore.SceneSecretEnv, strings.Repeat("s", 32))
	stores, err := proto_sql.NewStoreRegistry(proto_sql.StoreRegistryOptions{
		Open: func(context.Context, string) (*proto_sql.GameDB, error) {
			t.Fatal("startup opened a placement database")
			return nil, nil
		},
	})
	require.NoError(t, err)
	_, err = newPlayerStoreServer(&c, stores)
	require.NoError(t, err)
	c.ServerConfig.PlayerStore.MaxRowBytes = 2 << 20
	_, err = newPlayerStoreServer(&c, stores)
	require.ErrorContains(t, err, "cannot fit")
}
