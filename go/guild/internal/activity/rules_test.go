package activity

// 帮会活动纯规则的单测(06 §6.40 rules_test 一节)。
//
// 除 TestBuildRewardBundleFromGeneratedTable 外,全部用例只喂纯数据,不碰 table 包的全局快照:
// 本包的 ValidateTables / BuildView 读全局快照,单测改它会与同进程的其它用例互相污染
// (与 logic/economy_config_test.go 同一口径)。GuildActivity 表的导表产物也不是本测试的前置。
//
// 时刻一律用 gameday.Zone(UTC+8)构造:切日点是 UTC+8 的 05:00,用 UTC 写用例容易把边界写错一天。

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
	assetpb "proto/common/asset"
	guildpb "proto/guild"
	"shared/gameday"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

const (
	msPerDay uint64 = 86_400_000
	ms24h    uint64 = 24 * msPerHour
)

// at 构造 UTC+8 时刻。
func at(month time.Month, day, hour, minute, sec, milli int) time.Time {
	return time.Date(2026, month, day, hour, minute, sec, milli*int(time.Millisecond), gameday.Zone)
}

func msOf(t time.Time) uint64 { return uint64(t.UnixMilli()) }

// lanternRow 是一行合法的灯会(06 §6.2.2 默认值),档期由参数给。
func lanternRow(id uint32, enabled bool, start, end uint64) *tablepb.GuildActivityTable {
	return &tablepb.GuildActivityTable{
		Id:                   id,
		Name:                 "元宵灯会",
		Type:                 TypeLantern,
		Enabled:              enabled,
		StartAtMs:            start,
		EndAtMs:              end,
		MinGuildLevel:        1,
		PersonalContribution: 20,
		GuildFunds:           500,
		GuildThreshold:       3,
		DailyLimit:           1,
	}
}

// legalRows 是 GuildActivity.xlsx 的默认 3 行(06 §6.2.2,开发值:常开)。每次重新构造,用例之间不共享行指针。
func legalRows() []*tablepb.GuildActivityTable {
	return []*tablepb.GuildActivityTable{
		lanternRow(1, true, 0, 0),
		{Id: 2, Name: "中秋团圆", Type: TypeReunion, Enabled: true, MinGuildLevel: 1, PersonalContribution: 30, GuildThreshold: 3, RewardId: 1, DailyLimit: 1},
		{Id: 3, Name: "同道历练", Type: TypeTrial, Enabled: true, MinGuildLevel: 1, PersonalContribution: 50, GuildFunds: 300, GuildThreshold: 3, RewardId: 2, DungeonId: 1, TeamSizeMin: 2, TeamSizeMax: 5, DailyLimit: 2},
	}
}

// legalRule 是 GuildRule 的默认行 `1,72,3,50,600,1000,3,0,30,10`(90 D1)。
func legalRule() *tablepb.GuildRuleTable {
	return &tablepb.GuildRuleTable{
		Id:                              1,
		ApplicationExpireHours:          72,
		MaxPendingApplicationsPerPlayer: 3,
		MaxPendingApplicationsPerGuild:  50,
		AssetOpDeadlineSeconds:          600,
		AssetOpRetryBaseMs:              1000,
		ReunionMinOnlineMembers:         3,
		ActivityJoinMinHours:            0,
		TrialInviteTtlSeconds:           30,
		TrialInviteCooldownSeconds:      10,
	}
}

// twoSlotReward:Reward 表每行两个槽(cfg_slots = 2),两槽填同一物品。
func twoSlotReward(id, item, count uint32) *tablepb.RewardTable {
	return &tablepb.RewardTable{Id: id, Reward: []*tablepb.Rewardreward{
		{RewardItem: item, RewardCount: count},
		{RewardItem: item, RewardCount: count},
	}}
}

// legalRewards:Reward 1 / 2 与 generated/tables/reward.json 现状一致(两槽都是物品 1×2)。
func legalRewards() map[uint32]*tablepb.RewardTable {
	return map[uint32]*tablepb.RewardTable{1: twoSlotReward(1, 1, 2), 2: twoSlotReward(2, 1, 2)}
}

func rewardLookup(rows map[uint32]*tablepb.RewardTable) func(uint32) (*tablepb.RewardTable, bool) {
	return func(id uint32) (*tablepb.RewardTable, bool) {
		row, ok := rows[id]
		return row, ok
	}
}

func setOf(ids ...uint32) func(uint32) bool {
	m := make(map[uint32]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return func(id uint32) bool { return m[id] }
}

// legalTables:默认 3 行 + 默认规则行;GuildLevel 1–10、物品 1、副本 1 存在。
func legalTables() activityTables {
	return activityTables{
		rows:          legalRows(),
		rule:          legalRule(),
		levelExists:   setOf(1, 2, 3, 4, 5, 6, 7, 8, 9, 10),
		rewardOf:      rewardLookup(legalRewards()),
		itemExists:    setOf(1),
		dungeonExists: setOf(1),
	}
}

// itemsOf 把物品包摊平成 (config_id, count) 对,便于整体断言。
func itemsOf(b *assetpb.AssetBundle) [][2]uint32 {
	var out [][2]uint32
	for _, it := range b.GetItems() {
		out = append(out, [2]uint32{it.GetConfigId(), it.GetCount()})
	}
	return out
}

// 本包的类型 / 状态常量与 proto 枚举数值一致:视图直接做数值转换,错一个就是界面显示错活动或错状态。
func TestTypeAndStateMatchProto(t *testing.T) {
	assert.EqualValues(t, guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_LANTERN, TypeLantern)
	assert.EqualValues(t, guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_REUNION, TypeReunion)
	assert.EqualValues(t, guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_TRIAL, TypeTrial)
	assert.EqualValues(t, guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_DISABLED, StateDisabled)
	assert.EqualValues(t, guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_UPCOMING, StateUpcoming)
	assert.EqualValues(t, guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_OPEN, StateOpen)
	assert.EqualValues(t, guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_ENDED, StateEnded)
}

// 档期边界:左闭右开 [start, end)。
func TestStateOf(t *testing.T) {
	start, end := msOf(at(time.September, 20, 10, 0, 0, 0)), msOf(at(time.September, 27, 10, 0, 0, 0))
	row := lanternRow(1, true, start, end)

	assert.Equal(t, StateUpcoming, StateOf(row, start-1))
	assert.Equal(t, StateOpen, StateOf(row, start))
	assert.Equal(t, StateOpen, StateOf(row, end-1))
	assert.Equal(t, StateEnded, StateOf(row, end))

	// 0/0 常开:任何时刻都开。
	alwaysOpen := lanternRow(2, true, 0, 0)
	assert.Equal(t, StateOpen, StateOf(alwaysOpen, 0))
	assert.Equal(t, StateOpen, StateOf(alwaysOpen, math.MaxUint64))

	// 未启用优先于档期;nil 行当未启用。
	assert.Equal(t, StateDisabled, StateOf(lanternRow(3, false, start, end), start))
	assert.Equal(t, StateDisabled, StateOf(lanternRow(4, false, 0, 0), start))
	assert.Equal(t, StateDisabled, StateOf(nil, start))

	// 坏配表(只填了 start):判成已结束而不是一直开(fail-closed)。
	assert.Equal(t, StateEnded, StateOf(lanternRow(5, true, start, 0), start+1))
}

// U1:个人次数按游戏日,帮会进度按档期;历练与常开行按游戏日。
func TestGuildPeriodKey(t *testing.T) {
	start := msOf(at(time.September, 20, 10, 0, 0, 0))
	end := msOf(at(time.October, 1, 10, 0, 0, 0))
	lantern := lanternRow(1, true, start, end)
	alwaysOpen := lanternRow(2, true, 0, 0)
	trial := &tablepb.GuildActivityTable{Id: 3, Type: TypeTrial, Enabled: true, StartAtMs: start, EndAtMs: end}

	for _, now := range []time.Time{at(time.September, 21, 12, 0, 0, 0), at(time.September, 25, 4, 0, 0, 0)} {
		assert.Equal(t, uint32(20260920), GuildPeriodKey(lantern, now), "档期内任何时刻都是 start 当天的键,now=%v", now)
	}
	assert.Equal(t, uint32(20260921), GuildPeriodKey(trial, at(time.September, 21, 12, 0, 0, 0)))
	// 09-25 04:00 还没到 05:00,属于 09-24 游戏日。
	assert.Equal(t, uint32(20260924), GuildPeriodKey(trial, at(time.September, 25, 4, 0, 0, 0)))
	assert.Equal(t, uint32(20260921), GuildPeriodKey(alwaysOpen, at(time.September, 21, 12, 0, 0, 0)))

	// 档期从 04:00 开始:start 那一刻属于前一游戏日,档期键跟着取前一天(只是个键,不影响开放判定)。
	early := lanternRow(4, true, msOf(at(time.September, 20, 4, 0, 0, 0)), end)
	assert.Equal(t, uint32(20260919), GuildPeriodKey(early, at(time.September, 21, 12, 0, 0, 0)))
}

// 跨 05:00:个人次数的键换新一天(次数重置),有档期的帮会进度键不变(阈值、锁存、资金不重置)。
func TestPeriodKeysAcrossDailyReset(t *testing.T) {
	before := at(time.September, 21, 4, 59, 59, 999)
	after := at(time.September, 21, 5, 0, 0, 0)
	lantern := lanternRow(1, true, msOf(at(time.September, 20, 10, 0, 0, 0)), msOf(at(time.October, 1, 10, 0, 0, 0)))
	alwaysOpen := lanternRow(2, true, 0, 0)
	trial := &tablepb.GuildActivityTable{Id: 3, Type: TypeTrial, Enabled: true}

	assert.Equal(t, uint32(20260920), DayKey(before))
	assert.Equal(t, uint32(20260921), DayKey(after))

	assert.Equal(t, uint32(20260920), GuildPeriodKey(lantern, before))
	assert.Equal(t, uint32(20260920), GuildPeriodKey(lantern, after))

	assert.Equal(t, uint32(20260920), GuildPeriodKey(alwaysOpen, before))
	assert.Equal(t, uint32(20260921), GuildPeriodKey(alwaysOpen, after))
	assert.Equal(t, uint32(20260920), GuildPeriodKey(trial, before))
	assert.Equal(t, uint32(20260921), GuildPeriodKey(trial, after))
}

func TestNextResetMs(t *testing.T) {
	four := at(time.September, 21, 4, 0, 0, 0)
	five := at(time.September, 21, 5, 0, 0, 0)

	assert.Equal(t, msOf(at(time.September, 21, 5, 0, 0, 0)), NextResetMs(four))
	// 恰在 05:00 → 次日 05:00(严格晚于 now)。
	assert.Equal(t, msOf(at(time.September, 22, 5, 0, 0, 0)), NextResetMs(five))
	assert.Equal(t, uint64(gameday.NextDailyReset(four).UnixMilli()), NextResetMs(four))
	// 入参时区不影响结果:同一时刻换成 UTC 表示。
	assert.Equal(t, NextResetMs(four), NextResetMs(four.UTC()))
}

// 入帮时长边界:恰好满算满,差 1ms 不算。
func TestJoinedLongEnough(t *testing.T) {
	now := msOf(at(time.September, 21, 12, 0, 0, 0))

	assert.True(t, JoinedLongEnough(0, now, 0), "minHours=0 恒真,入帮时刻未知也放行")
	assert.True(t, JoinedLongEnough(now, now, 0))
	assert.True(t, JoinedLongEnough(now-ms24h, now, 24))
	assert.False(t, JoinedLongEnough(now-ms24h+1, now, 24))
	assert.False(t, JoinedLongEnough(0, now, 24), "入帮时刻未知不能当入帮很久")
	assert.False(t, JoinedLongEnough(now+1, now, 1), "入帮时刻在未来(时钟偏差)按未满处理")
	// 极端值不溢出。
	assert.True(t, JoinedLongEnough(1, math.MaxUint64, math.MaxUint32))
}

func TestReunionThreshold(t *testing.T) {
	rule := &tablepb.GuildRuleTable{Id: 1, ReunionMinOnlineMembers: 3}

	assert.Equal(t, uint32(3), ReunionThreshold(&tablepb.GuildActivityTable{GuildThreshold: 0}, rule), "行填 0 用 GuildRule 兜底")
	assert.Equal(t, uint32(10), ReunionThreshold(&tablepb.GuildActivityTable{GuildThreshold: 10}, rule), "档期加码")
	assert.Equal(t, uint32(3), ReunionThreshold(&tablepb.GuildActivityTable{GuildThreshold: 2}, rule), "GuildRule 是下限")
	assert.Equal(t, uint32(5), ReunionThreshold(&tablepb.GuildActivityTable{GuildThreshold: 5}, nil))

	assert.True(t, ReunionObserved(3, 3), "刚好达标")
	assert.False(t, ReunionObserved(2, 3), "差一")
	assert.False(t, ReunionObserved(5, 0), "阈值 0 只可能是坏配表,按不可达处理")
}

// 灯会阈值:刚好达标那一次首次达成并发资金;差一不达;之后不重复。
func TestLanternOutcome(t *testing.T) {
	row := &tablepb.GuildActivityTable{Type: TypeLantern, GuildThreshold: 3, GuildFunds: 500}

	assert.Equal(t, GuildOutcome{}, LanternOutcome(row, Progress{Count: 2}), "差一")
	assert.Equal(t, GuildOutcome{ReachedNow: true, GrantFunds: true}, LanternOutcome(row, Progress{Count: 3}), "刚好达标")
	assert.Equal(t, GuildOutcome{}, LanternOutcome(row, Progress{Count: 4, ThresholdReachedMs: 1, FundsGranted: true}), "已锁存且已发")
	// 达标时 guild_funds 为 0、档期内又调成正数:锁存不重复,资金补发一次。
	assert.Equal(t, GuildOutcome{GrantFunds: true}, LanternOutcome(row, Progress{Count: 4, ThresholdReachedMs: 1}))

	noFunds := &tablepb.GuildActivityTable{Type: TypeLantern, GuildThreshold: 3}
	assert.Equal(t, GuildOutcome{ReachedNow: true}, LanternOutcome(noFunds, Progress{Count: 3}))

	zeroThreshold := &tablepb.GuildActivityTable{Type: TypeLantern, GuildFunds: 500}
	assert.Equal(t, GuildOutcome{}, LanternOutcome(zeroThreshold, Progress{Count: 100}), "阈值 0 是坏配表,不发资金")
}

// 团圆锁存:未锁存必须本次凑够人;锁存后不再要求凑人;锁存与资金各只发生一次。
func TestReunionOutcome(t *testing.T) {
	row := &tablepb.GuildActivityTable{Type: TypeReunion, GuildThreshold: 3}

	_, ok := ReunionOutcome(row, Progress{}, false)
	assert.False(t, ok, "未锁存且没凑够人 → 回滚")

	out, ok := ReunionOutcome(row, Progress{}, true)
	assert.True(t, ok)
	assert.Equal(t, GuildOutcome{ReachedNow: true}, out, "团圆默认不发资金")

	out, ok = ReunionOutcome(row, Progress{ThresholdReachedMs: 1}, false)
	assert.True(t, ok, "锁存跨游戏日有效,之后不必再凑人")
	assert.Equal(t, GuildOutcome{}, out)

	funded := &tablepb.GuildActivityTable{Type: TypeReunion, GuildThreshold: 3, GuildFunds: 800}
	out, ok = ReunionOutcome(funded, Progress{}, true)
	assert.True(t, ok)
	assert.Equal(t, GuildOutcome{ReachedNow: true, GrantFunds: true}, out)
	out, ok = ReunionOutcome(funded, Progress{ThresholdReachedMs: 1, FundsGranted: true}, true)
	assert.True(t, ok)
	assert.Equal(t, GuildOutcome{}, out)
}

// 历练资金:每游戏日计资金胜场上限,差一还能计,满了只发个人奖。
func TestTrialFundsCounted(t *testing.T) {
	row := &tablepb.GuildActivityTable{Type: TypeTrial, GuildThreshold: 3, GuildFunds: 300}

	assert.True(t, TrialFundsCounted(row, 0))
	assert.True(t, TrialFundsCounted(row, 2))
	assert.False(t, TrialFundsCounted(row, 3))
	assert.False(t, TrialFundsCounted(&tablepb.GuildActivityTable{Type: TypeTrial, GuildThreshold: 3}, 0), "资金 0 不计")
}

// 优先级:NotOpen > LevelTooLow > JoinTooRecent > AlreadyClaimed > ThresholdNotReached。
func TestBlockedTipPriority(t *testing.T) {
	// 从"全都不满足"开始逐项修好,每修一项,返回值降到下一优先级。
	in := BlockInput{
		State:      StateUpcoming,
		GuildLevel: 1,
		MinLevel:   2,
		Joined:     false,
		Used:       1,
		DailyLimit: 1,
		Type:       TypeReunion,
		Progress:   2,
		Threshold:  3,
	}
	assert.Equal(t, constants.ErrActivityNotOpen, BlockedTip(in))
	in.State = StateOpen
	assert.Equal(t, constants.ErrActivityLevelTooLow, BlockedTip(in))
	in.GuildLevel = 2
	assert.Equal(t, constants.ErrActivityJoinTooRecent, BlockedTip(in))
	in.Joined = true
	assert.Equal(t, constants.ErrActivityAlreadyClaimed, BlockedTip(in))
	in.Used = 0
	assert.Equal(t, constants.ErrActivityThresholdNotReached, BlockedTip(in), "差一")
	in.Progress = 3
	assert.Zero(t, BlockedTip(in), "刚好达标")

	// 锁存后不看人数。
	latched := in
	latched.Progress, latched.ThresholdReached = 0, true
	assert.Zero(t, BlockedTip(latched))

	// 非团圆忽略阈值。
	lantern := in
	lantern.Type, lantern.Progress = TypeLantern, 0
	assert.Zero(t, BlockedTip(lantern))

	// 团圆阈值 0(坏配表)按不可达处理,不白送。
	broken := in
	broken.Threshold = 0
	assert.Equal(t, constants.ErrActivityThresholdNotReached, BlockedTip(broken))
}

// 提示参数与 06 §6.10 的错误映射一致。
func TestBlockedParams(t *testing.T) {
	base := BlockInput{State: StateOpen, GuildLevel: 5, MinLevel: 5, Joined: true, JoinMinHours: 24, DailyLimit: 1, Type: TypeReunion, Progress: 3, Threshold: 3}

	low := base
	low.GuildLevel = 4
	tip, params := Blocked(low)
	assert.Equal(t, constants.ErrActivityLevelTooLow, tip)
	assert.Equal(t, []string{"5"}, params)

	recent := base
	recent.Joined = false
	tip, params = Blocked(recent)
	assert.Equal(t, constants.ErrActivityJoinTooRecent, tip)
	assert.Equal(t, []string{"24"}, params)

	few := base
	few.Progress = 2
	tip, params = Blocked(few)
	assert.Equal(t, constants.ErrActivityThresholdNotReached, tip)
	assert.Equal(t, []string{"2", "3"}, params)

	tip, params = Blocked(base)
	assert.Zero(t, tip)
	assert.Nil(t, params)
}

func TestSelectVisibleAndPickForWrite(t *testing.T) {
	now := msOf(at(time.September, 21, 12, 0, 0, 0))

	// 同类型 Open(id 5)+ Upcoming(id 2)→ 选 Open 的 5;写 RPC 只接受 5。
	open5 := lanternRow(5, true, 0, 0)
	upcoming2 := lanternRow(2, true, now+msPerDay, now+2*msPerDay)
	rows := []*tablepb.GuildActivityTable{upcoming2, open5}
	got := SelectVisible(rows, now)
	require.Len(t, got, 1)
	assert.Equal(t, uint32(5), got[0].GetId())

	_, ok := PickForWrite(rows, TypeLantern, 2, now)
	assert.False(t, ok, "伪造同类型另一行的 id")
	row, ok := PickForWrite(rows, TypeLantern, 5, now)
	assert.True(t, ok)
	assert.Same(t, open5, row)
	_, ok = PickForWrite(rows, TypeReunion, 5, now)
	assert.False(t, ok, "类型不符")
	_, ok = PickForWrite(rows, TypeLantern, 99, now)
	assert.False(t, ok, "id 不存在")

	// 只有两行 Upcoming → start 小者;它不是 Open,写 RPC 拒。
	up7 := lanternRow(7, true, now+2*msPerDay, now+3*msPerDay)
	up8 := lanternRow(8, true, now+msPerDay, now+2*msPerDay)
	got = SelectVisible([]*tablepb.GuildActivityTable{up7, up8}, now)
	require.Len(t, got, 1)
	assert.Equal(t, uint32(8), got[0].GetId())
	_, ok = PickForWrite([]*tablepb.GuildActivityTable{up7, up8}, TypeLantern, 8, now)
	assert.False(t, ok, "选中行未开放")

	// 只剩已结束 / 未启用 → id 最小者,不可写。
	ended4 := lanternRow(4, true, now-2*msPerDay, now-msPerDay)
	disabled3 := lanternRow(3, false, 0, 0)
	got = SelectVisible([]*tablepb.GuildActivityTable{ended4, disabled3}, now)
	require.Len(t, got, 1)
	assert.Equal(t, uint32(3), got[0].GetId())
	_, ok = PickForWrite([]*tablepb.GuildActivityTable{ended4, disabled3}, TypeLantern, 3, now)
	assert.False(t, ok)

	// 缺类型跳过,结果按 type 升序;nil 行忽略。
	trial9 := &tablepb.GuildActivityTable{Id: 9, Name: "同道历练", Type: TypeTrial, Enabled: true}
	got = SelectVisible([]*tablepb.GuildActivityTable{trial9, nil, open5}, now)
	require.Len(t, got, 2)
	assert.Equal(t, uint32(5), got[0].GetId())
	assert.Equal(t, uint32(9), got[1].GetId())
	assert.Empty(t, SelectVisible(nil, now))
}

func TestBuildRewardBundle(t *testing.T) {
	lookup := rewardLookup(map[uint32]*tablepb.RewardTable{
		1: twoSlotReward(1, 1, 2),
		3: {Id: 3, Reward: []*tablepb.Rewardreward{{RewardItem: 7, RewardCount: 1}, {RewardItem: 3, RewardCount: 2}, {RewardItem: 7, RewardCount: 2}}},
		4: {Id: 4, Reward: []*tablepb.Rewardreward{{}, {RewardItem: 5}}},
		5: {Id: 5, Reward: []*tablepb.Rewardreward{{RewardItem: 1, RewardCount: math.MaxUint32}, {RewardItem: 1, RewardCount: 1}}},
		6: {Id: 6, Reward: []*tablepb.Rewardreward{{RewardItem: 0, RewardCount: 3}}},
	})

	b, err := buildRewardBundle(0, nil)
	require.NoError(t, err)
	assert.Nil(t, b, "0 = 无物品,且不查表")

	b, err = buildRewardBundle(1, lookup)
	require.NoError(t, err)
	assert.Equal(t, [][2]uint32{{1, 4}}, itemsOf(b), "两槽同物品合并")

	b, err = buildRewardBundle(3, lookup)
	require.NoError(t, err)
	assert.Equal(t, [][2]uint32{{3, 2}, {7, 3}}, itemsOf(b), "合并后按 item_id 升序")
	assert.Empty(t, b.GetCurrencies())

	b, err = buildRewardBundle(4, lookup)
	require.NoError(t, err)
	assert.Nil(t, b, "全是空槽 → 无物品")

	_, err = buildRewardBundle(99, lookup)
	assert.Error(t, err, "行不存在")
	_, err = buildRewardBundle(5, lookup)
	assert.Error(t, err, "合并后超过 uint32")
	_, err = buildRewardBundle(6, lookup)
	assert.Error(t, err, "有数量没物品")
	_, err = buildRewardBundle(1, nil)
	assert.Error(t, err, "缺 Reward 查询")
}

// 锁住"代码读的列"与"导表产物"没有脱节:Reward 1 两槽物品 1×2 → 单项物品 1×4。
// 只 Load 这一张表、用新建的管理器实例:不改全局快照;table.LoadTables 任何一张表缺文件都是 log.Fatalf。
// 前置:导表器已跑过(generated/tables/reward.json 存在)。
func TestBuildRewardBundleFromGeneratedTable(t *testing.T) {
	const tableDir = "../../../../generated/tables"
	m := table.NewRewardTableManager()
	require.NoError(t, m.Load(tableDir, false), "加载 %s/reward.json 失败(先跑导表器)", tableDir)

	b, err := buildRewardBundle(1, m.FindById)
	require.NoError(t, err)
	assert.Equal(t, [][2]uint32{{1, 4}}, itemsOf(b))
}

func TestRewardItems(t *testing.T) {
	assert.Nil(t, RewardItems(nil))
	assert.Nil(t, RewardItems(&assetpb.AssetBundle{}))

	got := RewardItems(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 3, Count: 2}, {ConfigId: 7, Count: 3}}})
	require.Len(t, got, 2)
	assert.Equal(t, uint32(3), got[0].GetItemId())
	assert.Equal(t, uint32(2), got[0].GetCount())
	assert.Equal(t, uint32(7), got[1].GetItemId())
	assert.Equal(t, uint32(3), got[1].GetCount())
}

func TestValidateRowsAcceptsDefaultRows(t *testing.T) {
	require.NoError(t, validateRows(legalTables()))
}

// 合法但容易被误判的情形。
func TestValidateRowsAccepts(t *testing.T) {
	t0 := msOf(at(time.September, 20, 10, 0, 0, 0))
	cases := []struct {
		name   string
		mutate func(*activityTables)
	}{
		{"重叠行未启用", func(e *activityTables) {
			e.rows = append(e.rows, lanternRow(4, false, t0, t0+msPerDay))
		}},
		{"首尾相接不算重叠", func(e *activityTables) {
			e.rows[0] = lanternRow(1, true, t0, t0+msPerDay)
			e.rows = append(e.rows, lanternRow(4, true, t0+msPerDay, t0+2*msPerDay))
		}},
		{"不同类型的时间窗互不影响", func(e *activityTables) {
			e.rows[1].StartAtMs, e.rows[1].EndAtMs = t0, t0+msPerDay
		}},
		{"团圆行阈值 0 用 GuildRule 兜底", func(e *activityTables) { e.rows[1].GuildThreshold = 0 }},
		{"团圆行阈值填了、GuildRule 为 0", func(e *activityTables) { e.rule.ReunionMinOnlineMembers = 0 }},
		{"建房冷却为 0", func(e *activityTables) { e.rule.TrialInviteCooldownSeconds = 0 }},
		{"入帮时长 720 小时", func(e *activityTables) { e.rule.ActivityJoinMinHours = 720 }},
		{"不发物品", func(e *activityTables) { e.rows[1].RewardId, e.rows[2].RewardId = 0, 0 }},
		{"活动表为空", func(e *activityTables) { e.rows = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tables := legalTables()
			tc.mutate(&tables)
			assert.NoError(t, validateRows(tables))
		})
	}
}

// 每条规则各造一个坏样例。错误文案必须带表名、行 id 与列名,策划看到启动日志要能直接定位到格子。
func TestValidateRowsRejects(t *testing.T) {
	t0 := msOf(at(time.September, 20, 10, 0, 0, 0))
	cases := []struct {
		name         string
		mutate       func(*activityTables)
		wantContains []string
	}{
		// GuildRule。
		{"缺少规则行", func(e *activityTables) { e.rule = nil }, []string{"GuildRule"}},
		{"入帮时长 721 小时", func(e *activityTables) { e.rule.ActivityJoinMinHours = 721 }, []string{"GuildRule[1]", "activity_join_min_hours"}},
		{"邀请有效期 5 秒", func(e *activityTables) { e.rule.TrialInviteTtlSeconds = 5 }, []string{"GuildRule[1]", "trial_invite_ttl_seconds"}},
		{"邀请有效期 121 秒", func(e *activityTables) { e.rule.TrialInviteTtlSeconds = 121 }, []string{"trial_invite_ttl_seconds"}},
		{"建房冷却 601 秒", func(e *activityTables) { e.rule.TrialInviteCooldownSeconds = 601 }, []string{"trial_invite_cooldown_seconds"}},
		{"团圆兜底阈值超过成员上限", func(e *activityTables) { e.rule.ReunionMinOnlineMembers = 101 }, []string{"reunion_min_online_members"}},

		// 行结构。
		{"空行", func(e *activityTables) { e.rows = append(e.rows, nil) }, []string{"GuildActivity", "第 4 行"}},
		{"id 为 0", func(e *activityTables) { e.rows[0].Id = 0 }, []string{"GuildActivity", "id=0"}},
		{"id 重复", func(e *activityTables) { e.rows[1].Id = 1 }, []string{"GuildActivity.id=1"}},
		{"名字为空", func(e *activityTables) { e.rows[0].Name = "  " }, []string{"GuildActivity[1].name"}},
		{"type=4", func(e *activityTables) { e.rows[0].Type = 4 }, []string{"GuildActivity[1].type=4"}},
		{"type=0", func(e *activityTables) { e.rows[0].Type = 0 }, []string{"GuildActivity[1].type=0"}},
		{"daily_limit=0", func(e *activityTables) { e.rows[0].DailyLimit = 0 }, []string{"GuildActivity[1].daily_limit"}},

		// 档期。
		{"end<start", func(e *activityTables) { e.rows[0].StartAtMs, e.rows[0].EndAtMs = t0+1, t0 }, []string{"GuildActivity[1]", "end_at_ms"}},
		{"end==start", func(e *activityTables) { e.rows[0].StartAtMs, e.rows[0].EndAtMs = t0, t0 }, []string{"GuildActivity[1]", "end_at_ms"}},
		{"只填了 end", func(e *activityTables) { e.rows[0].EndAtMs = t0 }, []string{"GuildActivity[1]", "start_at_ms"}},
		{"只填了 start", func(e *activityTables) { e.rows[0].StartAtMs = t0 }, []string{"GuildActivity[1]", "start_at_ms"}},
		{"超过 int64", func(e *activityTables) {
			e.rows[0].StartAtMs, e.rows[0].EndAtMs = 1<<63, 1<<63+1
		}, []string{"GuildActivity[1]", "int64"}},

		// 等级与数额。
		{"等级 0", func(e *activityTables) { e.rows[0].MinGuildLevel = 0 }, []string{"GuildActivity[1].min_guild_level=0"}},
		{"等级不存在", func(e *activityTables) { e.rows[0].MinGuildLevel = 11 }, []string{"GuildActivity[1].min_guild_level=11"}},
		{"帮贡超大", func(e *activityTables) { e.rows[0].PersonalContribution = maxPersonalContribution + 1 }, []string{"GuildActivity[1].personal_contribution"}},
		{"资金超大", func(e *activityTables) { e.rows[0].GuildFunds = maxActivityGuildFunds + 1 }, []string{"GuildActivity[1].guild_funds"}},

		// 奖励。
		{"奖励不存在", func(e *activityTables) { e.rows[1].RewardId = 99 }, []string{"GuildActivity[2].reward_id=99"}},
		{"奖励包全空", func(e *activityTables) {
			e.rewardOf = rewardLookup(map[uint32]*tablepb.RewardTable{1: twoSlotReward(1, 1, 0), 2: twoSlotReward(2, 1, 2)})
		}, []string{"GuildActivity[2].reward_id=1", "奖励包为空"}},
		{"奖励物品不在 Item 表", func(e *activityTables) { e.itemExists = setOf() }, []string{"GuildActivity[2].reward_id=1", "物品 1"}},
		{"奖励超过 16 种", func(e *activityTables) {
			slots := make([]*tablepb.Rewardreward, 0, 17)
			for item := uint32(1); item <= 17; item++ {
				slots = append(slots, &tablepb.Rewardreward{RewardItem: item, RewardCount: 1})
			}
			e.rewardOf = rewardLookup(map[uint32]*tablepb.RewardTable{1: {Id: 1, Reward: slots}, 2: twoSlotReward(2, 1, 2)})
			e.itemExists = func(uint32) bool { return true }
		}, []string{"GuildActivity[2].reward_id=1", "16"}},

		// 按类型。
		{"灯会阈值 0", func(e *activityTables) { e.rows[0].GuildThreshold = 0 }, []string{"GuildActivity[1].guild_threshold"}},
		{"灯会填了副本", func(e *activityTables) { e.rows[0].DungeonId = 1 }, []string{"GuildActivity[1]", "dungeon_id"}},
		{"团圆填了人数", func(e *activityTables) { e.rows[1].TeamSizeMax = 5 }, []string{"GuildActivity[2]", "team_size_max"}},
		{"团圆两个阈值都为 0", func(e *activityTables) {
			e.rows[1].GuildThreshold, e.rule.ReunionMinOnlineMembers = 0, 0
		}, []string{"GuildActivity[2].guild_threshold", "reunion_min_online_members"}},
		{"团圆阈值超过成员上限", func(e *activityTables) { e.rows[1].GuildThreshold = 101 }, []string{"GuildActivity[2].guild_threshold=101"}},
		{"历练副本为 0", func(e *activityTables) { e.rows[2].DungeonId = 0 }, []string{"GuildActivity[3].dungeon_id=0"}},
		{"历练副本不存在", func(e *activityTables) { e.rows[2].DungeonId = 99 }, []string{"GuildActivity[3].dungeon_id=99"}},
		{"历练人数上限 6", func(e *activityTables) { e.rows[2].TeamSizeMax = 6 }, []string{"GuildActivity[3]", "team_size_max=6"}},
		{"历练人数下限 1", func(e *activityTables) { e.rows[2].TeamSizeMin = 1 }, []string{"GuildActivity[3]", "team_size_min=1"}},
		{"历练下限大于上限", func(e *activityTables) { e.rows[2].TeamSizeMin, e.rows[2].TeamSizeMax = 4, 3 }, []string{"GuildActivity[3]", "team_size_min=4"}},
		{"历练阈值 0", func(e *activityTables) { e.rows[2].GuildThreshold = 0 }, []string{"GuildActivity[3].guild_threshold"}},

		// 同类型启用行时间窗重叠。
		{"两行灯会时间窗重叠", func(e *activityTables) {
			e.rows[0] = lanternRow(1, true, t0, t0+10*msPerDay)
			e.rows = append(e.rows, lanternRow(4, true, t0+5*msPerDay, t0+15*msPerDay))
		}, []string{"GuildActivity[1]", "GuildActivity[4]", "重叠"}},
		{"常开灯会与另一启用灯会并存", func(e *activityTables) {
			e.rows = append(e.rows, lanternRow(4, true, t0, t0+msPerDay))
		}, []string{"GuildActivity[1]", "GuildActivity[4]", "重叠"}},

		// 接线错误。
		{"缺 Item 表查询", func(e *activityTables) { e.itemExists = nil }, []string{"Item"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tables := legalTables()
			tc.mutate(&tables)
			err := validateRows(tables)
			require.Error(t, err)
			for _, want := range tc.wantContains {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestBuildView(t *testing.T) {
	now := at(time.September, 21, 12, 0, 0, 0)
	nowMs := msOf(now)
	rule := legalRule()
	rule.ActivityJoinMinHours = 24
	lookup := rewardLookup(legalRewards())
	joinedLongAgo := nowMs - 25*msPerHour

	t.Run("灯会:本档期已达成、今日已点", func(t *testing.T) {
		v, err := buildView(legalRows()[0], ViewInput{
			Now:         now,
			Rule:        rule,
			GuildLevel:  1,
			JoinTimeMs:  joinedLongAgo,
			MyUsedCount: 1,
			Progress:    Progress{Count: 3, ThresholdReachedMs: 123, FundsGranted: true},
		}, lookup)
		require.NoError(t, err)
		assert.Equal(t, uint32(1), v.GetActivityId())
		assert.Equal(t, guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_LANTERN, v.GetType())
		assert.Equal(t, guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_OPEN, v.GetState())
		assert.Equal(t, uint32(3), v.GetProgress())
		assert.Equal(t, uint32(3), v.GetGuildThreshold())
		assert.True(t, v.GetThresholdReached())
		assert.True(t, v.GetFundsGranted())
		assert.Equal(t, constants.ErrActivityAlreadyClaimed, v.GetBlockedTipId())
		assert.Equal(t, uint32(20260921), v.GetPeriodKey())
		assert.Equal(t, uint32(20260921), v.GetGuildPeriodKey(), "0/0 常开行的档期键退化为当天")
		assert.Equal(t, msOf(at(time.September, 22, 5, 0, 0, 0)), v.GetNextResetMs())
		assert.Equal(t, nowMs, v.GetServerTimeMs())
		assert.Equal(t, uint32(24), v.GetJoinMinHours())
		assert.Equal(t, uint32(1), v.GetMyUsedCount())
		assert.Empty(t, v.GetRewardItems())
	})

	t.Run("团圆未锁存:进度是合格在线人数,差一人", func(t *testing.T) {
		v, err := buildView(legalRows()[1], ViewInput{Now: now, Rule: rule, GuildLevel: 1, JoinTimeMs: joinedLongAgo, ReunionOnline: 2}, lookup)
		require.NoError(t, err)
		assert.Equal(t, uint32(2), v.GetProgress())
		assert.Equal(t, uint32(3), v.GetGuildThreshold())
		assert.False(t, v.GetThresholdReached())
		assert.Equal(t, constants.ErrActivityThresholdNotReached, v.GetBlockedTipId())
		require.Len(t, v.GetRewardItems(), 1)
		assert.Equal(t, uint32(1), v.GetRewardItems()[0].GetItemId())
		assert.Equal(t, uint32(4), v.GetRewardItems()[0].GetCount())
	})

	t.Run("团圆已锁存:进度填 0、不再要求凑人", func(t *testing.T) {
		v, err := buildView(legalRows()[1], ViewInput{
			Now:           now,
			Rule:          rule,
			GuildLevel:    1,
			JoinTimeMs:    joinedLongAgo,
			Progress:      Progress{ThresholdReachedMs: 1},
			ReunionOnline: 1,
		}, lookup)
		require.NoError(t, err)
		assert.Zero(t, v.GetProgress())
		assert.True(t, v.GetThresholdReached())
		assert.Zero(t, v.GetBlockedTipId())
	})

	t.Run("入帮差 1ms 满 24 小时", func(t *testing.T) {
		v, err := buildView(legalRows()[0], ViewInput{Now: now, Rule: rule, GuildLevel: 1, JoinTimeMs: nowMs - ms24h + 1}, lookup)
		require.NoError(t, err)
		assert.Equal(t, constants.ErrActivityJoinTooRecent, v.GetBlockedTipId())
	})

	t.Run("历练不可用:强制 DISABLED + NotOpen,不带对局与房间", func(t *testing.T) {
		v, err := buildView(legalRows()[2], ViewInput{
			Now:              now,
			Rule:             rule,
			GuildLevel:       1,
			JoinTimeMs:       joinedLongAgo,
			MyTrialBattleID:  77,
			TrialUnavailable: true,
		}, lookup)
		require.NoError(t, err)
		assert.Equal(t, guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_DISABLED, v.GetState())
		assert.Equal(t, constants.ErrActivityNotOpen, v.GetBlockedTipId())
		assert.Zero(t, v.GetMyTrialBattleId())
	})

	t.Run("历练可用:进度是今日计资金胜场,阈值是每日上限", func(t *testing.T) {
		v, err := buildView(legalRows()[2], ViewInput{
			Now:             now,
			Rule:            rule,
			GuildLevel:      1,
			JoinTimeMs:      joinedLongAgo,
			Progress:        Progress{Count: 2},
			MyTrialBattleID: 77,
		}, lookup)
		require.NoError(t, err)
		assert.Equal(t, guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_OPEN, v.GetState())
		assert.Zero(t, v.GetBlockedTipId())
		assert.Equal(t, uint32(2), v.GetProgress())
		assert.Equal(t, uint32(3), v.GetGuildThreshold())
		assert.False(t, v.GetThresholdReached())
		assert.Equal(t, uint64(77), v.GetMyTrialBattleId())
		assert.Equal(t, uint32(1), v.GetDungeonId())
		assert.Equal(t, uint32(2), v.GetTeamSizeMin())
		assert.Equal(t, uint32(5), v.GetTeamSizeMax())
	})

	t.Run("坏输入报错而不是回零值视图", func(t *testing.T) {
		_, err := buildView(nil, ViewInput{Now: now, Rule: rule}, lookup)
		assert.Error(t, err)
		_, err = buildView(legalRows()[0], ViewInput{Now: now}, lookup)
		assert.Error(t, err, "缺规则行")
		_, err = buildView(legalRows()[0], ViewInput{Rule: rule}, lookup)
		assert.Error(t, err, "缺 now")
		_, err = buildView(legalRows()[1], ViewInput{Now: now, Rule: rule}, rewardLookup(nil))
		assert.Error(t, err, "奖励行不存在")
	})
}
