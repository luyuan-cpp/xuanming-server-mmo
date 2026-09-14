package logic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dspb "proto/data_service"
)

// HomeZoneLookup 返回玩家的归属 zone。
//
// 帮会按 zone 隔离(建帮落哪个区、能看见 / 加入哪些帮、看哪张榜),而客户端请求里的
// zone_id 是玩家自己选的,不能当依据。唯一真源是 data_service 的 player:zone:{id}
// 映射:建角时登记、合服时由 RemapHomeZoneForMerge 与公会行一起搬迁,两边始终一致。
//
// 返回 (0, nil) 表示映射里没有这名玩家(数据状态);err 非 nil 表示查询失败(故障)。
type HomeZoneLookup interface {
	HomeZone(ctx context.Context, playerID uint64) (uint32, error)
}

// DefaultHomeZoneLookupTimeout 是一次 GetPlayerHomeZone 的预算:data_service 一次 Redis 往返,
// 与 login EnterGame 的单查同值;远小于路由服 ForwardTimeoutMs,超时能以故障形式及时返回。
const DefaultHomeZoneLookupTimeout = 1500 * time.Millisecond

// unmappedErrText 是老版 data_service 对缺失映射返回 Unknown 时的消息片段;新版返回 NotFound,
// 且消息仍含这段文本(go/data_service/internal/server/dataserviceserver.go 的契约注释)。
// 与 login 的 homezone.IsUnmapped 同一判据,两个 module 不互相 import,所以各留一份。
const unmappedErrText = "no home zone mapping"

// DataServiceHomeZone 用 data_service.GetPlayerHomeZone 实现 HomeZoneLookup。
type DataServiceHomeZone struct {
	client  dspb.DataServiceClient
	timeout time.Duration
}

// NewDataServiceHomeZone。timeout<=0 用 DefaultHomeZoneLookupTimeout。
func NewDataServiceHomeZone(client dspb.DataServiceClient, timeout time.Duration) *DataServiceHomeZone {
	if timeout <= 0 {
		timeout = DefaultHomeZoneLookupTimeout
	}
	return &DataServiceHomeZone{client: client, timeout: timeout}
}

func (h *DataServiceHomeZone) HomeZone(ctx context.Context, playerID uint64) (uint32, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	resp, err := h.client.GetPlayerHomeZone(lookupCtx, &dspb.GetPlayerHomeZoneRequest{PlayerId: playerID})
	if err != nil {
		if isHomeZoneUnmapped(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("get home zone of player %d: %w", playerID, err)
	}
	return resp.GetHomeZoneId(), nil
}

func isHomeZoneUnmapped(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.NotFound:
		return true
	case codes.Unknown:
		return strings.Contains(st.Message(), unmappedErrText)
	default:
		return false
	}
}
