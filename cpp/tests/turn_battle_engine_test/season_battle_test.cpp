// Battle 引擎侧赛季技能复核与单槽赛季印（方案 A）机制回归测试（docs/design/season-skills-and-seal-design.md）。

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <memory>

#include "memory_battle_data_provider.h"
#include "system/turn_battle_engine.h"

namespace {

using turnbattle::BattleEffectOp;
using turnbattle::TurnBattleEngine;

constexpr uint64_t kHero = 101;
constexpr uint64_t kEnemy = 201;

BattleAction Command(eBattleActionType type, uint64_t target = 0, uint32_t skill = 0) {
    BattleAction action;
    action.set_action_type(type);
    action.set_target_id(target);
    action.set_skill_table_id(skill);
    return action;
}

const BattleActorState& ActorOf(const BattleStateS2C& state, uint64_t id) {
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() == id) return actor;
    }
    ADD_FAILURE() << "Missing actor " << id;
    return BattleActorState::default_instance();
}

uint64_t ShieldCapacity(const BattleStateS2C& state, uint64_t actorId, uint32_t buffId) {
    for (const auto& buff : ActorOf(state, actorId).buffs()) {
        if (buff.buff_table_id() == buffId) return buff.remaining_capacity();
    }
    return 0;
}

class SeasonBattleTest : public ::testing::Test {
protected:
    void SetUp() override {
        data = std::make_shared<MemoryBattleDataProvider>();
        request.set_battle_id(91001);
        request.set_match_mode(3);
        request.set_seed(7);
        for (const uint64_t id : {kHero, kEnemy}) {
            auto* p = request.add_players();
            p->set_player_id(id);
            p->set_team_index(id == kHero ? 0 : 1);
            p->set_level(30);
            p->set_max_health(10000);
            p->set_max_mana(10000);
            p->mutable_base_attributes()->set_health(10000);
            p->mutable_base_attributes()->set_mana(10000);
            p->mutable_base_attributes()->set_speed(id == kHero ? 300 : 200);
            p->set_physical_attack(1000);
            p->set_magic_attack(1000);
            p->set_defense(100);
        }
    }

    BattlePlayerSnapshot& Player(uint64_t id) {
        for (auto& p : *request.mutable_players()) {
            if (p.player_id() == id) return p;
        }
        return *request.mutable_players(0);
    }

    SkillTable& AddModernSkill(uint32_t id, uint32_t seasonId, uint32_t damageType, double flatDamage) {
        auto& row = data->AddSkill(id);
        row.Clear();
        row.set_id(id);
        row.set_battle_model(1);
        row.set_caster_mask(1);
        row.set_ability_kind(1);
        row.set_unlock_level(1);
        row.set_max_rank(10);
        row.set_target_team(1);
        row.set_max_targets(1);
        row.set_pet_max_targets(1);
        row.add_skill_type(1);
        row.set_damage_type(damageType);
        row.set_mana_percent(0.05);
        row.set_season_id(seasonId);
        row.add_effect(id * 10);
        auto& buff = data->AddBuff(id * 10);
        buff.Clear();
        buff.set_id(id * 10);
        buff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Damage));
        buff.set_flat_value(flatDamage);
        data->SetSkillDamage(id, flatDamage);
        return row;
    }

    void AddShieldBuff(uint64_t actorId, uint32_t buffId, uint64_t capacity) {
        auto& row = data->AddBuff(buffId);
        row.Clear();
        row.set_id(buffId);
        row.set_infinite_duration(1);
        row.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
        row.set_effect_priority(20);
        auto* entry = Player(actorId).add_buffs();
        entry->set_buff_id(buffId);
        entry->set_buff_table_id(buffId);
        entry->set_caster_id(actorId);
        entry->set_layer(1);
        entry->set_remaining_capacity(capacity);
        entry->set_snapshot_value(capacity);
        entry->set_tick_round(999);
    }

    std::shared_ptr<MemoryBattleDataProvider> data;
    CreateBattleRequest request;
};

} // namespace

TEST_F(SeasonBattleTest, RejectsOverCapSeasonStatsOrInvalidSeasonSeal) {
    // 超过 500 bps 硬上限拒绝开局
    Player(kHero).mutable_equipment_effects()->set_season_max_health_bps(501);
    TurnBattleEngine overStat(data);
    EXPECT_FALSE(overStat.Initialize(request));

    Player(kHero).mutable_equipment_effects()->set_season_max_health_bps(300);
    auto* badSeal = Player(kHero).mutable_equipment_effects()->mutable_season_seal();
    badSeal->set_season_id(1);
    badSeal->set_seal_id(0); // 非法 seal_id
    badSeal->set_effect_id(1);
    badSeal->set_bonus_bps(1000);
    badSeal->set_per_round_limit(1);
    TurnBattleEngine badSealEngine(data);
    EXPECT_FALSE(badSealEngine.Initialize(request));
}

TEST_F(SeasonBattleTest, ValidatesSeasonalSkillAgainstFrozenActiveSeasonId) {
    AddModernSkill(15001, /*seasonId=*/1, /*damageType=*/1, 500);
    Player(kHero).set_active_season_id(2); // 非第 1 季快照伪造第 1 季技能
    Player(kHero).add_skill_table_ids(15001);
    (*Player(kHero).mutable_skill_ranks())[15001] = 10;

    TurnBattleEngine wrongSeason(data);
    ASSERT_TRUE(wrongSeason.Initialize(request));
    const auto stateWrong = wrongSeason.BuildStateSnapshot();
    EXPECT_TRUE(ActorOf(stateWrong, kHero).skill_table_ids().empty()) << "非当季技能被引擎剔除";

    // 匹配冻结赛季 1 时放行
    Player(kHero).set_active_season_id(1);
    TurnBattleEngine rightSeason(data);
    ASSERT_TRUE(rightSeason.Initialize(request));
    const auto stateRight = rightSeason.BuildStateSnapshot();
    ASSERT_EQ(ActorOf(stateRight, kHero).skill_table_ids_size(), 1);
    EXPECT_EQ(ActorOf(stateRight, kHero).skill_table_ids(0), 15001u);
}

TEST_F(SeasonBattleTest, SeasonSealShieldBreakExecutesAndTagsSeasonSealEvent) {
    AddModernSkill(15001, 1, /*physical=*/1, 1000);
    Player(kHero).set_active_season_id(1);
    Player(kHero).add_skill_table_ids(15001);
    (*Player(kHero).mutable_skill_ranks())[15001] = 1;
    auto* seal = Player(kHero).mutable_equipment_effects()->mutable_season_seal();
    seal->set_season_id(1);
    seal->set_seal_id(101);
    seal->set_effect_id(1);
    seal->set_bonus_bps(1000);
    seal->set_per_round_limit(1);
    AddShieldBuff(kEnemy, 9901, 5000);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    engine.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kEnemy, 15001));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
    const auto turn = engine.ResolveCurrentRound();

    bool sawSealEvent = false;
    for (const auto& ev : turn.events()) {
        if (ev.event_type() == BATTLE_EVENT_ARTIFACT && ev.season_seal_id() == 101u) {
            EXPECT_EQ(ev.artifact_item_id(), 0u);
            EXPECT_EQ(ev.artifact_effect_id(), 1u);
            EXPECT_GT(ev.value(), 0u);
            sawSealEvent = true;
        }
    }
    EXPECT_TRUE(sawSealEvent);
    EXPECT_LT(ShieldCapacity(engine.BuildStateSnapshot(), kEnemy, 9901), 5000u);
}

TEST_F(SeasonBattleTest, SeasonSealOpeningDamageReductionAndDefendManaRestoreCoexistWithTwoArtifacts) {
    // kHero 带满 2 件永久归元葫 + 1 枚聚息援印(104)，验证三者共存且互不冲突
    for (uint64_t itemId : {uint64_t{501}, uint64_t{502}}) {
        auto* art = Player(kHero).mutable_equipment_effects()->add_artifacts();
        art->set_item_id(itemId);
        art->set_effect_id(7);
        art->set_bonus_bps(500);
        art->set_per_round_limit(1);
        art->set_battle_limit(3);
    }
    auto* heroSeal = Player(kHero).mutable_equipment_effects()->mutable_season_seal();
    heroSeal->set_season_id(1);
    heroSeal->set_seal_id(104);
    heroSeal->set_effect_id(7);
    heroSeal->set_bonus_bps(400);
    heroSeal->set_per_round_limit(1);
    heroSeal->set_battle_limit(3);
    Player(kHero).mutable_base_attributes()->set_mana(5000); // 缺 5000 蓝

    // kEnemy 带固阵守印(103)：前两回合首伤减免 10%，上限入战最大气血 3% (300)
    auto* enemySeal = Player(kEnemy).mutable_equipment_effects()->mutable_season_seal();
    enemySeal->set_season_id(1);
    enemySeal->set_seal_id(103);
    enemySeal->set_effect_id(6);
    enemySeal->set_bonus_bps(1000);
    enemySeal->set_cap_bps(300);
    enemySeal->set_active_rounds(2);
    enemySeal->set_battle_limit(1);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    // 客户端状态快照必须清除 season_seal_state
    BattleStateS2C publicState = engine.BuildStateSnapshot();
    TurnBattleEngine::StripEngineOnlyState(publicState);
    EXPECT_FALSE(ActorOf(publicState, kHero).has_season_seal_state());

    // Round 1: kHero 防御（回蓝 500+500+400 = 1400），kEnemy 普攻
    engine.SubmitAction(kHero, Command(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_ATTACK, kHero));
    const auto r1 = engine.ResolveCurrentRound();
    EXPECT_EQ(ActorOf(engine.BuildStateSnapshot(), kHero).attributes().mana(), 6400u);
    bool sawHeroSeal = false;
    for (const auto& ev : r1.events()) {
        if (ev.event_type() == BATTLE_EVENT_ARTIFACT && ev.season_seal_id() == 104u) {
            EXPECT_EQ(ev.value(), 400u);
            sawHeroSeal = true;
        }
    }
    EXPECT_TRUE(sawHeroSeal);

    // Round 2: kHero 攻击 kEnemy，触发 kEnemy 的固阵守印(103)
    engine.SubmitAction(kHero, Command(BATTLE_ACTION_ATTACK, kEnemy));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
    const auto r2 = engine.ResolveCurrentRound();
    bool sawEnemySeal = false;
    for (const auto& ev : r2.events()) {
        if (ev.event_type() == BATTLE_EVENT_ARTIFACT && ev.season_seal_id() == 103u) {
            EXPECT_GT(ev.value(), 0u);
            EXPECT_LE(ev.value(), 300u) << "减伤受入战最大气血 3% 上限约束";
            sawEnemySeal = true;
        }
    }
    EXPECT_TRUE(sawEnemySeal);
}
