// 赛季技能与单槽赛季印（方案 A）单测（docs/design/season-skills-and-seal-design.md）。
// 覆盖：纯规则、表校验、GetState、战外免费切印、小额灵石追赶（精确/一键补满/幂等重放/报价防呆/封顶）、
// 正规 PVE/PVP 战斗结算加分、轻属性仅加成气血/防御不碰速度与暴击、以及赛季技能免费生效与换季退场。

#include <gtest/gtest.h>

#include <cstdint>
#include <limits>
#include <memory>

#include "engine/core/time/system/time.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_season_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/player_season.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "services/battle/constants/wuxing_expansion_rules.h"
#include "services/battle/system/season_rules.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/player_attribute.h"
#include "services/scene/player/system/player_equipment_v1.h"
#include "services/scene/player/system/player_season.h"
#include "services/scene/player/system/player_skill.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/buff_table.h"
#include "table/code/class_table.h"
#include "table/code/seasonrule_table.h"
#include "table/code/seasonseal_table.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/season_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {

seasonrules::RuleParams GoodRuleParams() {
    seasonrules::RuleParams p;
    p.seasonId = 1;
    p.startsAtMs = 1000;
    p.endsAtMs = 5000;
    p.enabled = true;
    p.sealMaxPoints = 12;
    p.pveWinPoints = 1;
    p.pvpWinPoints = 2;
    p.pvpOtherPoints = 1;
    p.maxHealthBpsCap = 300;
    p.defenseBpsCap = 300;
    p.catchupCurrencyType = static_cast<uint32_t>(kCurrencyDiamond);
    p.catchupPricePerPoint = 10;
    p.defaultSealId = 101;
    return p;
}

std::shared_ptr<SeasonRuleTableSnapshot> MakeRuleSnapshot(const std::vector<SeasonRuleTable>& rows) {
    auto snap = std::make_shared<SeasonRuleTableSnapshot>();
    for (const auto& row : rows) {
        *snap->data.add_data() = row;
    }
    for (int i = 0; i < snap->data.data_size(); ++i) {
        const auto& r = snap->data.data(i);
        snap->idMap[r.id()] = &r;
    }
    return snap;
}

std::shared_ptr<SeasonSealTableSnapshot> MakeSealSnapshot(const std::vector<SeasonSealTable>& rows) {
    auto snap = std::make_shared<SeasonSealTableSnapshot>();
    for (const auto& row : rows) {
        *snap->data.add_data() = row;
    }
    for (int i = 0; i < snap->data.data_size(); ++i) {
        const auto& r = snap->data.data(i);
        snap->idMap[r.id()] = &r;
        snap->seasonIdIndex[r.season_id()].push_back(&r);
    }
    return snap;
}

} // namespace

// ── 纯规则测试 ──────────────────────────────────────────────────────────────

TEST(SeasonRulesTest, WindowAndOverlapAndValidationBounds) {
    EXPECT_TRUE(seasonrules::IsWindowActive(1000, 5000, true, 1000));
    EXPECT_TRUE(seasonrules::IsWindowActive(1000, 5000, true, 4999));
    EXPECT_FALSE(seasonrules::IsWindowActive(1000, 5000, true, 5000)) << "左闭右开";
    EXPECT_FALSE(seasonrules::IsWindowActive(1000, 5000, false, 2000));
    EXPECT_TRUE(seasonrules::IsWindowActive(0, 0, true, 999999));

    EXPECT_TRUE(seasonrules::WindowsOverlap(100, 500, 499, 900));
    EXPECT_FALSE(seasonrules::WindowsOverlap(100, 500, 500, 900));
    EXPECT_TRUE(seasonrules::WindowsOverlap(100, 0, 9999, 0));

    EXPECT_EQ(seasonrules::ValidateRule(GoodRuleParams()), nullptr);
    auto bad = GoodRuleParams();
    bad.maxHealthBpsCap = seasonrules::kHardStatCapBps + 1;
    EXPECT_NE(seasonrules::ValidateRule(bad), nullptr) << "禁止突破 5% 轻属性硬上限";
    bad = GoodRuleParams();
    bad.defenseBpsCap = seasonrules::kHardStatCapBps + 1;
    EXPECT_NE(seasonrules::ValidateRule(bad), nullptr);
    bad = GoodRuleParams();
    bad.sealMaxPoints = 0;
    EXPECT_NE(seasonrules::ValidateRule(bad), nullptr);
    bad = GoodRuleParams();
    bad.endsAtMs = bad.startsAtMs;
    EXPECT_NE(seasonrules::ValidateRule(bad), nullptr);
}

TEST(SeasonRulesTest, ScaledBonusAndCatchupPlanAndSeasonalSkill) {
    EXPECT_EQ(seasonrules::ScaledBonusBps(0, 12, 300), 0u);
    EXPECT_EQ(seasonrules::ScaledBonusBps(6, 12, 300), 150u);
    EXPECT_EQ(seasonrules::ScaledBonusBps(12, 12, 300), 300u);
    EXPECT_EQ(seasonrules::ScaledBonusBps(99, 12, 300), 300u) << "超额点数按满级封顶";

    seasonrules::CatchupPlan plan;
    ASSERT_TRUE(seasonrules::ComputeCatchupPlan(4, 12, 0, 10, plan));
    EXPECT_EQ(plan.pointsToBuy, 8u);
    EXPECT_EQ(plan.totalPrice, 80u);

    ASSERT_TRUE(seasonrules::ComputeCatchupPlan(4, 12, 3, 10, plan));
    EXPECT_EQ(plan.pointsToBuy, 3u);
    EXPECT_EQ(plan.totalPrice, 30u);

    EXPECT_FALSE(seasonrules::ComputeCatchupPlan(4, 12, 9, 10, plan)) << "不可超过剩余上限";
    EXPECT_FALSE(seasonrules::ComputeCatchupPlan(12, 12, 0, 10, plan)) << "满级拒绝追赶";
    EXPECT_FALSE(seasonrules::ComputeCatchupPlan(0, 12, 0, 0, plan)) << "单价 0 表示关闭追赶";

    EXPECT_TRUE(seasonrules::IsSkillActiveInSeason(0, 0));
    EXPECT_TRUE(seasonrules::IsSkillActiveInSeason(0, 1));
    EXPECT_TRUE(seasonrules::IsSkillActiveInSeason(1, 1));
    EXPECT_FALSE(seasonrules::IsSkillActiveInSeason(1, 0));
    EXPECT_FALSE(seasonrules::IsSkillActiveInSeason(1, 2));
}

// ── Scene 集成测试 ──────────────────────────────────────────────────────────

namespace {

class PlayerSeasonSystemTest : public ::testing::Test {
protected:
    void SetUp() override {
        prevRuleSnap = SeasonRuleTableManager::Instance().CurrentSnapshot();
        prevSealSnap = SeasonSealTableManager::Instance().CurrentSnapshot();
        SeasonRuleTableManager::Instance().Load();
        SeasonSealTableManager::Instance().Load();
        SkillTableManager::Instance().Load();
        BuffTableManager::Instance().Load();
        ClassTableManager::Instance().Load();
        AttributePoolTableManager::Instance().Load();
        AttributeDimensionTableManager::Instance().Load();
        AttributeAllocRatioTableManager::Instance().Load();
        AttributeRuleTableManager::Instance().Load();

        persistCalls = 0;
        PlayerSeasonSystem::SetPersistForTest(&CountPersist);
        PlayerSeasonSystem::ResetTowerLeaderboardForTest();

        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player).value = 99001;
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 1;
        tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player).set_class_(1);
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(20);
        auto& base = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player);
        base.set_health(1000);
        base.set_mana(500);
        tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerSeasonComp>(player);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        tlsEcs.playerList[99001] = player;
        ASSERT_EQ(CurrencySystem::AddCurrency(player, kCurrencyGold, 100000), kSuccess);
        ASSERT_EQ(CurrencySystem::AddCurrency(player, kCurrencyDiamond, 500), kSuccess);

        PlayerSkillSystem::InitializeOnLoad(player);
        PlayerAttributeSystem::InitializeOnLoad(player);
    }

    void TearDown() override {
        PlayerSeasonSystem::SetPersistForTest(nullptr);
        PlayerSeasonSystem::ResetTowerLeaderboardForTest();
        tlsEcs.playerList.erase(99001);
        if (tlsEcs.actorRegistry.valid(player)) {
            tlsEcs.actorRegistry.destroy(player);
        }
        SeasonRuleTableManager::Instance().InstallSnapshot(prevRuleSnap);
        SeasonSealTableManager::Instance().InstallSnapshot(prevSealSnap);
    }

    static bool CountPersist(entt::entity) {
        ++persistCalls;
        return true;
    }

    static inline uint32_t persistCalls{0};
    entt::entity player{entt::null};
    std::shared_ptr<const SeasonRuleTableSnapshot> prevRuleSnap;
    std::shared_ptr<const SeasonSealTableSnapshot> prevSealSnap;
};

} // namespace

TEST_F(PlayerSeasonSystemTest, RealExportedTablesValidateAndPopulateState) {
    ASSERT_TRUE(PlayerSeasonSystem::ValidateTables());
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
    EXPECT_EQ(PlayerSeasonSystem::ActiveSeasonId(nowMs), 1u);

    GetSeasonStateResponse resp;
    ASSERT_EQ(PlayerSeasonSystem::GetState(player, nowMs, resp), kSuccess);
    const auto& s = resp.state();
    EXPECT_TRUE(s.available());
    EXPECT_EQ(s.season_id(), 1u);
    EXPECT_EQ(s.equipped_seal_id(), 101u);
    EXPECT_EQ(s.progress_points(), 0u);
    EXPECT_EQ(s.max_points(), 12u);
    EXPECT_EQ(s.current_max_health_bps(), 0u);
    EXPECT_EQ(s.cap_max_health_bps(), 300u);
    EXPECT_EQ(s.current_defense_bps(), 0u);
    EXPECT_EQ(s.cap_defense_bps(), 300u);
    EXPECT_EQ(s.catchup_price_per_point(), 10u);
    EXPECT_EQ(s.catchup_remaining_total_price(), 120u);
    EXPECT_EQ(s.seals_size(), 4);
    EXPECT_EQ(s.season_skills_size(), 4);
    EXPECT_TRUE(s.can_switch_seal());
    EXPECT_TRUE(s.can_catchup());
}

TEST_F(PlayerSeasonSystemTest, EquipSealSwitchesFreeOutsideBattleAndRejectsInBattle) {
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
    EquipSeasonSealRequest req;
    req.set_season_id(1);
    req.set_seal_id(103);
    EquipSeasonSealResponse resp;
    PlayerSeasonSystem::EquipSeal(player, nowMs, req, resp);
    ASSERT_EQ(resp.error_message().id(), 0u);
    EXPECT_EQ(resp.state().equipped_seal_id(), 103u);

    // 重复切同一枚印拒绝
    EquipSeasonSealResponse dup;
    PlayerSeasonSystem::EquipSeal(player, nowMs, req, dup);
    EXPECT_EQ(dup.error_message().id(), static_cast<uint32_t>(kSeasonSealAlreadyEquipped));

    // 战斗中禁止切换
    auto& battle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    battle.set_battle_id(8801);
    battle.set_state(IN_BATTLE_STATE_FIGHTING);
    req.set_seal_id(104);
    EquipSeasonSealResponse inBattleResp;
    PlayerSeasonSystem::EquipSeal(player, nowMs, req, inBattleResp);
    EXPECT_EQ(inBattleResp.error_message().id(), static_cast<uint32_t>(kSeasonInBattle));
    EXPECT_FALSE(inBattleResp.state().can_switch_seal());
    EXPECT_FALSE(inBattleResp.state().can_catchup());
    tlsEcs.actorRegistry.remove<InBattleComp>(player);
}

TEST_F(PlayerSeasonSystemTest, CatchupIdempotentQuoteGuardedAndCappedAtMax) {
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();

    // 1. 报价不符拒绝且不扣灵石
    CatchupSeasonSealRequest badQuote;
    badQuote.set_client_request_id(1001);
    badQuote.set_season_id(1);
    badQuote.set_points_to_buy(2);
    badQuote.set_expected_currency_type(static_cast<uint32_t>(kCurrencyDiamond));
    badQuote.set_expected_total_price(15); // 实际应为 20
    CatchupSeasonSealResponse badResp;
    PlayerSeasonSystem::Catchup(player, nowMs, badQuote, badResp);
    EXPECT_EQ(badResp.error_message().id(), static_cast<uint32_t>(kSeasonCatchupPriceChanged));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 500u);

    // 2. 精确补 2 点成功（扣 20 灵石）
    CatchupSeasonSealRequest req1;
    req1.set_client_request_id(1002);
    req1.set_season_id(1);
    req1.set_points_to_buy(2);
    req1.set_expected_currency_type(static_cast<uint32_t>(kCurrencyDiamond));
    req1.set_expected_total_price(20);
    CatchupSeasonSealResponse resp1;
    PlayerSeasonSystem::Catchup(player, nowMs, req1, resp1);
    ASSERT_EQ(resp1.error_message().id(), 0u);
    EXPECT_FALSE(resp1.replayed());
    EXPECT_EQ(resp1.added_points(), 2u);
    EXPECT_EQ(resp1.spent_currency(), 20u);
    EXPECT_EQ(resp1.state().progress_points(), 2u);
    EXPECT_EQ(resp1.state().catchup_points(), 2u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 480u);
    EXPECT_EQ(persistCalls, 1u);

    // 3. 同幂等键重放不双扣
    CatchupSeasonSealResponse replay1;
    PlayerSeasonSystem::Catchup(player, nowMs, req1, replay1);
    ASSERT_EQ(replay1.error_message().id(), 0u);
    EXPECT_TRUE(replay1.replayed());
    EXPECT_EQ(replay1.added_points(), 2u);
    EXPECT_EQ(replay1.spent_currency(), 20u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 480u);

    // 4. 一键补满剩余 10 点（扣 100 灵石）
    CatchupSeasonSealRequest reqFill;
    reqFill.set_client_request_id(1003);
    reqFill.set_season_id(1);
    reqFill.set_points_to_buy(0);
    reqFill.set_expected_currency_type(static_cast<uint32_t>(kCurrencyDiamond));
    reqFill.set_expected_total_price(100);
    CatchupSeasonSealResponse respFill;
    PlayerSeasonSystem::Catchup(player, nowMs, reqFill, respFill);
    ASSERT_EQ(respFill.error_message().id(), 0u);
    EXPECT_EQ(respFill.added_points(), 10u);
    EXPECT_EQ(respFill.spent_currency(), 100u);
    EXPECT_EQ(respFill.state().progress_points(), 12u);
    EXPECT_EQ(respFill.state().current_max_health_bps(), 300u);
    EXPECT_EQ(respFill.state().current_defense_bps(), 300u);
    EXPECT_FALSE(respFill.state().can_catchup());
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 380u);

    // 5. 满级后再追赶拒绝（与免费严格同上限）
    CatchupSeasonSealRequest reqOver;
    reqOver.set_client_request_id(1004);
    reqOver.set_season_id(1);
    reqOver.set_points_to_buy(1);
    reqOver.set_expected_currency_type(static_cast<uint32_t>(kCurrencyDiamond));
    reqOver.set_expected_total_price(10);
    CatchupSeasonSealResponse respOver;
    PlayerSeasonSystem::Catchup(player, nowMs, reqOver, respOver);
    EXPECT_EQ(respOver.error_message().id(), static_cast<uint32_t>(kSeasonSealAlreadyMax));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 380u);
}

TEST_F(PlayerSeasonSystemTest, BattleCreditHonorsModeEligibilityAndCap) {
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
    PlayerSeasonComp current;
    PlayerSeasonComp next;
    bool increased = false;

    auto makeSettlement = [](uint32_t mode, eBattleOutcome outcome, bool fled = false, uint32_t rounds = 3) {
        BattleSettlementData s;
        s.set_battle_id(5001);
        s.set_player_id(99001);
        s.set_player_team_index(0);
        s.set_match_mode(mode);
        s.set_outcome(outcome);
        s.set_fled(fled);
        s.set_total_rounds(rounds);
        return s;
    };

    // 切磋 / 逃跑 / PVE 败局 / 0 回合均不给点
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeSettlement(match::MATCH_MODE_PVP_CHALLENGE, BATTLE_OUTCOME_SIDE_A_WIN), nowMs, next, increased), kSuccess);
    EXPECT_FALSE(increased);
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeSettlement(match::MATCH_MODE_PVE_SOLO, BATTLE_OUTCOME_SIDE_B_WIN), nowMs, next, increased), kSuccess);
    EXPECT_FALSE(increased);
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeSettlement(match::MATCH_MODE_PVP_TEAM, BATTLE_OUTCOME_SIDE_A_WIN, true), nowMs, next, increased), kSuccess);
    EXPECT_FALSE(increased);

    // PVE 胜 +1
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeSettlement(match::MATCH_MODE_PVE_SOLO, BATTLE_OUTCOME_SIDE_A_WIN), nowMs, next, increased), kSuccess);
    ASSERT_TRUE(increased);
    EXPECT_EQ(next.records(0).progress_points(), 1u);
    current = next;

    // 正规排位胜 +2
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeSettlement(match::MATCH_MODE_PVP_TEAM, BATTLE_OUTCOME_SIDE_A_WIN), nowMs, next, increased), kSuccess);
    ASSERT_TRUE(increased);
    EXPECT_EQ(next.records(0).progress_points(), 3u);
    current = next;

    // 正规排位负 +1
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeSettlement(match::MATCH_MODE_1V1, BATTLE_OUTCOME_SIDE_B_WIN), nowMs, next, increased), kSuccess);
    ASSERT_TRUE(increased);
    EXPECT_EQ(next.records(0).progress_points(), 4u);
    EXPECT_EQ(next.records(0).battle_points(), 4u);
    current = next;

    // 正式排位败局防挂机：回合不足 / 手动回合不足 / 无有效贡献均不给点；达标败局 +1，1 回合速胜 +2
    auto makeRanked = [&](eBattleOutcome outcome, uint32_t rounds, uint32_t manualRounds, bool contributed) {
        auto s = makeSettlement(match::MATCH_MODE_PVP_TEAM, outcome, false, rounds);
        s.set_competitive_ranked(true);
        s.set_participated(true);
        s.set_manual_action_rounds(manualRounds);
        s.set_effective_combat_contribution(contributed);
        return s;
    };
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeRanked(BATTLE_OUTCOME_SIDE_B_WIN, 2, 2, true), nowMs, next, increased), kSuccess);
    EXPECT_FALSE(increased);
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeRanked(BATTLE_OUTCOME_SIDE_B_WIN, 3, 1, true), nowMs, next, increased), kSuccess);
    EXPECT_FALSE(increased);
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeRanked(BATTLE_OUTCOME_SIDE_B_WIN, 3, 2, false), nowMs, next, increased), kSuccess);
    EXPECT_FALSE(increased);

    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeRanked(BATTLE_OUTCOME_SIDE_B_WIN, 3, 2, true), nowMs, next, increased), kSuccess);
    ASSERT_TRUE(increased);
    EXPECT_EQ(next.records(0).progress_points(), 5u);
    current = next;

    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(
        current, makeRanked(BATTLE_OUTCOME_SIDE_A_WIN, 1, 1, true), nowMs, next, increased), kSuccess);
    ASSERT_TRUE(increased);
    EXPECT_EQ(next.records(0).progress_points(), 7u);
}

TEST_F(PlayerSeasonSystemTest, LightStatsAffectHealthAndDefenseOnlyNeverSpeedOrCrit) {
    const auto& derivedBefore = tlsEcs.actorRegistry.get<DerivedAttributesComp>(player);
    const uint64_t hpBefore = derivedBefore.max_health();
    const uint64_t defBefore = derivedBefore.defense();
    const uint64_t spdBefore = derivedBefore.speed();
    const uint64_t patkBefore = derivedBefore.physical_attack();
    const uint64_t matkBefore = derivedBefore.magic_attack();

    // 补满 12 点 -> +3% 气血、+3% 防御
    CatchupSeasonSealRequest req;
    req.set_client_request_id(2001);
    req.set_season_id(1);
    req.set_points_to_buy(0);
    req.set_expected_currency_type(static_cast<uint32_t>(kCurrencyDiamond));
    req.set_expected_total_price(120);
    CatchupSeasonSealResponse resp;
    PlayerSeasonSystem::Catchup(player, TimeSystem::NowMillisecondsUTC(), req, resp);
    ASSERT_EQ(resp.error_message().id(), 0u);

    const auto& derivedAfter = tlsEcs.actorRegistry.get<DerivedAttributesComp>(player);
    EXPECT_GT(derivedAfter.max_health(), hpBefore);
    EXPECT_GT(derivedAfter.defense(), defBefore);
    EXPECT_EQ(derivedAfter.speed(), spdBefore) << "赛季印严禁加速度";
    EXPECT_EQ(derivedAfter.physical_attack(), patkBefore);
    EXPECT_EQ(derivedAfter.magic_attack(), matkBefore);

    EquipmentEffects effects;
    PlayerEquipmentV1System::CollectEffects(player, effects);
    EXPECT_EQ(effects.season_max_health_bps(), 300u);
    EXPECT_EQ(effects.season_defense_bps(), 300u);
    ASSERT_TRUE(effects.has_season_seal());
    EXPECT_EQ(effects.season_seal().season_id(), 1u);
    EXPECT_EQ(effects.season_seal().seal_id(), 101u);
}

TEST_F(PlayerSeasonSystemTest, SeasonalSkillsFreeInBookAndSnapshotAndVanishWhenSeasonSwitches) {
    // 1. 当季技能书包含 15001..15004，自动生效且不扣银两修习
    ListSkillsResponse book;
    ASSERT_EQ(PlayerSkillSystem::BuildSkillBook(player, book), kSuccess);
    int seasonalCount = 0;
    for (const auto& entry : book.catalog()) {
        if (entry.season_id() == 1) {
            ++seasonalCount;
            EXPECT_GT(entry.rank(), 0u);
            EXPECT_EQ(entry.next_rank_cost(), 0u);
            EXPECT_EQ(entry.upgrade_block_tip_id(), static_cast<uint32_t>(kSeasonSkillNoUpgrade));
        }
    }
    EXPECT_EQ(seasonalCount, 4);
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, 15001));

    // 修习赛季技能被拒绝且银两不变
    const uint64_t goldBefore = CurrencySystem::GetBalance(player, kCurrencyGold);
    UpgradeSkillRequest upReq;
    upReq.set_skill_table_id(15001);
    upReq.set_expected_rank(0);
    upReq.set_count(1);
    UpgradeSkillResponse upResp;
    PlayerSkillSystem::UpgradeSkill(player, upReq, upResp);
    EXPECT_EQ(upResp.error_message().id(), static_cast<uint32_t>(kSeasonSkillNoUpgrade));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), goldBefore);

    // 战斗快照带入 active_season_id = 1 与 15001
    BattlePlayerSnapshot snap;
    PlayerSkillSystem::FillBattleSnapshot(player, snap);
    EXPECT_EQ(snap.active_season_id(), 1u);
    EXPECT_TRUE(snap.skill_ranks().contains(15001));

    // 2. 切到赛季 2（或关闭赛季 1）：赛季 1 技能与赛季 1 印自动退场
    SeasonRuleTable s2;
    s2.set_id(2);
    s2.set_name("第二季");
    s2.set_enabled(true);
    s2.set_seal_max_points(12);
    s2.set_default_seal_id(201);
    SeasonSealTable seal2;
    seal2.set_id(201);
    seal2.set_season_id(2);
    seal2.set_name("二季印");
    seal2.set_effect_id(1);
    seal2.set_bonus_bps(1000);
    seal2.set_per_round_limit(1);
    SeasonRuleTableManager::Instance().InstallSnapshot(MakeRuleSnapshot({s2}));
    SeasonSealTableManager::Instance().InstallSnapshot(MakeSealSnapshot({seal2}));

    EXPECT_FALSE(PlayerSkillSystem::HasSkill(player, 15001)) << "换季后旧赛季技能自动退场";
    BattlePlayerSnapshot snapS2;
    PlayerSkillSystem::FillBattleSnapshot(player, snapS2);
    EXPECT_EQ(snapS2.active_season_id(), 2u);
    EXPECT_FALSE(snapS2.skill_ranks().contains(15001));

    EquipmentEffects effectsS2;
    PlayerEquipmentV1System::CollectEffects(player, effectsS2);
    EXPECT_EQ(effectsS2.season_max_health_bps(), 0u) << "新赛季旧进度归零不残留";
    EXPECT_EQ(effectsS2.season_seal().seal_id(), 201u);
}

TEST_F(PlayerSeasonSystemTest, WuxingFormationTowerRunSigilPickLeaderboardAndBuildBookmarks) {
    PlayerSeasonSystem::ResetTowerLeaderboardForTest();
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();

    // 1. 免费切换 5 套五行阵法，越界 6 被拒绝
    EquipWuxingFormationRequest badForm;
    badForm.set_formation_id(6);
    EquipWuxingFormationResponse badFormResp;
    PlayerSeasonSystem::EquipFormation(player, nowMs, badForm, badFormResp);
    EXPECT_NE(badFormResp.error_message().id(), 0u);

    EquipWuxingFormationRequest okForm;
    okForm.set_formation_id(3); // 风扬阵
    EquipWuxingFormationResponse okFormResp;
    PlayerSeasonSystem::EquipFormation(player, nowMs, okForm, okFormResp);
    ASSERT_EQ(okFormResp.error_message().id(), 0u);
    EXPECT_EQ(okFormResp.equipped_formation_id(), 3u);
    EXPECT_EQ(PlayerSeasonSystem::EquippedFormationId(player), 3u);

    // 2. 五行幻境开轮 -> 校验层上下文 -> 通关 1 层 -> 3 选 1 符印 -> 推进至 2 层 -> 榜更新 -> 放弃保留本周首通位图
    StartWuxingTowerRunResponse startResp;
    PlayerSeasonSystem::StartWuxingTowerRun(player, nowMs, startResp);
    ASSERT_EQ(startResp.error_message().id(), 0u);
    ASSERT_TRUE(startResp.state().run_active());
    EXPECT_EQ(startResp.state().current_floor(), 1u);
    EXPECT_EQ(startResp.state().current_floor_dungeon_id(), 101u);

    WuxingTowerBattleContext towerCtx;
    EXPECT_NE(PlayerSeasonSystem::PrepareWuxingTowerContext(player, nowMs, 999u, towerCtx), kSuccess);
    ASSERT_EQ(PlayerSeasonSystem::PrepareWuxingTowerContext(player, nowMs, 101u, towerCtx), kSuccess);
    EXPECT_EQ(towerCtx.floor(), 1u);

    // 模拟 1 层胜利结算推进
    auto& seasonComp = tlsEcs.actorRegistry.get<PlayerSeasonComp>(player);
    BattleSettlementData settle;
    settle.set_battle_id(8801);
    settle.set_player_id(99001);
    settle.set_player_team_index(0);
    settle.set_match_mode(match::MATCH_MODE_PVE_SOLO);
    settle.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
    settle.set_total_rounds(4);
    auto* ts = settle.mutable_tower_settlement();
    ts->set_week_key(towerCtx.week_key());
    ts->set_run_id(towerCtx.run_id());
    ts->set_floor(1);
    ts->set_total_rounds(4);

    PlayerSeasonComp candidate;
    bool pointsIncreased = false;
    bool towerChanged = false;
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(seasonComp, settle, nowMs, candidate, pointsIncreased, &towerChanged), kSuccess);
    ASSERT_TRUE(towerChanged);
    EXPECT_FALSE(pointsIncreased);
    seasonComp.Swap(&candidate);
    PlayerSeasonSystem::OnTowerSettlementApplied(player, seasonComp, nowMs);

    GetWuxingTowerStateResponse stateAfterF1;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nowMs, stateAfterF1), kSuccess);
    EXPECT_EQ(stateAfterF1.state().weekly_cleared_mask(), 1u);
    EXPECT_EQ(stateAfterF1.state().weekly_best_floor(), 1u);
    ASSERT_EQ(stateAfterF1.state().pending_offer_sigil_ids_size(), 3);
    EXPECT_GE(stateAfterF1.state().weekly_leaderboard_size(), 1);
    // 有未选符印时禁止直接开下一层
    EXPECT_NE(PlayerSeasonSystem::PrepareWuxingTowerContext(player, nowMs, 101u, towerCtx), kSuccess);

    // 选第 1 枚候选符印 → 推进到第 2 层
    PickWuxingTowerSigilRequest pickReq;
    pickReq.set_expected_run_id(stateAfterF1.state().run_id());
    pickReq.set_sigil_id(stateAfterF1.state().pending_offer_sigil_ids(0));
    PickWuxingTowerSigilResponse pickResp;
    PlayerSeasonSystem::PickWuxingTowerSigil(player, nowMs, pickReq, pickResp);
    ASSERT_EQ(pickResp.error_message().id(), 0u);
    EXPECT_EQ(pickResp.state().current_floor(), 2u);
    EXPECT_EQ(pickResp.state().pending_offer_sigil_ids_size(), 0);
    ASSERT_EQ(pickResp.state().active_sigil_ids_size(), 1);
    EXPECT_EQ(pickResp.state().active_sigil_ids(0), pickReq.sigil_id());

    // 放弃本轮：清局内符印，保留本周首通位图
    AbandonWuxingTowerRunRequest abReq;
    abReq.set_expected_run_id(pickResp.state().run_id());
    AbandonWuxingTowerRunResponse abResp;
    PlayerSeasonSystem::AbandonWuxingTowerRun(player, nowMs, abReq, abResp);
    ASSERT_EQ(abResp.error_message().id(), 0u);
    EXPECT_FALSE(abResp.state().run_active());
    EXPECT_EQ(abResp.state().active_sigil_ids_size(), 0);
    EXPECT_EQ(abResp.state().weekly_cleared_mask(), 1u);

    // 3. 配装收藏夹 CRUD + 12 条上限约束
    for (int i = 1; i <= 12; ++i) {
        SaveBattleBuildBookmarkRequest bmReq;
        bmReq.mutable_bookmark()->set_title("配装" + std::to_string(i));
        bmReq.mutable_bookmark()->set_formation_id(static_cast<uint32_t>((i % 5) + 1));
        bmReq.mutable_bookmark()->set_season_seal_id(101);
        SaveBattleBuildBookmarkResponse bmResp;
        PlayerSeasonSystem::SaveBattleBuildBookmark(player, nowMs + static_cast<uint64_t>(i), bmReq, bmResp);
        ASSERT_EQ(bmResp.error_message().id(), 0u);
        EXPECT_EQ(bmResp.all_bookmarks_size(), i);
    }
    // 第 13 条必须拒绝
    SaveBattleBuildBookmarkRequest overReq;
    overReq.mutable_bookmark()->set_title("溢出");
    SaveBattleBuildBookmarkResponse overResp;
    PlayerSeasonSystem::SaveBattleBuildBookmark(player, nowMs + 99, overReq, overResp);
    EXPECT_NE(overResp.error_message().id(), 0u);

    // 删除第 1 条后剩余 11 条
    DeleteBattleBuildBookmarkRequest delReq;
    delReq.set_bookmark_id(1);
    DeleteBattleBuildBookmarkResponse delResp;
    PlayerSeasonSystem::DeleteBattleBuildBookmark(player, nowMs + 100, delReq, delResp);
    ASSERT_EQ(delResp.error_message().id(), 0u);
    EXPECT_EQ(delResp.all_bookmarks_size(), 11);
}

TEST_F(PlayerSeasonSystemTest, WeeklyAffixSettlementDeltaS2TemplateAndPermanentZeroStatHonor) {
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
    // 1. 导出表已含 S1(启用) + S2 玄脉之季(预埋待启) 且通过启动验表
    ASSERT_TRUE(PlayerSeasonSystem::ValidateTables());
    EXPECT_EQ(PlayerSeasonSystem::ActiveSeasonId(nowMs), 1u);
    const uint32_t activeAffix = PlayerSeasonSystem::ActiveWeeklyAffixId(nowMs);
    EXPECT_TRUE(seasonrules::IsValidWeeklyAffixId(activeAffix));

    GetSeasonStateResponse initResp;
    ASSERT_EQ(PlayerSeasonSystem::GetState(player, nowMs, initResp), kSuccess);
    EXPECT_EQ(initResp.state().weekly_affix().affix_id(), activeAffix);
    EXPECT_FALSE(initResp.state().weekly_affix().name().empty());
    EXPECT_GT(initResp.state().weekly_affix().recommended_seal_ids_size(), 0);
    ASSERT_EQ(initResp.state().honors_size(), 10);
    EXPECT_EQ(initResp.state().honors(0).season_id(), 1u);
    EXPECT_EQ(initResp.state().honors(0).honor_title_name(), "破阵宗师");
    EXPECT_FALSE(initResp.state().honors(0).unlocked());
    EXPECT_EQ(initResp.state().retro_honor_target_season_id(), 2u);
    EXPECT_FALSE(initResp.state().retro_honor_target_explicit());

    // 2. 战斗结算增量精确回显 + 跨越 11->12 仅首次触发 honorNewlyUnlocked
    auto& comp = tlsEcs.actorRegistry.get<PlayerSeasonComp>(player);
    auto* rec = comp.add_records();
    rec->set_season_id(1);
    rec->set_progress_points(11);
    rec->set_battle_points(11);

    BattleSettlementData pveWin;
    pveWin.set_battle_id(70001);
    pveWin.set_player_id(10001);
    pveWin.set_player_team_index(0);
    pveWin.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
    pveWin.set_match_mode(match::MATCH_MODE_PVE_SOLO);
    pveWin.set_total_rounds(3);

    PlayerSeasonComp cand1;
    bool inc1 = false;
    seasonrules::BattleCreditDelta delta1;
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(comp, pveWin, nowMs, cand1, inc1, nullptr, &delta1), kSuccess);
    EXPECT_TRUE(inc1);
    EXPECT_EQ(delta1.seasonId, 1u);
    EXPECT_EQ(delta1.pointsAdded, 1u);
    EXPECT_EQ(delta1.progressAfter, 12u);
    EXPECT_EQ(delta1.maxPoints, 12u);
    EXPECT_TRUE(delta1.honorNewlyUnlocked);
    EXPECT_EQ(delta1.retroHonorSeasonId, 0u);
    comp.Swap(&cand1);

    // 满级后继续打合格局：当季 pointsAdded == 0，溢出 +1 自动流向首个未解锁往季(S2)追忆
    PlayerSeasonComp cand2;
    bool inc2 = false;
    seasonrules::BattleCreditDelta delta2;
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(comp, pveWin, nowMs, cand2, inc2, nullptr, &delta2), kSuccess);
    EXPECT_TRUE(inc2);
    EXPECT_EQ(delta2.pointsAdded, 0u);
    EXPECT_EQ(delta2.progressAfter, 12u);
    EXPECT_EQ(delta2.maxPoints, 12u);
    EXPECT_FALSE(delta2.honorNewlyUnlocked);
    EXPECT_EQ(delta2.retroHonorSeasonId, 2u);
    EXPECT_EQ(delta2.retroHonorPointsAdded, 1u);
    EXPECT_EQ(delta2.retroHonorProgressAfter, 1u);
    EXPECT_EQ(delta2.retroHonorMaxPoints, 12u);
    EXPECT_FALSE(delta2.retroHonorNewlyUnlocked);

    // 3. 切到预埋的 S2「玄脉之季」：S2 技能 15101..15104 与印 201..204 自动生效，轻属性归 0，S1 纪念荣誉永久留存
    std::vector<SeasonRuleTable> switchedRules;
    for (auto row : SeasonRuleTableManager::Instance().FindAll().data()) {
        row.set_enabled(row.id() == 2u);
        switchedRules.push_back(std::move(row));
    }
    SeasonRuleTableManager::Instance().InstallSnapshot(MakeRuleSnapshot(switchedRules));
    ASSERT_TRUE(PlayerSeasonSystem::ValidateTables());
    EXPECT_EQ(PlayerSeasonSystem::ActiveSeasonId(nowMs), 2u);
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, 15101));
    EXPECT_FALSE(PlayerSkillSystem::HasSkill(player, 15001));

    GetSeasonStateResponse s2Resp;
    ASSERT_EQ(PlayerSeasonSystem::GetState(player, nowMs, s2Resp), kSuccess);
    EXPECT_TRUE(s2Resp.state().available());
    EXPECT_EQ(s2Resp.state().season_id(), 2u);
    EXPECT_EQ(s2Resp.state().season_name(), "玄脉之季");
    EXPECT_EQ(s2Resp.state().equipped_seal_id(), 201u);
    EXPECT_EQ(s2Resp.state().progress_points(), 0u);
    EXPECT_EQ(s2Resp.state().current_max_health_bps(), 0u);
    EXPECT_EQ(s2Resp.state().seals_size(), 4);
    EXPECT_EQ(s2Resp.state().season_skills_size(), 4);
    ASSERT_EQ(s2Resp.state().honors_size(), 10);
    EXPECT_EQ(s2Resp.state().honors(0).season_id(), 1u);
    EXPECT_TRUE(s2Resp.state().honors(0).unlocked()) << "S1 满级零数值纪念荣誉跨季永久留存";
    EXPECT_FALSE(s2Resp.state().honors(0).is_current_season());
    EXPECT_EQ(s2Resp.state().honors(1).season_id(), 2u);
    EXPECT_FALSE(s2Resp.state().honors(1).unlocked());
    EXPECT_TRUE(s2Resp.state().honors(1).is_current_season());

    // 4. 休赛期（全部关闭）仍展示全部十季荣誉墙与已解锁历史永久纪念荣誉
    for (auto& row : switchedRules) {
        row.set_enabled(false);
    }
    SeasonRuleTableManager::Instance().InstallSnapshot(MakeRuleSnapshot(switchedRules));
    GetSeasonStateResponse offResp;
    ASSERT_EQ(PlayerSeasonSystem::GetState(player, nowMs, offResp), kSuccess);
    EXPECT_FALSE(offResp.state().available());
    ASSERT_EQ(offResp.state().honors_size(), 10);
    EXPECT_EQ(offResp.state().honors(0).season_id(), 1u);
    EXPECT_TRUE(offResp.state().honors(0).unlocked());
}

TEST(SeasonRulesTest, InfiniteCycleResolutionBitmapAndResonanceBounds) {
    constexpr uint64_t kAnchorMs = 1800000000000ULL;
    constexpr uint64_t kPeriodMs = 28ULL * seasonrules::kMsPerDay;
    std::vector<seasonrules::RuleParams> pool;
    for (uint32_t s = 1; s <= 10; ++s) {
        auto r = GoodRuleParams();
        r.seasonId = s;
        r.startsAtMs = (s == 1u) ? kAnchorMs : 0u;
        r.endsAtMs = 0u;
        r.enabled = true;
        r.defaultSealId = s * 100u + 1u;
        r.cycleOrder = s;
        r.cycleDurationDays = 28u;
        pool.push_back(r);
    }
    EXPECT_EQ(seasonrules::ValidateSchedule(pool), nullptr);

    // 未到锚点不开启
    const auto before = seasonrules::ResolveActiveSeason(pool, kAnchorMs - 1u);
    EXPECT_FALSE(before.active);
    EXPECT_FALSE(before.invalidConfig);

    // 跨 22 届（超过 2 轮 10 季）精确轮换
    for (uint32_t term = 1; term <= 22; ++term) {
        const uint64_t t = kAnchorMs + static_cast<uint64_t>(term - 1u) * kPeriodMs + 1000ULL;
        const auto res = seasonrules::ResolveActiveSeason(pool, t);
        ASSERT_TRUE(res.active);
        EXPECT_FALSE(res.invalidConfig);
        EXPECT_EQ(res.seasonTerm, term);
        EXPECT_EQ(res.seasonId, ((term - 1u) % 10u) + 1u);
        EXPECT_EQ(res.cycleIndex, ((term - 1u) / 10u) + 1u);
        EXPECT_EQ(res.cyclePoolSize, 10u);
        EXPECT_EQ(res.termStartMs, kAnchorMs + static_cast<uint64_t>(term - 1u) * kPeriodMs);
        EXPECT_EQ(res.termEndMs, res.termStartMs + kPeriodMs);
    }

    // fail-closed：缺号 / 天数不一致 / 锚点为 0 均拒载
    auto gapPool = pool;
    gapPool[9].cycleOrder = 11;
    EXPECT_NE(seasonrules::ValidateSchedule(gapPool), nullptr);
    EXPECT_TRUE(seasonrules::ResolveActiveSeason(gapPool, kAnchorMs).invalidConfig);

    auto mismatchDays = pool;
    mismatchDays[3].cycleDurationDays = 14;
    EXPECT_NE(seasonrules::ValidateSchedule(mismatchDays), nullptr);

    auto zeroAnchor = pool;
    zeroAnchor[0].startsAtMs = 0;
    EXPECT_NE(seasonrules::ValidateSchedule(zeroAnchor), nullptr);

    // 动态位图支持 65+ 季扩容
    std::vector<uint64_t> words;
    EXPECT_TRUE(seasonrules::SetHonorBitVec(words, 1));
    EXPECT_FALSE(seasonrules::SetHonorBitVec(words, 1));
    EXPECT_TRUE(seasonrules::SetHonorBitVec(words, 64));
    EXPECT_TRUE(seasonrules::SetHonorBitVec(words, 65));
    EXPECT_TRUE(seasonrules::SetHonorBitVec(words, 130));
    EXPECT_EQ(words.size(), 3u);
    EXPECT_TRUE(seasonrules::IsHonorBitSet(words, 1));
    EXPECT_TRUE(seasonrules::IsHonorBitSet(words, 64));
    EXPECT_TRUE(seasonrules::IsHonorBitSet(words, 65));
    EXPECT_TRUE(seasonrules::IsHonorBitSet(words, 130));
    EXPECT_FALSE(seasonrules::IsHonorBitSet(words, 2));

    // 赛季印禁控命(3)/控抗(8) 与共鸣硬上限
    EXPECT_FALSE(seasonrules::ValidSealEffect(1, 101, 3, 500, 0, 1, 1, 0, 0));
    EXPECT_FALSE(seasonrules::ValidSealEffect(1, 101, 8, 500, 0, 1, 1, 0, 0));
    EXPECT_TRUE(seasonrules::ValidSealResonance(1, 15001, 1, 40));
    EXPECT_FALSE(seasonrules::ValidSealResonance(1, 15001, 1, 41));
    EXPECT_TRUE(seasonrules::ValidSealResonance(4, 15104, 5, 35));
    EXPECT_FALSE(seasonrules::ValidSealResonance(4, 15104, 5, 36));
    EXPECT_TRUE(seasonrules::ValidSealResonance(5, 15103, 6, 300));
    EXPECT_FALSE(seasonrules::ValidSealResonance(5, 15103, 6, 301));
    EXPECT_TRUE(seasonrules::ValidSealResonance(1, 15001, 0, 999)) << "resonance_kind=0 emergency disable";
}

TEST_F(PlayerSeasonSystemTest, InfiniteCycleTermResetRetroHonorCatchupAndRpc) {
    constexpr uint64_t kAnchorMs = 1800000000000ULL;
    constexpr uint64_t kPeriodMs = 28ULL * seasonrules::kMsPerDay;
    std::vector<SeasonRuleTable> cycleRules;
    for (auto row : SeasonRuleTableManager::Instance().FindAll().data()) {
        row.set_enabled(true);
        row.set_starts_at_ms(row.id() == 1u ? kAnchorMs : 0u);
        row.set_ends_at_ms(0u);
        row.set_cycle_order(row.id());
        row.set_cycle_duration_days(28u);
        cycleRules.push_back(std::move(row));
    }
    ASSERT_EQ(cycleRules.size(), 10u);
    SeasonRuleTableManager::Instance().InstallSnapshot(MakeRuleSnapshot(cycleRules));
    ASSERT_TRUE(PlayerSeasonSystem::ValidateTables());

    // Term 1 (S1): 玩家满 12/12 解锁 S1 永久徽章
    const uint64_t term1Now = kAnchorMs + 3600000ULL;
    auto& comp = tlsEcs.actorRegistry.get<PlayerSeasonComp>(player);
    BattleSettlementData pvpWin;
    pvpWin.set_battle_id(91001);
    pvpWin.set_player_id(10001);
    pvpWin.set_player_team_index(0);
    pvpWin.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
    pvpWin.set_match_mode(match::MATCH_MODE_1V1);
    pvpWin.set_competitive_ranked(true);
    pvpWin.set_participated(true);
    pvpWin.set_total_rounds(4);

    for (int i = 0; i < 6; ++i) {
        PlayerSeasonComp cand;
        bool inc = false;
        seasonrules::BattleCreditDelta delta;
        ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(comp, pvpWin, term1Now, cand, inc, nullptr, &delta), kSuccess);
        ASSERT_TRUE(inc);
        EXPECT_EQ(delta.pointsAdded, 2u);
        EXPECT_EQ(delta.honorNewlyUnlocked, i == 5);
        comp.Swap(&cand);
    }
    EXPECT_TRUE(seasonrules::IsHonorBitSet(comp.unlocked_honor_words(), 1u));

    // SetRetroHonorTarget RPC(569): 拒绝已解锁(S1)、拒绝当前活跃季，成功切到 S7(庚金之季)
    {
        SetRetroHonorTargetRequest badReq;
        badReq.set_season_id(1u);
        SetRetroHonorTargetResponse badResp;
        PlayerSeasonSystem::SetRetroHonorTarget(player, term1Now, badReq, badResp);
        EXPECT_EQ(badResp.error_message().id(), static_cast<uint32_t>(kSeasonHonorTargetInvalid));

        SetRetroHonorTargetRequest s7Req;
        s7Req.set_season_id(7u);
        SetRetroHonorTargetResponse s7Resp;
        PlayerSeasonSystem::SetRetroHonorTarget(player, term1Now, s7Req, s7Resp);
        ASSERT_EQ(s7Resp.error_message().id(), 0u);
        EXPECT_EQ(s7Resp.state().retro_honor_target_season_id(), 7u);
        EXPECT_TRUE(s7Resp.state().retro_honor_target_explicit());
        EXPECT_TRUE(s7Resp.state().honors(6).is_retro_target());
    }

    // 在满级状态下再打 6 场排位胜(+2*6=12) -> 溢出全部流向 S7，第 6 场永久解锁 S7 徽章并自动顺延回 S2
    for (int i = 0; i < 6; ++i) {
        PlayerSeasonComp cand;
        bool inc = false;
        seasonrules::BattleCreditDelta delta;
        ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(comp, pvpWin, term1Now, cand, inc, nullptr, &delta), kSuccess);
        ASSERT_TRUE(inc);
        EXPECT_EQ(delta.pointsAdded, 0u);
        EXPECT_EQ(delta.retroHonorSeasonId, 7u);
        EXPECT_EQ(delta.retroHonorPointsAdded, 2u);
        EXPECT_EQ(delta.retroHonorProgressAfter, static_cast<uint32_t>((i + 1) * 2));
        EXPECT_EQ(delta.retroHonorNewlyUnlocked, i == 5);
        comp.Swap(&cand);
    }
    EXPECT_TRUE(seasonrules::IsHonorBitSet(comp.unlocked_honor_words(), 7u));
    EXPECT_EQ(comp.retro_honor_progress().count(7u), 0u);
    EXPECT_EQ(comp.retro_honor_target_season_id(), 0u);

    // 尝试再将已解锁的 S7 设为追忆目标 -> 返回 kSeasonHonorAlreadyUnlocked
    {
        SetRetroHonorTargetRequest dupReq;
        dupReq.set_season_id(7u);
        SetRetroHonorTargetResponse dupResp;
        PlayerSeasonSystem::SetRetroHonorTarget(player, term1Now, dupReq, dupResp);
        EXPECT_EQ(dupResp.error_message().id(), static_cast<uint32_t>(kSeasonHonorAlreadyUnlocked));
    }

    // 跳转到第 11 届（第 2 轮 S1）：轻进度重置为 0/12，S1 与 S7 永久徽章永不收回
    const uint64_t term11Now = kAnchorMs + 10ULL * kPeriodMs + 3600000ULL;
    GetSeasonStateResponse t11Resp;
    ASSERT_EQ(PlayerSeasonSystem::GetState(player, term11Now, t11Resp), kSuccess);
    EXPECT_EQ(t11Resp.state().season_id(), 1u);
    EXPECT_EQ(t11Resp.state().season_term(), 11u);
    EXPECT_EQ(t11Resp.state().cycle_index(), 2u);
    EXPECT_EQ(t11Resp.state().cycle_pool_size(), 10u);
    EXPECT_EQ(t11Resp.state().progress_points(), 0u);
    EXPECT_EQ(t11Resp.state().current_max_health_bps(), 0u);
    EXPECT_TRUE(t11Resp.state().honors(0).unlocked()) << "S1 永久徽章跨轮不收回";
    EXPECT_TRUE(t11Resp.state().honors(6).unlocked()) << "S7 追忆徽章跨轮不收回";
    EXPECT_EQ(t11Resp.state().retro_honor_target_season_id(), 2u) << "自动顺延首个未解锁往季 S2";

    // 第 11 届灵石追赶补满当届 12 点：绝不溢出至往季追忆
    ASSERT_EQ(CurrencySystem::AddCurrency(player, kCurrencyDiamond, 500), kSuccess);
    CatchupSeasonSealRequest catchReq;
    catchReq.set_client_request_id(880011);
    catchReq.set_season_id(1u);
    catchReq.set_points_to_buy(0u);
    catchReq.set_expected_currency_type(1u);
    catchReq.set_expected_total_price(120u);
    CatchupSeasonSealResponse catchResp;
    PlayerSeasonSystem::Catchup(player, term11Now, catchReq, catchResp);
    ASSERT_EQ(catchResp.error_message().id(), 0u);
    EXPECT_EQ(catchResp.added_points(), 12u);
    EXPECT_EQ(catchResp.state().progress_points(), 12u);
    EXPECT_EQ(comp.retro_honor_progress().size(), 0u) << "灵石追赶绝不允许给往季追忆加点";
}

TEST_F(PlayerSeasonSystemTest, WuxingRealmV2SixNodeBranchingEventsFairPresetCrossWeekAndRedisKeys) {
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
    constexpr uint64_t kOneWeekMs = 7ULL * 24ULL * 3600ULL * 1000ULL;

    // 验证水平扩展周榜 Redis Cluster 键带同一 {weekKey} hash-tag 且本人/公平双榜隔离
    EXPECT_EQ(PlayerSeasonSystem::RealmLeaderboardZsetKey(202648u, false), "wuxing:realm:lb:{202648}:self");
    EXPECT_EQ(PlayerSeasonSystem::RealmLeaderboardMetaKey(202648u, false), "wuxing:realm:lb_meta:{202648}:self");
    EXPECT_EQ(PlayerSeasonSystem::RealmLeaderboardZsetKey(202648u, true), "wuxing:realm:lb:{202648}:preset");
    EXPECT_EQ(PlayerSeasonSystem::RealmLeaderboardMetaKey(202648u, true), "wuxing:realm:lb_meta:{202648}:preset");

    // 1. 公平试炼模式非法预设必须拒收
    {
        StartWuxingTowerRunRequest badReq;
        badReq.set_use_free_preset(true);
        badReq.set_character_preset_id(999u);
        StartWuxingTowerRunResponse badResp;
        PlayerSeasonSystem::StartWuxingTowerRun(player, nowMs, badReq, badResp);
        EXPECT_NE(badResp.error_message().id(), 0u);
    }

    // 2. 开启公平 3x3 试炼 6 关 V2 轮次（人物 102 + 灵宠 202）
    StartWuxingTowerRunRequest startReq;
    startReq.set_use_free_preset(true);
    startReq.set_character_preset_id(102u);
    startReq.set_pet_preset_id(202u);
    StartWuxingTowerRunResponse startResp;
    PlayerSeasonSystem::StartWuxingTowerRun(player, nowMs, startReq, startResp);
    ASSERT_EQ(startResp.error_message().id(), 0u);
    ASSERT_TRUE(startResp.state().run_active());
    EXPECT_EQ(startResp.state().realm_version(), 2u);
    EXPECT_EQ(startResp.state().max_floors(), 6u);
    EXPECT_TRUE(startResp.state().use_free_preset());
    EXPECT_EQ(startResp.state().character_preset_id(), 102u);
    EXPECT_EQ(startResp.state().pet_preset_id(), 202u);
    EXPECT_EQ(startResp.state().current_node_kind(), turnbattle::wuxing::kRealmNodeNormal);

    auto& seasonComp = tlsEcs.actorRegistry.get<PlayerSeasonComp>(player);
    auto winNode = [&](uint32_t expectedFloor, uint32_t expectedDungeon, uint64_t stampMs, uint32_t rounds) {
        WuxingTowerBattleContext ctx;
        uint32_t charId = 0u, petId = 0u;
        ASSERT_EQ(PlayerSeasonSystem::PrepareWuxingTowerContext(player, stampMs, expectedDungeon, ctx, &charId, &petId), kSuccess);
        EXPECT_EQ(ctx.floor(), expectedFloor);
        EXPECT_EQ(ctx.realm_version(), 2u);
        EXPECT_TRUE(ctx.use_free_preset());
        EXPECT_EQ(charId, 102u);
        EXPECT_EQ(petId, 202u);

        BattleSettlementData settle;
        settle.set_battle_id(92000u + expectedFloor);
        settle.set_player_id(99001u);
        settle.set_player_team_index(0u);
        settle.set_match_mode(match::MATCH_MODE_PVE_SOLO);
        settle.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
        settle.set_total_rounds(rounds);
        auto* ts = settle.mutable_tower_settlement();
        ts->set_week_key(ctx.week_key());
        ts->set_run_id(ctx.run_id());
        ts->set_floor(expectedFloor);
        ts->set_total_rounds(rounds);
        ts->set_realm_version(2u);
        ts->set_node_kind(ctx.node_kind());
        ts->set_use_free_preset(true);

        PlayerSeasonComp candidate;
        bool pts = false;
        bool towerChanged = false;
        ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(seasonComp, settle, stampMs, candidate, pts, &towerChanged), kSuccess);
        ASSERT_TRUE(towerChanged);
        seasonComp.Swap(&candidate);
        PlayerSeasonSystem::OnTowerSettlementApplied(player, seasonComp, stampMs);
    };

    // 第 1 关普通战胜利 -> 三选一奇策 -> 推进至第 2 关（待选分岔）
    winNode(1u, 101u, nowMs, 3u);
    GetWuxingTowerStateResponse st1;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nowMs, st1), kSuccess);
    ASSERT_EQ(st1.state().pending_offer_sigil_ids_size(), 3);
    EXPECT_TRUE(turnbattle::wuxing::IsValidRealmBoonId(st1.state().pending_offer_sigil_ids(0)));
    EXPECT_EQ(st1.state().weekly_leaderboard_size(), 0) << "公平模式严禁污染本人流派周榜";
    EXPECT_EQ(st1.state().weekly_leaderboard_preset_size(), 1) << "公平模式写入独立公平周榜";

    PickWuxingTowerSigilRequest pick1;
    pick1.set_expected_run_id(st1.state().run_id());
    pick1.set_sigil_id(st1.state().pending_offer_sigil_ids(0));
    PickWuxingTowerSigilResponse pick1Resp;
    PlayerSeasonSystem::PickWuxingTowerSigil(player, nowMs, pick1, pick1Resp);
    ASSERT_EQ(pick1Resp.error_message().id(), 0u);
    EXPECT_EQ(pick1Resp.state().current_floor(), 2u);
    EXPECT_EQ(pick1Resp.state().current_node_kind(), turnbattle::wuxing::kRealmNodeNeedBranch);

    // 未选分岔前禁止直接开战
    WuxingTowerBattleContext blockedCtx;
    EXPECT_NE(PlayerSeasonSystem::PrepareWuxingTowerContext(player, nowMs, 103u, blockedCtx), kSuccess);

    // 第 2 关选右路「奇遇事件」-> 道缘寻策直接获 1 枚灵品奇策并推进至第 3 关
    ChooseWuxingRealmBranchRequest br2;
    br2.set_expected_run_id(st1.state().run_id());
    br2.set_branch(turnbattle::wuxing::kRealmBranchSpecial);
    ChooseWuxingRealmBranchResponse br2Resp;
    PlayerSeasonSystem::ChooseWuxingRealmBranch(player, nowMs, br2, br2Resp);
    ASSERT_EQ(br2Resp.error_message().id(), 0u);
    EXPECT_EQ(br2Resp.state().current_node_kind(), turnbattle::wuxing::kRealmNodeEvent);
    EXPECT_EQ(br2Resp.state().pending_event_kind(), turnbattle::wuxing::kRealmEventEncounter);
    EXPECT_EQ(br2Resp.state().route_choice_mask(), 1u);

    ResolveWuxingRealmEventRequest ev2;
    ev2.set_expected_run_id(st1.state().run_id());
    ev2.set_option_index(1u);
    ResolveWuxingRealmEventResponse ev2Resp;
    PlayerSeasonSystem::ResolveWuxingRealmEvent(player, nowMs, ev2, ev2Resp);
    ASSERT_EQ(ev2Resp.error_message().id(), 0u);
    EXPECT_TRUE(turnbattle::wuxing::IsValidRealmBoonId(ev2Resp.granted_boon_id()));
    EXPECT_EQ(ev2Resp.state().current_floor(), 3u);
    EXPECT_EQ(ev2Resp.state().current_node_kind(), turnbattle::wuxing::kRealmNodeNormal);
    EXPECT_EQ(ev2Resp.state().active_sigil_ids_size(), 2);

    // 第 3 关跨越周一 05:00 进入下一周：在途进度必须完整保留，不得清空！
    const uint64_t nextWeekMs = nowMs + kOneWeekMs;
    GetWuxingTowerStateResponse stNextWeek;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nextWeekMs, stNextWeek), kSuccess);
    ASSERT_TRUE(stNextWeek.state().run_active()) << "跨周不得清空尚未打完的在途局";
    EXPECT_EQ(stNextWeek.state().current_floor(), 3u);
    EXPECT_EQ(stNextWeek.state().active_sigil_ids_size(), 2);
    EXPECT_EQ(stNextWeek.state().weekly_cleared_mask(), 0u) << "新周首通位图归零";

    // 跨周继续打通第 3 关 -> 选第 3 枚奇策 -> 第 4 关右路休整「奇策淬炼升品」-> 第 5 关精英 -> 第 6 关首领通关
    winNode(3u, 105u, nextWeekMs, 2u);
    GetWuxingTowerStateResponse st3;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nextWeekMs, st3), kSuccess);
    ASSERT_EQ(st3.state().pending_offer_sigil_ids_size(), 3);
    PickWuxingTowerSigilRequest pick3;
    pick3.set_expected_run_id(st3.state().run_id());
    pick3.set_sigil_id(st3.state().pending_offer_sigil_ids(0));
    PickWuxingTowerSigilResponse pick3Resp;
    PlayerSeasonSystem::PickWuxingTowerSigil(player, nextWeekMs, pick3, pick3Resp);
    ASSERT_EQ(pick3Resp.error_message().id(), 0u);
    EXPECT_EQ(pick3Resp.state().current_floor(), 4u);
    EXPECT_EQ(pick3Resp.state().current_node_kind(), turnbattle::wuxing::kRealmNodeNeedBranch);

    ChooseWuxingRealmBranchRequest br4;
    br4.set_expected_run_id(st3.state().run_id());
    br4.set_branch(turnbattle::wuxing::kRealmBranchSpecial);
    ChooseWuxingRealmBranchResponse br4Resp;
    PlayerSeasonSystem::ChooseWuxingRealmBranch(player, nextWeekMs, br4, br4Resp);
    ASSERT_EQ(br4Resp.error_message().id(), 0u);
    EXPECT_EQ(br4Resp.state().pending_event_kind(), turnbattle::wuxing::kRealmEventRest);

    ResolveWuxingRealmEventRequest ev4;
    ev4.set_expected_run_id(st3.state().run_id());
    ev4.set_option_index(1u); // 淬炼升品
    ResolveWuxingRealmEventResponse ev4Resp;
    PlayerSeasonSystem::ResolveWuxingRealmEvent(player, nextWeekMs, ev4, ev4Resp);
    ASSERT_EQ(ev4Resp.error_message().id(), 0u);
    EXPECT_TRUE(turnbattle::wuxing::IsValidRealmBoonId(ev4Resp.granted_boon_id()));
    EXPECT_EQ(ev4Resp.state().current_floor(), 5u);
    EXPECT_EQ(ev4Resp.state().current_node_kind(), turnbattle::wuxing::kRealmNodeElite);

    winNode(5u, 105u, nextWeekMs, 3u);
    GetWuxingTowerStateResponse st5;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nextWeekMs, st5), kSuccess);
    ASSERT_EQ(st5.state().pending_offer_sigil_ids_size(), 3);
    PickWuxingTowerSigilRequest pick5;
    pick5.set_expected_run_id(st5.state().run_id());
    pick5.set_sigil_id(st5.state().pending_offer_sigil_ids(0));
    PickWuxingTowerSigilResponse pick5Resp;
    PlayerSeasonSystem::PickWuxingTowerSigil(player, nextWeekMs, pick5, pick5Resp);
    ASSERT_EQ(pick5Resp.error_message().id(), 0u);
    EXPECT_EQ(pick5Resp.state().current_floor(), 6u);
    EXPECT_EQ(pick5Resp.state().current_node_kind(), turnbattle::wuxing::kRealmNodeBoss);

    winNode(6u, 107u, nextWeekMs, 4u);
    GetWuxingTowerStateResponse stDone;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nextWeekMs, stDone), kSuccess);
    EXPECT_FALSE(stDone.state().run_active());
    EXPECT_EQ(stDone.state().best_rounds_preset(), 12u) << "3+2+3+4 = 12 回合写入个人永久公平最速";
    EXPECT_EQ(stDone.state().weekly_leaderboard_preset_size(), 0) << "上周旧局跨周完结绝不污染新周榜";

    // 新周开一轮打到第 3 关拿到 2 枚奇策写榜，再放弃重开打第 1 关（0 奇策）：单调防倒退守卫绝不覆盖旧的高分与奇策构筑
    StartWuxingTowerRunResponse newWeekRun;
    PlayerSeasonSystem::StartWuxingTowerRun(player, nextWeekMs, startReq, newWeekRun);
    ASSERT_EQ(newWeekRun.error_message().id(), 0u);
    winNode(1u, 101u, nextWeekMs, 2u);
    GetWuxingTowerStateResponse stNw1;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nextWeekMs, stNw1), kSuccess);
    ASSERT_EQ(stNw1.state().weekly_leaderboard_preset_size(), 1);
    PickWuxingTowerSigilRequest nwPick1;
    nwPick1.set_expected_run_id(stNw1.state().run_id());
    nwPick1.set_sigil_id(stNw1.state().pending_offer_sigil_ids(0));
    PickWuxingTowerSigilResponse nwPick1Resp;
    PlayerSeasonSystem::PickWuxingTowerSigil(player, nextWeekMs, nwPick1, nwPick1Resp);
    ASSERT_EQ(nwPick1Resp.error_message().id(), 0u);
    ChooseWuxingRealmBranchRequest nwBr2;
    nwBr2.set_expected_run_id(stNw1.state().run_id());
    nwBr2.set_branch(turnbattle::wuxing::kRealmBranchCombat);
    ChooseWuxingRealmBranchResponse nwBr2Resp;
    PlayerSeasonSystem::ChooseWuxingRealmBranch(player, nextWeekMs, nwBr2, nwBr2Resp);
    ASSERT_EQ(nwBr2Resp.error_message().id(), 0u);
    winNode(2u, 103u, nextWeekMs, 2u);
    GetWuxingTowerStateResponse stNw2;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nextWeekMs, stNw2), kSuccess);
    ASSERT_EQ(stNw2.state().weekly_leaderboard_preset_size(), 1);
    EXPECT_EQ(stNw2.state().weekly_leaderboard_preset(0).best_floor(), 2u);
    ASSERT_EQ(stNw2.state().weekly_leaderboard_preset(0).sigil_ids_size(), 1);
    const uint32_t savedBestBoon = stNw2.state().weekly_leaderboard_preset(0).sigil_ids(0);

    // 同一周再开新轮仅打通第 1 关（此时无奇策），榜单必须保留第 2 关成绩与已选奇策
    StartWuxingTowerRunResponse rerunResp;
    PlayerSeasonSystem::StartWuxingTowerRun(player, nextWeekMs, startReq, rerunResp);
    ASSERT_EQ(rerunResp.error_message().id(), 0u);
    winNode(1u, 101u, nextWeekMs, 1u);
    GetWuxingTowerStateResponse stRerun;
    ASSERT_EQ(PlayerSeasonSystem::GetWuxingTowerState(player, nextWeekMs, stRerun), kSuccess);
    ASSERT_EQ(stRerun.state().weekly_leaderboard_preset_size(), 1);
    EXPECT_EQ(stRerun.state().weekly_leaderboard_preset(0).best_floor(), 2u);
    ASSERT_EQ(stRerun.state().weekly_leaderboard_preset(0).sigil_ids_size(), 1);
    EXPECT_EQ(stRerun.state().weekly_leaderboard_preset(0).sigil_ids(0), savedBestBoon)
        << "低层重开局绝不允许覆盖本周最高层数的上榜奇策构筑";
}
