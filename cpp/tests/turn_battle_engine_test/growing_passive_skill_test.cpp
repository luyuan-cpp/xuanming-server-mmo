// 率土式“每回合叠加至战斗结束（5 层封顶）”3 个永久成长战法战斗引擎单测：
// 12020 愈战愈勇（物理直伤）、12021 深谋远虑（法术直伤）、12022 步步为营（自身防御）。
#include <gtest/gtest.h>

#include <cstdint>
#include <filesystem>
#include <memory>

#include "../test_config_helper.h"
#include "constants/battle_effect_constants.h"
#include "memory_battle_data_provider.h"
#include "system/combat_damage_rules.h"
#include "system/turn_battle_engine.h"
#include "table/code/buff_table.h"
#include "table/code/skill_table.h"

namespace {
using turnbattle::BattleEffectOp;
using turnbattle::BattleModifier;
using turnbattle::TurnBattleEngine;

constexpr uint64_t kHero = 101;
constexpr uint64_t kAlly = 102;
constexpr uint64_t kEnemy = 201;

constexpr uint32_t kSkillGrowingPhys = 12020;  // 愈战愈勇
constexpr uint32_t kSkillGrowingMagic = 12021; // 深谋远虑
constexpr uint32_t kSkillGrowingDef = 12022;   // 步步为营
constexpr uint32_t kSkillO01 = 14001;          // 后发制人（全队 OutgoingDamage +5%×3）
constexpr uint32_t kSkillA14 = 10014;          // 铁壁/加防
constexpr uint32_t kSkillA34 = 10034;          // 驱散增益
constexpr uint32_t kSkillA26 = 10026;          // 生生不息（或使用自建测试复活技）

constexpr uint32_t kBuffGrowingPhys = 120200;
constexpr uint32_t kBuffGrowingMagic = 120210;
constexpr uint32_t kBuffGrowingDef = 120220;

constexpr uint32_t kTestIdleSkill = 88000;
constexpr uint32_t kTestPhysStrike = 88001;
constexpr uint32_t kTestMagicStrike = 88002;
constexpr uint32_t kTestKillStrike = 88003;
constexpr uint32_t kTestReviveSkill = 88004;

BattleAction Cast(uint32_t skillId, uint64_t targetId) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_SKILL);
    action.set_skill_table_id(skillId);
    action.set_target_id(targetId);
    return action;
}

const BattleActorState& FindActorState(const BattleStateS2C& state, uint64_t id) {
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() == id) return actor;
    }
    ADD_FAILURE() << "missing actor " << id;
    return BattleActorState::default_instance();
}

uint32_t FindBuffStacks(const BattleActorState& actor, uint32_t buffTableId) {
    for (const auto& buff : actor.buffs()) {
        if (buff.buff_table_id() == buffTableId) return buff.stacks();
    }
    return 0;
}

bool HasBuff(const BattleActorState& actor, uint32_t buffTableId) {
    for (const auto& buff : actor.buffs()) {
        if (buff.buff_table_id() == buffTableId) return true;
    }
    return false;
}

uint64_t FirstDamageTo(const TurnResultS2C& result, uint64_t sourceId, uint64_t targetId) {
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_DAMAGE &&
            event.source_id() == sourceId && event.target_id() == targetId) {
            return event.value();
        }
    }
    return 0;
}

class GrowingPassiveSkillTest : public testing::Test {
protected:
    static inline std::filesystem::path oldDir;
    static inline bool loaded = false;

    static void SetUpTestSuite() {
        oldDir = std::filesystem::current_path();
        loaded = test_config::FindAndLoadTestConfig(0, nullptr);
        if (loaded) {
            SkillTableManager::Instance().Load();
            BuffTableManager::Instance().Load();
        }
    }
    static void TearDownTestSuite() { std::filesystem::current_path(oldDir); }

    void SetUp() override {
        ASSERT_TRUE(loaded);
        data = std::make_shared<MemoryBattleDataProvider>();
        data->AddBattleReward(0);
        data->AddPet(1);
        SkillTableManager::Instance().SetDamageParam({30.0});
        for (const auto& [id, row] : SkillTableManager::Instance().GetIdMap()) {
            data->AddSkill(id) = *row;
            data->SetSkillDamage(id, SkillTableManager::Instance().GetDamage(id));
        }
        for (const auto& [id, row] : BuffTableManager::Instance().GetIdMap()) {
            data->AddBuff(id) = *row;
        }

        // 添加纯固定数值的测试招式，精准测量每层 +5% 比例与加算不复利。
        auto addTestSkill = [&](uint32_t id, uint32_t targetTeam, uint32_t dmgType,
                                BattleEffectOp op, double damageValue, bool allowDead = false) {
            auto& s = data->AddSkill(id);
            s.set_battle_model(1);
            s.set_caster_mask(3);
            s.set_ability_kind(1);
            s.set_target_team(targetTeam);
            s.set_max_targets(1);
            s.set_pet_max_targets(1);
            s.set_unlock_level(1);
            s.set_max_rank(10);
            s.add_skill_type(1);
            s.set_damage_type(dmgType);
            if (allowDead) s.set_allow_dead_target(1);
            s.add_effect(id);
            auto& b = data->AddBuff(id);
            b.set_battle_op(static_cast<uint32_t>(op));
            b.set_max_layer(1);
            if (op == BattleEffectOp::Damage) {
                data->SetSkillDamage(id, damageValue);
            } else if (op == BattleEffectOp::ReviveMark) {
                b.set_keep_on_death(1);
                b.set_tick_count(1);
                b.set_duration_rounds(1);
                b.set_tick_on_apply(1);
                b.set_secondary_ratio(0.5);
            }
        };
        addTestSkill(kTestIdleSkill, 3, 0, BattleEffectOp::RestoreMana, 0.0);
        addTestSkill(kTestPhysStrike, 1, combatdamage::kPhysicalDamage, BattleEffectOp::Damage, 1000.0);
        addTestSkill(kTestMagicStrike, 1, combatdamage::kMagicDamage, BattleEffectOp::Damage, 1000.0);
        addTestSkill(kTestKillStrike, 1, combatdamage::kPhysicalDamage, BattleEffectOp::Damage, 100000.0);
        addTestSkill(kTestReviveSkill, 2, 0, BattleEffectOp::ReviveMark, 0.0, true);
    }

    CreateBattleRequest BaseRequest(uint64_t heroDefense = 0) {
        CreateBattleRequest req;
        req.set_battle_id(88001);
        req.set_match_mode(3);
        req.set_seed(20261124);
        auto addPlayer = [&](uint64_t pid, uint32_t team, uint32_t speed, uint64_t def) {
            auto* p = req.add_players();
            p->set_player_id(pid);
            p->set_team_index(team);
            p->set_level(30);
            p->set_class_id(1);
            p->set_max_health(20000);
            p->set_max_mana(5000);
            p->set_physical_attack(1000);
            p->set_magic_attack(1000);
            p->set_defense(def);
            p->mutable_base_attributes()->set_health(20000);
            p->mutable_base_attributes()->set_mana(5000);
            p->mutable_base_attributes()->set_speed(speed);
            for (const auto sid : {kTestIdleSkill, kTestPhysStrike, kTestMagicStrike, kTestKillStrike, kTestReviveSkill}) {
                (*p->mutable_skill_ranks())[sid] = 1;
                p->add_skill_table_ids(sid);
            }
        };
        addPlayer(kHero, 0, 300, heroDefense);
        addPlayer(kAlly, 0, 250, 0);
        addPlayer(kEnemy, 1, 100, 0);
        return req;
    }

    std::shared_ptr<MemoryBattleDataProvider> data;
};

TEST_F(GrowingPassiveSkillTest, PhysicalAndMagicPassivesStackToFiveCapAndNeverCrossApply) {
    auto req = BaseRequest();
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillGrowingPhys] = 1;
    // 给友军挂深谋远虑验证法术对称生效、物理不吃深谋远虑。
    (*req.mutable_players(1)->mutable_skill_ranks())[kSkillGrowingMagic] = 1;

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    // 第 1..6 回合：rank=1 的基础系数为 0.6（1000*0.6=600），每回合开始叠 1 层（+5%），第 5 回合封顶 5 层（+25%=750），第 6 回合保持 750。
    const std::array<uint64_t, 6> expectedScaled = {630, 660, 690, 720, 750, 750};
    for (int round = 0; round < 6; ++round) {
        engine.SubmitAction(kHero, Cast(kTestPhysStrike, kEnemy));
        engine.SubmitAction(kAlly, Cast(kTestMagicStrike, kEnemy));
        engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
        const auto res = engine.ResolveCurrentRound();
        const uint32_t expectedStacks = std::min<uint32_t>(5u, static_cast<uint32_t>(round + 1));
        EXPECT_EQ(expectedStacks, FindBuffStacks(FindActorState(res.state(), kHero), kBuffGrowingPhys));
        EXPECT_EQ(expectedStacks, FindBuffStacks(FindActorState(res.state(), kAlly), kBuffGrowingMagic));
        EXPECT_EQ(expectedScaled[round], FirstDamageTo(res, kHero, kEnemy)) << "round=" << (round + 1);
        EXPECT_EQ(expectedScaled[round], FirstDamageTo(res, kAlly, kEnemy)) << "round=" << (round + 1);
    }

    // 第 7 回合互换伤害类型：愈战愈勇不加成法术，深谋远虑不加成物理（维持基础 600）。
    engine.SubmitAction(kHero, Cast(kTestMagicStrike, kEnemy));
    engine.SubmitAction(kAlly, Cast(kTestPhysStrike, kEnemy));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    const auto crossRes = engine.ResolveCurrentRound();
    EXPECT_EQ(600u, FirstDamageTo(crossRes, kHero, kEnemy));
    EXPECT_EQ(600u, FirstDamageTo(crossRes, kAlly, kEnemy));
}

TEST_F(GrowingPassiveSkillTest, StacksAdditivelyWithO01WithoutCompounding) {
    auto req = BaseRequest();
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillGrowingPhys] = 1;
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillO01] = 1;

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    // O01 从第 3 回合开始每回合 +5%（上限 3 层 = +15%），愈战愈勇从第 1 回合开始 +5%（上限 5 层 = +25%）。
    // 第 5 回合：愈战愈勇 5 层（+25%）+ O01 3 层（+15%）= 加算 +40% -> 600*1.40=840（而非复利 600*1.25*1.15=862）。
    for (int round = 1; round <= 5; ++round) {
        engine.SubmitAction(kHero, Cast(kTestPhysStrike, kEnemy));
        engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
        engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
        const auto res = engine.ResolveCurrentRound();
        if (round == 5) {
            EXPECT_EQ(840u, FirstDamageTo(res, kHero, kEnemy));
        }
    }
}

TEST_F(GrowingPassiveSkillTest, DefensePassiveGrowsToFiveAndRespectsSixtyPercentCap) {
    // 使用高基础防御验证步步为营随回合降低承伤，且叠满 5 层仍受 60% 减伤红线封顶（600 基础伤承伤不低于 240）。
    auto req = BaseRequest(/*heroDefense=*/500);
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillGrowingDef] = 1;

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    uint64_t r1Damage = 0;
    uint64_t r5Damage = 0;
    uint64_t r6Damage = 0;
    for (int round = 1; round <= 6; ++round) {
        engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
        engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
        engine.SubmitAction(kEnemy, Cast(kTestPhysStrike, kHero));
        const auto res = engine.ResolveCurrentRound();
        const uint64_t dmg = FirstDamageTo(res, kEnemy, kHero);
        if (round == 1) r1Damage = dmg;
        if (round == 5) r5Damage = dmg;
        if (round == 6) r6Damage = dmg;
    }
    EXPECT_LT(r5Damage, r1Damage);
    EXPECT_EQ(r5Damage, r6Damage); // 5 层封顶不再涨。

    // 极端高防（20000）+ 步步为营 5 层：60% 减伤封顶保证 600 直伤承伤恒 >= 240。
    auto capReq = BaseRequest(/*heroDefense=*/20000);
    (*capReq.mutable_players(0)->mutable_skill_ranks())[kSkillGrowingDef] = 1;
    TurnBattleEngine capEngine(data);
    ASSERT_TRUE(capEngine.Initialize(capReq));
    for (int round = 1; round <= 5; ++round) {
        capEngine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
        capEngine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
        capEngine.SubmitAction(kEnemy, Cast(kTestPhysStrike, kHero));
        const auto res = capEngine.ResolveCurrentRound();
        EXPECT_GE(FirstDamageTo(res, kEnemy, kHero), 240u);
    }
}

TEST_F(GrowingPassiveSkillTest, UndispellableByA34AndPausesWhileDeadThenKeepsStacksAfterRevive) {
    auto req = BaseRequest();
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillGrowingPhys] = 1;
    (*req.mutable_players(2)->mutable_skill_ranks())[kSkillA34] = 1;
    req.mutable_players(2)->add_skill_table_ids(kSkillA34);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    // 第 1 回合：敌方施放 A34 驱散，愈战愈勇必须保留且 1 层生效。
    engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kSkillA34, kHero));
    auto res = engine.ResolveCurrentRound();
    EXPECT_TRUE(HasBuff(FindActorState(res.state(), kHero), kBuffGrowingPhys));
    EXPECT_EQ(1u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffGrowingPhys));

    // 第 2 回合：Hero 叠到 2 层后被敌方秒杀（keep_on_death=1 保留 2 层）。
    engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestKillStrike, kHero));
    res = engine.ResolveCurrentRound();
    ASSERT_TRUE(FindActorState(res.state(), kHero).is_dead())
        << "hp=" << FindActorState(res.state(), kHero).attributes().health()
        << " dmg=" << FirstDamageTo(res, kEnemy, kHero);
    EXPECT_TRUE(HasBuff(FindActorState(res.state(), kHero), kBuffGrowingPhys));
    EXPECT_EQ(2u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffGrowingPhys));

    // 第 3 回合：Hero 阵亡期间回合开始暂停叠层（仍保持 2 层），友军本回合将其复活。
    engine.SubmitAction(kAlly, Cast(kTestReviveSkill, kHero));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    res = engine.ResolveCurrentRound();
    ASSERT_FALSE(FindActorState(res.state(), kHero).is_dead());
    EXPECT_EQ(2u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffGrowingPhys));

    // 第 4 回合：复活存活后恢复叠层（2 -> 3 层 = 600*1.15=690 物理伤害）。
    engine.SubmitAction(kHero, Cast(kTestPhysStrike, kEnemy));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    res = engine.ResolveCurrentRound();
    EXPECT_EQ(3u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffGrowingPhys));
    EXPECT_EQ(690u, FirstDamageTo(res, kHero, kEnemy));
}

} // namespace
