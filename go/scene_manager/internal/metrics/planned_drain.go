package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// scene 节点计划排空的指标(docs/design/scene-node-planned-drain.md §4.8)。注册在 metrics.go 的 register() 里。
// 频道迁移沿用 rebalance_migrations_total,reason 多一个 node_draining,不另起计数。

// 疏散请求(EnterSceneRequest.evacuation)解析到了哪一级,取值固定。
const (
	// EvacuationResolvedSameChannel:原频道已归别的开放节点,玩家回到同一个频道。
	EvacuationResolvedSameChannel = "same_channel"
	// EvacuationResolvedSameMap:原频道去不了,落到同一张图最空的频道。
	EvacuationResolvedSameMap = "same_map"
	// EvacuationResolvedDefaultWorld:原地图没有可去的频道(或玩家来自副本),落到默认大世界。
	EvacuationResolvedDefaultWorld = "default_world"
	// EvacuationResolvedNone:三级都没有可去的地方,回 ErrNoAvailableNode。
	EvacuationResolvedNone = "none"
)

var (
	// evacuationResolveTotal:疏散请求的解析结局。same_map / default_world 占比升高说明频道迁移没赶在疏散之前完成
	// (scene_manager 慢、目标没容量),none 说明本 zone 已无可去的节点。
	evacuationResolveTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "evacuation_resolve_total",
		Help:      "Evacuation EnterScene requests by resolution: same_channel (moved with its channel), same_map (another channel of the same map), default_world, or none (no destination).",
	}, []string{"zone_id", "result"})
)

// ObserveEvacuationResolve 记一次疏散请求的解析结局,result 取上面的常量之一。
func ObserveEvacuationResolve(zoneID uint32, result string) {
	register()
	evacuationResolveTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10), result).Inc()
}
