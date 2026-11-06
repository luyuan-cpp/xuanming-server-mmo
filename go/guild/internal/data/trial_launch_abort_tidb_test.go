package data

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	pb "proto/guild"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 2026-10-23 修复（审计 r2-guild#1）回归：帮会历练开局成功（match formation=4）后战斗不出结果 —— battle 停机作废、
// 崩溃或活动结果没发出 —— launch 与参与者 ACTIVE 占用的唯一出口，是 match 在权威截止后原子提交的 SQL ABORT
// （取消回执 formation=5、TerminalDecision=ABORT）。截止前的 formation=4、COMPLETE、自相矛盾的回执都不能释放；
// 释放之后同一回执的重放（多副本、提交结果未知后重试）不再改任何行。帮会已解散时同一出口照样可用。
// match 侧「截止前不 ABORT、截止后只 ABORT 一次」由 go/match/internal/authority/started_formation_exit_test.go 覆盖。

func TestTrialCancelReceiptIsTerminalOnlyForAuthoritativeEnds(t *testing.T) {
	abort := matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT
	complete := matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE
	cases := []struct {
		name     string
		receipt  *matchpb.CancelActivityBattleResponse
		terminal bool
	}{
		{"缺回执", nil, false},
		{"零battle取消墓碑", &matchpb.CancelActivityBattleResponse{RequestCancelled: true}, true},
		{"取消墓碑却带battle", &matchpb.CancelActivityBattleResponse{RequestCancelled: true, BattleId: 900}, false},
		{"开局成功截止前", &matchpb.CancelActivityBattleResponse{BattleId: 900, FormationState: 4}, false},
		{"已有COMPLETE等结果事件", &matchpb.CancelActivityBattleResponse{BattleId: 900, FormationState: 4, TerminalDecision: complete}, false},
		{"成局中", &matchpb.CancelActivityBattleResponse{BattleId: 900, FormationState: 2}, false},
		{"发房前中止", &matchpb.CancelActivityBattleResponse{BattleId: 900, FormationState: 5}, true},
		{"开局后SQL ABORT", &matchpb.CancelActivityBattleResponse{BattleId: 900, FormationState: 5, TerminalDecision: abort}, true},
		{"state5却是COMPLETE", &matchpb.CancelActivityBattleResponse{BattleId: 900, FormationState: 5, TerminalDecision: complete}, false},
		{"state5缺battle", &matchpb.CancelActivityBattleResponse{FormationState: 5, TerminalDecision: abort}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.terminal, TrialCancelReceiptIsTerminal(c.receipt))
		})
	}
}

// trialLaunchOccupancy 读一份 launch 的占用现状：launch 行、ACTIVE(state=1)参与者数、大厅占位数。
type trialLaunchOccupancy struct {
	launchState   uint32
	launchBattle  uint64
	updatedMs     uint64
	cancelReceipt []byte
	activeRoles   int
	terminalRoles int
	lobbySlots    int
}

func readTrialLaunchOccupancy(t *testing.T, ctx context.Context, db *sql.DB, requestID uint64) trialLaunchOccupancy {
	t.Helper()
	var out trialLaunchOccupancy
	require.NoError(t, db.QueryRowContext(ctx, "SELECT state,battle_id,updated_ms,cancel_receipt FROM guild_trial_launch WHERE request_id=?", requestID).
		Scan(&out.launchState, &out.launchBattle, &out.updatedMs, &out.cancelReceipt))
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_launch_role WHERE request_id=? AND state=1", requestID).Scan(&out.activeRoles))
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_launch_role WHERE request_id=? AND state=2", requestID).Scan(&out.terminalRoles))
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot WHERE lobby_id=?", requestID).Scan(&out.lobbySlots))
	return out
}

// readTrialBattleRow 读对局行的 state / settle_result;found=false = 没有这一行。
func readTrialBattleRow(t *testing.T, ctx context.Context, db *sql.DB, battleID uint64) (state, result uint32, found bool) {
	t.Helper()
	err := db.QueryRowContext(ctx, "SELECT state,settle_result FROM guild_trial_battle WHERE battle_id=?", battleID).Scan(&state, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false
	}
	require.NoError(t, err)
	return state, result, true
}

// 2026-11-05 修复（r2-guild#1 评审第 4、5 条）在原用例上追加：
//   - 作废写 LOSS 走与结算 / 标毒 / 巡检同一个带状态与 guild_id 闸门的写入口:巡检已判 EXPIRED 的行同样收成 SETTLED/LOSS;
//     对局行登记在别的帮名下时拒绝、什么都不释放;帮会已解散时不碰对局行(解散后留下的 EXPIRED 历史行保持原样);
//   - 发房前中止回执(formation=5、TerminalDecision=UNKNOWN、带 battle)配已登记的 STARTED 行同样释放并记 LOSS;
//   - 大厅 ENDED 带结束原因 trialCancelledLobbyEndTip,客户端不再停在 LAUNCHED 而不知原因。
//
// 2026-11-05 复审追加「从未登记」:实时路径没登记对局(Start 回包丢失或超时),launch 停在 state1、没有对局行,
// 回执是带 battle 的发房前中止 —— 作废写入口补一行 SETTLED/LOSS(改回裸 UPDATE 时这一行不会出现,用例变红)。
func TestTiDBTrialStartedBattleReleasesOnlyOnSignedSQLAbort(t *testing.T) {
	const requestID, battleID = 381, 900
	sqlAbort := &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5,
		TerminalDecision: matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, RequestDigest: "fixture-verifier-covered-separately"}
	preCreateAbort := &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5, RequestDigest: "fixture-verifier-covered-separately"}
	settled, loss := uint32(pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED), uint32(pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS)
	expiredState := uint32(pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_EXPIRED)
	cases := []struct {
		name         string
		unregistered bool // 实时路径没登记对局:launch 停在 state1,没有对局行
		expire       bool // 巡检先把对局行判成 EXPIRED
		disband      bool
		receipt      *matchpb.CancelActivityBattleResponse
		// 释放之后对局行的期望:found=false = 没有这一行
		wantFound          bool
		wantState, wantRes uint32
	}{
		{name: "帮会在", receipt: sqlAbort, wantFound: true, wantState: settled, wantRes: loss},
		{name: "帮会在_巡检已判EXPIRED", expire: true, receipt: sqlAbort, wantFound: true, wantState: settled, wantRes: loss},
		{name: "帮会在_发房前中止", receipt: preCreateAbort, wantFound: true, wantState: settled, wantRes: loss},
		// 缺行补一行 SETTLED/LOSS,成为这一局的终态闸门(迟到的结果事件按重复处理,不会再发奖)。
		{name: "帮会在_从未登记_发房前中止", unregistered: true, receipt: preCreateAbort, wantFound: true, wantState: settled, wantRes: loss},
		// 解散同事务删了在途(STARTED)对局行,作废不补造。
		{name: "帮会已解散", disband: true, receipt: sqlAbort},
		// 解散只删 STARTED 行,EXPIRED 行作为历史留下;此后没有任何路径再写它,作废也不改。
		{name: "帮会已解散_巡检已判EXPIRED", expire: true, disband: true, receipt: sqlAbort, wantFound: true, wantState: expiredState},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db, repo, activities := guildRoleTiDB(t)
			seedLobbyOwners(t, ctx, db)
			actor := lobbyActor(t, ctx, 11, "seller", 1)
			store := consentedSQLLobby(t, actor, repo, requestID, 44, 3, 11, 22)
			lobby, err := store.Load(ctx, requestID)
			require.NoError(t, err)
			roster, err := store.LoadConsents(ctx, lobby)
			require.NoError(t, err)
			battle := actTrialKey(actTrialRow(), battleID, 44, 11, testNowMs)
			battle.LaunchRequestID = requestID
			prepared, err := activities.PrepareTrialLaunch(actor, requestID, battle, 1, roster, testNowMs)
			require.NoError(t, err)
			// 开局成功：match 编队 Finish(true) 到 formation=4，guild 登记对局，launch 进 state2。
			wantLaunchState, wantLaunchBattle := uint32(2), uint64(battleID)
			if c.unregistered {
				wantLaunchState, wantLaunchBattle = 1, 0
			} else {
				require.NoError(t, activities.RegisterTrialBattle(ctx, battle, testNowMs))
			}
			if c.expire {
				expired, err := activities.ExpireTrialBattle(ctx, battleID, 44, testNowMs+1)
				require.NoError(t, err)
				require.True(t, expired)
			}
			if c.disband {
				_, err = repo.DisbandGuild(actor, 44, 11, testNowMs+1)
				require.NoError(t, err)
			}
			started := readTrialLaunchOccupancy(t, ctx, db, requestID)
			require.EqualValues(t, wantLaunchState, started.launchState)
			require.EqualValues(t, wantLaunchBattle, started.launchBattle)
			if c.unregistered {
				_, _, found := readTrialBattleRow(t, ctx, db, battleID)
				require.False(t, found, "前置:实时路径没登记,没有对局行")
			}
			require.Equal(t, 2, started.activeRoles)
			require.Equal(t, 2, started.lobbySlots)

			// battle 停机作废或崩溃之后、权威截止之前，恢复器每轮重放取消只拿到这些回执：都不是终态，一个占用也不能放。
			complete := matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE
			for i, receipt := range []*matchpb.CancelActivityBattleResponse{
				{BattleId: battleID, FormationState: 4},
				{BattleId: battleID, FormationState: 4, TerminalDecision: complete},
				{BattleId: battleID, FormationState: 5, TerminalDecision: complete},
			} {
				require.ErrorIs(t, activities.FinishCancelledTrialLaunch(ctx, prepared, receipt, testNowMs+2), ErrWriteConflict, "receipt #%d", i)
				require.Equal(t, started, readTrialLaunchOccupancy(t, ctx, db, requestID), "非终态回执不改任何行 receipt #%d", i)
			}

			// 对局行登记在别的帮名下(只可能是数据被改坏):作废写入口的 guild_id 闸门拒绝,一个占用也不放。
			if !c.disband && !c.unregistered {
				_, err = db.ExecContext(ctx, "UPDATE guild_trial_battle SET guild_id=45 WHERE battle_id=?", battleID)
				require.NoError(t, err)
				require.ErrorIs(t, activities.FinishCancelledTrialLaunch(ctx, prepared, c.receipt, testNowMs+2), errTrialForeignGuild)
				require.Equal(t, started, readTrialLaunchOccupancy(t, ctx, db, requestID), "别帮的对局行不改写,也不释放")
				_, err = db.ExecContext(ctx, "UPDATE guild_trial_battle SET guild_id=44 WHERE battle_id=?", battleID)
				require.NoError(t, err)
			}

			// 截止后 match 原子提交 SQL ABORT（formation 4→5、原票置 4）或发房前中止，签回执：launch、两名参与者与大厅占位同一事务释放。
			require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, c.receipt, testNowMs+3))
			released := readTrialLaunchOccupancy(t, ctx, db, requestID)
			require.EqualValues(t, 3, released.launchState)
			require.EqualValues(t, battleID, released.launchBattle)
			require.Zero(t, released.activeRoles)
			require.Equal(t, 2, released.terminalRoles)
			require.Zero(t, released.lobbySlots)
			saved := &matchpb.CancelActivityBattleResponse{}
			require.NoError(t, proto.Unmarshal(released.cancelReceipt, saved))
			require.True(t, proto.Equal(c.receipt, saved), "终局证明与释放同事务落库")
			state, result, found := readTrialBattleRow(t, ctx, db, battleID)
			require.Equal(t, c.wantFound, found)
			require.Equal(t, c.wantState, state)
			require.Equal(t, c.wantRes, result)
			ended, err := store.Load(ctx, requestID)
			require.NoError(t, err)
			require.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, ended.State)
			require.Equal(t, trialCancelledLobbyEndTip, ended.EndTipID, "作废的大厅带结束原因")

			// 多副本拿到同一份回执、或提交结果未知后重放：只释放一次，不再改任何行。
			require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, c.receipt, testNowMs+4))
			require.Equal(t, released, readTrialLaunchOccupancy(t, ctx, db, requestID))
			againState, againResult, againFound := readTrialBattleRow(t, ctx, db, battleID)
			require.Equal(t, []any{found, state, result}, []any{againFound, againState, againResult})
		})
	}
}
