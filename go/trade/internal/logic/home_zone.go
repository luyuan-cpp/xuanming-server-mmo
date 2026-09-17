package logic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"trade/internal/constants"
	"trade/internal/svc"

	dspb "proto/data_service"
)

// HomeZoneLookup 返回玩家的归属 zone(聚宝斋市场分区的唯一依据)。
//
// 客户端请求里没有也不许有 zone:唯一真源是 data_service 的 player:zone:{id} 映射
// (契约 §0/§4;建角登记、合服时由 RemapHomeZoneForMerge 搬迁)。
//
// 返回 (0, nil) 表示映射里没有这名玩家(数据状态,调用方回 TradeHomeZoneUnknown);
// err 非 nil 表示查询失败(故障,调用方回 kServiceUnavailable)。
type HomeZoneLookup interface {
	HomeZone(ctx context.Context, playerID uint64) (uint32, error)
}

// DataServiceHomeZone 用 data_service.BatchGetPlayerHomeZone 实现 HomeZoneLookup(P1-8)。
//
// 为什么用批量接口而不是 guild 的单查 GetPlayerHomeZone:契约 §4 规定的写入路径就是 Batch;
// 它**没有 NotFound 语义** —— 缺席的 id 直接不出现在 map 里,值为 0 同样视为未映射(fail-closed)。
type DataServiceHomeZone struct {
	client  dspb.DataServiceClient
	timeout time.Duration
}

// NewDataServiceHomeZone。timeout<=0 用 constants.HomeZoneLookupTimeout。
func NewDataServiceHomeZone(client dspb.DataServiceClient, timeout time.Duration) *DataServiceHomeZone {
	if timeout <= 0 {
		timeout = constants.HomeZoneLookupTimeout
	}
	return &DataServiceHomeZone{client: client, timeout: timeout}
}

var errNoDataServiceClient = errors.New("trade: data_service client is not wired")

func (h *DataServiceHomeZone) HomeZone(ctx context.Context, playerID uint64) (uint32, error) {
	if h == nil || h.client == nil {
		svc.ObserveHomeZoneLookup(svc.ResultError)
		return 0, errNoDataServiceClient
	}
	zones, err := h.batch(ctx, []uint64{playerID})
	if err != nil {
		svc.ObserveHomeZoneLookup(svc.ResultError)
		return 0, fmt.Errorf("batch get home zone of player %d: %w", playerID, err)
	}
	zone := zones[playerID] // 缺 key 与值为 0 同义:未映射
	if zone == 0 {
		svc.ObserveHomeZoneLookup(svc.ResultUnmapped)
		return 0, nil
	}
	svc.ObserveHomeZoneLookup(svc.ResultOK)
	return zone, nil
}

// batch 一次 RPC 查多个玩家。过滤掉 0 之后为空就不发请求:data_service 对空列表没有判空,
// 空 MGET 大概率报错后被当成 Unavailable 故障(地图 8.md B10)。
func (h *DataServiceHomeZone) batch(ctx context.Context, playerIDs []uint64) (map[uint64]uint32, error) {
	ids := make([]uint64, 0, len(playerIDs))
	for _, id := range playerIDs {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return map[uint64]uint32{}, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	resp, err := h.client.BatchGetPlayerHomeZone(lookupCtx, &dspb.BatchGetPlayerHomeZoneRequest{PlayerIds: ids})
	if err != nil {
		return nil, err
	}
	return resp.GetPlayerZoneMap(), nil
}
