package metrics

import "github.com/prometheus/client_golang/prometheus"

// 战斗剧本的指标(docs/design/battle-script.md §9)。label 只用封闭集合,script_id / player_id 一律只进日志。
var (
	battleScriptResolveTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "battle_script_resolve_total",
		Help: "建房时每一方的剧本解析结局:side = player / monster;outcome = chosen_official / chosen_player / bound / none / fell_back。fell_back 表示选用没用上(读库失败、剧本失效、配表变了),已退回官方绑定或不带剧本。",
	}, []string{"side", "outcome"})
	battleScriptRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "battle_script_request_total",
		Help: "剧本接口的结局:rpc = list / submit / delete / loadout / review_list / review;result = ok / rejected / denied / error。",
	}, []string{"rpc", "result"})
)

func battleScriptCollectors() []prometheus.Collector {
	return []prometheus.Collector{battleScriptResolveTotal, battleScriptRequestTotal}
}

// ObserveBattleScriptResolve 记建房时一方的剧本解析结局。
func ObserveBattleScriptResolve(side, outcome string) {
	battleScriptResolveTotal.WithLabelValues(side, outcome).Inc()
}

// ObserveBattleScriptRequest 记一次剧本接口调用的结局。
func ObserveBattleScriptRequest(rpc, result string) {
	battleScriptRequestTotal.WithLabelValues(rpc, result).Inc()
}
