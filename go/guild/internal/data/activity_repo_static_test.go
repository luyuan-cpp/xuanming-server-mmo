package data

// activity_repo_static_test.go —— 帮会活动 repo(B6a)**不连库**的契约:入参校验先于碰库、语句文本形状、源码里的取锁先后。
//
// 为什么单独成文件、且**不带 build tag**:这三条既不连 MySQL 也不连 Redis,原先放在 activity_repo_integration_test.go
// (`//go:build integration`)里,普通 `go test ./...` 根本编译不到它们 —— 锁序被人调换、点锁被删、候选读被补上 FOR UPDATE,
// 只有在专门带 -tags=integration 且配了库的那一轮才会红,等于日常回归里没有这道闸。死锁是用户硬要求(92-handoff §12.0),
// 这类确定性的结构检查必须在任何环境、每一次 go test 里都跑。
//
// 与 point_lock_order_test.go 同一套判据(pointLockOrderCase / pointLockFuncBody / pointLockFirstIdents 定义在那边,同样不带 tag)。
// 需要真库的事务语义与并发死锁回归(I1–I12)仍在 activity_repo_integration_test.go。
//
// actLanternRow / actInput 两个助手也挪到这里:本文件与集成测试都用它们,放在不带 tag 的文件里两种构建都能看见;
// 反过来放在集成文件里,本文件在普通构建下就编译不过。

import (
	"context"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	} {
		assert.True(t, strings.HasSuffix(query, progressPK), "%s 必须以完整主键等值结尾:%s", name, query)
	}
	// 普通读:不许带任何锁定子句。
	for name, query := range map[string]string{
		"sqlSelectActivityProgress":            sqlSelectActivityProgress,
		"sqlSelectActivityProgressKeysOfGuild": sqlSelectActivityProgressKeysOfGuild,
		"sqlSelectActivityUsage":               sqlSelectActivityUsage,
		"sqlSelectPendingActivityRewards":      sqlSelectPendingActivityRewards,
		"sqlSelectRecentRejectedRewards":       sqlSelectRecentRejectedRewards,
	} {
		upper := strings.ToUpper(query)
		for _, locking := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
			assert.NotContains(t, upper, locking, "%s 是普通读:%s", name, query)
		}
	}
}

// TestActivityLockOrderInSource:事务体里各表第一次被碰到的先后 = 表间全序(92-handoff §12.2 第 1 条,P 在 C 之后),
// 解散里删进度行排在提前截止之后、删 guild 行之前(X-14)。判据与 point_lock_order_test.go 相同:
// 只认标识符在函数体内第一次出现的源码位置;有人调换语句、删掉某一步或改名,这里都会红。
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
		{"guild_manage_repo.go", "DisbandGuild", []string{
			"lockGuildRow", "checkFence", "lockAllMembers", "sqlDeleteMember", "deleteApplicationRows",
			"accelerateDonationDeadlines", "deleteGuildActivityProgress", "sqlDeleteGuild",
		}},
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
