package data

// activity_repo_static_test.go —— 帮会活动 repo(B6a 点灯 / 团圆,B6b 同道历练)**不连库**的契约:
// 入参校验先于碰库、语句文本形状、源码里的取锁先后,以及历练结算的两个纯函数(入参规范化、历史周期键是否还能写)。
//
// 为什么单独成文件、且**不带 build tag**:这三条既不连 MySQL 也不连 Redis,原先放在 activity_repo_integration_test.go
// (`//go:build integration`)里,普通 `go test ./...` 根本编译不到它们 —— 锁序被人调换、点锁被删、候选读被补上 FOR UPDATE,
// 只有在专门带 -tags=integration 且配了库的那一轮才会红,等于日常回归里没有这道闸。死锁是用户硬要求(92-handoff §12.0),
// 这类确定性的结构检查必须在任何环境、每一次 go test 里都跑。
//
// 与 point_lock_order_test.go 同一套判据(pointLockOrderCase / pointLockFuncBody / pointLockFirstIdents 定义在那边,同样不带 tag)。
// 需要真库的事务语义与并发死锁回归(I1–I23)仍在 activity_repo_integration_test.go。
//
// actLanternRow / actTrialRow / actInput 几个助手也放在这里:本文件与集成测试都用它们,放在不带 tag 的文件里两种构建都能看见;
// 反过来放在集成文件里,本文件在普通构建下就编译不过。

import (
	"context"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "proto/guild"

	tablepb "shared/generated/pb/table"

	"guild/internal/activity"
)

// actLanternRow 取 06 §6.2.2 的灯会开发默认行(0/0 常开);用例按需改字段。团圆行 actReunionRow 只有集成测试用,留在那边。
func actLanternRow() *tablepb.GuildActivityTable {
	return &tablepb.GuildActivityTable{
		Id: 1, Name: "元宵灯会", Type: activity.TypeLantern, Enabled: true, MinGuildLevel: 1,
		PersonalContribution: 20, GuildFunds: 500, GuildThreshold: 3, DailyLimit: 1,
	}
}

func actInput(row *tablepb.GuildActivityTable, guildID, playerID, nowMs uint64) ActivityTxInput {
	return ActivityTxInput{PlayerID: playerID, GuildID: guildID, Activity: row, NowMs: nowMs}
}

// actTrialRow 取 06 §6.2.2 的历练开发默认行(0/0 常开):每胜个人帮贡 50、帮会资金 300、每游戏日计资金胜场上限 3、
// 每人每游戏日得奖 2 次;用例按需改字段。
func actTrialRow() *tablepb.GuildActivityTable {
	return &tablepb.GuildActivityTable{
		Id: 3, Name: "同道历练", Type: activity.TypeTrial, Enabled: true, MinGuildLevel: 1,
		PersonalContribution: 50, GuildFunds: 300, GuildThreshold: 3, RewardId: 2,
		DungeonId: 1, TeamSizeMin: 2, TeamSizeMax: 5, DailyLimit: 2,
	}
}

// actTrialKey 是一局历练的身份:开战时刻 startMs 决定两个周期键(历练的帮会期键就是游戏日键)。
func actTrialKey(row *tablepb.GuildActivityTable, battleID, guildID, initiator, startMs uint64) TrialBattleKey {
	start := time.UnixMilli(int64(startMs))
	return TrialBattleKey{
		BattleID: battleID, GuildID: guildID, ActivityID: row.GetId(),
		PeriodKey: activity.DayKey(start), GuildPeriodKey: activity.GuildPeriodKey(row, start),
		InitiatorPlayerID: initiator,
	}
}

// actTrialWin 是一局胜利结算的输入骨架(不带物品,开战与结算同一时刻);用例按需改字段。
func actTrialWin(row *tablepb.GuildActivityTable, battleID, guildID uint64, nowMs uint64, candidates ...uint64) TrialSettleInput {
	var initiator uint64
	if len(candidates) > 0 {
		initiator = candidates[0]
	}
	return TrialSettleInput{
		Battle:   actTrialKey(row, battleID, guildID, initiator, nowMs),
		Activity: row, Win: true, Candidates: candidates,
		FinishedAtMs: nowMs, NowMs: nowMs,
	}
}

// TestActivityTxRejectsMalformedInputBeforeTouchingStorage:编程错误在碰库之前失败(GuildRepo 没有连接池,
// 越过校验就会当场 panic),而且不许伪装成任何业务拒绝 —— 否则一个坏配表会被 logic 翻成"今日已领"之类的正常提示。
func TestActivityTxRejectsMalformedInputBeforeTouchingStorage(t *testing.T) {
	_, err := NewActivityRepo(nil)
	assert.Error(t, err)

	act, err := NewActivityRepo(econNoDBRepo(t))
	require.NoError(t, err)
	ctx := context.Background()
	good := func() ActivityTxInput {
		in := actInput(actLanternRow(), 3, 2, testNowMs)
		in.Reward = &ActivityRewardOp{OpID: 1, Payload: []byte{0x08}, LeaseUntilMs: testNowMs + econLeaseMs, LeaseToken: 7}
		return in
	}
	breakers := []struct {
		name    string
		breakIt func(*ActivityTxInput)
	}{
		{"player 为 0", func(in *ActivityTxInput) { in.PlayerID = 0 }},
		{"guild 为 0", func(in *ActivityTxInput) { in.GuildID = 0 }},
		{"配表行为空", func(in *ActivityTxInput) { in.Activity = nil }},
		{"行类型与事务不符", func(in *ActivityTxInput) { in.Activity.Type = activity.TypeReunion }},
		{"daily_limit 为 0", func(in *ActivityTxInput) { in.Activity.DailyLimit = 0 }},
		{"now 为 0", func(in *ActivityTxInput) { in.NowMs = 0 }},
		{"活动已关闭", func(in *ActivityTxInput) { in.Activity.Enabled = false }},
		{"档期已结束", func(in *ActivityTxInput) { in.Activity.StartAtMs, in.Activity.EndAtMs = 1, testNowMs }},
		{"op_id 为 0", func(in *ActivityTxInput) { in.Reward.OpID = 0 }},
		{"lease token 为 0", func(in *ActivityTxInput) { in.Reward.LeaseToken = 0 }},
		{"租约不晚于 now", func(in *ActivityTxInput) { in.Reward.LeaseUntilMs = in.NowMs }},
		{"payload 为空", func(in *ActivityTxInput) { in.Reward.Payload = nil }},
	}
	businessRejections := []error{
		ErrActivityAlreadyClaimed, ErrActivityJoinTooRecent, ErrActivityThresholdNotReached,
		ErrGuildGone, ErrNotGuildMember, ErrGuildLevelTooLow, ErrZoneMerging, ErrWriteConflict,
	}
	for _, b := range breakers {
		in := good()
		b.breakIt(&in)
		_, err := act.LightLanternTx(ctx, in)
		require.Error(t, err, b.name)
		for _, sentinel := range businessRejections {
			assert.NotErrorIs(t, err, sentinel, "%s:编程错误不能伪装成业务拒绝", b.name)
		}
	}
	_, err = act.ClaimReunionTx(ctx, good(), true)
	assert.Error(t, err, "团圆事务拒收灯会行")
}

// TestTrialTxRejectsMalformedInputBeforeTouchingStorage:历练各写事务的畸形输入在碰库之前失败(GuildRepo 没有连接池,
// 越过校验就会当场 panic),并且一律包 ErrTrialInputInvalid —— 结算消费者靠它区分"确定性失败(标毒、提交 offset)"
// 与"暂时性失败(退避重试)",分错了要么卡住分区、要么把本可重试的局标成毒消息。
func TestTrialTxRejectsMalformedInputBeforeTouchingStorage(t *testing.T) {
	act, err := NewActivityRepo(econNoDBRepo(t))
	require.NoError(t, err)
	ctx := context.Background()
	const (
		battleID uint64 = 5001
		guildID  uint64 = 3
		first    uint64 = 11
		second   uint64 = 12
	)
	good := func() TrialSettleInput {
		in := actTrialWin(actTrialRow(), battleID, guildID, testNowMs, first, second)
		in.Reward = &TrialReward{
			Payload: []byte{0x08}, LeaseUntilMs: testNowMs + econLeaseMs,
			Ops: []TrialRewardOp{{PlayerID: first, OpID: 1, LeaseToken: 7}, {PlayerID: second, OpID: 2, LeaseToken: 8}},
		}
		return in
	}
	_, err = good().plan()
	require.NoError(t, err, "基准输入本身必须合法,否则下面每一条都是假阳性")

	breakers := []struct {
		name    string
		breakIt func(*TrialSettleInput)
	}{
		{"battle 为 0", func(in *TrialSettleInput) { in.Battle.BattleID = 0 }},
		{"guild 为 0", func(in *TrialSettleInput) { in.Battle.GuildID = 0 }},
		{"activity id 为 0", func(in *TrialSettleInput) { in.Battle.ActivityID = 0 }},
		{"个人周期键为 0", func(in *TrialSettleInput) { in.Battle.PeriodKey = 0 }},
		{"帮会期键为 0", func(in *TrialSettleInput) { in.Battle.GuildPeriodKey = 0 }},
		{"now 为 0", func(in *TrialSettleInput) { in.NowMs = 0 }},
		{"配表行不是历练", func(in *TrialSettleInput) { in.Activity = actLanternRow(); in.Battle.ActivityID = in.Activity.GetId() }},
		{"配表行与 activity id 不符", func(in *TrialSettleInput) { in.Battle.ActivityID = 99 }},
		{"daily_limit 为 0", func(in *TrialSettleInput) { in.Activity.DailyLimit = 0 }},
		{"候选含 0", func(in *TrialSettleInput) { in.Candidates = []uint64{first, 0} }},
		{"候选超上限", func(in *TrialSettleInput) { in.Candidates = make([]uint64, maxTrialCandidates+1) }},
		{"payload 为空", func(in *TrialSettleInput) { in.Reward.Payload = nil }},
		{"租约不晚于 now", func(in *TrialSettleInput) { in.Reward.LeaseUntilMs = in.NowMs }},
		{"候选人缺指令参数", func(in *TrialSettleInput) { in.Reward.Ops = in.Reward.Ops[:1] }},
		{"同一人两份指令参数", func(in *TrialSettleInput) { in.Reward.Ops[1].PlayerID = first }},
		{"op_id 为 0", func(in *TrialSettleInput) { in.Reward.Ops[0].OpID = 0 }},
		{"lease token 为 0", func(in *TrialSettleInput) { in.Reward.Ops[1].LeaseToken = 0 }},
	}
	for _, b := range breakers {
		in := good()
		b.breakIt(&in)
		_, err := act.SettleTrialBattleTx(ctx, in)
		require.ErrorIs(t, err, ErrTrialInputInvalid, b.name)
		assert.NotErrorIs(t, err, ErrActivityPoison, "%s:畸形输入与溢出是两类确定性失败,不混用哨兵", b.name)
		assert.NotErrorIs(t, err, ErrWriteConflict, "%s:畸形输入不能伪装成可重试的写冲突", b.name)
	}

	key := good().Battle
	incomplete := key
	incomplete.GuildPeriodKey = 0
	assert.ErrorIs(t, act.RegisterTrialBattle(ctx, incomplete, testNowMs), ErrTrialInputInvalid, "登记要完整的上下文")
	assert.ErrorIs(t, act.RegisterTrialBattle(ctx, key, 0), ErrTrialInputInvalid)
	_, err = act.MarkTrialBattlePoison(ctx, TrialBattleKey{GuildID: guildID}, 0, testNowMs)
	assert.ErrorIs(t, err, ErrTrialInputInvalid, "标记至少要 battle_id 与 guild_id")
	_, err = act.MarkTrialBattlePoison(ctx, key, 0, 0)
	assert.ErrorIs(t, err, ErrTrialInputInvalid)
	_, err = act.ExpireTrialBattle(ctx, battleID, 0, testNowMs)
	assert.ErrorIs(t, err, ErrTrialInputInvalid)
	_, err = act.ExpireTrialBattle(ctx, battleID, guildID, 0)
	assert.ErrorIs(t, err, ErrTrialInputInvalid)

	owed := OwedReward{PlayerID: first, BattleID: battleID, GuildID: guildID, ActivityID: 3, PeriodKey: key.PeriodKey, Payload: []byte{0x08}}
	owedBreakers := []struct {
		name    string
		breakIt func(row *OwedReward, opID, nowMs *uint64)
	}{
		{"player 为 0", func(row *OwedReward, _, _ *uint64) { row.PlayerID = 0 }},
		{"battle 为 0", func(row *OwedReward, _, _ *uint64) { row.BattleID = 0 }},
		{"payload 为空", func(row *OwedReward, _, _ *uint64) { row.Payload = nil }},
		{"op_id 为 0", func(_ *OwedReward, opID, _ *uint64) { *opID = 0 }},
		{"now 为 0", func(_ *OwedReward, _, nowMs *uint64) { *nowMs = 0 }},
	}
	for _, b := range owedBreakers {
		row, opID, nowMs := owed, uint64(9), testNowMs
		b.breakIt(&row, &opID, &nowMs)
		_, err := act.ConvertOwedReward(ctx, row, opID, nowMs)
		assert.ErrorIs(t, err, ErrTrialInputInvalid, b.name)
	}

	_, err = act.TrialRosterMembers(ctx, guildID, make([]uint64, maxTrialCandidates+1))
	assert.ErrorIs(t, err, ErrTrialInputInvalid)
	members, err := act.TrialRosterMembers(ctx, guildID, nil)
	require.NoError(t, err, "空名单不碰库")
	assert.Empty(t, members)
}

// TestTrialSettlePlan:结算入参的规范化是纯函数 —— 候选去重升序(取锁顺序由它定);结论与"发不发奖"只由胜负、配表行、候选决定。
// U2:候选由 logic 给(team 0 − 逃跑,含阵亡者),repo 不再二次过滤。
func TestTrialSettlePlan(t *testing.T) {
	const (
		battleID uint64 = 5002
		guildID  uint64 = 3
	)
	win := pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN
	loss := pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS
	configMissing := pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_CONFIG_MISSING

	plan, err := actTrialWin(actTrialRow(), battleID, guildID, testNowMs, 9, 3, 9, 5, 3).plan()
	require.NoError(t, err)
	assert.Equal(t, []uint64{3, 5, 9}, plan.candidates, "去重、升序")
	assert.Equal(t, win, plan.result)
	assert.True(t, plan.grant)
	assert.Nil(t, plan.ops, "没有物品奖励")

	lost := actTrialWin(actTrialRow(), battleID, guildID, testNowMs, 3, 5)
	lost.Win = false
	// 不发奖的局不看 Reward:即便给了一份不完整的也不算畸形。
	lost.Reward = &TrialReward{}
	plan, err = lost.plan()
	require.NoError(t, err)
	assert.Equal(t, loss, plan.result, "失败 / 平局")
	assert.False(t, plan.grant)

	missing := actTrialWin(actTrialRow(), battleID, guildID, testNowMs, 3, 5)
	missing.Activity = nil
	plan, err = missing.plan()
	require.NoError(t, err)
	assert.Equal(t, configMissing, plan.result, "赢了但配表行没了:记 CONFIG_MISSING,不发奖")
	assert.False(t, plan.grant)

	lostAndMissing := missing
	lostAndMissing.Win = false
	plan, err = lostAndMissing.plan()
	require.NoError(t, err)
	assert.Equal(t, configMissing, plan.result, "配表缺行优先于胜负")

	nobody := actTrialWin(actTrialRow(), battleID, guildID, testNowMs)
	plan, err = nobody.plan()
	require.NoError(t, err)
	assert.Equal(t, win, plan.result, "全员逃跑的胜局仍记 WIN")
	assert.False(t, plan.grant, "没有候选就没有可发的奖")

	withItems := actTrialWin(actTrialRow(), battleID, guildID, testNowMs, 5, 3)
	withItems.Reward = &TrialReward{
		Payload: []byte{0x08}, LeaseUntilMs: testNowMs + 1,
		// 多给一份不在候选里的参数是允许的(logic 按 team 0 全员预备也无妨),少给才是错。
		Ops: []TrialRewardOp{{PlayerID: 3, OpID: 31, LeaseToken: 1}, {PlayerID: 5, OpID: 51, LeaseToken: 2}, {PlayerID: 7, OpID: 71, LeaseToken: 3}},
	}
	plan, err = withItems.plan()
	require.NoError(t, err)
	require.Len(t, plan.ops, 3)
	assert.Equal(t, uint64(51), plan.ops[5].OpID)
}

// TestTrialCounterPeriodWritable:结算写的是开战那天的计数行。只有当这个周期键严格晚于"计数行清理在本次结算期间可能用到的
// 最大截止键"时才允许写(activity_repo.go 文件头第 8 条)—— 否则清理(不持 seq 行守卫)可能与本次 upsert 落在同一行上。
func TestTrialCounterPeriodWritable(t *testing.T) {
	now := time.UnixMilli(int64(testNowMs))
	dayKeyAgo := func(d time.Duration) uint32 { return activity.DayKey(now.Add(-d)) }

	assert.True(t, trialCounterPeriodWritable(dayKeyAgo(0), testNowMs), "当天开战")
	assert.True(t, trialCounterPeriodWritable(dayKeyAgo(5*time.Minute), testNowMs), "常态:几分钟前开战")
	// 结果记录与 Kafka 都只留 7 天:6 天 23 小时前开战的局必须还能结算(与边界相隔整 24 小时,游戏日键必然更大)。
	assert.True(t, trialCounterPeriodWritable(dayKeyAgo(7*24*time.Hour-time.Hour), testNowMs))

	boundary := dayKeyAgo(minCounterCleanupAge - trialCounterCleanupMargin)
	assert.False(t, trialCounterPeriodWritable(boundary, testNowMs), "恰好落在边界那一天:不写")
	assert.False(t, trialCounterPeriodWritable(dayKeyAgo(minCounterCleanupAge), testNowMs), "已在清理范围内")
	assert.False(t, trialCounterPeriodWritable(dayKeyAgo(30*24*time.Hour), testNowMs))

	// 与清理的关系:墙钟不晚于"本次结算的 now + 余量"的任何一轮清理,截止键都 ≤ boundary;保留期调长只会让截止键更早。
	for _, retention := range []time.Duration{0, minCounterCleanupAge, 30 * 24 * time.Hour} {
		for _, lead := range []time.Duration{-time.Hour, 0, trialCounterCleanupMargin} {
			dayCutoff, _ := counterCleanupCutoffs(now.Add(lead), retention)
			assert.LessOrEqual(t, dayCutoff, boundary, "retention=%v lead=%v:清理截止键不得越过结算的可写下界", retention, lead)
		}
	}
}

// TestActivityStatementsShape:锁定语句只做完整主键等值点操作、候选 / 视图读不带锁定子句(92-handoff §12.2 第 2 条)。
// EXPLAIN 回归管不到"语句文本被顺手改了"(比如给候选读补一个 FOR UPDATE、把逐行点删改回前缀范围删),这里钉文本。
func TestActivityStatementsShape(t *testing.T) {
	const progressPK = "WHERE guild_id = ? AND activity_id = ? AND period_key = ?"

	assert.True(t, strings.HasSuffix(sqlLockActivityGuild, "FROM guild WHERE guild_id = ? FOR UPDATE"), sqlLockActivityGuild)
	assert.Contains(t, sqlLockActivityMember, "FORCE INDEX (PRIMARY)", "成员行锁定读必须钉死主键(理由见 sqlLockMemberRole)")
	assert.True(t, strings.HasSuffix(sqlLockActivityMember, "WHERE guild_id = ? AND player_id = ? FOR UPDATE"), sqlLockActivityMember)
	assert.Contains(t, sqlSetActivityContribution, "FORCE INDEX (PRIMARY)")
	assert.True(t, strings.HasSuffix(sqlSetActivityContribution, "WHERE guild_id = ? AND player_id = ?"), sqlSetActivityContribution)
	assert.True(t, strings.HasSuffix(sqlSetActivityFunds, "WHERE guild_id = ?"), sqlSetActivityFunds)
	assert.True(t, strings.HasSuffix(sqlLockActivityProgress, progressPK+" FOR UPDATE"), sqlLockActivityProgress)

	// 进度表的写:完整主键等值,不带复核条件(TiDB 走点写快路径,见 activity_repo.go 文件头)。
	for name, query := range map[string]string{
		"sqlLatchActivityProgress":  sqlLatchActivityProgress,
		"sqlDeleteActivityProgress": sqlDeleteActivityProgress,
		"sqlSetTrialProgressCount":  sqlSetTrialProgressCount,
	} {
		assert.True(t, strings.HasSuffix(query, progressPK), "%s 必须以完整主键等值结尾:%s", name, query)
	}

	// 历练(B6b)。点锁只许"完整主键等值 + FOR UPDATE、不带复核条件"(TiDB 的 Point_Get 快路径);
	// 写语句以完整主键等值结尾、不带复核条件(状态在锁内读回、在 Go 里判)。
	assert.True(t, strings.HasSuffix(sqlLockTrialGuild, "FROM guild WHERE guild_id = ? FOR UPDATE"), sqlLockTrialGuild)
	assert.Equal(t, "SELECT state FROM "+guildTrialBattleTable+" WHERE battle_id = ? FOR UPDATE", sqlLockTrialBattle)
	for name, query := range map[string]string{
		"sqlSetTrialBattleState": sqlSetTrialBattleState,
		"sqlDeleteTrialBattle":   sqlDeleteTrialBattle,
	} {
		assert.True(t, strings.HasSuffix(query, "WHERE battle_id = ?"), "%s 必须以完整主键等值结尾:%s", name, query)
	}
	for name, query := range map[string]string{
		"sqlDeleteOwedReward":       sqlDeleteOwedReward,
		"sqlBumpOwedRewardAttempts": sqlBumpOwedRewardAttempts,
	} {
		assert.True(t, strings.HasSuffix(query, "WHERE player_id = ? AND battle_id = ?"), "%s 必须以完整主键等值结尾:%s", name, query)
	}
	// 诊断计数是自动提交的单 key 写:只许改无索引列 attempts(改到 created_ms 就会动 idx_0,不再是单 key)。
	assert.Contains(t, sqlBumpOwedRewardAttempts, " SET attempts = attempts + 1 WHERE ")
	// 两张表的插入都是普通 INSERT:同帮没有第二个插入者,撞键必须报出来,不许被 upsert / IGNORE 吞掉。
	for name, query := range map[string]string{
		"sqlInsertTrialBattle": sqlInsertTrialBattle,
		"sqlInsertOwedReward":  sqlInsertOwedReward,
	} {
		upper := strings.ToUpper(query)
		assert.True(t, strings.HasPrefix(upper, "INSERT INTO "), "%s:%s", name, query)
		assert.NotContains(t, upper, "DUPLICATE", "%s:%s", name, query)
		assert.NotContains(t, upper, "IGNORE", "%s:%s", name, query)
	}

	// 普通读:不许带任何锁定子句。
	for name, query := range map[string]string{
		"sqlSelectActivityProgress":            sqlSelectActivityProgress,
		"sqlSelectActivityProgressKeysOfGuild": sqlSelectActivityProgressKeysOfGuild,
		"sqlSelectActivityUsage":               sqlSelectActivityUsage,
		"sqlSelectPendingActivityRewards":      sqlSelectPendingActivityRewards,
		"sqlSelectRecentRejectedRewards":       sqlSelectRecentRejectedRewards,
		"sqlSelectActivityCounter":             sqlSelectActivityCounter,
		"trial roster members":                 sqlSelectTrialRosterMembersHead + placeholders(2) + sqlSelectTrialRosterMembersTail,
		"sqlSelectTrialBattleGate":             sqlSelectTrialBattleGate,
		"sqlSelectTrialBattle":                 sqlSelectTrialBattle,
		"sqlSelectOverdueTrialBattles":         sqlSelectOverdueTrialBattles,
		"sqlSelectStartedTrialBattlesOfGuild":  sqlSelectStartedTrialBattlesOfGuild,
		"sqlSelectOwedRewardsPage":             sqlSelectOwedRewardsPage,
		"sqlCountOwedRewardsByActivity":        sqlCountOwedRewardsByActivity,
		"sqlCountOwedRewardsCapped":            sqlCountOwedRewardsCapped,
	} {
		upper := strings.ToUpper(query)
		for _, locking := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
			assert.NotContains(t, upper, locking, "%s 是普通读:%s", name, query)
		}
	}
}

// TestActivityLockOrderInSource:事务体里各表第一次被碰到的先后 = 表间全序(92-handoff §12.2 第 1 条:… < C < P < T < W),
// 解散里删进度行、删在途对局行排在提前截止之后、删 guild 行之前(X-14)。判据与 point_lock_order_test.go 相同:
// 只认标识符在函数体内第一次出现的源码位置;有人调换语句、删掉某一步或改名,这里都会红。
// 历练结算的事务体拆成了若干步骤函数:这里先钉步骤的先后(settleTrialBattle),再逐个钉步骤内部的语句先后。
func TestActivityLockOrderInSource(t *testing.T) {
	cases := []pointLockOrderCase{
		{"activity_repo.go", "participate", []string{
			// G → 闸门 → M
			"sqlLockActivityGuild", "checkFence", "sqlLockActivityMember",
			// Q(建行 / 分配 / 守卫)→ O
			"ensureSeqRowTx", "AllocateSeq", "insertAssetOp", "sqlLockSeqGuard",
			// C
			"upsertCounterWithLimit",
			// P
			"sqlUpsertLanternProgress", "sqlLockActivityProgress",
			// 再写本事务已持有的 G / P / M
			"sqlSetActivityFunds", "sqlLatchActivityProgress", "sqlSetActivityContribution",
		}},
		{"activity_repo.go", "deleteGuildActivityProgress", []string{"readActivityProgressKeys", "sqlDeleteActivityProgress"}},
		{"activity_repo.go", "deleteGuildTrialBattles", []string{"sqlSelectStartedTrialBattlesOfGuild", "sqlDeleteTrialBattle"}},
		{"guild_manage_repo.go", "DisbandGuild", []string{
			"lockGuildRow", "checkFence", "lockAllMembers", "sqlDeleteMember", "deleteApplicationRows",
			"accelerateDonationDeadlines", "deleteGuildActivityProgress", "deleteGuildTrialBattles", "sqlDeleteGuild",
		}},

		// 历练结算:G → 闸门 → [T 普通读] → M → Q(全部)→ [C 普通读] → O → C → P(+ 再写 G)→ 再写 M → T → W。
		{"activity_repo.go", "settleTrialBattle", []string{
			"lockTrialGuild", "checkFence", "readTrialGate",
			"lockTrialMembers", "lockTrialSeqGuards", "readTrialRewarded",
			"enqueueTrialRewards", "occupyTrialCounters", "grantTrialFunds", "creditTrialContribution",
			"writeTrialBattleState", "insertOwedRewards",
		}},
		{"activity_repo.go", "lockTrialGuild", []string{"sqlLockTrialGuild"}},
		{"activity_repo.go", "readTrialGate", []string{"sqlSelectTrialBattleGate"}},
		{"activity_repo.go", "lockTrialMembers", []string{"sqlLockActivityMember"}},
		// Q:缺行先建、再点锁。分 seq / 插指令行不在这一步里("逐人 (Q → O)"的写法已弃用,见 TestTrialSeqGuardsLockedBeforeAnyOpInsert)。
		{"activity_repo.go", "lockTrialSeqGuards", []string{"ensureSeqRowTx", "sqlLockSeqGuard"}},
		{"activity_repo.go", "readTrialRewarded", []string{"sqlSelectActivityCounter"}},
		{"activity_repo.go", "enqueueTrialRewards", []string{"AllocateSeq", "insertAssetOp"}},
		{"activity_repo.go", "occupyTrialCounters", []string{"upsertCounterWithLimit", "errRetryTx"}},
		{"activity_repo.go", "grantTrialFunds", []string{
			"sqlEnsureReunionProgress", "sqlLockActivityProgress", "sqlSetActivityFunds", "sqlSetTrialProgressCount",
		}},
		{"activity_repo.go", "creditTrialContribution", []string{"sqlSetActivityContribution"}},
		// T:有行时先主键点锁、再点改。
		{"activity_repo.go", "writeTrialBattleState", []string{"sqlInsertTrialBattle", "sqlLockTrialBattle", "sqlSetTrialBattleState"}},
		{"activity_repo.go", "insertOwedRewards", []string{"sqlInsertOwedReward"}},

		// 登记 / 标记 / 判过期:第一把锁都是 G(T 行的守卫),之后才读、写 T。
		{"activity_repo.go", "RegisterTrialBattle", []string{"lockTrialGuild", "readTrialGate", "writeTrialBattleState"}},
		{"activity_repo.go", "MarkTrialBattlePoison", []string{"lockTrialGuild", "readTrialGate", "writeTrialBattleState"}},
		{"activity_repo.go", "ExpireTrialBattle", []string{"lockTrialGuild", "readTrialGate", "writeTrialBattleState"}},

		// 待入队转换:Q(AllocateSeq)→ 插 O → 点删 W;诊断计数在事务之外、排在最后。
		{"activity_repo.go", "ConvertOwedReward", []string{"AllocateSeq", "insertAssetOp", "sqlDeleteOwedReward", "sqlBumpOwedRewardAttempts"}},
	}
	fset := token.NewFileSet()
	for _, tc := range cases {
		t.Run(tc.file+"/"+tc.fn, func(t *testing.T) {
			file, err := parser.ParseFile(fset, tc.file, nil, parser.SkipObjectResolution)
			require.NoError(t, err, "go test 的工作目录是包目录,%s 必须能直接读到", tc.file)
			first := pointLockFirstIdents(pointLockFuncBody(t, file, tc.fn), tc.order)
			for i, name := range tc.order {
				pos, found := first[name]
				require.True(t, found, "%s.%s 里找不到 %s:步骤被删或改名了,先回来确认新写法仍守表间全序", tc.file, tc.fn, name)
				if i > 0 {
					prev := tc.order[i-1]
					assert.Less(t, first[prev], pos, "%s.%s:%s 必须先于 %s(%s 在 %s,%s 在 %s)",
						tc.file, tc.fn, prev, name, prev, fset.Position(first[prev]), name, fset.Position(pos))
				}
			}
		})
	}
}

// TestTrialSeqGuardsLockedBeforeAnyOpInsert:历练结算必须"先锁完全部 Q,再插 O"(activity_repo.go 文件头第 7 条)。
// 上面的次序表只能证明 lockTrialSeqGuards 排在 enqueueTrialRewards 之前;这里再钉死反方向 ——
// 锁 Q 的那一步里不出现任何碰 guild_asset_op 的调用,事务体自己也不直接分 seq / 插指令行。
// 有人把它改回"逐人 (Q → O)"时,就得同时来改这条测试,并把 tables.go 里弃用的"新插 O 行"例外重新论证一遍。
func TestTrialSeqGuardsLockedBeforeAnyOpInsert(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "activity_repo.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	touchesOpTable := []string{"AllocateSeq", "insertAssetOp", "sqlInsertAssetOp", "sqlLockAssetOp"}
	for _, fn := range []string{"lockTrialSeqGuards", "lockTrialMembers", "settleTrialBattle"} {
		found := pointLockFirstIdents(pointLockFuncBody(t, file, fn), touchesOpTable)
		assert.Empty(t, found, "%s 里不得直接碰 guild_asset_op(只许经 enqueueTrialRewards)", fn)
	}
}
