package data

import (
	"context"
	"database/sql"
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

func TestTiDBTrialStartedBattleReleasesOnlyOnSignedSQLAbort(t *testing.T) {
	for _, disband := range []bool{false, true} {
		name := "帮会在"
		if disband {
			name = "帮会已解散"
		}
		t.Run(name, func(t *testing.T) {
			ctx, db, repo, activities := guildRoleTiDB(t)
			seedLobbyOwners(t, ctx, db)
			actor := lobbyActor(t, ctx, 11, "seller", 1)
			const requestID, battleID = 381, 900
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
			require.NoError(t, activities.RegisterTrialBattle(ctx, battle, testNowMs))
			if disband {
				_, err = repo.DisbandGuild(actor, 44, 11, testNowMs+1, nil)
				require.NoError(t, err)
			}
			started := readTrialLaunchOccupancy(t, ctx, db, requestID)
			require.EqualValues(t, 2, started.launchState)
			require.EqualValues(t, battleID, started.launchBattle)
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

			// 截止后 match 原子提交 SQL ABORT（formation 4→5、原票置 4），签回执：launch、两名参与者与大厅占位同一事务释放。
			abort := &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5,
				TerminalDecision: matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, RequestDigest: "fixture-verifier-covered-separately"}
			require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, abort, testNowMs+3))
			released := readTrialLaunchOccupancy(t, ctx, db, requestID)
			require.EqualValues(t, 3, released.launchState)
			require.EqualValues(t, battleID, released.launchBattle)
			require.Zero(t, released.activeRoles)
			require.Equal(t, 2, released.terminalRoles)
			require.Zero(t, released.lobbySlots)
			saved := &matchpb.CancelActivityBattleResponse{}
			require.NoError(t, proto.Unmarshal(released.cancelReceipt, saved))
			require.True(t, proto.Equal(abort, saved), "终局证明与释放同事务落库")
			if disband {
				var rows int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_battle WHERE battle_id=?", battleID).Scan(&rows))
				require.Zero(t, rows, "解散同事务删了在途对局行，作废不补造")
			} else {
				var state, result uint32
				require.NoError(t, db.QueryRowContext(ctx, "SELECT state,settle_result FROM guild_trial_battle WHERE battle_id=?", battleID).Scan(&state, &result))
				require.EqualValues(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, state)
				require.EqualValues(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS, result)
			}

			// 多副本拿到同一份回执、或提交结果未知后重放：只释放一次，不再改任何行。
			require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, abort, testNowMs+4))
			require.Equal(t, released, readTrialLaunchOccupancy(t, ctx, db, requestID))
		})
	}
}
