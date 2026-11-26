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
    ASSERT_EQ(initResp.state().honors_size(), 1);
    EXPECT_EQ(initResp.state().honors(0).season_id(), 1u);
    EXPECT_EQ(initResp.state().honors(0).honor_title_name(), "破阵宗师");
    EXPECT_FALSE(initResp.state().honors(0).unlocked());

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
    comp.Swap(&cand1);

    // 满级后继续打合格局：pointsAdded == 0, honorNewlyUnlocked == false，但仍带回 12/12 供结算页展示
    PlayerSeasonComp cand2;
    bool inc2 = false;
    seasonrules::BattleCreditDelta delta2;
    ASSERT_EQ(PlayerSeasonSystem::PrepareBattleCredit(comp, pveWin, nowMs, cand2, inc2, nullptr, &delta2), kSuccess);
    EXPECT_FALSE(inc2);
    EXPECT_EQ(delta2.pointsAdded, 0u);
    EXPECT_EQ(delta2.progressAfter, 12u);
    EXPECT_EQ(delta2.maxPoints, 12u);
    EXPECT_FALSE(delta2.honorNewlyUnlocked);

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
    ASSERT_EQ(s2Resp.state().honors_size(), 2);
    EXPECT_EQ(s2Resp.state().honors(0).season_id(), 1u);
    EXPECT_TRUE(s2Resp.state().honors(0).unlocked()) << "S1 满级零数值纪念荣誉跨季永久留存";
    EXPECT_FALSE(s2Resp.state().honors(0).is_current_season());
    EXPECT_EQ(s2Resp.state().honors(1).season_id(), 2u);
    EXPECT_FALSE(s2Resp.state().honors(1).unlocked());
    EXPECT_TRUE(s2Resp.state().honors(1).is_current_season());

    // 4. 休赛期（全部关闭）仍展示历史永久纪念荣誉
    for (auto& row : switchedRules) {
        row.set_enabled(false);
    }
    SeasonRuleTableManager::Instance().InstallSnapshot(MakeRuleSnapshot(switchedRules));
    GetSeasonStateResponse offResp;
    ASSERT_EQ(PlayerSeasonSystem::GetState(player, nowMs, offResp), kSuccess);
    EXPECT_FALSE(offResp.state().available());
    ASSERT_EQ(offResp.state().honors_size(), 1);
    EXPECT_EQ(offResp.state().honors(0).season_id(), 1u);
    EXPECT_TRUE(offResp.state().honors(0).unlocked());
}
