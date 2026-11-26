// 论剑首发 V1 回合引擎回归测试：临时竞技宠、同速 SplitMix64 确定性决胜、回合上限平局与有效参与事实。

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>

#include "memory_battle_data_provider.h"
#include "system/turn_battle_engine.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/match_error_tip.pb.h"

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

TEST(RankedBattleTest, RankedDefaultRoundCapOverridesTimeLimitFallbackAndAllowsExplicitOverride) {
    // 1. Dungeon 5 未填 round_limit 且 time_limit=720（换算 120 回合）时，正式排位默认收敛到 20 回合判平
    {
        auto data = std::make_shared<MemoryBattleDataProvider>();
        auto& dungeon = data->AddDungeon(5);
        dungeon.set_id(5);
        dungeon.set_time_limit(720);
        dungeon.set_max_team_size(5);

        auto req = MakeRanked1v1Request(21, 5);
        req.mutable_players(0)->clear_pets();
        req.mutable_players(1)->clear_pets();
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        for (int r = 0; r < 19; ++r) {
            engine.SubmitAction(101, Defend());
            engine.SubmitAction(201, Defend());
            engine.ResolveCurrentRound();
            EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);
        }
        engine.SubmitAction(101, Defend());
        engine.SubmitAction(201, Defend());
        engine.ResolveCurrentRound();
        EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_DRAW);
    }

    // 2. 显式配置 Dungeon.round_limit=25 时尊重表值，不强行压回 20 回合
    {
        auto data = std::make_shared<MemoryBattleDataProvider>();
        auto& dungeon = data->AddDungeon(5);
        dungeon.set_id(5);
        dungeon.set_round_limit(25);
        dungeon.set_time_limit(720);
        dungeon.set_max_team_size(5);

        auto req = MakeRanked1v1Request(22, 5);
        req.mutable_players(0)->clear_pets();
        req.mutable_players(1)->clear_pets();
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        for (int r = 0; r < 24; ++r) {
            engine.SubmitAction(101, Defend());
            engine.SubmitAction(201, Defend());
            engine.ResolveCurrentRound();
            EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);
        }
        engine.SubmitAction(101, Defend());
        engine.SubmitAction(201, Defend());
        engine.ResolveCurrentRound();
        EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_DRAW);
    }
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

TEST(RankedBattleTest, PrimaryTargetFocusBonusAppliesIn1v1CompetitivePvpAndMarksEvent) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(5);
    dungeon.set_id(5);
    dungeon.set_round_limit(20);
    dungeon.set_time_limit(720);
    dungeon.set_max_team_size(5);

    auto& sk = data->AddSkill(10002);
    sk.set_id(10002);
    sk.set_battle_model(1);
    sk.set_ability_kind(1);
    sk.set_caster_mask(3);
    sk.set_unlock_level(1);
    sk.set_max_rank(10);
    sk.set_max_targets(5);
    sk.set_pet_max_targets(2);
    sk.set_damage_type(1);  // physical
    sk.set_attack_multiplier(1.0);
    sk.set_target_team(1);  // enemy
    sk.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    sk.add_effect(100020);
    data->SetSkillDamage(10002, 200.0);
    auto& bf = data->AddBuff(100020);
    bf.set_id(100020);
    bf.set_battle_op(1);   // Damage
    bf.set_value_basis(1); // PhysicalAttack
    bf.set_ratio(1.0);

    auto req = MakeRanked1v1Request(77, 5);
    req.set_match_mode(7);  // MATCH_MODE_PVP_TEAM
    req.mutable_players(0)->add_skill_table_ids(10002);
    (*req.mutable_players(0)->mutable_skill_ranks())[10002] = 1;
    // 让对手人物与对手宝宝拥有相同防御，便于精确比对显式主目标 +20% 凝锋伤害。
    req.mutable_players(1)->set_defense(100);
    req.mutable_players(1)->mutable_pets(0)->set_defense(100);

    TurnBattleEngine focusEngine(data);
    ASSERT_TRUE(focusEngine.Initialize(req));
    uint64_t enemyPetActorId = 0;
    for (const auto& actor : focusEngine.BuildStateSnapshot().actors()) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && actor.team_index() == 1) {
            enemyPetActorId = actor.actor_id();
        }
    }
    ASSERT_NE(enemyPetActorId, 0u);

    BattleAction cast;
    cast.set_action_type(BATTLE_ACTION_SKILL);
    cast.set_skill_table_id(10002);
    cast.set_target_id(201);  // 显式点选敌方人物为主目标
    focusEngine.SubmitAction(101, cast);
    focusEngine.SubmitAction(201, Attack(101));
    for (const auto& actor : focusEngine.BuildStateSnapshot().actors()) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET) {
            focusEngine.SubmitActorAction(actor.owner_player_id(), actor.actor_id(), Attack(actor.team_index() == 0 ? 201 : 101));
        }
    }
    const auto res = focusEngine.ResolveCurrentRound();
    uint64_t primaryDmg = 0;
    uint64_t secondaryDmg = 0;
    bool primaryMarked = false;
    bool secondaryMarked = false;
    for (const auto& ev : res.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 101 && ev.skill_table_id() == 10002) {
            if (ev.target_id() == 201) {
                primaryDmg = ev.value();
                primaryMarked = ev.is_primary_focus();
            } else if (ev.target_id() == enemyPetActorId) {
                secondaryDmg = ev.value();
                secondaryMarked = ev.is_primary_focus();
            }
        }
    }
    EXPECT_GT(primaryDmg, 0u);
    EXPECT_GT(secondaryDmg, 0u);
    EXPECT_TRUE(primaryMarked);
    EXPECT_FALSE(secondaryMarked);
    EXPECT_GT(primaryDmg, secondaryDmg);
    EXPECT_NEAR(static_cast<double>(primaryDmg), static_cast<double>(secondaryDmg) * 1.20, 2.0);
}

TEST(RankedBattleTest, SuddenDeathEscalationBoostsDamageAndReducesHealAndShieldFromRound13) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(5);
    dungeon.set_id(5);
    dungeon.set_round_limit(20);
    dungeon.set_time_limit(720);
    dungeon.set_max_team_size(5);

    auto& healSk = data->AddSkill(10010);
    healSk.set_id(10010);
    healSk.set_battle_model(1);
    healSk.set_ability_kind(1);
    healSk.set_caster_mask(3);
    healSk.set_unlock_level(1);
    healSk.set_max_rank(10);
    healSk.set_max_targets(7);
    healSk.set_target_team(2);  // ally
    healSk.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    healSk.add_effect(100100);
    auto& healBf = data->AddBuff(100100);
    healBf.set_id(100100);
    healBf.set_battle_op(2);  // Heal
    healBf.set_per_level(20);

    auto& shieldSk = data->AddSkill(10040);
    shieldSk.set_id(10040);
    shieldSk.set_battle_model(1);
    shieldSk.set_ability_kind(1);
    shieldSk.set_caster_mask(1);
    shieldSk.set_unlock_level(1);
    shieldSk.set_max_rank(10);
    shieldSk.set_max_targets(7);
    shieldSk.set_target_team(2);  // ally
    shieldSk.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    shieldSk.add_effect(100400);
    auto& shieldBf = data->AddBuff(100400);
    shieldBf.set_id(100400);
    shieldBf.set_battle_op(7);  // Shield
    shieldBf.set_per_level(20);
    shieldBf.set_duration_rounds(2);
    shieldBf.set_tick_on_apply(1);

    auto req = MakeRanked1v1Request(88, 5);
    req.set_match_mode(7);
    req.mutable_players(0)->clear_pets();
    req.mutable_players(1)->clear_pets();
    req.mutable_players(0)->set_max_health(20000);
    req.mutable_players(0)->mutable_base_attributes()->set_health(10000);
    req.mutable_players(1)->set_max_health(20000);
    req.mutable_players(1)->mutable_base_attributes()->set_health(20000);
    req.mutable_players(0)->add_skill_table_ids(10010);
    req.mutable_players(0)->add_skill_table_ids(10040);
    (*req.mutable_players(0)->mutable_skill_ranks())[10010] = 1;
    (*req.mutable_players(0)->mutable_skill_ranks())[10040] = 1;

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    const auto s1 = engine.BuildStateSnapshot();
    EXPECT_EQ(s1.round_limit(), 20u);
    EXPECT_EQ(s1.escalation_start_round(), 13u);
    EXPECT_EQ(s1.escalation_stacks(), 0u);
    EXPECT_EQ(s1.surrender_unlock_round(), 5u);

    // R1 基线普攻伤害与治疗量
    BattleAction healAct;
    healAct.set_action_type(BATTLE_ACTION_SKILL);
    healAct.set_skill_table_id(10010);
    healAct.set_target_id(101);
    engine.SubmitAction(101, healAct);
    engine.SubmitAction(201, Attack(101));
    const auto r1 = engine.ResolveCurrentRound();
    uint64_t r1Heal = 0;
    uint64_t r1AtkDmg = 0;
    for (const auto& ev : r1.events()) {
        if (ev.event_type() == BATTLE_EVENT_HEAL && ev.source_id() == 101) r1Heal = ev.value();
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 201) r1AtkDmg = ev.value();
    }
    ASSERT_GT(r1Heal, 0u);
    ASSERT_GT(r1AtkDmg, 0u);

    // 推进到 R13（第 1 层战意渐盛：伤 +10%、疗盾 -10%）
    for (int r = 2; r <= 12; ++r) {
        engine.SubmitAction(101, Defend());
        engine.SubmitAction(201, Defend());
        engine.ResolveCurrentRound();
    }
    const auto s13 = engine.BuildStateSnapshot();
    EXPECT_EQ(s13.round_index(), 13u);
    EXPECT_EQ(s13.escalation_stacks(), 1u);

    engine.SubmitAction(101, healAct);
    engine.SubmitAction(201, Attack(101));
    const auto r13 = engine.ResolveCurrentRound();
    uint64_t r13Heal = 0;
    uint64_t r13AtkDmg = 0;
    for (const auto& ev : r13.events()) {
        if (ev.event_type() == BATTLE_EVENT_HEAL && ev.source_id() == 101) r13Heal = ev.value();
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 201) r13AtkDmg = ev.value();
    }
    EXPECT_LT(r13Heal, r1Heal);
    EXPECT_GT(r13AtkDmg, r1AtkDmg);
}

TEST(RankedBattleTest, PvpSurrenderUnlockVoteCooldownIdleExclusionAndPrivacy) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(5);
    dungeon.set_id(5);
    dungeon.set_round_limit(20);
    dungeon.set_time_limit(720);
    dungeon.set_max_team_size(5);

    // 1. PVE 禁止投降
    {
        CreateBattleRequest pve;
        pve.set_battle_id(99001);
        pve.set_battle_config_id(1);
        pve.set_match_mode(4);  // PVE_SOLO
        pve.set_seed(1);
        auto* p = pve.add_players();
        p->set_player_id(101);
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(5000);
        p->mutable_base_attributes()->set_health(5000);
        p->mutable_base_attributes()->set_speed(200);
        auto& m1 = data->AddMonster(1);
        m1.set_id(1);
        m1.set_health(500);
        m1.set_strength(50);
        m1.set_armor(20);
        m1.set_speed(100);
        auto& d1 = data->AddDungeon(1);
        d1.set_id(1);
        d1.set_max_team_size(5);
        d1.set_time_limit(600);
        d1.add_monster(1);
        data->SetDungeonMonsters(1, {1});
        auto& reward1 = data->AddBattleReward(1);
        reward1.set_exp_gain(10);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(pve));
        EXPECT_EQ(engine.SurrenderUnlockRound(), 0u);
        EXPECT_EQ(engine.SubmitSurrenderVote(101, true), kMatchBattleSurrenderNotAllowed);
    }

    // 2. 1V1 前 4 回合拒绝，第 5 回合秒投且 surrendered=true, fled=false, total_rounds=4
    {
        auto req = MakeRanked1v1Request(91, 5);
        req.set_match_mode(7);
        req.mutable_players(0)->clear_pets();
        req.mutable_players(1)->clear_pets();
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        engine.MarkPlayerEntered(101);
        engine.MarkPlayerEntered(201);
        EXPECT_EQ(engine.SubmitSurrenderVote(101, true), kMatchBattleSurrenderTooEarly);
        for (int r = 1; r <= 4; ++r) {
            engine.SubmitAction(101, Attack(201));
            engine.SubmitAction(201, Defend());
            engine.ResolveCurrentRound();
        }
        BattleSurrenderVoteState vote;
        ASSERT_EQ(engine.SubmitSurrenderVote(101, true, &vote), 0u);
        EXPECT_TRUE(vote.passed());
        EXPECT_FALSE(vote.active());
        EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);

        const auto s101 = engine.BuildSettlement(101);
        const auto s201 = engine.BuildSettlement(201);
        EXPECT_TRUE(s101.surrendered());
        EXPECT_FALSE(s101.fled());
        EXPECT_EQ(s101.total_rounds(), 4u);
        EXPECT_GE(s101.manual_action_rounds(), 4u);
        EXPECT_TRUE(s101.effective_combat_contribution());
        EXPECT_FALSE(s201.surrendered());
    }

    // 3. 3V3 投票、否决 3 回合冷却、连续 2 回合未手动挂机队友不阻挡、敌方与观众视角隐藏投票
    {
        CreateBattleRequest req;
        req.set_battle_id(99002);
        req.set_battle_config_id(5);
        req.set_match_mode(7);
        req.set_seed(92);
        auto* rctx = req.mutable_ranked_context();
        rctx->set_season_id(1);
        rctx->set_season_key("s1");
        rctx->set_rank_generation(1);
        rctx->set_rule_version(2);
        rctx->set_team_size(3);
        rctx->set_use_free_preset(false);
        for (uint64_t pid : {uint64_t{101}, uint64_t{102}, uint64_t{103}, uint64_t{201}, uint64_t{202}, uint64_t{203}}) {
            auto* p = req.add_players();
            p->set_player_id(pid);
            p->set_team_index(pid < 200 ? 0 : 1);
            p->set_level(30);
            p->set_max_health(50000);
            p->mutable_base_attributes()->set_health(50000);
            p->mutable_base_attributes()->set_speed(200);
            p->set_physical_attack(100);
            p->set_defense(100);
        }
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        // R1..R4：101/102/103 全手动防御进入 R5
        for (int r = 1; r <= 4; ++r) {
            for (uint64_t pid : {uint64_t{101}, uint64_t{102}, uint64_t{103}, uint64_t{201}, uint64_t{202}, uint64_t{203}}) {
                engine.SubmitAction(pid, Defend());
            }
            engine.ResolveCurrentRound();
        }
        // R5：101 发起投降（需要 3 票），查重与视图隔离
        BattleSurrenderVoteState vote;
        ASSERT_EQ(engine.SubmitSurrenderVote(101, true, &vote), 0u);
        EXPECT_TRUE(vote.active());
        EXPECT_EQ(vote.required_agrees(), 3u);
        EXPECT_EQ(engine.SubmitSurrenderVote(101, true), kMatchBattleSurrenderAlreadyVoted);

        auto teammateState = engine.BuildStateSnapshot();
        engine.FillSurrenderVoteForViewer(teammateState, 102);
        EXPECT_TRUE(teammateState.has_surrender_vote());
        EXPECT_TRUE(teammateState.surrender_vote().active());

        auto enemyState = engine.BuildStateSnapshot();
        engine.FillSurrenderVoteForViewer(enemyState, 201);
        EXPECT_FALSE(enemyState.has_surrender_vote());

        auto observerState = engine.BuildStateSnapshot();
        engine.FillSurrenderVoteForViewer(observerState, 0);
        EXPECT_FALSE(observerState.has_surrender_vote());

        // 102 投反对 -> 投票关闭、冷却至 R8（5+3）
        ASSERT_EQ(engine.SubmitSurrenderVote(102, false, &vote), 0u);
        EXPECT_FALSE(vote.active());
        EXPECT_TRUE(vote.rejected());
        EXPECT_EQ(vote.next_allowed_round(), 8u);
        EXPECT_EQ(engine.SubmitSurrenderVote(101, true), kMatchBattleSurrenderCooldown);

        // R5..R7：103 连续挂机不提交手动指令（R5/R6/R7 均未手操 -> idle >= 2），101/102 正常手操
        for (int r = 5; r <= 7; ++r) {
            engine.SubmitAction(101, Defend());
            engine.SubmitAction(102, Defend());
            engine.SubmitAction(201, Defend());
            engine.SubmitAction(202, Defend());
            engine.SubmitAction(203, Defend());
            engine.ResolveCurrentRound();
        }
        // R8 冷却解除，103 因连续挂机 >=2 回合被自动移出必要表决池 -> 仅需 2 票（101 + 102）即可通过！
        ASSERT_EQ(engine.SubmitSurrenderVote(101, true, &vote), 0u);
        EXPECT_TRUE(vote.active());
        EXPECT_EQ(vote.required_agrees(), 2u);
        ASSERT_EQ(engine.SubmitSurrenderVote(102, true, &vote), 0u);
        EXPECT_TRUE(vote.passed());
        EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);
    }
}

TEST(RankedBattleTest, TacticalPrepFormalRankedSwapSealAndPetPrivacyAndLockIn) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(5);
    dungeon.set_id(5);
    dungeon.set_round_limit(20);
    dungeon.set_time_limit(720);
    dungeon.set_max_team_size(5);

    auto makeSeal = [](uint32_t id, uint32_t effectId, const char* name, uint32_t activeRounds, uint32_t battleLimit) {
        SeasonSealEffect s;
        s.set_season_id(1);
        s.set_seal_id(id);
        s.set_effect_id(effectId);
        s.set_bonus_bps(1000);
        s.set_cap_bps(activeRounds > 0 ? 300 : 0);
        s.set_per_round_limit(activeRounds == 0 ? 1 : 0);
        s.set_battle_limit(battleLimit);
        s.set_active_rounds(activeRounds);
        s.set_name(name);
        return s;
    };
    const auto seal101 = makeSeal(101, 1, "破阵锐印", 0, 0);
    const auto seal103 = makeSeal(103, 6, "固阵守印", 2, 1);

    auto makePet = [](uint64_t petId, uint64_t ownerId, uint32_t tableId, const char* name, uint64_t hp, uint64_t spd) {
        BattlePetSnapshot p;
        p.set_pet_id(petId);
        p.set_owner_player_id(ownerId);
        p.set_pet_table_id(tableId);
        p.set_pet_name(name);
        p.set_level(30);
        p.set_max_health(hp);
        p.set_max_mana(1000);
        p.set_physical_attack(200);
        p.set_magic_attack(100);
        p.set_defense(120);
        p.mutable_base_attributes()->set_health(hp);
        p.mutable_base_attributes()->set_mana(1000);
        p.mutable_base_attributes()->set_speed(spd);
        return p;
    };

    CreateBattleRequest req;
    req.set_battle_id(99301);
    req.set_battle_config_id(5);
    req.set_match_mode(7);
    req.set_seed(301);
    auto* rctx = req.mutable_ranked_context();
    rctx->set_season_id(1);
    rctx->set_season_key("s1");
    rctx->set_rank_generation(1);
    rctx->set_rule_version(2);
    rctx->set_team_size(1);
    rctx->set_use_free_preset(false);

    auto* p1 = req.add_players();
    p1->set_player_id(101);
    p1->set_team_index(0);
    p1->set_level(30);
    p1->set_active_season_id(1);
    p1->set_max_health(10000);
    p1->mutable_base_attributes()->set_health(10000);
    p1->mutable_base_attributes()->set_speed(200);
    p1->set_physical_attack(300);
    p1->set_defense(100);
    *p1->mutable_equipment_effects()->mutable_season_seal() = seal101;
    *p1->add_available_season_seals() = seal101;
    *p1->add_available_season_seals() = seal103;
    *p1->add_pets() = makePet(5001, 101, 1, "灵狐", 3000, 260);
    *p1->add_standby_pets() = makePet(5002, 101, 2, "石灵", 5500, 140);

    auto* p2 = req.add_players();
    p2->set_player_id(201);
    p2->set_team_index(1);
    p2->set_level(30);
    p2->set_active_season_id(1);
    p2->set_max_health(10000);
    p2->mutable_base_attributes()->set_health(10000);
    p2->mutable_base_attributes()->set_speed(190);
    p2->set_physical_attack(300);
    p2->set_defense(100);
    *p2->mutable_equipment_effects()->mutable_season_seal() = seal101;
    *p2->add_available_season_seals() = seal101;
    *p2->add_available_season_seals() = seal103;
    *p2->add_pets() = makePet(6001, 201, 3, "金猊", 3200, 280);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    ASSERT_TRUE(engine.IsInPrepPhase());
    EXPECT_FALSE(engine.AllPrepPlayersReady());

    // 变阵期严禁提交行动、切自动或投降；pending_actor_ids 为空。
    EXPECT_EQ(engine.ValidateAction(101, Attack(201)), kFeatureUnavailable);
    EXPECT_FALSE(engine.SubmitAction(101, Attack(201)));
    EXPECT_EQ(engine.SetActorAuto(101, true), kFeatureUnavailable);
    EXPECT_EQ(engine.SubmitSurrenderVote(101, true), kFeatureUnavailable);

    auto snap = engine.BuildStateSnapshot();
    EXPECT_TRUE(snap.pending_actor_ids().empty());
    uint64_t initialPetActorId = 0;
    uint32_t initialPetSlot = 0;
    for (const auto& a : snap.actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_PET && a.owner_player_id() == 101) {
            initialPetActorId = a.actor_id();
            initialPetSlot = a.formation_slot();
        }
    }
    ASSERT_NE(initialPetActorId, 0u);

    // 视角脱敏：101 只能看到自己的选项与同队状态，看不到 201；观众仅看倒计时。
    engine.FillPrepStateForViewer(snap, 101, 123456);
    ASSERT_TRUE(snap.has_prep_state());
    EXPECT_TRUE(snap.prep_state().active());
    EXPECT_EQ(snap.prep_state().deadline_ms(), 123456u);
    EXPECT_EQ(snap.prep_state().self_available_seals_size(), 2);
    EXPECT_EQ(snap.prep_state().self_available_pets_size(), 2);
    ASSERT_EQ(snap.prep_state().team_states_size(), 1);
    EXPECT_EQ(snap.prep_state().team_states(0).player_id(), 101u);

    BattleStateS2C obsSnap;
    engine.FillPrepStateForViewer(obsSnap, 0, 123456);
    EXPECT_TRUE(obsSnap.prep_state().active());
    EXPECT_EQ(obsSnap.prep_state().deadline_ms(), 123456u);
    EXPECT_EQ(obsSnap.prep_state().self_available_seals_size(), 0);
    EXPECT_EQ(obsSnap.prep_state().self_available_pets_size(), 0);
    EXPECT_EQ(obsSnap.prep_state().team_states_size(), 0);

    // 越权选择对手灵宠或非法赛季印一律拒绝。
    EXPECT_EQ(engine.UpdateBattlePrep(101, 999, 0, false), kInvalidParameter);
    EXPECT_EQ(engine.UpdateBattlePrep(101, 0, 6001, false), kInvalidParameter);

    // 101 临场切换固阵守印 + 石灵并锁定；锁定后禁止再改，重复锁定幂等。
    BattlePrepState prepOut;
    ASSERT_EQ(engine.UpdateBattlePrep(101, 103, 5002, true, &prepOut), 0u);
    ASSERT_EQ(prepOut.team_states_size(), 1);
    EXPECT_EQ(prepOut.team_states(0).selected_seal_id(), 103u);
    EXPECT_EQ(prepOut.team_states(0).selected_pet_id(), 5002u);
    EXPECT_TRUE(prepOut.team_states(0).ready());
    EXPECT_EQ(engine.UpdateBattlePrep(101, 101, 0, true), kFeatureUnavailable);
    EXPECT_EQ(engine.UpdateBattlePrep(101, 103, 5002, true), 0u);
    EXPECT_FALSE(engine.AllPrepPlayersReady());

    // 201 保持原配锁定 -> 全员就绪。
    ASSERT_EQ(engine.UpdateBattlePrep(201, 0, 0, true), 0u);
    EXPECT_TRUE(engine.AllPrepPlayersReady());
    ASSERT_TRUE(engine.FinalizePrepPhase());
    EXPECT_FALSE(engine.IsInPrepPhase());

    // 验证亮牌后的最终阵容：101 赛季印为 103，宝宝原地换为石灵(5002)并保留原局内号与阵位。
    const auto postSnap = engine.BuildStateSnapshot();
    EXPECT_FALSE(postSnap.has_prep_state());
    EXPECT_FALSE(postSnap.pending_actor_ids().empty());
    bool found101 = false;
    bool foundSwappedPet = false;
    for (const auto& a : postSnap.actors()) {
        if (a.actor_id() == 101) {
            found101 = true;
            EXPECT_EQ(a.equipment_effects().season_seal().seal_id(), 103u);
        }
        if (a.actor_type() == BATTLE_ACTOR_TYPE_PET && a.owner_player_id() == 101) {
            foundSwappedPet = true;
            EXPECT_EQ(a.actor_id(), initialPetActorId);
            EXPECT_EQ(a.formation_slot(), initialPetSlot);
            EXPECT_EQ(a.pet_id(), 5002u);
            EXPECT_EQ(a.pet_table_id(), 2u);
            EXPECT_EQ(a.max_health(), 5500u);
        }
    }
    EXPECT_TRUE(found101);
    EXPECT_TRUE(foundSwappedPet);

    // 亮牌后第 1 回合正常接收指令并结算。
    EXPECT_EQ(engine.ValidateAction(101, Attack(201)), kSuccess);
    engine.SubmitAction(101, Attack(201));
    engine.SubmitAction(201, Defend());
    const auto turn1 = engine.ResolveCurrentRound();
    EXPECT_EQ(turn1.round_index(), 1u);
}

TEST(RankedBattleTest, TacticalPrepSkippedForFreePresetAndZeroOptionMatches) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(5);
    dungeon.set_id(5);
    dungeon.set_round_limit(20);
    dungeon.set_time_limit(720);
    dungeon.set_max_team_size(5);

    auto req = MakeRanked1v1Request(302, 5);
    req.set_match_mode(7);
    req.mutable_ranked_context()->set_use_free_preset(true);
    TurnBattleEngine freePresetEngine(data);
    ASSERT_TRUE(freePresetEngine.Initialize(req));
    EXPECT_FALSE(freePresetEngine.IsInPrepPhase());

    req.mutable_ranked_context()->set_use_free_preset(false);
    req.mutable_players(0)->clear_pets();
    req.mutable_players(1)->clear_pets();
    TurnBattleEngine noOptionEngine(data);
    ASSERT_TRUE(noOptionEngine.Initialize(req));
    EXPECT_FALSE(noOptionEngine.IsInPrepPhase());
}

TEST(RankedBattleTest, TacticalPingsMultiPlayerIsolationRateLimitAndRoundClear) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(5);
    dungeon.set_id(5);
    dungeon.set_round_limit(20);
    dungeon.set_time_limit(720);
    dungeon.set_max_team_size(5);

    // 1V1 不开放队内标点。
    auto soloReq = MakeRanked1v1Request(401, 5);
    TurnBattleEngine soloEngine(data);
    ASSERT_TRUE(soloEngine.Initialize(soloReq));
    EXPECT_EQ(soloEngine.SubmitBattlePing(101, 201, BATTLE_PING_FOCUS), kFeatureUnavailable);

    // 2V2 校验阵营约束、单回合 2 次上限、同玩家覆写、队内脱敏隔离与回合推进清空。
    CreateBattleRequest req;
    req.set_battle_id(402);
    req.set_battle_config_id(5);
    req.set_match_mode(7);
    req.set_seed(402);
    req.mutable_ranked_context()->set_season_id(1);
    req.mutable_ranked_context()->set_rule_version(2);
    req.mutable_ranked_context()->set_team_size(2);
    auto addP = [&](uint64_t id, uint32_t team) {
        auto* p = req.add_players();
        p->set_player_id(id);
        p->set_team_index(team);
        p->set_player_name("P" + std::to_string(id));
        p->set_level(30);
        p->set_max_health(4000);
        p->set_max_mana(1000);
        p->mutable_base_attributes()->set_health(4000);
        p->mutable_base_attributes()->set_mana(1000);
        p->mutable_base_attributes()->set_attack(300);
        p->mutable_base_attributes()->set_speed(100);
    };
    addP(101, 0);
    addP(102, 0);
    addP(201, 1);
    addP(202, 1);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    // 阵营非法拒绝
    EXPECT_EQ(engine.SubmitBattlePing(101, 102, BATTLE_PING_FOCUS), kInvalidParameter);
    EXPECT_EQ(engine.SubmitBattlePing(101, 201, BATTLE_PING_PROTECT), kInvalidParameter);
    EXPECT_EQ(engine.SubmitBattlePing(101, 999, BATTLE_PING_DISPEL), kInvalidParameter);

    // 第 1、2 次合法，最新覆盖前一条，第 3 次触发限频
    BattleTacticalPing out;
    ASSERT_EQ(engine.SubmitBattlePing(101, 201, BATTLE_PING_FOCUS, &out), 0u);
    EXPECT_EQ(out.sender_player_id(), 101u);
    EXPECT_EQ(out.target_actor_id(), 201u);
    EXPECT_EQ(out.kind(), BATTLE_PING_FOCUS);
    EXPECT_EQ(out.round_index(), 1u);

    ASSERT_EQ(engine.SubmitBattlePing(101, 102, BATTLE_PING_PROTECT, &out), 0u);
    EXPECT_EQ(out.kind(), BATTLE_PING_PROTECT);
    EXPECT_EQ(engine.SubmitBattlePing(101, 202, BATTLE_PING_DISPEL), kRateLimitExceeded);

    // 队友 102 也可标敌方驱散；对手 201 标己方保核
    ASSERT_EQ(engine.SubmitBattlePing(102, 202, BATTLE_PING_DISPEL), 0u);
    ASSERT_EQ(engine.SubmitBattlePing(201, 202, BATTLE_PING_PROTECT), 0u);

    // 视角隔离：A 队看 A 队 2 条（101 仅保留最新 1 条），B 队只看 B 队 1 条，观众为空
    BattleStateS2C snapA;
    engine.FillTeamPingsForViewer(snapA, 102);
    ASSERT_EQ(snapA.team_pings_size(), 2);
    EXPECT_EQ(snapA.team_pings(0).sender_player_id(), 101u);
    EXPECT_EQ(snapA.team_pings(0).kind(), BATTLE_PING_PROTECT);
    EXPECT_EQ(snapA.team_pings(1).sender_player_id(), 102u);
    EXPECT_EQ(snapA.team_pings(1).kind(), BATTLE_PING_DISPEL);

    BattleStateS2C snapB;
    engine.FillTeamPingsForViewer(snapB, 202);
    ASSERT_EQ(snapB.team_pings_size(), 1);
    EXPECT_EQ(snapB.team_pings(0).sender_player_id(), 201u);

    BattleStateS2C snapObs;
    engine.FillTeamPingsForViewer(snapObs, 0);
    EXPECT_EQ(snapObs.team_pings_size(), 0);

    // 回合推进自动清空标点并重置限频配额
    engine.SubmitAction(101, Defend());
    engine.SubmitAction(102, Defend());
    engine.SubmitAction(201, Defend());
    engine.SubmitAction(202, Defend());
    engine.ResolveCurrentRound();

    engine.FillTeamPingsForViewer(snapA, 101);
    EXPECT_EQ(snapA.team_pings_size(), 0);
    ASSERT_EQ(engine.SubmitBattlePing(101, 201, BATTLE_PING_CONTROL, &out), 0u);
    EXPECT_EQ(out.round_index(), 2u);
}

TEST(RankedBattleTest, CombatHighlightsAndStreakBreakerSettlement) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(5);
    dungeon.set_id(5);
    dungeon.set_round_limit(20);
    dungeon.set_time_limit(720);
    dungeon.set_max_team_size(5);

    auto req = MakeRanked1v1Request(403, 5);
    req.set_match_mode(7);
    (*req.mutable_ranked_context()->mutable_member_win_streaks())[101] = 2; // <3 不展示
    (*req.mutable_ranked_context()->mutable_member_win_streaks())[201] = 5; // >=3 展示连胜势并触发破势终结
    // 让 101 在 1 回合内压低血后反杀 201，验证绝境翻盘 + 破势终结 + 承伤统计
    req.mutable_players(0)->set_max_health(1000);
    req.mutable_players(0)->mutable_base_attributes()->set_health(1000);
    req.mutable_players(0)->mutable_base_attributes()->set_attack(600);
    req.mutable_players(0)->mutable_base_attributes()->set_speed(50);
    req.mutable_players(1)->set_max_health(500);
    req.mutable_players(1)->mutable_base_attributes()->set_health(500);
    req.mutable_players(1)->mutable_base_attributes()->set_attack(850);
    req.mutable_players(1)->mutable_base_attributes()->set_speed(150);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    const auto startSnap = engine.BuildStateSnapshot();
    uint32_t streak101 = 99;
    uint32_t streak201 = 99;
    for (const auto& a : startSnap.actors()) {
        if (a.actor_id() == 101) streak101 = a.win_streak();
        if (a.actor_id() == 201) streak201 = a.win_streak();
    }
    EXPECT_EQ(streak101, 0u);
    EXPECT_EQ(streak201, 5u);

    engine.SubmitAction(101, Attack(201));
    engine.SubmitAction(201, Attack(101));
    const auto r1 = engine.ResolveCurrentRound();
    ASSERT_EQ(r1.state().outcome(), BATTLE_OUTCOME_SIDE_A_WIN);

    const auto settleA = engine.BuildSettlement(101);
    const auto settleB = engine.BuildSettlement(201);
    EXPECT_GT(settleA.damage_taken(), 0u);
    EXPECT_GT(settleA.total_damage_dealt(), 0u);
    EXPECT_EQ(settleA.highlight_kind(), BATTLE_HIGHLIGHT_CLUTCH_COMEBACK);
    EXPECT_EQ(settleA.ended_opponent_streak(), 5u);
    EXPECT_EQ(settleB.ended_opponent_streak(), 0u);
}

TEST(GuildWarBattleTest, AiDefenderAutoActsAndRound12EscalationDrawsAtMaxRounds) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(1);
    dungeon.set_id(1);
    dungeon.set_round_limit(14);
    dungeon.set_time_limit(600);
    dungeon.set_max_team_size(5);

    CreateBattleRequest req;
    req.set_battle_id(99001);
    req.set_battle_config_id(1);
    req.set_match_mode(turnbattle::kMatchModeGuildWarSiege);
    req.set_seed(778899);
    auto* actx = req.mutable_activity_context();
    actx->set_kind(BATTLE_ACTIVITY_KIND_GUILD_WAR);
    auto* gw = actx->mutable_guild_war();
    gw->set_war_battle_kind(2);
    gw->set_leyline_id(2);
    gw->set_attacker_guild_id(100);
    gw->set_defender_guild_id(200);

    auto* p1 = req.add_players();
    p1->set_player_id(1001);
    p1->set_team_index(0);
    p1->set_player_name("Attacker");
    p1->set_class_id(1);
    p1->set_level(50);
    p1->set_max_health(900000);
    p1->set_max_mana(2000);
    p1->mutable_base_attributes()->set_health(900000);
    p1->mutable_base_attributes()->set_mana(2000);
    p1->mutable_base_attributes()->set_speed(100);
    p1->set_defense(500);
    auto* item = p1->add_items();
    item->set_item_table_id(1);
    item->set_count(5);

    auto* aiDef = req.add_players();
    aiDef->set_player_id(2001);
    aiDef->set_team_index(1);
    aiDef->set_player_name("GarrisonAI");
    aiDef->set_class_id(2);
    aiDef->set_level(50);
    aiDef->set_ai_controlled(true);
    aiDef->set_max_health(900000);
    aiDef->set_max_mana(2000);
    aiDef->mutable_base_attributes()->set_health(900000);
    aiDef->mutable_base_attributes()->set_mana(2000);
    aiDef->mutable_base_attributes()->set_speed(90);
    aiDef->set_defense(500);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    EXPECT_TRUE(engine.IsGuildWarBattle());
    EXPECT_TRUE(engine.IsAiControlledPlayer(2001));
    EXPECT_FALSE(engine.IsAiControlledPlayer(1001));
    EXPECT_TRUE(engine.SelfItems(1001).empty());
    EXPECT_EQ(engine.EscalationStartRound(), turnbattle::kGuildWarEscalationStartRound);
    EXPECT_NE(engine.ValidateAction(2001, Defend()), kSuccess);
    EXPECT_NE(engine.SetActorAuto(2001, false), 0u);

    // 仅提交进攻方手动指令即可全员就绪（AI 守擂方不阻塞回合），第 12 回合起进入决胜煞气，打满 14 回合判平。
    for (uint32_t round = 1; round <= 14; ++round) {
        if (round == 11) {
            EXPECT_EQ(engine.EscalationStacks(), 0u);
            EXPECT_EQ(engine.EscalationDamagePct(), 100u);
        } else if (round == 12) {
            EXPECT_EQ(engine.EscalationStacks(), 1u);
            EXPECT_EQ(engine.EscalationDamagePct(), 115u);
            EXPECT_EQ(engine.EscalationSupportPct(), 85u);
        } else if (round == 14) {
            EXPECT_EQ(engine.EscalationStacks(), 3u);
            EXPECT_EQ(engine.EscalationDamagePct(), 145u);
            EXPECT_EQ(engine.EscalationSupportPct(), 55u);
        }
        EXPECT_TRUE(engine.SubmitAction(1001, Defend()));
        engine.ResolveCurrentRound();
    }
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_DRAW);
}

TEST(GuildWarBattleTest, RejectsMissingGuildWarActivityContext) {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    auto& dungeon = data->AddDungeon(1);
    dungeon.set_id(1);
    dungeon.set_round_limit(20);
    dungeon.set_max_team_size(5);

    CreateBattleRequest req;
    req.set_battle_id(99002);
    req.set_battle_config_id(1);
    req.set_match_mode(turnbattle::kMatchModeGuildWarPvp);
    req.set_seed(112233);
    auto* p1 = req.add_players();
    p1->set_player_id(101);
    p1->set_team_index(0);
    p1->set_max_health(1000);
    p1->mutable_base_attributes()->set_health(1000);
    auto* p2 = req.add_players();
    p2->set_player_id(201);
    p2->set_team_index(1);
    p2->set_max_health(1000);
    p2->mutable_base_attributes()->set_health(1000);

    TurnBattleEngine engine(data);
    EXPECT_FALSE(engine.Initialize(req));
}

}  // namespace
