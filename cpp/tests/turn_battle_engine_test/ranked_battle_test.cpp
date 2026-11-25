// 论剑首发 V1 回合引擎回归测试：临时竞技宠、同速 SplitMix64 确定性决胜、回合上限平局与有效参与事实。

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>

#include "memory_battle_data_provider.h"
#include "system/turn_battle_engine.h"
#include "table/proto/tip/common_error_tip.pb.h"

namespace {

using turnbattle::TurnBattleEngine;

BattleAction Attack(uint64_t targetId) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_ATTACK);
    action.set_target_id(targetId);
    return action;
}

BattleAction Defend() {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_DEFEND);
    return action;
}

BattleAction Flee() {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_FLEE);
    return action;
}

CreateBattleRequest MakeRanked1v1Request(uint64_t seed, uint32_t battleConfigId = 5) {
    CreateBattleRequest req;
    req.set_battle_id(77001);
    req.set_battle_config_id(battleConfigId);
    req.set_match_mode(6);  // MATCH_MODE_PVP_TEAM
    req.set_seed(seed);
    auto* rctx = req.mutable_ranked_context();
    rctx->set_season_id(1);
    rctx->set_season_key("s1");
    rctx->set_rank_generation(1);
    rctx->set_rule_version(1);
    rctx->set_team_size(1);
    rctx->set_use_free_preset(true);

    for (const uint64_t pid : {uint64_t{101}, uint64_t{201}}) {
        auto* p = req.add_players();
        p->set_player_id(pid);
        p->set_team_index(pid == 101 ? 0 : 1);
        p->set_player_name(pid == 101 ? "Alpha" : "Beta");
        p->set_level(30);
        p->set_max_health(5000);
        p->set_max_mana(1000);
        p->mutable_base_attributes()->set_health(5000);
        p->mutable_base_attributes()->set_mana(1000);
        p->mutable_base_attributes()->set_speed(200);
        p->set_physical_attack(300);
        p->set_magic_attack(300);
        p->set_defense(100);

        auto* pet = p->add_pets();
        pet->set_pet_id(0);
        pet->set_competitive_temporary(true);
        pet->set_owner_player_id(pid);
        pet->set_pet_table_id(2);
        pet->set_combat_pet_table_id(2);
        pet->set_pet_name("DefaultPet");
        pet->set_level(30);
        pet->set_max_health(2000);
        pet->set_max_mana(600);
        pet->mutable_base_attributes()->set_health(2000);
        pet->mutable_base_attributes()->set_mana(600);
        pet->mutable_base_attributes()->set_speed(180);
        pet->set_physical_attack(200);
        pet->set_magic_attack(200);
        pet->set_defense(80);
    }
    return req;
}

TEST(RankedBattleTest, TemporaryCompetitivePetAllowedInRankedAndOmittedFromSettlement) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto req = MakeRanked1v1Request(42);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    const auto state = engine.BuildStateSnapshot();
    int tempPets = 0;
    for (const auto& actor : state.actors()) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET) {
            EXPECT_EQ(actor.pet_id(), 0u);
            EXPECT_TRUE(actor.competitive_temporary());
            EXPECT_NE(actor.actor_id() & turnbattle::kEngineLocalActorIdFlag, 0u);
            ++tempPets;
        }
    }
    EXPECT_EQ(tempPets, 2);

    engine.MarkPlayerEntered(101);
    engine.MarkPlayerEntered(201);
    engine.SubmitAction(101, Defend());
    engine.SubmitAction(201, Defend());
    engine.ResolveCurrentRound();

    const auto settlement = engine.BuildSettlement(101);
    EXPECT_TRUE(settlement.competitive_ranked());
    EXPECT_EQ(settlement.season_id(), 1u);
    EXPECT_EQ(settlement.ranked_rule_version(), 1u);
    EXPECT_TRUE(settlement.participated());
    EXPECT_EQ(settlement.pets_size(), 0);
}

TEST(RankedBattleTest, TemporaryCompetitivePetRejectedInNonRankedBattle) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto req = MakeRanked1v1Request(42);
    req.clear_ranked_context();
    TurnBattleEngine engine(data);
    EXPECT_FALSE(engine.Initialize(req));
}

TEST(RankedBattleTest, MirrorSpeedTieBreakFairAcrossSeedsAndDeterministicReplay) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    int first101 = 0;
    int first201 = 0;
    for (uint64_t seed = 1; seed <= 64; ++seed) {
        auto req = MakeRanked1v1Request(seed);
        req.mutable_players(0)->clear_pets();
        req.mutable_players(1)->clear_pets();
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        engine.SubmitAction(101, Defend());
        engine.SubmitAction(201, Defend());
        engine.ResolveCurrentRound();
        ASSERT_EQ(engine.LastActionOrder().size(), 2u);
        if (engine.LastActionOrder()[0] == 101) {
            ++first101;
        } else if (engine.LastActionOrder()[0] == 201) {
            ++first201;
        }
    }
    EXPECT_GT(first101, 0);
    EXPECT_GT(first201, 0);

    // 同种子同指令逐回合完全一致（确定性回放）
    auto req = MakeRanked1v1Request(987654321ULL);
    TurnBattleEngine a(data);
    TurnBattleEngine b(data);
    ASSERT_TRUE(a.Initialize(req));
    ASSERT_TRUE(b.Initialize(req));
    for (int r = 0; r < 3; ++r) {
        a.SubmitAction(101, Attack(201));
        a.SubmitAction(201, Attack(101));
        b.SubmitAction(101, Attack(201));
        b.SubmitAction(201, Attack(101));
        const auto ra = a.ResolveCurrentRound();
        const auto rb = b.ResolveCurrentRound();
        EXPECT_EQ(ra.SerializeAsString(), rb.SerializeAsString());
        EXPECT_EQ(a.LastActionOrder(), b.LastActionOrder());
    }
}

TEST(RankedBattleTest, RankedMaxRoundsEndsInDraw) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(5);
    dungeon.set_id(5);
    dungeon.set_time_limit(18);  // 18s -> 3 logical rounds
    dungeon.set_max_team_size(5);

    auto req = MakeRanked1v1Request(17, 5);
    req.mutable_players(0)->clear_pets();
    req.mutable_players(1)->clear_pets();
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    for (int r = 0; r < 3; ++r) {
        engine.SubmitAction(101, Defend());
        engine.SubmitAction(201, Defend());
        engine.ResolveCurrentRound();
    }
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_DRAW);
}

TEST(RankedBattleTest, ParticipationFactsCoverManualAutoNoOpportunityFledAndAfk) {
    auto data = std::make_shared<MemoryBattleDataProvider>();

    // 1. 显式自动 vs AFK
    {
        auto req = MakeRanked1v1Request(11);
        req.mutable_players(0)->clear_pets();
        req.mutable_players(1)->clear_pets();
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        engine.MarkPlayerEntered(101);
        engine.MarkPlayerEntered(201);
        ASSERT_EQ(engine.SetActorAuto(101, true), 0u);
        // 201 既不提交也不开自动 -> AFK
        engine.ResolveCurrentRound();

        const auto f101 = engine.BuildParticipationFact(101);
        EXPECT_TRUE(f101.entered_room());
        EXPECT_TRUE(f101.explicit_auto_enabled());
        EXPECT_TRUE(engine.DidPlayerParticipate(101));

        const auto f201 = engine.BuildParticipationFact(201);
        EXPECT_TRUE(f201.entered_room());
        EXPECT_FALSE(f201.manual_action_accepted());
        EXPECT_FALSE(f201.explicit_auto_enabled());
        EXPECT_FALSE(f201.no_action_opportunity());
        EXPECT_FALSE(engine.DidPlayerParticipate(201));
    }

    // 2. 进房后首次可操作前即阵亡 -> no_action_opportunity=true, participated=true
    {
        auto req = MakeRanked1v1Request(12);
        req.mutable_players(0)->clear_pets();
        req.mutable_players(1)->clear_pets();
        req.mutable_players(0)->mutable_base_attributes()->set_speed(500);
        req.mutable_players(0)->set_physical_attack(50000);
        req.mutable_players(1)->mutable_base_attributes()->set_health(10);
        req.mutable_players(1)->set_max_health(10);
        req.mutable_players(1)->mutable_base_attributes()->set_speed(100);

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        engine.MarkPlayerEntered(101);
        engine.MarkPlayerEntered(201);
        engine.SubmitAction(101, Attack(201));
        engine.ResolveCurrentRound();
        EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);

        const auto f201 = engine.BuildParticipationFact(201);
        EXPECT_TRUE(f201.entered_room());
        EXPECT_TRUE(f201.no_action_opportunity());
        EXPECT_TRUE(engine.DidPlayerParticipate(201));
        EXPECT_TRUE(engine.BuildSettlement(201).participated());
    }

    // 3. 未进房直接阵亡 -> participated=false
    {
        auto req = MakeRanked1v1Request(13);
        req.mutable_players(0)->clear_pets();
        req.mutable_players(1)->clear_pets();
        req.mutable_players(0)->mutable_base_attributes()->set_speed(500);
        req.mutable_players(0)->set_physical_attack(50000);
        req.mutable_players(1)->mutable_base_attributes()->set_health(10);
        req.mutable_players(1)->set_max_health(10);
        req.mutable_players(1)->mutable_base_attributes()->set_speed(100);

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        engine.MarkPlayerEntered(101);
        engine.SubmitAction(101, Attack(201));
        engine.ResolveCurrentRound();
        EXPECT_FALSE(engine.DidPlayerParticipate(201));
    }

    // 4. PVP 禁止逃跑（拒绝 FLEE 指令）且未提交任何有效行动/未开自动 -> participated=false
    {
        auto req = MakeRanked1v1Request(14);
        req.mutable_players(0)->clear_pets();
        req.mutable_players(1)->clear_pets();
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        engine.MarkPlayerEntered(101);
        engine.MarkPlayerEntered(201);
        EXPECT_EQ(engine.ValidateAction(101, Defend()), kSuccess);
        engine.SubmitAction(101, Defend());
        EXPECT_NE(engine.ValidateAction(201, Flee()), kSuccess);
        EXPECT_FALSE(engine.SubmitAction(201, Flee()));
        engine.ResolveCurrentRound();
        EXPECT_TRUE(engine.DidPlayerParticipate(101));
        EXPECT_FALSE(engine.DidPlayerParticipate(201));
        EXPECT_FALSE(engine.BuildSettlement(201).participated());
    }
}

TEST(RankedBattleTest, SelfGrownRankedUsesPermanentPetAndRejectsTemporaryPet) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    // 正式自养会武（season_id=1, use_free_preset=false）拒绝免费临时宠
    auto badReq = MakeRanked1v1Request(51);
    badReq.mutable_ranked_context()->set_use_free_preset(false);
    TurnBattleEngine badEngine(data);
    EXPECT_FALSE(badEngine.Initialize(badReq));

    // 携带真实永久宠（pet_id != 0, competitive_temporary=false）正常开局并产出正式排位结算
    auto req = MakeRanked1v1Request(52);
    req.mutable_ranked_context()->set_use_free_preset(false);
    req.mutable_players(0)->mutable_pets(0)->set_pet_id(9001);
    req.mutable_players(0)->mutable_pets(0)->set_competitive_temporary(false);
    req.mutable_players(1)->mutable_pets(0)->set_pet_id(9002);
    req.mutable_players(1)->mutable_pets(0)->set_competitive_temporary(false);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    EXPECT_TRUE(engine.IsRankedBattle());
    EXPECT_FALSE(engine.UsesFreePreset());

    engine.MarkPlayerEntered(101);
    engine.MarkPlayerEntered(201);
    engine.SubmitAction(101, Defend());
    engine.SubmitAction(201, Defend());
    engine.ResolveCurrentRound();

    const auto s = engine.BuildSettlement(101);
    EXPECT_TRUE(s.competitive_ranked());
    EXPECT_FALSE(s.use_free_preset());
    EXPECT_EQ(s.season_id(), 1u);
    EXPECT_EQ(s.pets_size(), 1);
    EXPECT_EQ(s.pets(0).pet_id(), 9001u);
}

TEST(RankedBattleTest, EntertainmentFreePresetAllowsTemporaryPetWithoutRankedSeason) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    // 免费预设娱乐场（如物法易势 5V5 / 护卫救援）：season_id=0, use_free_preset=true
    auto req = MakeRanked1v1Request(53);
    req.set_match_mode(8);  // MATCH_MODE_POLARITY_5V5
    req.mutable_ranked_context()->set_season_id(0);
    req.mutable_ranked_context()->set_use_free_preset(true);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    EXPECT_FALSE(engine.IsRankedBattle());
    EXPECT_TRUE(engine.UsesFreePreset());

    engine.MarkPlayerEntered(101);
    engine.MarkPlayerEntered(201);
    engine.SubmitAction(101, Defend());
    engine.SubmitAction(201, Defend());
    engine.ResolveCurrentRound();

    const auto s = engine.BuildSettlement(101);
    EXPECT_FALSE(s.competitive_ranked());
    EXPECT_TRUE(s.use_free_preset());
    EXPECT_EQ(s.season_id(), 0u);
    EXPECT_EQ(s.pets_size(), 0);
}

}  // namespace
