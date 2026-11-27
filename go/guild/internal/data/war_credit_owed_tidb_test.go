package data

// 帮战入账的护栏分流(数据覆写审计 r2 第 4 轮 r2-gw#6)真库用例:独占本地 TiDB(GUILD_ROLE_TEST_DSN,与 role_fence_tidb_test.go
// 同一套夹具;未配置即 Skip)。用例直接改副本行摆状态,ReplicaReader 把副本自己当目录,自愈读回副本现状。

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	assetpb "proto/common/asset"
	pb "proto/guild"
	"shared/rolefence/fencetest"

	"guild/internal/constants"
)

// warTiDBCreditSeqRow 读一名玩家 GUILD_CREDIT 流的 next_seq;没有这一行时 found=false。
func warTiDBCreditSeqRow(t *testing.T, ctx context.Context, db *sql.DB, playerID uint64) (next uint64, found bool) {
	t.Helper()
	err := db.QueryRowContext(ctx, "SELECT next_seq FROM guild_player_op_seq WHERE player_id=? AND stream=?",
		playerID, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT)).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, err)
	return next, true
}

func warTiDBRowCount(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, query, args...).Scan(&n))
	return n
}

// 一局三名参战者:在役的照常入账;护栏冻结中的不插指令、不分 seq,资产入账记进待入账行,帮贡与个人态照写;
// 已删除的这一局什么都不写(不建 seq 行、不加帮贡、不建个人态)。一个人的护栏状态不挡整局。
// 冻结期间转换什么都不写;解冻后转成恰好一条指令(op_id 就是预发的那个);墓碑的待入账行被转换删掉放弃。
func TestTiDBWarSettleOwesFencedAndSkipsRemovedParticipants(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	wr, err := NewWarRepo(repo)
	require.NoError(t, err)
	const (
		attackGuild uint64 = 9200
		defendGuild uint64 = 9300
		openPlayer  uint64 = 9201
		fenced      uint64 = 9202
		removed     uint64 = 9301
		battleID    uint64 = 5201
		openOp      uint64 = 880401
		fencedOp    uint64 = 880402
		removedOp   uint64 = 880403
	)
	seedManagedGuild(t, ctx, db, attackGuild, warTestZone, 1, 30, openPlayer, map[uint64]uint32{fenced: constants.RoleMember})
	seedManagedGuild(t, ctx, db, defendGuild, warTestZone, 1, 30, removed, nil)
	fencetest.Seed(t, ctx, db, openPlayer, "open", 1)
	fencetest.Seed(t, ctx, db, fenced, "seller", 1)
	fencetest.Update(t, ctx, db, fenced, "seller", 1, 901) // 寄售中
	fencetest.Seed(t, ctx, db, removed, "gone", 1)
	fencetest.Update(t, ctx, db, removed, "", 2, 0) // 墓碑
	bundle, err := warSilverBundle(3000)
	require.NoError(t, err)

	in := SettleWarBattleInput{
		Record: &pb.GuildWarBattleRecord{
			BattleId: battleID, ZoneId: warTestZone,
			WarBattleKind:   uint32(pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_FIELD_ROAD),
			AttackerGuildId: attackGuild, DefenderGuildId: defendGuild,
			Outcome: 1, TotalRounds: 6, DiminishPercent: 100, SummaryPayload: []byte("{}"), FinishedAtMs: testNowMs,
		},
		WeekKey: warTestWeekKey, DayKey: econDayKey, NowMs: testNowMs,
		ProtectSec: 10, PeaceCooldownSec: 60, BountyProtectSec: 1800, DailyPayoutLimit: 5,
		PlayerCredits: []PlayerSettlementCredit{
			{PlayerID: openPlayer, GuildID: attackGuild, Won: true, ContributionGain: 80, OpID: openOp, CreditBundle: bundle},
			{PlayerID: fenced, GuildID: attackGuild, Won: true, ContributionGain: 80, OpID: fencedOp, CreditBundle: bundle},
			{PlayerID: removed, GuildID: defendGuild, TeamIndex: 1, ContributionGain: 35, OpID: removedOp, CreditBundle: bundle},
		},
		LeaseUntilMs: testNowMs + econLeaseMs, LeaseToken: battleID,
	}
	out, err := wr.SettleWarBattle(ctx, in)
	require.NoError(t, err, "一名参战者冻结、一名已删除,都不挡整局结算")
	assert.Equal(t, 1, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_battle WHERE battle_id=?", battleID))
	require.Len(t, out.CreatedOps, 1)
	assert.Equal(t, openOp, out.CreatedOps[0].OpID, "在役的照常入账")
	assert.Equal(t, []uint64{fenced}, out.FencedPlayers)
	assert.Equal(t, []uint64{removed}, out.RemovedPlayers)
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_asset_op WHERE op_id IN (?, ?)", fencedOp, removedOp),
		"冻结中与已删除的都不插资产指令")
	next, found := warTiDBCreditSeqRow(t, ctx, db, fenced)
	require.True(t, found, "冻结中的 seq 行照建(计数守卫),但不分 seq")
	assert.Equal(t, uint64(1), next)
	_, found = warTiDBCreditSeqRow(t, ctx, db, removed)
	assert.False(t, found, "墓碑之后不再建他的 seq 行")
	require.Len(t, out.OwedCredits, 1)
	assert.Equal(t, fenced, out.OwedCredits[0].PlayerID)
	assert.Equal(t, fencedOp, out.OwedCredits[0].OpID)
	assert.Equal(t, 1, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_credit_owed WHERE player_id=? AND op_id=?", fenced, fencedOp))
	assert.Equal(t, 80, warTiDBRowCount(t, ctx, db, "SELECT contribution_total FROM guild_member WHERE guild_id=? AND player_id=?", attackGuild, fenced),
		"帮贡不是挂起项,冻结中照写")
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT contribution_total FROM guild_member WHERE guild_id=? AND player_id=?", defendGuild, removed),
		"已删除的不加帮贡")
	assert.Equal(t, 1, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_player_state WHERE player_id=?", fenced))
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_player_state WHERE player_id=?", removed),
		"已删除的不建个人态")

	owed := out.OwedCredits[0]
	status, err := wr.ConvertWarCreditOwed(ctx, owed, testNowMs+10_000)
	require.NoError(t, err)
	assert.Equal(t, WarOwedFrozen, status, "冻结期间转换什么都不写")
	assert.Equal(t, 1, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_credit_owed WHERE player_id=?", fenced))

	fencetest.Update(t, ctx, db, fenced, "seller", 1, 0) // 寄售撤销,解冻
	status, err = wr.ConvertWarCreditOwed(ctx, owed, testNowMs+20_000)
	require.NoError(t, err)
	assert.Equal(t, WarOwedConverted, status)
	assert.Equal(t, 1, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_asset_op WHERE op_id=? AND player_id=? AND seq=1", fencedOp, fenced),
		"转成恰好一条指令,op_id 就是预发的那个")
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_credit_owed WHERE player_id=?", fenced))

	// 墓碑的待入账行(冻结期间记下、之后角色被删):转换删掉它、这一笔放弃,不建 seq 行、不插指令。
	tomb := WarCreditOwed{PlayerID: removed, OpID: removedOp, GuildID: defendGuild,
		Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_WAR_REWARD, Payload: bundle, SourceID: battleID, CreatedMs: testNowMs}
	insertPendingFixture(t, ctx, db, "guild_war_credit_owed", &pb.GuildWarCreditOwedRecord{
		PlayerId: tomb.PlayerID, OpId: tomb.OpID, GuildId: tomb.GuildID, Kind: uint32(tomb.Kind), Payload: tomb.Payload,
		SourceId: tomb.SourceID, CreatedMs: tomb.CreatedMs,
	})
	status, err = wr.ConvertWarCreditOwed(ctx, tomb, testNowMs+30_000)
	require.NoError(t, err)
	assert.Equal(t, WarOwedAbandoned, status)
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_credit_owed WHERE player_id=?", removed))
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_asset_op WHERE op_id=?", removedOp))
	_, found = warTiDBCreditSeqRow(t, ctx, db, removed)
	assert.False(t, found)
}

// 领赏人已删除:追杀令整笔不转移、托管不销毁(没人结单就到期原路退回发布者),本局其余照常(r2-gw#6)。
func TestTiDBWarSettleHoldsTheBountyWhenTheRecipientIsRemoved(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	wr, err := NewWarRepo(repo)
	require.NoError(t, err)
	seedManagedGuild(t, ctx, db, warTestHunterGuild, warTestZone, 1, 30, warTestHunter, nil)
	seedManagedGuild(t, ctx, db, warTestTargetGuild, warTestZone, 1, 30, warTestTarget, nil)
	fencetest.Seed(t, ctx, db, warTestHunter, "hunter", 1)
	fencetest.Update(t, ctx, db, warTestHunter, "", 2, 0)
	fencetest.Seed(t, ctx, db, warTestTarget, "target", 1)
	insertPendingFixture(t, ctx, db, "guild_war_bounty", warActiveSilverBounty())

	out, err := wr.SettleWarBattle(ctx, warHuntInput(5202, warTestHunter, warTestTarget, true))
	require.NoError(t, err)
	assert.Equal(t, warTestHunter, out.BountyHeldRecipientID)
	assert.Equal(t, BountyHeldRecipientRemoved, out.BountyHeldReason)
	assert.Zero(t, out.BountyTag)
	assert.Empty(t, out.OwedCredits)
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_asset_op WHERE op_id=?", warTestRewardOp))
	assert.Equal(t, int(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE),
		warTiDBRowCount(t, ctx, db, "SELECT status FROM guild_war_bounty WHERE bounty_id=?", warTestBounty), "追杀令原样保留")
	assert.Equal(t, 1, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_battle WHERE battle_id=?", uint64(5202)))
}

// 到期收尾:发布者冻结中,追杀令照样到期,退款记进待入账行;发布者已删除,追杀令照样到期,银两退款放弃、不碰他的任何行(r2-gw#6)。
func TestTiDBExpireBountyOwesTheRefundOfAFencedIssuerAndAbandonsARemovedOne(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	wr, err := NewWarRepo(repo)
	require.NoError(t, err)
	const removedIssuer uint64 = 9105
	fencetest.Seed(t, ctx, db, warTestIssuer, "issuer", 1)
	fencetest.Update(t, ctx, db, warTestIssuer, "issuer", 1, 901)
	fencetest.Seed(t, ctx, db, removedIssuer, "gone", 1)
	fencetest.Update(t, ctx, db, removedIssuer, "", 2, 0)
	bundle, err := warSilverBundle(4501)
	require.NoError(t, err)

	fencedBounty := warActiveSilverBounty()
	fencedBounty.ExpiresAtMs = testNowMs - 1
	insertPendingFixture(t, ctx, db, "guild_war_bounty", fencedBounty)
	res, err := wr.ExpireBounty(ctx, ExpireBountyInput{Bounty: fencedBounty, RefundOpID: 880501, RefundBundle: bundle, NowMs: testNowMs})
	require.NoError(t, err)
	assert.True(t, res.Expired)
	assert.True(t, res.RefundOwed)
	assert.False(t, res.HasOp)
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_asset_op WHERE op_id=?", uint64(880501)))
	assert.Equal(t, 1, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_credit_owed WHERE player_id=? AND op_id=?", warTestIssuer, uint64(880501)))

	removedBounty := warActiveSilverBounty()
	removedBounty.BountyId, removedBounty.IssuerPlayerId, removedBounty.TargetPlayerId = warTestBounty+1, removedIssuer, warTestHunter2
	removedBounty.ExpiresAtMs = testNowMs - 1
	insertPendingFixture(t, ctx, db, "guild_war_bounty", removedBounty)
	res, err = wr.ExpireBounty(ctx, ExpireBountyInput{Bounty: removedBounty, RefundOpID: 880502, RefundBundle: bundle, NowMs: testNowMs})
	require.NoError(t, err)
	assert.True(t, res.Expired)
	assert.True(t, res.RefundAbandoned)
	assert.False(t, res.RefundOwed)
	assert.False(t, res.HasOp)
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_asset_op WHERE op_id=?", uint64(880502)))
	assert.Zero(t, warTiDBRowCount(t, ctx, db, "SELECT COUNT(*) FROM guild_war_credit_owed WHERE player_id=?", removedIssuer))
	_, found := warTiDBCreditSeqRow(t, ctx, db, removedIssuer)
	assert.False(t, found, "墓碑之后不再建他的 seq 行")
	assert.Equal(t, int(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_EXPIRED),
		warTiDBRowCount(t, ctx, db, "SELECT status FROM guild_war_bounty WHERE bounty_id=?", removedBounty.GetBountyId()))
}
