package logic

// 帮会活动的指标(docs/design/guild-phase2/06-activities.md §6.15;同道历练的结算与后台见 §6.35)。
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

// action label。invite = 建邀请房间(StartGuildTrial);respond = 同意 / 拒绝 / 取消(RespondGuildTrialInvite);
// launch = 全员同意后的确认开战(发生在凑齐最后一票的那次 respond 里,单独再计一次:
// "开战成功率"要与"点同意的次数"分开看)。结算不走这条指标,见下面的 guild_trial_result_total。
const (
	activityActionView    = "view"
	activityActionLight   = "light"
	activityActionClaim   = "claim"
	activityActionInvite  = "invite"
	activityActionRespond = "respond"
	activityActionLaunch  = "launch"
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

	// 同道历练(B6b)。前四个由 activityResultOf 从 tip 推出;declined 没有对应的 tip(拒绝 / 取消是一次成功的响应),
	// 由 RespondGuildTrialInvite 在那一支显式给出。
	activityResultTeamInvalid   = "team_invalid"
	activityResultCooldown      = "cooldown"
	activityResultInviteExpired = "invite_expired"
	activityResultServiceBusy   = "service_busy"
	activityResultDeclined      = "declined"
)

// reward result label:enqueued = 本次插了一行物品指令;no_items = 本活动没有物品奖励;
// owed = 历练结算时该玩家未决指令已满,物品转存待入队表(之后由待入队循环转成指令,绝不跳过)。
// 灯会 / 团圆按"次"计(一次参与一个 label);历练结算按"人"计(一局里每位得奖者一个 label)。
const (
	activityRewardEnqueued = "enqueued"
	activityRewardNoItems  = "no_items"
	activityRewardOwed     = "owed"
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
		Help:      "帮会活动 RPC 的结果。type: lantern / reunion / trial / all;action: view / light / claim / invite / respond / launch;result 为固定集合(见 activity_metrics.go)。",
		Labels:    []string{"type", "action", "result"},
	})

	guildActivityRewardTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "activity",
		Name:      "reward_total",
		Help:      "成功参与后的物品奖励去向。result: enqueued(插了资产指令)/ no_items(本活动无物品)/ owed(历练结算时未决指令已满,转存待入队表)。",
		Labels:    []string{"type", "result"},
	})

	guildActivityFundsGrantedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "activity",
		Name:      "funds_granted_total",
		Help:      "活动给帮会发放资金的次数。灯会 / 团圆每帮每档期至多一次(同一档期同一帮出现第二次即为 bug);历练每个计资金的胜场一次,每帮每游戏日不超过配表上限。",
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

// ── 同道历练的结算与后台(06 §6.35)────────────────────────────

// guild_trial_result_total 的 result 取值。前七个由 SettleTrialResult 计(每局每次处理计一次);
// sweeper_* 由巡检器计;decode_error / handler_retry 由结果消费者经 CountTrialResultDecodeError /
// CountTrialResultHandlerRetry 计 —— 指标只在本包注册一次,消费者不许另建同名指标(重复注册会 panic)。
const (
	trialResultSettledWin      = "settled_win"
	trialResultSettledLoss     = "settled_loss"
	trialResultConfigMissing   = "config_missing"
	trialResultDuplicate       = "duplicate"
	trialResultGuildGone       = "guild_gone"
	trialResultBadContext      = "bad_context"
	trialResultContextMismatch = "context_mismatch"
	trialResultPoison          = "poison"
	trialResultDecodeError     = "decode_error"
	trialResultHandlerRetry    = "handler_retry"
	trialResultSweeperRecover  = "sweeper_recovered"
	trialResultSweeperExpired  = "sweeper_expired"
)

// guild_trial_owed_total 的 result 取值。skipped = 资产通道关闭或发号器不可用,本轮不转换(行原样留着)。
const (
	trialOwedConverted = "converted"
	trialOwedStillFull = "still_full"
	trialOwedGone      = "gone"
	trialOwedError     = "error"
	trialOwedSkipped   = "skipped"
)

// trialOwedRowsGaugeCap:待入队积压 gauge 的计数上限(06 §6.32:超过只报上限,不为一个指标做全表扫描)。
const trialOwedRowsGaugeCap = 10000

var (
	guildTrialResultTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "result_total",
		Help: "同道历练对局结果的处理去向。settled_win / settled_loss / config_missing(已结算);duplicate(重复事件);" +
			"guild_gone(帮会已解散);bad_context(上下文残缺);context_mismatch(登记行属于别帮);poison(确定性失败,需人工补发);" +
			"decode_error / handler_retry(消费者);sweeper_recovered / sweeper_expired(巡检器)。",
		Labels: []string{"result"},
	})

	guildTrialResultLagSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "result_lag_seconds",
		Help:      "历练结算完成时刻距对局结束时刻(结果事件的 finished_at_ms)的秒数。只在本次真正结算时观测。",
		Buckets:   []float64{0.1, 0.5, 1, 5, 30, 120, 600},
	})

	guildTrialResultOverdue = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "result_overdue",
		Help: "巡检器本轮看到的、结果超时且尚无结果记录的在途对局数。只有持有巡检租约的副本上报真实值,其余副本报 0," +
			"告警取各副本的最大值:持续大于 0 说明有对局的结果既没到 Kafka 也没落进 Redis。",
	})

	guildTrialOwedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "owed_total",
		Help:      "待入队物品循环逐行的去向。converted / still_full(未决指令仍满)/ gone(别的副本已转走)/ error / skipped(资产通道关闭或发号器不可用)。",
		Labels:    []string{"result"},
	})

	guildTrialOwedRows = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "owed_rows",
		Help:      "待入队物品表的积压行数(数到 10000 为止)。长期不降说明有玩家的背包一直满着,或资产通道关着。",
	})

	guildTrialRegisterFailTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "register_fail_total",
		Help:      "开战成功后登记对局行失败的次数。不影响发奖(结算会补登记),但这一局在结果到达之前巡检器看不见它。",
	})
)

// CountTrialResultDecodeError 给结果消费者用:一条消息反序列化失败(已提交 offset、不再处理)。
func CountTrialResultDecodeError() { guildTrialResultTotal.Inc(trialResultDecodeError) }

// CountTrialResultHandlerRetry 给结果消费者用:SettleTrialResult 回了暂时性错误,消费者将退避后重调。
func CountTrialResultHandlerRetry() { guildTrialResultTotal.Inc(trialResultHandlerRetry) }
