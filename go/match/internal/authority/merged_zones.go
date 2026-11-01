package authority

import (
	"strings"

	"shared/placement"
)

// 别名合服的读时解析(docs/design/player-data-tidb-scaling-plan.md §7.1、docs/ops/merge-zone-runbook.md)。
//
// 合服只写 zone_merged_into 一条映射,票和队伍记录里存的原始区不改写。判定「同一个区」时两边都解析到最终区,
// 按区查询用 Members 展开成 IN。映射缓存最多滞后一个刷新周期,而且只放宽同区判定、不决定任何资产:
// 最坏结果是一次可重试的拒绝,窗口过后自己回到正确。映射从 data_service 读(shared/mergedzone),和 guild、trade 同一套。
//
// 这些 zone 不解析,它们说的是物理节点或副本之间的抄写,不是玩家归属:
//   - 玩家位置 player:{id}:location 的 zone_id、BattleRouting.zone_id、节点注册路径的 ZoneId:定位物理节点;
//   - 票与投影、票与 launch 原件之间的相等比较(team_claim、team_launch、team_queue、candidatePeers 的 t.zone_id=r.home_zone):
//     两边抄的是同一个原始值,比的是「投影没有过期」,解析了反而放过真正的不一致。

// BindMergedZones 接入合服映射缓存。启动时写一次,在 Store 交给任何调用方之前;之后只读。
// 没绑(只有单测)等于没有任何合服,生产路径读不到映射已经拒绝启动。
func (s *Store) BindMergedZones(zones *placement.MergedZoneCache) {
	s.zones = zones
}

// MergedZones 返回当前映射快照。一次判定只取一次,两边用同一份解析,不会被一次刷新夹在中间。
// nil Store、没绑缓存都回 nil 快照,它的 Final、Members 按「没有合服」回答。
func (s *Store) MergedZones() *placement.MergedZones {
	if s == nil {
		return nil
	}
	return s.zones.Snapshot()
}

// zoneInSQL 拼 "column IN(?,?)" 和对应的绑定参数,值只走绑定参数。zones 来自 MergedZones.Members,恒含最终区,
// 个数等于合进这个最终区的原始区数(行数 = 合过的区数,跳数另有 MaxMergedZoneHops 兜底),不需要再设上限。
// 空集合不匹配任何区:不能拼出语法错误的 IN(),更不能退化成不按区过滤。
func zoneInSQL(column string, zones []uint32) (string, []any) {
	if len(zones) == 0 {
		return "1=0", nil
	}
	args := make([]any, len(zones))
	for index, zone := range zones {
		args[index] = zone
	}
	return column + " IN(" + strings.TrimSuffix(strings.Repeat("?,", len(zones)), ",") + ")", args
}
