package team

import (
	"context"
	"errors"
	"fmt"
	"time"

	dspb "proto/data_service"
)

// 玩家 home zone 查询(设计文档 docs/design/team-system.md §D.3)。
//
// 为什么要查:队伍 zone 与成员 zone 必须写进记录(跨区校验、合服 preflight 都靠它),
// 而客户端请求里没有、也不能信任 zone。唯一真源是 data_service 的 player:zone:{id} 映射
// (建角登记、合服 RemapHomeZoneForMerge 搬迁),风格照 guild 的 home_zone.go。
//
// 查不到一律 fail-closed:不论 AllowCrossZone 开关如何,zone=0 不许写进记录。

// HomeZoneLookup 批量查玩家 home zone。
//
// 契约:
//   - 返回 map 缺某个 id(或值为 0)= 映射里没有这名玩家(数据状态),调用方回 ErrHomeZoneUnknown;
//   - err 非 nil = 查询失败(故障:data_service 不可用、超时、客户端未配置),调用方回 ErrInternal;
//   - 一次最多 2 个 id(组队只查调用者或目标);实现自带超时,调用方 ctx 仍能截断它;
//   - 线程安全。
type HomeZoneLookup interface {
	HomeZones(ctx context.Context, playerIds []uint64) (map[uint64]uint32, error)
}

// DefaultHomeZoneLookupTimeout 单次查询预算:与 guild 同值(一次 data_service Redis 往返),
// 远小于 team 的 3500ms 请求预算,超时能以故障形式及时返回。
const DefaultHomeZoneLookupTimeout = 1500 * time.Millisecond

// errHomeZoneClientMissing DataServiceRpc 没配任何目标时 svc 不建客户端(nil)。
var errHomeZoneClientMissing = errors.New("team: data_service 客户端未配置(DataServiceRpc),无法查询 home zone")

// DataServiceHomeZone 用 data_service.BatchGetPlayerHomeZone 实现 HomeZoneLookup。
type DataServiceHomeZone struct {
	client  dspb.DataServiceClient
	timeout time.Duration
}

// NewDataServiceHomeZone client 可为 nil(DataServiceRpc 未配置):此时每次查询都返回错误,
// 组队需要 home zone 的请求回 ErrInternal,match 其余功能不受影响。timeout<=0 用默认值。
func NewDataServiceHomeZone(client dspb.DataServiceClient, timeout time.Duration) *DataServiceHomeZone {
	if timeout <= 0 {
		timeout = DefaultHomeZoneLookupTimeout
	}
	return &DataServiceHomeZone{client: client, timeout: timeout}
}

// HomeZones 见 HomeZoneLookup。data_service 对缺失映射的 id 不放进 map
// (go/data_service/internal/server/dataserviceserver.go 的 BatchGetPlayerHomeZone),
// 这里原样返回,由调用方按"缺项 = 未映射"处理。
func (h *DataServiceHomeZone) HomeZones(ctx context.Context, playerIds []uint64) (map[uint64]uint32, error) {
	if h == nil || h.client == nil {
		return nil, errHomeZoneClientMissing
	}
	lookupCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	resp, err := h.client.BatchGetPlayerHomeZone(lookupCtx, &dspb.BatchGetPlayerHomeZoneRequest{PlayerIds: playerIds})
	if err != nil {
		return nil, fmt.Errorf("team: 批量查询 home zone 失败 players=%v: %w", playerIds, err)
	}
	return resp.GetPlayerZoneMap(), nil
}
