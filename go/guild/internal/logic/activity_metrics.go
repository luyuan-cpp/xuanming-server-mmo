package logic

// 帮会活动的指标(docs/design/guild-phase2/06-activities.md §6.15)。
//
// label 纪律(AGENTS.md §9):取值全部是本文件写死的有限集合,**不放** player_id / guild_id,也不放 activity_id ——
// activity_id 虽是配表 id,但策划每届节庆都会加新行,放进 label 等于让时间序列随档期无限增长。
// 需要按活动区分时看 type(三种玩法),按届区分看日志。
//
// 用 go-zero core/metric 而不是 06 正文写的 promauto:guild 的配置内嵌 zrpc.RpcServerConf、etc/guild.yaml 配了
// Prometheus.Host,go-zero 的指标开关是打开的(92-handoff §8.2);同一服务里两套注册方式并存,排障时会漏看一半。
// 与 economy_logic.go 的 guild_economy_requests_total 同一写法。

import "github.com/zeromicro/go-zero/core/metric"

// type label。all 只给读 RPC 用:GetGuildActivities 一次返回全部类型,拆不到某一种玩法上。
const (
	activityTypeLantern = "lantern"
	activityTypeReunion = "reunion"
	activityTypeTrial   = "trial"
	activityTypeAll     = "all"
)

// action label。invite / respond 在 B6a 是桩(恒回未开放),B6b 起才有真实结果;launch / settle 归 B6b。
const (
	activityActionView    = "view"
	activityActionLight   = "light"
	activityActionClaim   = "claim"
	activityActionInvite  = "invite"
	activityActionRespond = "respond"
)

// result label。每个响应都经 activityResultOf 从"tip id / gRPC 错误码"推出,不在各分支手写字符串:
// 手写迟早有一支漏计或写错,而 tip → label 的映射只有一处就不会。
const (
	activityResultOK            = "ok"
	activityResultNotOpen       = "not_open"
	activityResultLevelLow      = "level_low"
	activityResultJoinRecent    = "join_recent"
	activityResultClaimed       = "claimed"
	activityResultThreshold     = "threshold"
	activityResultAssetPending  = "asset_pending"
	activityResultMerging       = "merging"
	activityResultNotInGuild    = "not_in_guild"
	activityResultHomeZone      = "home_zone"
	activityResultBusyRetry     = "busy_retry"
	activityResultIDUnavailable = "id_unavailable"
	activityResultOtherReject   = "other_reject"
	activityResultDenied        = "denied"
	activityResultUnavailable   = "unavailable"
	activityResultError         = "error"
)

// reward result label:enqueued = 本次插了一行物品指令;no_items = 本活动没有物品奖励。
// owed(历练结算转存待入队表)归 B6b。
const (
	activityRewardEnqueued = "enqueued"
	activityRewardNoItems  = "no_items"
)

// online lookup path label:view 失败只降级展示(进度显示 0),claim 失败整次领奖回故障 ——
// 两者的影响不同,分开计才看得出"界面数字不对"与"领不了奖"各是多少。
const (
	activityOnlinePathView  = "view"
	activityOnlinePathClaim = "claim"
)

// assetKindActivity 是 guild_asset_sync_skipped_total 的 kind 取值(复用 B5 的指标,06 §6.15 末行)。
const assetKindActivity = "activity"

var (
	guildActivityActionTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "activity",
		Name:      "action_total",
		Help:      "帮会活动 RPC 的结果。type: lantern / reunion / trial / all;action: view / light / claim / invite / respond;result 为固定集合(见 activity_metrics.go)。",
		Labels:    []string{"type", "action", "result"},
	})

	guildActivityRewardTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "activity",
		Name:      "reward_total",
		Help:      "成功参与后的物品奖励去向。result: enqueued(插了资产指令)/ no_items(本活动无物品)。",
		Labels:    []string{"type", "result"},
	})

	guildActivityFundsGrantedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "activity",
		Name:      "funds_granted_total",
		Help:      "活动给帮会发放资金的次数(灯会 / 团圆每帮每档期至多一次)。同一档期同一帮出现第二次即为 bug。",
		Labels:    []string{"type"},
	})

	guildActivityOnlineLookupFailedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "activity",
		Name:      "online_lookup_failed_total",
		Help:      "团圆数在线人数时会话 Redis 读不到(超时 / 出错 / 会话值解不开)的次数。path: view(降级显示 0)/ claim(整次领奖回故障)。",
		Labels:    []string{"path"},
	})
)
