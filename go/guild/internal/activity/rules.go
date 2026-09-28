// Package activity 是帮会活动(元宵灯会 / 中秋团圆 / 同道历练)的纯规则:
// 输入配表行 + 时刻 + 进度,输出"现在开没开 / 本人能不能参与 / 该发什么 / 帮会级进度是否达标"。
//
// 为什么单独成包,而且不连库、不依赖 logic / data:
//   - 同一条规则要在三处得出相同结论 —— 查询视图(告诉玩家能不能点)、写 RPC 预检(快速失败)、
//     repo 事务(权威判定)。三处各写一份,迟早出现"界面说能点、点了回未开放"或"资金发两次"。
//   - 时间一律参数传入,不读墙钟:档期起止、05:00 切日、入帮满 N 小时这些边界才能在单测里逐毫秒钉住;
//     调用方一次请求只取一次 now,周期键、状态、视图里的 server_time_ms 才不会落在切点两侧。
//
// 两种周期(用户拍板 U1,README §3,不得混用):
//   - 个人次数按**游戏日**(DayKey,UTC+8 每天 05:00 重置)。计数挂在玩家上,换帮不重置。
//   - 帮会级进度按**活动档期**(GuildPeriodKey):点灯阈值、团圆锁存、资金只发一次。
//     历练的"每游戏日计资金胜场上限"例外,按游戏日。
//
// 设计:docs/design/guild-phase2/06-activities.md §6.2.4 / §6.3 / §6.8 / §6.11 / §6.12。
package activity

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"guild/internal/constants"
	assetpb "proto/common/asset"
	guildpb "proto/guild"
	"shared/gameday"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

// 活动类型。数值与配表 GuildActivity.type、proto GuildActivityType 一致(rules_test 有对照断言)。
const (
	TypeLantern uint32 = 1 // 元宵灯会
	TypeReunion uint32 = 2 // 中秋团圆
	TypeTrial   uint32 = 3 // 同道历练
)

// allTypes 按 type 升序:视图"每种类型至多一条、按 type 升序"靠这个顺序,不靠 map 遍历。
var allTypes = [...]uint32{TypeLantern, TypeReunion, TypeTrial}

// State 是活动行在某一时刻的状态。数值与 proto GuildActivityState 一致,视图直接数值转换。
type State uint8

const (
	StateDisabled State = 1
	StateUpcoming State = 2
	StateOpen     State = 3
	StateEnded    State = 4
)

// msPerHour:入帮时长按毫秒比较。写成常量是为了让"小时 → 毫秒"只有一处换算。
const msPerHour uint64 = 3_600_000

// StateOf 返回 row 在 nowMs(UTC Unix 毫秒)时刻的状态。档期是左闭右开区间 [start, end)。
//
// 0/0 = 常开(仅开发)。start 非 0 而 end 为 0 是坏配表(启动期 ValidateTables 会拒),
// 这里落到 Ended —— 判不清就当"没开",不当"一直开"(fail-closed)。
func StateOf(row *tablepb.GuildActivityTable, nowMs uint64) State {
	if row == nil || !row.GetEnabled() {
		return StateDisabled
	}
	start, end := row.GetStartAtMs(), row.GetEndAtMs()
	if start == 0 && end == 0 {
		return StateOpen
	}
	if nowMs < start {
		return StateUpcoming
	}
	if nowMs >= end {
		return StateEnded
	}
	return StateOpen
}

// DayKey 是个人次数的周期键(游戏日 YYYYMMDD)。只是 gameday.DayKey 的别名:
// 活动代码统一从本包取键,读代码的人不必去猜"这里的日是不是 00:00 切"。
func DayKey(now time.Time) uint32 { return gameday.DayKey(now) }

// GuildPeriodKey 是帮会级进度(guild_activity_progress.period_key)的周期键(U1):
//   - 历练:当前游戏日 —— 它的阈值是"每游戏日计资金胜场上限",按天重置;
//   - 0/0 常开行(仅开发):退化为当前游戏日,否则常开行的阈值与资金一辈子只能达成一次;
//   - 有档期的灯会 / 团圆:start_at_ms 那一刻所在的游戏日。整个档期内键不变,
//     所以跨 05:00 个人次数重置、帮会进度(点灯人次、团圆锁存、资金已发)不重置。
//
// start_at_ms ≤ MaxInt64 由 ValidateTables 保证,转 int64 不会变负。
func GuildPeriodKey(row *tablepb.GuildActivityTable, now time.Time) uint32 {
	if row == nil || row.GetType() == TypeTrial {
		return gameday.DayKey(now)
	}
	start, end := row.GetStartAtMs(), row.GetEndAtMs()
	if start == 0 && end == 0 {
		return gameday.DayKey(now)
	}
	return gameday.DayKey(time.UnixMilli(int64(start)))
}

// NextResetMs 是下一次个人次数重置时刻(UTC+8 的 05:00)的 Unix 毫秒。复用 B5 的
// gameday.NextDailyReset,不另写切点 —— 两处各算一遍,迟早有一处与计数键对不上。
func NextResetMs(now time.Time) uint64 {
	return uint64(gameday.NextDailyReset(now).UnixMilli())
}

// SelectVisible 给每种类型选出"当前代表行",按 type 升序返回;该类型没有行就跳过。
// 选法(每种类型一行):Open 中 id 最小;否则 Upcoming 中 start 最小(同 start 取 id 小);否则 id 最小。
//
// 配表禁止同类型启用行时间窗重叠(ValidateTables),正常只会有一行 Open;
// 这里仍按 id 取最小,保证坏数据下选择也是确定的,不随 FindAll 的顺序漂。
func SelectVisible(rows []*tablepb.GuildActivityTable, nowMs uint64) []*tablepb.GuildActivityTable {
	out := make([]*tablepb.GuildActivityTable, 0, len(allTypes))
	for _, typ := range allTypes {
		if row := selectForType(rows, typ, nowMs); row != nil {
			out = append(out, row)
		}
	}
	return out
}

func selectForType(rows []*tablepb.GuildActivityTable, typ uint32, nowMs uint64) *tablepb.GuildActivityTable {
	var open, upcoming, lowest *tablepb.GuildActivityTable
	for _, row := range rows {
		if row == nil || row.GetType() != typ {
			continue
		}
		switch StateOf(row, nowMs) {
		case StateOpen:
			if open == nil || row.GetId() < open.GetId() {
				open = row
			}
		case StateUpcoming:
			if upcoming == nil || row.GetStartAtMs() < upcoming.GetStartAtMs() ||
				(row.GetStartAtMs() == upcoming.GetStartAtMs() && row.GetId() < upcoming.GetId()) {
				upcoming = row
			}
		}
		if lowest == nil || row.GetId() < lowest.GetId() {
			lowest = row
		}
	}
	switch {
	case open != nil:
		return open
	case upcoming != nil:
		return upcoming
	default:
		return lowest
	}
}

// PickForWrite 是写 RPC 的入口闸:只接受"该类型当前选中行"且它正处于 Open。
// 行不存在、类型不符、不是选中行、未开放 → (nil, false),写 RPC 回 GuildActivityNotOpen。
//
// 为什么不直接按 reqID 查行:客户端可以伪造同类型另一行的 id(比如一个已结束档期),
// 若那行恰好也能判成 Open,同一时刻就有两行在计进度、资金可能发两次(06 §6.13 C15)。
func PickForWrite(rows []*tablepb.GuildActivityTable, typ uint32, reqID uint32, nowMs uint64) (*tablepb.GuildActivityTable, bool) {
	row := selectForType(rows, typ, nowMs)
	if row == nil || row.GetId() != reqID || StateOf(row, nowMs) != StateOpen {
		return nil, false
	}
	return row, true
}

// JoinedLongEnough:入帮时刻 joinTimeMs 到 nowMs 是否已满 minHours 小时(恰好满算满)。
//   - minHours == 0:不限制,恒真(开发配表值);
//   - joinTimeMs == 0:入帮时刻未知(存量行或写入缺陷),不能当"入帮很久了"放行;
//   - joinTimeMs > nowMs:多实例时钟偏差让入帮时刻落在"未来",同样按未满处理。
//
// 用 now − join 而不是 join + minHours×3600000 比较,避免 join 接近 MaxUint64 时加法溢出。
func JoinedLongEnough(joinTimeMs uint64, nowMs uint64, minHours uint32) bool {
	if minHours == 0 {
		return true
	}
	if joinTimeMs == 0 || joinTimeMs > nowMs {
		return false
	}
	return nowMs-joinTimeMs >= uint64(minHours)*msPerHour
}

// ReunionThreshold 是团圆的生效在线人数阈值 = max(活动行 guild_threshold, GuildRule.reunion_min_online_members)。
//
// 取 max 而不是"行填 0 才用 GuildRule":两种读法在行填 0(兜底)与行填更高值(档期加码)时结果相同;
// 行填了比 GuildRule 更低的值时,GuildRule 那列的 schema 注释写明"这里是下限",取 max 才守得住下限。
// 两者都为 0 只可能来自坏配表(ValidateTables 会拒);结果 0 在 ReunionObserved / Blocked 里按"不可达"处理。
func ReunionThreshold(row *tablepb.GuildActivityTable, rule *tablepb.GuildRuleTable) uint32 {
	return max(row.GetGuildThreshold(), rule.GetReunionMinOnlineMembers())
}

// ReunionObserved:数到的合格在线人数是否达到团圆阈值。threshold == 0(坏配表)恒为假:
// 否则"0 人在线也达标",等于把锁存与领奖白送出去。
func ReunionObserved(online, threshold uint32) bool {
	return threshold > 0 && online >= threshold
}

// Progress 是 guild_activity_progress 一行里与规则有关的列;行不存在时用零值。
type Progress struct {
	// Count 是 progress_count:灯会 = 本档期点灯人次;历练 = 本游戏日已计资金胜场;团圆不用(恒 0)。
	Count uint32
	// ThresholdReachedMs 非 0 = 本档期已达阈值(锁存),值为首次达成时刻。
	ThresholdReachedMs uint64
	// FundsGranted 是 funds_granted = 1:本档期资金已发。
	FundsGranted bool
}

// Latched:本档期阈值已锁存。锁存后团圆不再要求凑人,灯会不再重复"首次达成"推送。
func (p Progress) Latched() bool { return p.ThresholdReachedMs != 0 }

// GuildOutcome 是一次成功参与之后,帮会级进度要在**同一事务**里做的事。
type GuildOutcome struct {
	// ReachedNow:本次首次达到阈值 → 写 threshold_reached_ms(锁存),提交后推 ACTIVITY_CHANGED。
	ReachedNow bool
	// GrantFunds:本档期资金在本次发放 → guild.funds += guild_funds,并置 funds_granted = 1。
	GrantFunds bool
}

// LanternOutcome:灯会。p 必须是本次 progress_count + 1 之后、在事务内加锁读回的那一行
// (06 §6.11.2 e/f 步),否则两个并发点灯者会各自以为自己是"第 3 个"。
//
// 资金只看 funds_granted 不看锁存:达标时 guild_funds 恰为 0、档期内策划又调成正数,
// 下一个点灯者仍会补发一次;"本档期只发一次"由 funds_granted 这一列保证。
func LanternOutcome(row *tablepb.GuildActivityTable, p Progress) GuildOutcome {
	threshold := row.GetGuildThreshold()
	reached := threshold > 0 && p.Count >= threshold
	return GuildOutcome{
		ReachedNow: reached && !p.Latched(),
		GrantFunds: reached && !p.FundsGranted && row.GetGuildFunds() > 0,
	}
}

// ReunionOutcome:团圆。p 是事务内加锁读回的进度行,observed 是预检时数到的合格在线人数是否达标
// (ReunionObserved)。ok == false:未锁存且本次没凑够人 → 整事务回滚,回 GuildActivityThresholdNotReached。
//
// 已锁存时不再看 observed:锁存语义是"本档期曾经同时在线过",之后每人每游戏日照常领(06 §6.0)。
// 在线人数只用来开门、事务内不重数(C11):数完到提交之间有人下线,仍以数人数那一刻为准。
func ReunionOutcome(row *tablepb.GuildActivityTable, p Progress, observed bool) (GuildOutcome, bool) {
	if !p.Latched() && !observed {
		return GuildOutcome{}, false
	}
	return GuildOutcome{
		ReachedNow: !p.Latched(),
		GrantFunds: !p.FundsGranted && row.GetGuildFunds() > 0,
	}, true
}

// TrialFundsCounted:历练胜利是否计入帮会资金。countedWins 是本游戏日已计资金的胜场
// (事务内加锁读回的 progress_count);guild_threshold 是每游戏日上限,满了只发个人奖。
func TrialFundsCounted(row *tablepb.GuildActivityTable, countedWins uint32) bool {
	return row.GetGuildFunds() > 0 && countedWins < row.GetGuildThreshold()
}

// BlockInput 是 Blocked / BlockedTip 的输入。调用方负责把各字段算好(都是同一个 now 下的值)。
type BlockInput struct {
	// State 是 StateOf(row, nowMs)。
	State State
	// GuildLevel 是帮会当前等级;MinLevel 是 row.min_guild_level。
	GuildLevel, MinLevel uint32
	// Joined 是 JoinedLongEnough(member.join_time_ms, nowMs, rule.activity_join_min_hours)。
	Joined bool
	// JoinMinHours 只用于提示参数([N]),不参与判定。
	JoinMinHours uint32
	// Used 是本人本游戏日已参与次数;DailyLimit 是 row.daily_limit。
	Used, DailyLimit uint32
	// Type 是 row.type。只有团圆看下面三项。
	Type uint32
	// ThresholdReached 是本档期已锁存(锁存后不再要求凑人)。
	ThresholdReached bool
	// Progress 是当前合格在线人数;Threshold 是 ReunionThreshold 的生效值。
	Progress, Threshold uint32
}

// Blocked 按固定优先级返回第一个不满足项及其提示参数;tip == 0 表示本人现在可参与
// (不含在线、战斗这类瞬时条件,它们在写 RPC 里另判)。
//
// 优先级(06 §6.3):未开放 > 帮会等级不足 > 入帮未满 N 小时 > 今日次数已满 > 团圆人数不足。
// 顺序即玩家要先解决的顺序:活动没开时提示"次数已满"只会误导。参数与 06 §6.10 的错误映射一致。
func Blocked(in BlockInput) (tip uint32, params []string) {
	switch {
	case in.State != StateOpen:
		return constants.ErrActivityNotOpen, nil
	case in.GuildLevel < in.MinLevel:
		return constants.ErrActivityLevelTooLow, []string{formatU32(in.MinLevel)}
	case !in.Joined:
		return constants.ErrActivityJoinTooRecent, []string{formatU32(in.JoinMinHours)}
	case in.Used >= in.DailyLimit:
		return constants.ErrActivityAlreadyClaimed, nil
	case in.Type == TypeReunion && !in.ThresholdReached && !ReunionObserved(in.Progress, in.Threshold):
		return constants.ErrActivityThresholdNotReached, []string{formatU32(in.Progress), formatU32(in.Threshold)}
	default:
		return 0, nil
	}
}

// BlockedTip 是 Blocked 只取 tip 的形式,供视图的 blocked_tip_id 使用。
func BlockedTip(in BlockInput) uint32 {
	tip, _ := Blocked(in)
	return tip
}

func formatU32(v uint32) string { return strconv.FormatUint(uint64(v), 10) }

// maxRewardItemKinds:一个奖励包最多几种物品。与 scene 端 ValidateCreditBundle
// (cpp/libs/services/scene/player/system/asset_op_system.cpp,"货币 4 / 物品 16")一致:
// 超过就被 scene 判成非法包、永久 REJECTED,奖励静默丢失,所以启动期就拒。
const maxRewardItemKinds = 16

// BuildRewardBundle 把 Reward 表的一行转成资产通道的物品包:
//   - rewardID == 0 → (nil, nil),无物品;
//   - 行不存在 → error(启动期已校验,运行期出现 = 配表被错误热更,调用方按故障处理);
//   - 各槽按 item_id 合并数量、跳过数量为 0 的空槽,按 item_id 升序输出:scene 拒绝同一 config_id
//     出现两次(同上 ValidateCreditBundle),而导表的 Reward 行允许两个槽填同一物品;
//   - 合并后为空 → (nil, nil);合并后单项超过 uint32 → error;数量非 0 而 item_id 为 0 → error。
func BuildRewardBundle(rewardID uint32) (*assetpb.AssetBundle, error) {
	return buildRewardBundle(rewardID, table.RewardTableManagerInstance.FindById)
}

// buildRewardBundle 是 BuildRewardBundle 的纯函数部分:Reward 表查询由参数注入,
// 单测与启动校验不必动 table 包的全局快照。
func buildRewardBundle(rewardID uint32, rewardOf func(uint32) (*tablepb.RewardTable, bool)) (*assetpb.AssetBundle, error) {
	if rewardID == 0 {
		return nil, nil
	}
	if rewardOf == nil {
		return nil, errors.New("奖励包构建缺少 Reward 表查询")
	}
	row, ok := rewardOf(rewardID)
	if !ok || row == nil {
		return nil, fmt.Errorf("Reward 表缺少 id=%d 的行", rewardID)
	}
	sums := make(map[uint32]uint64, len(row.GetReward()))
	for i, slot := range row.GetReward() {
		count := slot.GetRewardCount()
		if count == 0 {
			continue
		}
		item := slot.GetRewardItem()
		// 有数量却没物品:多半是漏填了物品列。当空槽跳过会让玩家少拿一份而没人发现。
		if item == 0 {
			return nil, fmt.Errorf("Reward[%d] 第 %d 槽 reward_item=0 而 reward_count=%d", rewardID, i+1, count)
		}
		sums[item] += uint64(count)
		if sums[item] > math.MaxUint32 {
			return nil, fmt.Errorf("Reward[%d] 物品 %d 合并后数量 %d 超过 uint32", rewardID, item, sums[item])
		}
	}
	if len(sums) == 0 {
		return nil, nil
	}
	ids := make([]uint32, 0, len(sums))
	for id := range sums {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	items := make([]*assetpb.ItemGrant, 0, len(ids))
	for _, id := range ids {
		items = append(items, &assetpb.ItemGrant{ConfigId: id, Count: uint32(sums[id])})
	}
	return &assetpb.AssetBundle{Items: items}, nil
}

// RewardItems 把物品包转成视图用的 GuildRewardItem。guild.proto 不 import asset_op.proto
// (契约偏差 6):内部资产通道的 schema 不进客户端包。nil 或空包 → nil。
func RewardItems(b *assetpb.AssetBundle) []*guildpb.GuildRewardItem {
	if len(b.GetItems()) == 0 {
		return nil
	}
	out := make([]*guildpb.GuildRewardItem, 0, len(b.GetItems()))
	for _, it := range b.GetItems() {
		out = append(out, &guildpb.GuildRewardItem{ItemId: it.GetConfigId(), Count: it.GetCount()})
	}
	return out
}

// ViewInput 是 BuildView 的全部输入。库 / Redis 读出来的值由调用方(logic)填好;
// 只读路径的 Redis 失败按 06 §6.8 降级为 0,MySQL 失败由调用方直接返回错误,不进这里。
type ViewInput struct {
	// Now 是本次请求唯一的"现在"。零值视为调用方漏填,BuildView 报错而不是按 1970 年算状态。
	Now time.Time
	// Rule 是 GuildRule[RuleRowID];nil 视为配表坏,BuildView 报错。
	Rule *tablepb.GuildRuleTable
	// GuildLevel 是帮会当前等级(缓存读,视图用)。
	GuildLevel uint32
	// JoinTimeMs 是本人 guild_member.join_time_ms。
	JoinTimeMs uint64
	// MyUsedCount 是本人本游戏日(DayKey)已参与次数。
	MyUsedCount uint32
	// Progress 是本行 GuildPeriodKey 那一期的进度行;行不存在用零值。
	Progress Progress
	// ReunionOnline 只对未锁存的团圆有意义:入帮满 N 小时且在线的成员数。
	ReunionOnline uint32
	// MyTrialBattleID 只对历练有意义:battle:lock 指向、且仍为 STARTED 的历练对局。
	MyTrialBattleID uint64
	// MyPendingRewardCount / MyPendingReasonTipID / MyLastRewardRejectTipID:本人本活动的物品发放状态(06 §6.8 第 7 步)。
	MyPendingRewardCount    uint32
	MyPendingReasonTipID    uint32
	MyLastRewardRejectTipID uint32
	// TrialLobby 只对历练有意义:本人所在、属于本活动的邀请房间;无则 nil。
	TrialLobby *guildpb.GuildTrialLobbyView
	// TrialUnavailable:历练不可用(B6a 桩,或 B6b 的 match / 房间依赖缺失)。
	// 为真时历练行强制 DISABLED + GuildActivityNotOpen,不让界面显示一个点了必失败的按钮。
	TrialUnavailable bool
}

// BuildView 生成一行活动的视图。奖励包按当前 Reward 表现算(用时现查),构建失败返回 error。
func BuildView(row *tablepb.GuildActivityTable, in ViewInput) (*guildpb.GuildActivityView, error) {
	return buildView(row, in, table.RewardTableManagerInstance.FindById)
}

func buildView(row *tablepb.GuildActivityTable, in ViewInput, rewardOf func(uint32) (*tablepb.RewardTable, bool)) (*guildpb.GuildActivityView, error) {
	if row == nil {
		return nil, errors.New("活动视图:配表行为空")
	}
	if in.Rule == nil {
		return nil, fmt.Errorf("活动视图:GuildRule 表缺少 id=%d 的规则行", RuleRowID)
	}
	if in.Now.IsZero() {
		return nil, errors.New("活动视图:ViewInput.Now 未设置")
	}
	bundle, err := buildRewardBundle(row.GetRewardId(), rewardOf)
	if err != nil {
		return nil, fmt.Errorf("活动视图 GuildActivity[%d]:%w", row.GetId(), err)
	}

	nowMs := uint64(in.Now.UnixMilli())
	typ := row.GetType()
	state := StateOf(row, nowMs)
	joinMinHours := in.Rule.GetActivityJoinMinHours()

	threshold := row.GetGuildThreshold()
	var progress uint32
	var reached, fundsGranted bool
	switch typ {
	case TypeLantern:
		progress = in.Progress.Count
		reached, fundsGranted = in.Progress.Latched(), in.Progress.FundsGranted
	case TypeReunion:
		threshold = ReunionThreshold(row, in.Rule)
		reached, fundsGranted = in.Progress.Latched(), in.Progress.FundsGranted
		// 锁存后不再需要凑人,进度填 0(06 §6.4 progress 注释),界面据 threshold_reached 显示"已达成"。
		if !reached {
			progress = in.ReunionOnline
		}
	case TypeTrial:
		// 历练没有"锁存 / 只发一次",threshold_reached、funds_granted 恒为 false;
		// progress 是今日已计资金胜场,threshold 是每日上限。
		progress = in.Progress.Count
	}

	blocked := BlockedTip(BlockInput{
		State:            state,
		GuildLevel:       in.GuildLevel,
		MinLevel:         row.GetMinGuildLevel(),
		Joined:           JoinedLongEnough(in.JoinTimeMs, nowMs, joinMinHours),
		JoinMinHours:     joinMinHours,
		Used:             in.MyUsedCount,
		DailyLimit:       row.GetDailyLimit(),
		Type:             typ,
		ThresholdReached: reached,
		Progress:         progress,
		Threshold:        threshold,
	})
	if typ == TypeTrial && in.TrialUnavailable {
		state, blocked = StateDisabled, constants.ErrActivityNotOpen
	}

	view := &guildpb.GuildActivityView{
		ActivityId:              row.GetId(),
		Type:                    guildpb.GuildActivityType(typ),
		Name:                    row.GetName(),
		State:                   guildpb.GuildActivityState(state),
		StartAtMs:               row.GetStartAtMs(),
		EndAtMs:                 row.GetEndAtMs(),
		MinGuildLevel:           row.GetMinGuildLevel(),
		PeriodKey:               DayKey(in.Now),
		NextResetMs:             NextResetMs(in.Now),
		ServerTimeMs:            nowMs,
		PersonalContribution:    row.GetPersonalContribution(),
		GuildFunds:              row.GetGuildFunds(),
		GuildThreshold:          threshold,
		RewardItems:             RewardItems(bundle),
		DailyLimit:              row.GetDailyLimit(),
		MyUsedCount:             in.MyUsedCount,
		Progress:                progress,
		ThresholdReached:        reached,
		FundsGranted:            fundsGranted,
		BlockedTipId:            blocked,
		DungeonId:               row.GetDungeonId(),
		TeamSizeMin:             row.GetTeamSizeMin(),
		TeamSizeMax:             row.GetTeamSizeMax(),
		MyPendingRewardCount:    in.MyPendingRewardCount,
		MyPendingReasonTipId:    in.MyPendingReasonTipID,
		MyLastRewardRejectTipId: in.MyLastRewardRejectTipID,
		JoinMinHours:            joinMinHours,
		GuildPeriodKey:          GuildPeriodKey(row, in.Now),
	}
	if typ == TypeTrial && !in.TrialUnavailable {
		view.MyTrialBattleId = in.MyTrialBattleID
		view.TrialLobby = in.TrialLobby
	}
	return view, nil
}
