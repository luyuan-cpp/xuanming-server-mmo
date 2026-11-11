package zonepolicy

import "github.com/zeromicro/go-zero/core/metric"

// refreshTotal 按结果数区服策略的每次读取。健康时只有 ok 在涨;missing 持续涨说明本区网关没在发布
// (gateway.home-zone-ids 没含本区,或网关连不上 etcd / 库),error / invalid 持续涨说明 etcd 读不到或值被写坏。
var refreshTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "zone_policy",
	Name:      "refresh_total",
	Help: "Reads of this zone's policy key in etcd (/mmorpg/zone_policy/zone/<zone>), by outcome: 'ok' = valid value; " +
		"'missing' = key absent (the queue uses the soft cap, character creation is not restricted); 'invalid' = value " +
		"could not be parsed, last known policy kept; 'error' = etcd read failed, last known policy kept.",
	Labels: []string{"outcome"},
})
