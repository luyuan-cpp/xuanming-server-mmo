package data

// 帮战追杀令与灵脉结算的真库用例(数据覆写审计 r2 第 4 轮 r2-gw#1~#3;设计见
// docs/planning/guild-play-v2-implementation-plan.md 末节「帮战追杀令与灵脉结算的数据一致性定案」)。
//
// GUILD_TEST_MYSQL_DSN 未设即 Skip(与 economy_repo_test.go 同一套夹具:库名只许 guild_test / guild_it_<pid>_<n>,
// 每个用例经 schemamigrate 重建全部表)。规则本身的组合在 war/rules_test.go 里纯函数覆盖,这里只验「同一事务里真的这样写了库」:
// 第 7 步目标不符不动追杀令、撕单两半的去向、日上限时整笔不转移、到期收尾重复跑只退一次、灵脉快照不一致回冲突。
//
// 时间一律 testNowMs 派生,不读墙钟。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	"shared/assetop"
	"shared/currencytype"
	"shared/gameday"

	"guild/internal/constants"
	"guild/internal/war"
)

const (
	warTestZone        uint32 = 7
	warTestIssuerGuild uint64 = 9100
	warTestHunterGuild uint64 = 9200
	warTestTargetGuild uint64 = 9300
	warTestIssuer      uint64 = 9101
	warTestHunter      uint64 = 9201
	warTestHunter2     uint64 = 9202
	warTestTarget      uint64 = 9301
	warTestBounty      uint64 = 77001
	warTestRewardOp    uint64 = 880001
	warTestRefundOp    uint64 = 880002
)

var warTestWeekKey = gameday.WeekKey(time.UnixMilli(int64(testNowMs)))

type warFixture struct {
	econFixture
	war *WarRepo
}

// openWarFixture 建三个帮:发单帮(发布者)、猎手帮(两名猎手)、目标帮(目标)。资金全 0。
func openWarFixture(t *testing.T) warFixture {
	t.Helper()
	f := openEconomyFixture(t)
	repo, err := NewWarRepo(f.repo)
	require.NoError(t, err)
	seedManagedGuild(t, f.ctx, f.db, warTestIssuerGuild, warTestZone, 1, 30, warTestIssuer, nil)
	seedManagedGuild(t, f.ctx, f.db, warTestHunterGuild, warTestZone, 1, 30, warTestHunter,
		map[uint64]uint32{warTestHunter2: constants.RoleMember})
	seedManagedGuild(t, f.ctx, f.db, warTestTargetGuild, warTestZone, 1, 30, warTestTarget, nil)
	return warFixture{econFixture: f, war: repo}
}

func warSilverBundle(silver uint64) ([]byte, error) {
	return proto.Marshal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: currencytype.Gold, Amount: silver}}})
}

// warSeedBounty 直接插一张追杀令(绕开 PostBounty 的预扣流程,夹具要的是「已经 ACTIVE」的行)。
func warSeedBounty(t *testing.T, f warFixture, rec *pb.GuildWarBountyRecord) {
	t.Helper()
	mustExec(t, f.ctx, f.db,
		"INSERT INTO guild_war_bounty (bounty_id,zone_id,issuer_player_id,issuer_guild_id,target_player_id,target_guild_id,tier,escrow_silver,public_board,status,escrow_op_id,counter_kills,last_counter_attacker_id,hunter_player_id,created_at_ms,expires_at_ms,resolved_at_ms,escrow_guild_funds) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		rec.GetBountyId(), rec.GetZoneId(), rec.GetIssuerPlayerId(), rec.GetIssuerGuildId(), rec.GetTargetPlayerId(), rec.GetTargetGuildId(),
		rec.GetTier(), rec.GetEscrowSilver(), rec.GetPublicBoard(), rec.GetStatus(), rec.GetEscrowOpId(), rec.GetCounterKills(),
		rec.GetLastCounterAttackerId(), rec.GetHunterPlayerId(), rec.GetCreatedAtMs(), rec.GetExpiresAtMs(), rec.GetResolvedAtMs(),
		rec.GetEscrowGuildFunds())
}

// warActiveSilverBounty:发单帮发的全区江湖榜银两档,托管 4501(奇数,钉住撕单取整),还有 1 小时到期。
func warActiveSilverBounty() *pb.GuildWarBountyRecord {
	return &pb.GuildWarBountyRecord{
		BountyId: warTestBounty, ZoneId: warTestZone,
		IssuerPlayerId: warTestIssuer, IssuerGuildId: warTestIssuerGuild,
		TargetPlayerId: warTestTarget, TargetGuildId: warTestTargetGuild,
		Tier:         uint32(pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_NORMAL_SILVER),
		EscrowSilver: 4501, PublicBoard: 1,
		Status:      uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE),
		CreatedAtMs: testNowMs - 3_600_000, ExpiresAtMs: testNowMs + 3_600_000,
	}
}

// warHuntInput 造一局「hunter 攻、defender 守」的追杀局结算输入:只带追杀令,不带战后奖励指令,便于数清本局插了哪些指令。
func warHuntInput(battleID, hunter, defender uint64, hunterWon bool) SettleWarBattleInput {
	outcome := uint32(2)
	if hunterWon {
		outcome = 1
	}
	return SettleWarBattleInput{
		Record: &pb.GuildWarBattleRecord{
			BattleId: battleID, ZoneId: warTestZone,
			WarBattleKind:   uint32(pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_BOUNTY_HUNT),
			BountyId:        warTestBounty,
			AttackerGuildId: warTestHunterGuild, DefenderGuildId: warTestTargetGuild,
			Outcome: outcome, TotalRounds: 6, DiminishPercent: 100,
			SummaryPayload: []byte("{}"), FinishedAtMs: testNowMs,
		},
		WeekKey: warTestWeekKey, DayKey: econDayKey, NowMs: testNowMs,
		ProtectSec: 10, PeaceCooldownSec: 60, BountyProtectSec: 1800, DailyPayoutLimit: 5,
		PlayerCredits: []PlayerSettlementCredit{
			{PlayerID: hunter, GuildID: warTestHunterGuild, TeamIndex: 0, Won: hunterWon, OpponentID: defender},
			{PlayerID: defender, GuildID: warTestTargetGuild, TeamIndex: 1, Won: !hunterWon, OpponentID: hunter},
		},
		BountyID: warTestBounty,
		BountyFacts: war.BountyFacts{
			WarBattleKind: pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_BOUNTY_HUNT,
			ZoneID:        warTestZone, HunterGuildID: warTestHunterGuild, HunterID: hunter, TargetID: defender,
			HunterWon: hunterWon, TargetWon: !hunterWon, CounterKillTear: 2, NowMs: testNowMs,
		},
		BountyRewardOpID: warTestRewardOp, BountyRefundOpID: warTestRefundOp,
		BuildSilverBundle: warSilverBundle,
		LeaseUntilMs:      testNowMs + econLeaseMs, LeaseToken: battleID,
	}
}

func warBounty(t *testing.T, f warFixture) *pb.GuildWarBountyRecord {
	t.Helper()
	b, err := f.war.GetBounty(f.ctx, warTestBounty)
	require.NoError(t, err)
	require.NotNil(t, b)
	return b
}

// warOpSilver 读一条指令,断言它是追杀令转账,返回收款人与银两数。
func warOpSilver(t *testing.T, f warFixture, opID uint64) (playerID, silver uint64) {
	t.Helper()
	rec := econRecord(t, f.econFixture, opID)
	require.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_BOUNTY_PAYOUT, rec.GetKind())
	require.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_BOUNTY_PAYOUT), rec.GetTxType())
	require.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), rec.GetStream())
	var bundle assetpb.AssetBundle
	require.NoError(t, proto.Unmarshal(rec.GetPayload(), &bundle))
	require.Len(t, bundle.GetCurrencies(), 1)
	return rec.GetPlayerId(), bundle.GetCurrencies()[0].GetAmount()
}

func warBattleRows(t *testing.T, f warFixture, battleID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM guild_war_battle WHERE battle_id=?", battleID).Scan(&n))
	return n
}

// TestGuildWarBountyColumnsCoverEveryProtoField:guildWarBountyColumns 是全部读路径共用的手写列清单(扫描顺序按它排)。
// 往 GuildWarBountyRecord 加字段却忘了同步,新列会永远读成零值(escrow_guild_funds 读成 0 = 公费档到期一分不退)。不连库。
func TestGuildWarBountyColumnsCoverEveryProtoField(t *testing.T) {
	fields := (&pb.GuildWarBountyRecord{}).ProtoReflect().Descriptor().Fields()
	want := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		want = append(want, string(fields.Get(i).Name()))
	}
	require.Equal(t, want, econSplitColumns(guildWarBountyColumns), "guildWarBountyColumns 的列名与顺序必须与 GuildWarBountyRecord 的字段序逐一一致")
}

// 第 7 步(r2-gw#1):本局防守方不是追杀令目标 —— 带着别人的 bounty_id 打任意一局 —— 不动追杀令、不发赏金,本局其余照常落定。
func TestSettleWarBattle_BountyTargetMismatchLeavesBountyActive(t *testing.T) {
	f := openWarFixture(t)
	warSeedBounty(t, f, warActiveSilverBounty())
	const bystander uint64 = 9302
	seedMemberRow(t, f.ctx, f.db, warTestTargetGuild, bystander, constants.RoleMember)

	out, err := f.war.SettleWarBattle(f.ctx, warHuntInput(5001, warTestHunter, bystander, true))
	require.NoError(t, err)
	assert.Zero(t, out.BountyTag)
	assert.Empty(t, out.CreatedOps)
	assert.False(t, econOpExists(t, f.econFixture, warTestRewardOp), "目标不符不得发赏金")

	b := warBounty(t, f)
	assert.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE), b.GetStatus())
	assert.Zero(t, b.GetHunterPlayerId())
	assert.Equal(t, 1, warBattleRows(t, f, 5001), "这一局的其余结算照常提交")
}

// 追杀成功:销单、给猎手插领赏指令、占当日领赏次数、给目标 30 分钟免再挂保护,全在结算事务里一次提交(r2-gw#2)。
func TestSettleWarBattle_BountyHuntedPaysHunterInTheSameTransaction(t *testing.T) {
	f := openWarFixture(t)
	warSeedBounty(t, f, warActiveSilverBounty())

	out, err := f.war.SettleWarBattle(f.ctx, warHuntInput(5002, warTestHunter, warTestTarget, true))
	require.NoError(t, err)
	assert.Equal(t, war.BountyTagHunted, out.BountyTag)
	require.Len(t, out.CreatedOps, 1)

	b := warBounty(t, f)
	assert.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED), b.GetStatus())
	assert.Equal(t, warTestHunter, b.GetHunterPlayerId())
	to, silver := warOpSilver(t, f, warTestRewardOp)
	assert.Equal(t, warTestHunter, to)
	assert.Equal(t, uint64(4501), silver)
	assert.False(t, econOpExists(t, f.econFixture, warTestRefundOp), "追杀成功不退发布者")
	used, _ := econCounterUsed(t, f.econFixture, warTestHunter, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_WAR_BOUNTY_PAYOUT, bountyPayoutCounterRefID, econDayKey)
	assert.Equal(t, uint32(1), used)
	st, err := f.war.GetPlayerWarState(f.ctx, warTestTarget, warTestZone, warTestWeekKey)
	require.NoError(t, err)
	assert.Equal(t, testNowMs+1800*1000, st.GetBountyProtectUntilMs())

	// Kafka 重投同一局:幂等闸门挡住,不重复发赏金。
	_, err = f.war.SettleWarBattle(f.ctx, warHuntInput(5002, warTestHunter, warTestTarget, true))
	require.ErrorIs(t, err, ErrWarBattleAlreadyDone)
}

// 撕单两半去向(r2-gw#2):银两档一半(向下取整)给目标、其余退给发布者本人,两条指令与撕单同事务。
func TestSettleWarBattle_BountyTornSplitsSilverBetweenTargetAndIssuer(t *testing.T) {
	f := openWarFixture(t)
	b := warActiveSilverBounty()
	b.CounterKills, b.LastCounterAttackerId = 1, warTestHunter // 已被第一名追兵反杀过
	warSeedBounty(t, f, b)

	out, err := f.war.SettleWarBattle(f.ctx, warHuntInput(5003, warTestHunter2, warTestTarget, false))
	require.NoError(t, err)
	assert.Equal(t, war.BountyTagTorn, out.BountyTag)
	require.Len(t, out.CreatedOps, 2)

	assert.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_TORN_BY_TARGET), warBounty(t, f).GetStatus())
	toTarget, half := warOpSilver(t, f, warTestRewardOp)
	assert.Equal(t, warTestTarget, toTarget)
	assert.Equal(t, uint64(2250), half)
	toIssuer, rest := warOpSilver(t, f, warTestRefundOp)
	assert.Equal(t, warTestIssuer, toIssuer)
	assert.Equal(t, uint64(2251), rest)
	assert.Equal(t, uint64(4501), half+rest, "两半合计等于托管")
}

// 公费档撕单:目标拿系统赏金池的一半银两,发单帮按实付资金退回一半(帮会资金,同事务),不退银两。
func TestSettleWarBattle_PublicBountyTornRefundsHalfTheFundsToIssuerGuild(t *testing.T) {
	f := openWarFixture(t)
	b := warActiveSilverBounty()
	b.Tier = uint32(pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC)
	b.EscrowSilver, b.EscrowGuildFunds = 6000, 2000
	b.CounterKills, b.LastCounterAttackerId = 1, warTestHunter
	warSeedBounty(t, f, b)
	before := econFunds(t, f.econFixture, warTestIssuerGuild)

	_, err := f.war.SettleWarBattle(f.ctx, warHuntInput(5004, warTestHunter2, warTestTarget, false))
	require.NoError(t, err)
	toTarget, half := warOpSilver(t, f, warTestRewardOp)
	assert.Equal(t, warTestTarget, toTarget)
	assert.Equal(t, uint64(3000), half)
	assert.False(t, econOpExists(t, f.econFixture, warTestRefundOp), "公费档不退银两")
	assert.Equal(t, before+1000, econFunds(t, f.econFixture, warTestIssuerGuild))
}

// 日上限(r2-gw#2):领赏人当日已领满就整笔不转移 —— 追杀令保持 ACTIVE、反杀数不加、不插指令,本局其余结算照常。
func TestSettleWarBattle_BountyPayoutLimitKeepsBountyAndEscrow(t *testing.T) {
	f := openWarFixture(t)
	warSeedBounty(t, f, warActiveSilverBounty())
	mustExec(t, f.ctx, f.db,
		"INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms) VALUES (?,?,?,?,?,?)",
		warTestHunter, int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_WAR_BOUNTY_PAYOUT), bountyPayoutCounterRefID, econDayKey, 5, testNowMs)

	out, err := f.war.SettleWarBattle(f.ctx, warHuntInput(5005, warTestHunter, warTestTarget, true))
	require.NoError(t, err)
	assert.Equal(t, warTestHunter, out.BountyHeldRecipientID)
	assert.Zero(t, out.BountyTag)
	assert.Empty(t, out.CreatedOps)
	b := warBounty(t, f)
	assert.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE), b.GetStatus())
	assert.Zero(t, b.GetHunterPlayerId())
	used, _ := econCounterUsed(t, f.econFixture, warTestHunter, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_WAR_BOUNTY_PAYOUT, bountyPayoutCounterRefID, econDayKey)
	assert.Equal(t, uint32(5), used, "不转移就不占次数")
	assert.Equal(t, 1, warBattleRows(t, f, 5005))
}

// 到期收尾(r2-gw#2):置 EXPIRED 并原路退回托管;同一张再跑一次(多副本并发、下一轮重扫)只退一次。
func TestExpireBounty_RefundsSilverOnceAcrossRepeatedRuns(t *testing.T) {
	f := openWarFixture(t)
	b := warActiveSilverBounty()
	b.ExpiresAtMs = testNowMs - 1
	warSeedBounty(t, f, b)

	due, err := f.war.ListDueBounties(f.ctx, testNowMs, BountyExpiryCursor{}, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)

	bundle, err := warSilverBundle(4501)
	require.NoError(t, err)
	res, err := f.war.ExpireBounty(f.ctx, ExpireBountyInput{Bounty: due[0], RefundOpID: warTestRefundOp, RefundBundle: bundle, NowMs: testNowMs})
	require.NoError(t, err)
	assert.True(t, res.Expired)
	require.True(t, res.HasOp)
	to, silver := warOpSilver(t, f, warTestRefundOp)
	assert.Equal(t, warTestIssuer, to)
	assert.Equal(t, uint64(4501), silver)
	assert.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_EXPIRED), warBounty(t, f).GetStatus())

	// 第二个副本拿着同一份候选再收一次:锁内复核已不是 ACTIVE,整笔回滚,不插第二条退款。
	res, err = f.war.ExpireBounty(f.ctx, ExpireBountyInput{Bounty: due[0], RefundOpID: warTestRefundOp + 1, RefundBundle: bundle, NowMs: testNowMs})
	require.NoError(t, err)
	assert.False(t, res.Expired)
	assert.False(t, econOpExists(t, f.econFixture, warTestRefundOp+1))
	due, err = f.war.ListDueBounties(f.ctx, testNowMs, BountyExpiryCursor{}, 10)
	require.NoError(t, err)
	assert.Empty(t, due)
}

// 公费档到期退实扣的帮会资金,也只退一次;扣款还没终结(status=1)的不在候选里、也收不了。
func TestExpireBounty_PublicFundsRefundOnceAndPendingEscrowUntouched(t *testing.T) {
	f := openWarFixture(t)
	pub := warActiveSilverBounty()
	pub.Tier = uint32(pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC)
	pub.EscrowSilver, pub.EscrowGuildFunds = 6000, 2000
	pub.ExpiresAtMs = testNowMs - 1
	warSeedBounty(t, f, pub)
	pending := warActiveSilverBounty()
	pending.BountyId, pending.TargetPlayerId = warTestBounty+1, warTestHunter2
	pending.Status = uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ESCROW_PENDING)
	pending.ExpiresAtMs = testNowMs - 1
	warSeedBounty(t, f, pending)
	before := econFunds(t, f.econFixture, warTestIssuerGuild)

	due, err := f.war.ListDueBounties(f.ctx, testNowMs, BountyExpiryCursor{}, 10)
	require.NoError(t, err)
	require.Len(t, due, 1, "status=1 的不在候选里")
	for i := 0; i < 2; i++ {
		res, err := f.war.ExpireBounty(f.ctx, ExpireBountyInput{Bounty: due[0], NowMs: testNowMs})
		require.NoError(t, err)
		assert.Equal(t, i == 0, res.Expired, "run %d", i)
		assert.False(t, res.HasOp, "公费档不插银两指令")
	}
	assert.Equal(t, before+2000, econFunds(t, f.econFixture, warTestIssuerGuild))

	res, err := f.war.ExpireBounty(f.ctx, ExpireBountyInput{Bounty: pending, RefundOpID: warTestRefundOp, RefundBundle: []byte{1}, NowMs: testNowMs})
	require.NoError(t, err)
	assert.False(t, res.Expired, "扣款还没终结的不收")
	assert.False(t, econOpExists(t, f.econFixture, warTestRefundOp))
}

// 灵脉快照(r2-gw#3):算 BannerDelta 之后、提交之前灵脉被另一场结算改过 → 回 ErrWarSettleStale,整笔不写;快照仍有效则照常落定。
func TestSettleWarBattle_StaleLeylineSnapshotConflicts(t *testing.T) {
	f := openWarFixture(t)
	require.NoError(t, f.war.EnsureZoneLeylines(f.ctx, warTestZone, testNowMs))
	mustExec(t, f.ctx, f.db, "UPDATE guild_war_leyline SET owner_guild_id=?, banner_hp=? WHERE zone_id=? AND leyline_id=?",
		warTestTargetGuild, 500, warTestZone, 1)
	d := war.ApplyBannerImpact(nil, testNowMs, warTestTargetGuild, 500, 0, warTestHunterGuild, false, true, true, false)
	require.Less(t, d.HPAfter, uint32(500))

	in := warHuntInput(5006, warTestHunter, warTestTarget, true)
	in.BountyID = 0 // 只验灵脉
	in.Record.WarBattleKind = uint32(pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_LEYLINE_PVP)
	in.Record.LeylineId = 1
	in.BannerDelta = &d

	// 另一场结算抢先提交,把旗势扣到 260(取一个与 d.HPAfter 不同的值:被盲写覆盖的话这里会变成 d.HPAfter)。
	require.NotEqual(t, uint32(260), d.HPAfter)
	mustExec(t, f.ctx, f.db, "UPDATE guild_war_leyline SET banner_hp=? WHERE zone_id=? AND leyline_id=?", 260, warTestZone, 1)
	_, err := f.war.SettleWarBattle(f.ctx, in)
	require.ErrorIs(t, err, ErrWarSettleStale)
	assert.Zero(t, warBattleRows(t, f, 5006), "冲突时整笔回滚")
	rows, err := f.war.ListLeylines(f.ctx, warTestZone)
	require.NoError(t, err)
	cur, _ := war.FindLeylineBasis(rows, 1, warTestHunterGuild)
	assert.Equal(t, uint32(260), cur.GetBannerHp(), "先提交的扣减没有被覆盖")

	// 攻方帮另占着一座灵脉,也算快照过时(每帮限占 1 座的判定变了)。
	mustExec(t, f.ctx, f.db, "UPDATE guild_war_leyline SET banner_hp=? WHERE zone_id=? AND leyline_id=?", 500, warTestZone, 1)
	mustExec(t, f.ctx, f.db, "UPDATE guild_war_leyline SET owner_guild_id=? WHERE zone_id=? AND leyline_id=?", warTestHunterGuild, warTestZone, 2)
	_, err = f.war.SettleWarBattle(f.ctx, in)
	require.ErrorIs(t, err, ErrWarSettleStale)

	// 快照与库一致:照常落定,写的是算好的新旗势。
	mustExec(t, f.ctx, f.db, "UPDATE guild_war_leyline SET owner_guild_id=0 WHERE zone_id=? AND leyline_id=?", warTestZone, 2)
	_, err = f.war.SettleWarBattle(f.ctx, in)
	require.NoError(t, err)
	rows, err = f.war.ListLeylines(f.ctx, warTestZone)
	require.NoError(t, err)
	cur, _ = war.FindLeylineBasis(rows, 1, warTestHunterGuild)
	assert.Equal(t, d.HPAfter, cur.GetBannerHp())
}

// ── 入账分流与终态清理(r2-gw#5 / #7) ───────────────────────────────────────────

// warFillCreditWindow 给 player 的 GUILD_CREDIT 流塞满 16 条未决兑换指令,返回纪元(与 activity_repo_integration_test.go 的
// actFillCreditWindow 同一写法;那个文件带 integration 构建标签,这里不能直接调)。
func warFillCreditWindow(t *testing.T, f warFixture, playerID, opBase uint64) uint64 {
	t.Helper()
	epoch := testNowMs - 100_000
	maxPending := uint64(assetop.DefaultLimits.MaxPending)
	mustExec(t, f.ctx, f.db,
		"INSERT INTO guild_player_op_seq (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, ?, ?, ?)",
		playerID, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), maxPending+1, epoch, testNowMs)
	for i := range maxPending {
		econInsertOp(t, f.econFixture, econShopRecord(t, opBase+i, playerID, epoch, i+1))
	}
	return epoch
}

// warFreeCreditWindow 把 warFillCreditWindow 塞进去的兑换指令全部记成已到账,腾出空位。
func warFreeCreditWindow(t *testing.T, f warFixture, playerID uint64) {
	t.Helper()
	mustExec(t, f.ctx, f.db, "UPDATE guild_asset_op SET status=? WHERE player_id=? AND kind=?",
		int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED), playerID, int32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP))
}

// warAllOwed 按循环的翻页方式(每页 2 行)读完全部待入账行。
func warAllOwed(t *testing.T, f warFixture) []WarCreditOwed {
	t.Helper()
	var all []WarCreditOwed
	var cursor WarCreditOwedCursor
	for {
		page, err := f.war.ListWarCreditOwed(f.ctx, cursor, 2)
		require.NoError(t, err)
		all = append(all, page...)
		if len(page) < 2 {
			return all
		}
		cursor = page[len(page)-1].Cursor()
	}
}

// warRewardInput 造一局只带战后奖励的野外直打:attacker 胜,两人各有一条银两指令(attacker 1000、defender 2000)。
func warRewardInput(t *testing.T, battleID, attacker, defender, attackerOp, defenderOp uint64) SettleWarBattleInput {
	t.Helper()
	in := warHuntInput(battleID, attacker, defender, true)
	in.Record.WarBattleKind = uint32(pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_FIELD_ROAD)
	in.Record.BountyId, in.BountyID, in.BountyFacts = 0, 0, war.BountyFacts{}
	for i, opID := range []uint64{attackerOp, defenderOp} {
		bundle, err := warSilverBundle(uint64(1000 * (i + 1)))
		require.NoError(t, err)
		in.PlayerCredits[i].OpID, in.PlayerCredits[i].CreditBundle = opID, bundle
	}
	return in
}

// r2-gw#5:一名参战者本流未决已满(16 条)不挡整局 —— 对局落定、另一人照常入账;他那一笔不插指令、seq 不前进,原样记进待入账表。
// Kafka 重投被幂等闸门挡住,不多记;窗口仍满时转换什么都不写;腾出空位后转成恰好一条指令(op_id 就是预发的那个,第 17 个 seq);
// 别的副本拿同一行再转,行已不在,什么都不写、不烧 seq。
func TestSettleWarBattle_FullCreditWindowDefersOnlyThatPlayer(t *testing.T) {
	f := openWarFixture(t)
	const (
		battleID   uint64 = 5101
		hunterOp   uint64 = 880101
		targetOp   uint64 = 880102
		fillOpBase uint64 = 880200
	)
	epoch := warFillCreditWindow(t, f, warTestHunter, fillOpBase)
	maxPending := uint64(assetop.DefaultLimits.MaxPending)
	in := warRewardInput(t, battleID, warTestHunter, warTestTarget, hunterOp, targetOp)

	out, err := f.war.SettleWarBattle(f.ctx, in)
	require.NoError(t, err, "一个人的积压不挡同局其他人的结算")
	assert.Equal(t, 1, warBattleRows(t, f, battleID))
	require.Len(t, out.CreatedOps, 1)
	assert.Equal(t, targetOp, out.CreatedOps[0].OpID, "另一名参战者照常入账")
	assert.False(t, econOpExists(t, f.econFixture, hunterOp), "未决已满者不插指令行")
	assert.Equal(t, maxPending+1, econNextSeq(t, f.econFixture, warTestHunter, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), "seq 不前进")
	want := WarCreditOwed{
		PlayerID: warTestHunter, OpID: hunterOp, GuildID: warTestHunterGuild,
		Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_WAR_REWARD, TxType: rollbackpb.TransactionType_TX_GUILD_WAR_REWARD,
		PeriodKey: warTestWeekKey, Payload: in.PlayerCredits[0].CreditBundle, SourceID: battleID, CreatedMs: testNowMs,
	}
	assert.Equal(t, []WarCreditOwed{want}, out.OwedCredits)
	owed := warAllOwed(t, f)
	require.Equal(t, []WarCreditOwed{want}, owed, "与结算同一事务落库")

	_, err = f.war.SettleWarBattle(f.ctx, in)
	require.ErrorIs(t, err, ErrWarBattleAlreadyDone)
	assert.Len(t, warAllOwed(t, f), 1, "重投不多记")

	later := testNowMs + 10_000
	status, err := f.war.ConvertWarCreditOwed(f.ctx, owed[0], later)
	require.NoError(t, err)
	assert.Equal(t, WarOwedStillFull, status)
	assert.False(t, econOpExists(t, f.econFixture, hunterOp))
	assert.Len(t, warAllOwed(t, f), 1)

	warFreeCreditWindow(t, f, warTestHunter)
	status, err = f.war.ConvertWarCreditOwed(f.ctx, owed[0], later)
	require.NoError(t, err)
	assert.Equal(t, WarOwedConverted, status)
	rec := econRecord(t, f.econFixture, hunterOp)
	assert.Equal(t, warTestHunter, rec.GetPlayerId())
	assert.Equal(t, warTestHunterGuild, rec.GetGuildId())
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_WAR_REWARD, rec.GetKind())
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_WAR_REWARD), rec.GetTxType())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, maxPending+1, rec.GetSeq(), "第 17 个 seq")
	assert.Equal(t, epoch, rec.GetStreamEpoch())
	assert.Equal(t, warTestWeekKey, rec.GetPeriodKey())
	assert.Equal(t, in.PlayerCredits[0].CreditBundle, rec.GetPayload(), "奖励包原样转入")
	assert.Zero(t, rec.GetLeaseUntilMs(), "不持租约")
	assert.Equal(t, later, rec.GetNextAttemptMs(), "重投循环立刻可领")
	assert.Empty(t, warAllOwed(t, f), "转换与删待入账行在同一事务里")

	status, err = f.war.ConvertWarCreditOwed(f.ctx, owed[0], later+1)
	require.NoError(t, err)
	assert.Equal(t, WarOwedGone, status)
	assert.Equal(t, maxPending+2, econNextSeq(t, f.econFixture, warTestHunter, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), "回滚的转换不烧 seq")
}

// r2-gw#5:追杀令的领赏人本流未决已满时照样结单 —— 赏金记进待入账行、当日领赏次数照占(钱只是晚到),腾出空位后恰好补发一次。
func TestSettleWarBattle_BountyPayoutOwedWhenHunterWindowIsFull(t *testing.T) {
	f := openWarFixture(t)
	warSeedBounty(t, f, warActiveSilverBounty())
	warFillCreditWindow(t, f, warTestHunter, 880600)

	out, err := f.war.SettleWarBattle(f.ctx, warHuntInput(5102, warTestHunter, warTestTarget, true))
	require.NoError(t, err)
	assert.Equal(t, war.BountyTagHunted, out.BountyTag)
	assert.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED), warBounty(t, f).GetStatus())
	assert.False(t, econOpExists(t, f.econFixture, warTestRewardOp))
	used, _ := econCounterUsed(t, f.econFixture, warTestHunter, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_WAR_BOUNTY_PAYOUT, bountyPayoutCounterRefID, econDayKey)
	assert.Equal(t, uint32(1), used, "领赏记进待入账行也占次数")
	owed := warAllOwed(t, f)
	require.Len(t, owed, 1)
	assert.Equal(t, warTestRewardOp, owed[0].OpID)
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_BOUNTY_PAYOUT, owed[0].Kind)
	assert.Equal(t, rollbackpb.TransactionType_TX_GUILD_BOUNTY_PAYOUT, owed[0].TxType)

	warFreeCreditWindow(t, f, warTestHunter)
	status, err := f.war.ConvertWarCreditOwed(f.ctx, owed[0], testNowMs+10_000)
	require.NoError(t, err)
	assert.Equal(t, WarOwedConverted, status)
	to, silver := warOpSilver(t, f, warTestRewardOp)
	assert.Equal(t, warTestHunter, to)
	assert.Equal(t, uint64(4501), silver)
	assert.Empty(t, warAllOwed(t, f))
}

// r2-gw#5:到期收尾时发布者本流未决已满 —— 追杀令照样置 EXPIRED,退款记进待入账行(不再每轮失败重扫);再收一次不多记;
// 腾出空位后恰好补发一次。
func TestExpireBounty_FullIssuerWindowExpiresAndOwesTheRefund(t *testing.T) {
	f := openWarFixture(t)
	b := warActiveSilverBounty()
	b.ExpiresAtMs = testNowMs - 1
	warSeedBounty(t, f, b)
	warFillCreditWindow(t, f, warTestIssuer, 880300)
	bundle, err := warSilverBundle(4501)
	require.NoError(t, err)

	res, err := f.war.ExpireBounty(f.ctx, ExpireBountyInput{Bounty: b, RefundOpID: warTestRefundOp, RefundBundle: bundle, NowMs: testNowMs})
	require.NoError(t, err)
	assert.True(t, res.Expired, "发布者未决已满不挡到期")
	assert.True(t, res.RefundOwed)
	assert.False(t, res.HasOp)
	assert.False(t, econOpExists(t, f.econFixture, warTestRefundOp))
	assert.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_EXPIRED), warBounty(t, f).GetStatus())
	owed := warAllOwed(t, f)
	require.Len(t, owed, 1)
	assert.Equal(t, warTestIssuer, owed[0].PlayerID)
	assert.Equal(t, warTestRefundOp, owed[0].OpID)
	assert.Equal(t, warTestBounty, owed[0].SourceID)

	res, err = f.war.ExpireBounty(f.ctx, ExpireBountyInput{Bounty: b, RefundOpID: warTestRefundOp + 1, RefundBundle: bundle, NowMs: testNowMs})
	require.NoError(t, err)
	assert.False(t, res.Expired)
	assert.Len(t, warAllOwed(t, f), 1, "已收尾的单不多记")

	warFreeCreditWindow(t, f, warTestIssuer)
	status, err := f.war.ConvertWarCreditOwed(f.ctx, owed[0], testNowMs+10_000)
	require.NoError(t, err)
	assert.Equal(t, WarOwedConverted, status)
	to, silver := warOpSilver(t, f, warTestRefundOp)
	assert.Equal(t, warTestIssuer, to)
	assert.Equal(t, uint64(4501), silver)
	assert.False(t, econOpExists(t, f.econFixture, warTestRefundOp+1))
}

// r2-gw#7:保留期清理只删到期已超保留期的终态行(结单 / 撕单 / 到期退回 / 扣款作废),每轮有上限;
// ACTIVE、扣款还没终结的,以及还在保留期内的终态行一律不动。
func TestPurgeTerminalBounties_DeletesOnlyOldTerminalRows(t *testing.T) {
	f := openWarFixture(t)
	const cutoff = testNowMs
	seed := func(id uint64, status pb.GuildWarBountyStatus, expiresAtMs uint64) {
		b := warActiveSilverBounty()
		b.BountyId, b.TargetPlayerId, b.Status, b.ExpiresAtMs = id, id, uint32(status), expiresAtMs
		warSeedBounty(t, f, b)
	}
	seed(1, pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED, cutoff-1)
	seed(2, pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_TORN_BY_TARGET, cutoff-1)
	seed(3, pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_EXPIRED, cutoff-1)
	seed(4, pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ESCROW_FAILED, cutoff-1)
	seed(5, pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE, cutoff-1)
	seed(6, pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ESCROW_PENDING, cutoff-1)
	seed(7, pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED, cutoff)

	n, err := f.war.PurgeTerminalBounties(f.ctx, cutoff, 3)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "每轮有上限")
	n, err = f.war.PurgeTerminalBounties(f.ctx, cutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = f.war.PurgeTerminalBounties(f.ctx, cutoff, 100)
	require.NoError(t, err)
	assert.Zero(t, n)
	for id := uint64(1); id <= 7; id++ {
		b, err := f.war.GetBounty(f.ctx, id)
		require.NoError(t, err)
		assert.Equal(t, id >= 5, b != nil, "bounty %d", id)
	}
}
