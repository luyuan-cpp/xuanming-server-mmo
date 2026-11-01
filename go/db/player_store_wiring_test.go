package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/zrpc"

	"db/internal/config"

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
