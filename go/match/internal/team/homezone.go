package team

import (
	"context"
	"errors"
	"fmt"
	"time"

	"match/internal/authority"
)

// 玩家 home zone 查询(设计文档 docs/design/team-system.md §D.3)。
//
// 为什么要查:队伍 zone 与成员 zone 必须写进记录(跨区校验靠它),而客户端请求里没有、也不能信任 zone。
// 唯一真源是全局库 player_route,match 有这条连接,提交事务也按它复核(RequireTeamHomeZoneTx)。
//
// 不走 data_service:它的归属接口回的是解析后的最终区,而记录要写原始区。合服不改写记录,比较时两边解析到最终区
// (别名合服,docs/design/player-data-tidb-scaling-plan.md §7.1);记了最终区,撤销合服之后记录就和路由行对不上了。
//
// 查不到一律 fail-closed:不论 AllowCrossZone 开关如何,zone=0 不许写进记录。

// HomeZoneLookup 批量查玩家 home zone(原始区)。
//
// 契约:
//   - 返回 map 缺某个 id(或值为 0)= 路由表里没有这名玩家的归属(数据状态),调用方回 ErrHomeZoneUnknown;
//   - err 非 nil = 查询失败(故障:库不可用、超时),调用方回 ErrInternal;
//   - 一次最多 2 个 id(组队只查调用者或目标);实现自带超时,调用方 ctx 仍能截断它;
//   - 线程安全。
type HomeZoneLookup interface {
	HomeZones(ctx context.Context, playerIds []uint64) (map[uint64]uint32, error)
}

// DefaultHomeZoneLookupTimeout 单次查询预算,远小于 team 的 3500ms 请求预算,超时能以故障形式及时返回。
const DefaultHomeZoneLookupTimeout = 1500 * time.Millisecond

// errHomeZoneStoreMissing 权威存储没装配(只有单测)。
var errHomeZoneStoreMissing = errors.New("team: 权威存储未装配,无法查询 home zone")

// AuthorityHomeZone 用全局库 player_route 实现 HomeZoneLookup。
type AuthorityHomeZone struct {
	store   *authority.Store
	timeout time.Duration
}

// NewAuthorityHomeZone timeout<=0 用默认值。
func NewAuthorityHomeZone(store *authority.Store, timeout time.Duration) *AuthorityHomeZone {
	if timeout <= 0 {
		timeout = DefaultHomeZoneLookupTimeout
	}
	return &AuthorityHomeZone{store: store, timeout: timeout}
}

// HomeZones 见 HomeZoneLookup。没有归属的 id 不放进 map,由调用方按"缺项 = 未映射"处理。
func (h *AuthorityHomeZone) HomeZones(ctx context.Context, playerIds []uint64) (map[uint64]uint32, error) {
	if h == nil || h.store == nil {
		return nil, errHomeZoneStoreMissing
	}
	lookupCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	zones := make(map[uint64]uint32, len(playerIds))
	for _, player := range playerIds {
		zone, err := h.store.PlayerHomeZone(lookupCtx, player)
		if errors.Is(err, authority.ErrTeamRoute) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("team: 查询 home zone 失败 player=%d: %w", player, err)
		}
		zones[player] = zone
	}
	return zones, nil
}
