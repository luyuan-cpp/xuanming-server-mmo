#include <gtest/gtest.h>

#include <cstdint>
#include <map>
#include <memory>
#include <vector>

#include "constants/turn_battle_constants.h"
#include "memory_battle_data_provider.h"
#include "system/dungeon_monster_ai.h"
#include "system/turn_battle_engine.h"

namespace
{

using turnbattle::BossPhaseId;
using turnbattle::BuildTrainedDefaultMonsterAiProfile;
using turnbattle::DungeonMonsterAiController;
using turnbattle::DungeonMonsterAiProfile;
using turnbattle::MonsterDecisionInput;
using turnbattle::TurnBattleEngine;

constexpr uint32_t kPveTeamMode = 5;
constexpr uint32_t kDungeonId = 41;
constexpr uint32_t kBossTableId = 201;
constexpr uint64_t kBossActorId = turnbattle::kMonsterActorIdBase + 0;
constexpr uint64_t kTankPlayerId = 9101;
constexpr uint64_t kLowHpPlayerId = 9102;
constexpr uint32_t kBossSlamSkillId = 9701;
constexpr uint32_t kCostlySkillId = 9702;

std::shared_ptr<MemoryBattleDataProvider> MakeDungeonProvider()
{
    auto provider = std::make_shared<MemoryBattleDataProvider>();
    provider->AddBattleReward(0);

    auto& slam = provider->AddSkill(kBossSlamSkillId);
    slam.add_targeting_mode(1);
    slam.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    provider->SetSkillDamage(kBossSlamSkillId, 40.0);

    auto& costly = provider->AddSkill(kCostlySkillId);
    costly.add_targeting_mode(1);
    costly.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    auto* cost = costly.add_cost_resource();
    cost->set_cost_resource_id(turnbattle::kSkillCostResourceMana);
    cost->set_cost_resource_cost(20);

    auto& boss = provider->AddMonster(kBossTableId);
    boss.set_health(2000);
    boss.set_strength(25);
    boss.set_speed(50);

    provider->AddDungeon(kDungeonId);
    provider->SetDungeonMonsters(kDungeonId, {kBossTableId});
    return provider;
}

CreateBattleRequest MakeDungeonRequest(uint64_t tankHp, uint64_t lowHp, uint64_t playerAtk = 50)
{
    CreateBattleRequest req;
    req.set_battle_id(88001);
    req.set_battle_config_id(kDungeonId);
    req.set_match_mode(kPveTeamMode);
    req.set_seed(20261120);

    auto addPlayer = [&](uint64_t pid, uint64_t hp, uint64_t speed) {
        auto* snap = req.add_players();
        snap->set_player_id(pid);
        snap->set_player_name("hero");
        snap->set_level(30);
        snap->set_team_index(0);
        snap->set_max_health(1000);
        snap->set_max_mana(100);
        snap->set_physical_attack(playerAtk);
        auto* attr = snap->mutable_base_attributes();
        attr->set_health(hp);
        attr->set_mana(100);
        attr->set_speed(speed);
        attr->set_strength(10);
    };
    addPlayer(kTankPlayerId, tankHp, 120);
    addPlayer(kLowHpPlayerId, lowHp, 150);
    return req;
}

TEST(DungeonMonsterAiTest, TrainedBossFocusesLowHpTargetWithZeroManaSkillAndFiltersCostlySkill)
{
    auto provider = MakeDungeonProvider();
    TurnBattleEngine engine(provider);

    DungeonMonsterAiProfile profile = BuildTrainedDefaultMonsterAiProfile();
    profile.monsterSkillIds = {kBossSlamSkillId, kCostlySkillId};
    engine.SetMonsterAiProfiles({{kBossTableId, profile}});

    const auto req = MakeDungeonRequest(/*tankHp=*/1000, /*lowHp=*/250);
    ASSERT_TRUE(engine.Initialize(req));
    EXPECT_EQ(BossPhaseId::kPhaseOpening, engine.MonsterAiPhaseOf(kBossActorId));

    // 开局快照里怪物只挂上了零耗蓝的 kBossSlamSkillId,耗蓝技能被闸门过滤。
    const auto initSnap = engine.BuildStateSnapshot();
    bool checkedBossSkills = false;
    for (const auto& actor : initSnap.actors())
    {
        if (actor.actor_id() == kBossActorId)
        {
            checkedBossSkills = true;
            ASSERT_EQ(1, actor.skill_table_ids_size());
            EXPECT_EQ(kBossSlamSkillId, actor.skill_table_ids(0));
        }
    }
    ASSERT_TRUE(checkedBossSkills);

    BattleAction defend;
    defend.set_action_type(BATTLE_ACTION_DEFEND);
    engine.SubmitAction(kTankPlayerId, defend);
    engine.SubmitAction(kLowHpPlayerId, defend);

    const auto turn = engine.ResolveCurrentRound();
    bool bossUsedSkillOnLowHp = false;
    for (const auto& ev : turn.events())
    {
        if (ev.source_id() == kBossActorId && ev.event_type() == BATTLE_EVENT_SKILL &&
            ev.skill_table_id() == kBossSlamSkillId && ev.target_id() == kLowHpPlayerId)
        {
            bossUsedSkillOnLowHp = true;
        }
    }
    EXPECT_TRUE(bossUsedSkillOnLowHp) << "训练后的副本 Boss 应优先用零耗蓝技能集火残血高速目标";
}

TEST(DungeonMonsterAiTest, BossHfsmTransitionsThroughEnrageAndDesperateWithoutReenteringCombat)
{
    DungeonMonsterAiProfile profile = BuildTrainedDefaultMonsterAiProfile();
    profile.enrageHpPermyriad = 6000;
    profile.desperateHpPermyriad = 3000;
    profile.emergencyDefendHpPermyriad = 2500;
    profile.defendCooldownRounds = 3;

    DungeonMonsterAiController controller(profile);
    EXPECT_EQ(BossPhaseId::kPhaseOpening, controller.CurrentPhase());
    EXPECT_EQ(1u, controller.CombatEnterCount());
    EXPECT_EQ(0u, controller.CombatExitCount());

    auto makeInput = [](uint32_t round, uint32_t hpPermyriad) {
        MonsterDecisionInput in;
        in.roundIndex = round;
        in.selfHpPermyriad = hpPermyriad;
        in.selfAlive = true;
        in.selfCanDefend = true;
        turnbattle::MonsterActionCandidate cand;
        cand.action.set_action_type(BATTLE_ACTION_ATTACK);
        cand.action.set_target_id(kTankPlayerId);
        cand.formationSlot = 0;
        cand.features = {5000, 10000, 5000, 0, 0, 10000};
        in.candidates.push_back(cand);
        return in;
    };

    // Round 1: 50% HP -> Opening 切到 Enrage,共享父态 Combat 不退出不重入。
    const auto a1 = controller.DecideAction(makeInput(1, 5000));
    ASSERT_TRUE(a1.has_value());
    EXPECT_EQ(BATTLE_ACTION_ATTACK, a1->action_type());
    EXPECT_EQ(BossPhaseId::kPhaseEnrage, controller.CurrentPhase());
    EXPECT_EQ(1u, controller.CombatEnterCount());
    EXPECT_EQ(0u, controller.CombatExitCount());

    // Round 2: 20% HP -> Enrage 切到 Desperate,且行为树高优分支触发 DEFEND。
    const auto a2 = controller.DecideAction(makeInput(2, 2000));
    ASSERT_TRUE(a2.has_value());
    EXPECT_EQ(BATTLE_ACTION_DEFEND, a2->action_type());
    EXPECT_EQ(BossPhaseId::kPhaseDesperate, controller.CurrentPhase());
    EXPECT_EQ(1u, controller.CombatEnterCount());
    EXPECT_EQ(0u, controller.CombatExitCount());

    // Round 3: 仍在 3 回合防御冷却内,回落到效用打分分支出击。
    const auto a3 = controller.DecideAction(makeInput(3, 2000));
    ASSERT_TRUE(a3.has_value());
    EXPECT_EQ(BATTLE_ACTION_ATTACK, a3->action_type());

    // Round 5: 冷却结束(2 + 3 = 5),再次触发防御自保。
    const auto a5 = controller.DecideAction(makeInput(5, 2000));
    ASSERT_TRUE(a5.has_value());
    EXPECT_EQ(BATTLE_ACTION_DEFEND, a5->action_type());

    // 阵亡事件冒泡至 Root -> Dead,退出 Combat 一次。
    controller.NotifyDead();
    EXPECT_EQ(BossPhaseId::kDead, controller.CurrentPhase());
    EXPECT_FALSE(controller.IsInCombat());
    EXPECT_EQ(1u, controller.CombatExitCount());
}

TEST(DungeonMonsterAiTest, SameSeedAndAiProfileProducesByteIdenticalTurnResults)
{
    auto provider = MakeDungeonProvider();
    DungeonMonsterAiProfile profile = BuildTrainedDefaultMonsterAiProfile();
    profile.monsterSkillIds = {kBossSlamSkillId};

    auto runOnce = [&]() {
        TurnBattleEngine engine(provider);
        engine.SetMonsterAiProfiles({{0, profile}});
        EXPECT_TRUE(engine.Initialize(MakeDungeonRequest(1000, 600, 180)));
        std::vector<std::string> rounds;
        for (int r = 0; r < 3 && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++r)
        {
            BattleAction atk;
            atk.set_action_type(BATTLE_ACTION_ATTACK);
            atk.set_target_id(kBossActorId);
            engine.SubmitAction(kTankPlayerId, atk);
            engine.SubmitAction(kLowHpPlayerId, atk);
            rounds.push_back(engine.ResolveCurrentRound().SerializeAsString());
        }
        return rounds;
    };

    const auto first = runOnce();
    const auto second = runOnce();
    ASSERT_FALSE(first.empty());
    EXPECT_EQ(first, second);
}

TEST(DungeonMonsterAiTest, UnconfiguredBattleLeavesDefaultMonsterBehaviorUnchanged)
{
    auto provider = MakeDungeonProvider();
    TurnBattleEngine engineA(provider);
    TurnBattleEngine engineB(provider);
    const auto req = MakeDungeonRequest(1000, 500);
    ASSERT_TRUE(engineA.Initialize(req));
    ASSERT_TRUE(engineB.Initialize(req));
    EXPECT_FALSE(engineA.MonsterAiPhaseOf(kBossActorId).has_value());

    BattleAction defend;
    defend.set_action_type(BATTLE_ACTION_DEFEND);
    engineA.SubmitAction(kTankPlayerId, defend);
    engineA.SubmitAction(kLowHpPlayerId, defend);
    engineB.SubmitAction(kTankPlayerId, defend);
    engineB.SubmitAction(kLowHpPlayerId, defend);
    EXPECT_EQ(engineA.ResolveCurrentRound().SerializeAsString(),
              engineB.ResolveCurrentRound().SerializeAsString());
}

} // namespace
