package authority

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "proto/match"
	rfpb "proto/role_fence"
	teampb "proto/team"
	"shared/rolefence"
	"shared/rolefence/fencetest"
	"shared/serviceauth"
)

const matchPurgeSecret = "fixture-only-coordinator-secret-0000"

// matchPurge 走真的 PurgeRemovedRole:鉴权、按版本合并墓碑、锁副本、清理,同一个事务。
func matchPurge(t *testing.T, ctx context.Context, participant *rolefence.ParticipantServer, state *rfpb.RoleFenceState) {
	t.Helper()
	in := &rfpb.PurgeRemovedRoleRequest{State: state}
	require.NoError(t, serviceauth.Sign(rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName, in, rolefence.CoordinatorCaller, matchPurgeSecret, uint64(time.Now().UnixMilli())))
	out, err := participant.PurgeRemovedRole(ctx, in)
	require.NoError(t, err)
	require.False(t, out.GetRetry(), "清理应一次成功")
}

// requireMatchPurged:他一个人的终态行都没了,同一批表里别人的行、多人共有与按局记的行都还在,共有战报的引用数减到真实人数。
func requireMatchPurged(t *testing.T, s *Store, removed, other uint64) {
	t.Helper()
	for _, query := range []string{
		"SELECT COUNT(*) FROM match_rating WHERE player_id=?",
		"SELECT COUNT(*) FROM match_network_profile WHERE player_id=?",
		"SELECT COUNT(*) FROM match_spectator WHERE player_id=?",
		"SELECT COUNT(*) FROM match_join_intent WHERE player_id=? AND state IN (2,3)",
		"SELECT COUNT(*) FROM team_action_receipt WHERE actor_id=?",
		"SELECT COUNT(*) FROM match_player_battle_report WHERE player_id=?",
		"SELECT COUNT(*) FROM match_battle_script WHERE owner_player_id=?",
		"SELECT COUNT(*) FROM match_battle_script_loadout WHERE player_id=?",
		"SELECT COUNT(*) FROM match_ranked_player_prefs WHERE player_id=?",
		"SELECT COUNT(*) FROM match_ranked_player_season WHERE player_id=?",
		"SELECT COUNT(*) FROM match_ranked_reward WHERE player_id=?",
		"SELECT COUNT(*) FROM match_ranked_battle WHERE player_id=?",
		"SELECT COUNT(*) FROM match_ranked_rivalry WHERE player_id=?",
	} {
		require.Zero(t, countRows(t, s, query, removed), query)
		require.Equal(t, 1, countRows(t, s, query, other), "别人的行不动: %s", query)
	}
	var mirrorOppName string
	require.NoError(t, s.DB.QueryRowContext(context.Background(),
		"SELECT opponent_name FROM match_ranked_rivalry WHERE season_id=1 AND player_id=? AND opponent_id=?", other, removed).Scan(&mirrorOppName))
	require.Empty(t, mirrorOppName, "对手宿敌账本保留交手统计但清空已删角色昵称")
	var removedFeaturedName, otherFeaturedName string
	require.NoError(t, s.DB.QueryRowContext(context.Background(),
		"SELECT featured_player_name FROM match_ranked_featured_build WHERE season_id=1 AND build_key='101:3:1001'").Scan(&removedFeaturedName))
	require.Empty(t, removedFeaturedName, "问道名局汇总表匿名化已删角色昵称")
	require.NoError(t, s.DB.QueryRowContext(context.Background(),
		"SELECT featured_player_name FROM match_ranked_featured_build WHERE season_id=1 AND build_key='102:4:2002'").Scan(&otherFeaturedName))
	require.Equal(t, "在役名家", otherFeaturedName, "其他角色的问道名局昵称不动")
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_spectator_intent WHERE player_id=? AND state=5", removed))
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_role_occupancy WHERE player_id=?", removed), "到期的占用删掉")
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_role_occupancy WHERE player_id=?", other))
	require.Equal(t, 1, countRows(t, s, "SELECT ref_count FROM match_battle_report WHERE battle_id=901"), "共有战报只减他那一份")
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_battle_report WHERE battle_id=902"), "只有他的战报正文随引用数归零删掉")
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_rating_applied WHERE battle_id=901"), "按局记的入账幂等行保留")
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_authority_ticket WHERE player_id=?", removed), "匹配票原件保留")
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE player_id=?", removed), "结算原件保留")
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_battle_script_review WHERE owner_player_id=?", removed), "剧本审核流水按审计保留")
	fence, err := rolefence.Read(context.Background(), s.DB, removed)
	require.NoError(t, err)
	require.Empty(t, fence.Account, "副本已合并成墓碑")
}

// 删角收尾(docs/design/account-deletion.md §6.2)。没终结的意图不删,请求头跟着留;意图终结之后再清一次,请求头也删掉;
// 之后的调用没有可删的行,照样成功。
func TestPurgeTiDBRemovedRoleDeletesPersonalRowsAndReleasesReports(t *testing.T) {
	ctx, s := pendingStore(t)
	const removed, other = uint64(11), uint64(12)
	for _, player := range []uint64{removed, other} {
		insertPendingFixture(t, s, "match_rating", &pb.MatchRatingRecord{PlayerId: player, Mode: 3, RatingCenti: 150000})
		insertPendingFixture(t, s, "match_network_profile", &pb.MatchNetworkProfileRecord{PlayerId: player, ExpiresMs: 1})
		insertPendingFixture(t, s, "match_spectator", &pb.MatchSpectatorRecord{PlayerId: player, State: 5})
		insertPendingFixture(t, s, "match_join_intent", &pb.MatchJoinIntentRecord{PlayerId: player, RequestHigh: 1, RequestLow: 1, State: 2})
		insertPendingFixture(t, s, "team_action_receipt", &teampb.TeamActionReceiptRecord{ActorId: player, RequestHigh: 1, RequestLow: 1})
		insertPendingFixture(t, s, "match_player_battle_report", &pb.MatchPlayerBattleReportRecord{PlayerId: player, CreatedMs: 1, BattleId: 901})
		insertPendingFixture(t, s, "match_battle_script", &pb.MatchBattleScriptRecord{ScriptId: 800 + player, OwnerPlayerId: player, Revision: 1, ApprovedRevision: 1})
		insertPendingFixture(t, s, "match_battle_script_loadout", &pb.MatchBattleScriptLoadoutRecord{PlayerId: player, MatchMode: 1, Origin: 2, PlayerScriptId: 800 + player})
		insertPendingFixture(t, s, "match_ranked_player_prefs", &pb.MatchRankedPlayerPrefsRecord{PlayerId: player, CharacterPresetId: 101, PetPresetId: 203, RuleVersion: 2})
		insertPendingFixture(t, s, "match_ranked_player_season", &pb.MatchRankedPlayerSeasonRecord{SeasonId: 1, PlayerId: player, Qualified: true, RatingCenti: 162000})
		insertPendingFixture(t, s, "match_ranked_reward", &pb.MatchRankedRewardRecord{PlayerId: player, SeasonId: 1, RewardKind: 1, ItemCode: "s1_avatar_pattern"})
		insertPendingFixture(t, s, "match_ranked_battle", &pb.MatchRankedBattleRecord{BattleId: 901, PlayerId: player, SeasonId: 1, SettledAtMs: 100})
	}
	insertPendingFixture(t, s, "match_ranked_season", &pb.MatchRankedSeasonRecord{SeasonId: 1, SeasonKey: "s1", SeasonName: "第一赛季", State: 1})
	insertPendingFixture(t, s, "match_ranked_rivalry", &pb.MatchRankedRivalryRecord{SeasonId: 1, PlayerId: removed, OpponentId: other, OpponentName: "在役名家", Wins: 1, Losses: 2})
	insertPendingFixture(t, s, "match_ranked_rivalry", &pb.MatchRankedRivalryRecord{SeasonId: 1, PlayerId: other, OpponentId: removed, OpponentName: "注销角色", Wins: 2, Losses: 1})
	insertPendingFixture(t, s, "match_ranked_featured_build", &pb.MatchRankedFeaturedBuildRecord{SeasonId: 1, BuildKey: "101:3:1001", FeaturedBattleId: 901, FeaturedPlayerId: removed, FeaturedPlayerName: "注销角色", Appearances: 4, Wins: 3})
	insertPendingFixture(t, s, "match_ranked_featured_build", &pb.MatchRankedFeaturedBuildRecord{SeasonId: 1, BuildKey: "102:4:2002", FeaturedBattleId: 903, FeaturedPlayerId: other, FeaturedPlayerName: "在役名家", Appearances: 5, Wins: 4})
	// 他有两份剧本;一份审过,审核流水留着。
	insertPendingFixture(t, s, "match_battle_script", &pb.MatchBattleScriptRecord{ScriptId: 850, OwnerPlayerId: removed, Revision: 2})
	insertPendingFixture(t, s, "match_battle_script_review", &pb.MatchBattleScriptReviewRecord{ScriptId: 800 + removed, Revision: 1, OwnerPlayerId: removed, Approved: true, Reviewer: "gm"})
	insertPendingFixture(t, s, "match_rating", &pb.MatchRatingRecord{PlayerId: removed, Mode: 4, RatingCenti: 150000})
	insertPendingFixture(t, s, "match_join_intent", &pb.MatchJoinIntentRecord{PlayerId: removed, RequestHigh: 2, RequestLow: 2, State: 3, Cancelled: true})
	insertPendingFixture(t, s, "match_join_intent", &pb.MatchJoinIntentRecord{PlayerId: removed, RequestHigh: 3, RequestLow: 3, State: 1})
	insertPendingFixture(t, s, "match_spectator_intent", &pb.MatchSpectatorIntentRecord{PlayerId: removed, RequestHigh: 1, RequestLow: 1, State: 5})
	insertPendingFixture(t, s, "match_spectator_intent", &pb.MatchSpectatorIntentRecord{PlayerId: removed, RequestHigh: 2, RequestLow: 2, State: 1})
	insertPendingFixture(t, s, "match_spectator_head", &pb.MatchSpectatorHeadRecord{PlayerId: removed, OperationId: "head"})
	insertPendingFixture(t, s, "match_role_occupancy", &pb.MatchRoleOccupancyRecord{PlayerId: removed, OwnerKind: 1, OwnerId: 71, ExpiresMs: 1})
	insertPendingFixture(t, s, "match_role_occupancy", &pb.MatchRoleOccupancyRecord{PlayerId: other, OwnerKind: 1, OwnerId: 71, ExpiresMs: ^uint64(0)})
	// 战报正文:901 两人共有,902 只有他。
	insertPendingFixture(t, s, "match_player_battle_report", &pb.MatchPlayerBattleReportRecord{PlayerId: removed, CreatedMs: 2, BattleId: 902})
	insertPendingFixture(t, s, "match_battle_report", &pb.MatchBattleReportRecord{BattleId: 901, RefCount: 2})
	insertPendingFixture(t, s, "match_battle_report", &pb.MatchBattleReportRecord{BattleId: 902, RefCount: 1})
	insertPendingFixture(t, s, "match_rating_applied", &pb.MatchRatingAppliedRecord{BattleId: 901})
	insertPendingFixture(t, s, "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: removed, Ticket: "finished", State: 4})
	insertPendingFixture(t, s, "match_battle_settlement_source", &pb.MatchBattleSettlementSourceRecord{BattleId: 901, PlayerId: removed})
	fencetest.Seed(t, ctx, s.DB, removed, "seller", 7)
	participant, err := rolefence.NewParticipantServer(s.DB, RoleFencePending, RoleFencePurge, matchPurgeSecret)
	require.NoError(t, err)
	tombstone := &rfpb.RoleFenceState{PlayerId: removed, OwnerRevision: 8, Version: 2, HomeZone: 1}

	matchPurge(t, ctx, participant, tombstone)
	requireMatchPurged(t, s, removed, other)
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_join_intent WHERE player_id=? AND state=1", removed), "没终结的入队意图不删")
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_spectator_intent WHERE player_id=? AND state=1", removed), "没终结的观战意图不删")
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_spectator_head WHERE player_id=?", removed), "还有没终结的意图,请求头留着")

	_, err = s.DB.ExecContext(ctx, "UPDATE match_spectator_intent SET state=5 WHERE player_id=?", removed)
	require.NoError(t, err)
	for round := 0; round < 2; round++ {
		matchPurge(t, ctx, participant, tombstone)
		requireMatchPurged(t, s, removed, other)
		require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_spectator_intent WHERE player_id=?", removed))
		require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_spectator_head WHERE player_id=?", removed), "意图全终结之后请求头一起删")
	}
}
