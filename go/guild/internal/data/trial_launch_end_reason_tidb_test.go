package data

// 作废释放把实时路径记下的业务拒绝原因写回大厅(RecordTrialLaunchEndReason / cancelledTrialLobbyEnd,2026-11-11,r2-guild#1 残余)。
// 真 TiDB,沿用 guildRoleTiDB(GUILD_ROLE_TEST_DSN / GUILD_ROLE_REQUIRE_TIDB)与 trial_launch_abort_tidb_test.go 的大厅夹具。

import (
	"testing"

	"github.com/stretchr/testify/require"
	"guild/internal/constants"
	base "proto/common/base"
	pb "proto/guild"
	matchpb "proto/match"
)

// 实时路径拿到 match 的业务拒绝(如有人离线)时把原因记在 launch 原件上;恢复器凭零 battle 的取消墓碑释放时用它关大厅,
// 名单上所有人看到与发起人同一个原因(修复前一律是「服务繁忙」)。只写一次,后来的记录不覆盖;回执带对局(开局后作废、
// 发房前中止)时不用它;没有记录时照旧写默认原因。
func TestTiDBTrialCancelledLobbyShowsRealtimeEndReason(t *testing.T) {
	const requestID, battleID = 383, 902
	tombstone := &matchpb.CancelActivityBattleResponse{RequestCancelled: true, RequestDigest: "fixture-verifier-covered-separately"}
	preCreateAbort := &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5, RequestDigest: "fixture-verifier-covered-separately"}
	offline := &base.TipInfoMessage{Id: constants.ErrTrialTeamInvalid, Parameters: []string{"offline", "22"}}
	cases := []struct {
		name       string
		record     *base.TipInfoMessage
		receipt    *matchpb.CancelActivityBattleResponse
		wantTip    uint32
		wantParams []string
	}{
		{name: "记了原因_零battle墓碑", record: offline, receipt: tombstone, wantTip: constants.ErrTrialTeamInvalid, wantParams: []string{"offline", "22"}},
		{name: "没有记录_零battle墓碑", receipt: tombstone, wantTip: trialCancelledLobbyEndTip},
		{name: "记了原因_回执带对局", record: offline, receipt: preCreateAbort, wantTip: trialCancelledLobbyEndTip},
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
			if c.record != nil {
				require.NoError(t, activities.RecordTrialLaunchEndReason(ctx, requestID, c.record))
				require.NoError(t, activities.RecordTrialLaunchEndReason(ctx, requestID, &base.TipInfoMessage{Id: constants.ErrTrialServiceBusy}),
					"重复记录不报错")
			}

			require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, c.receipt, testNowMs+3))
			ended, err := store.Load(ctx, requestID)
			require.NoError(t, err)
			require.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, ended.State)
			require.Equal(t, c.wantTip, ended.EndTipID, "只写一次:后来的记录不覆盖第一次")
			if c.wantParams == nil {
				require.Empty(t, ended.EndParams)
			} else {
				require.Equal(t, c.wantParams, ended.EndParams)
			}

			// 释放之后原件已终结(state 3):再记录影响 0 行,不报错。
			require.NoError(t, activities.RecordTrialLaunchEndReason(ctx, requestID, offline))
		})
	}
}

// 记录的前置校验:没有原件号或 tip 为 0 是调用方的缺陷,不写。
func TestTrialLaunchEndReasonRejectsEmptyInput(t *testing.T) {
	ctx, _, _, activities := guildRoleTiDB(t)
	require.ErrorIs(t, activities.RecordTrialLaunchEndReason(ctx, 0, &base.TipInfoMessage{Id: constants.ErrTrialTeamInvalid}), ErrWriteConflict)
	require.ErrorIs(t, activities.RecordTrialLaunchEndReason(ctx, 383, &base.TipInfoMessage{}), ErrWriteConflict)
	require.ErrorIs(t, activities.RecordTrialLaunchEndReason(ctx, 383, nil), ErrWriteConflict)
}
