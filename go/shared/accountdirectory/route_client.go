package accountdirectory

import (
	"context"
	"errors"
	"time"

	dspb "proto/data_service"
	"shared/serviceauth"
)

const (
	// RouteCaller 是 go/db 读落点时的 ServiceCallAuth.caller。只有它能调 GetPlayerRoute。
	RouteCaller = "db"
	// RouteSecretEnv 是这个调用方的密钥。和角色护栏的调用方同一命名,互不复用。
	RouteSecretEnv = "MMORPG_ROLE_FENCE_SECRET_DB"

	routeMethod = "GetPlayerRoute"
	// routeReadTimeout 是读一次落点的上限。认领时才读,读不到回暂不可用,scene 过一会儿重试。
	routeReadTimeout = 2 * time.Second
)

// RouteClient 经 data_service 读玩家的落点。go/db 选库用,它不连目录。
type RouteClient struct {
	rpc    dspb.DataServiceClient
	secret string
}

// NewRouteClient 校验密钥。密钥不合格就回错误,调用方拒绝启动:存盘接口选不了库,起来也没用。
func NewRouteClient(rpc dspb.DataServiceClient, secret string) (*RouteClient, error) {
	if rpc == nil {
		return nil, errors.New("落点读取缺 data_service 客户端")
	}
	if _, err := serviceauth.New(map[string]string{RouteCaller: secret}); err != nil {
		return nil, err
	}
	return &RouteClient{rpc: rpc, secret: secret}, nil
}

// StorageOf 读玩家的落点。found=false 只表示目录确定没有这名玩家的落点;读失败一律回 error,不能当成没有。
func (c *RouteClient) StorageOf(ctx context.Context, player uint64) (storageID uint32, found bool, err error) {
	in := &dspb.GetPlayerRouteRequest{PlayerId: player}
	if err := serviceauth.Sign(MethodPrefix+routeMethod, in, RouteCaller, c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return 0, false, errors.Join(ErrUnavailable, err)
	}
	call, cancel := context.WithTimeout(ctx, routeReadTimeout)
	defer cancel()
	out, err := c.rpc.GetPlayerRoute(call, in)
	if err != nil {
		return 0, false, errors.Join(ErrUnavailable, err)
	}
	if out.GetRetry() {
		return 0, false, ErrUnavailable
	}
	if !out.GetFound() {
		return 0, false, nil
	}
	if out.GetStorageId() == 0 {
		return 0, false, ErrUnavailable
	}
	return out.GetStorageId(), true, nil
}
