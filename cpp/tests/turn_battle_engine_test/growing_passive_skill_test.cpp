// 率土式持续叠加至战斗结束永久成长战法战斗引擎单测：
// 卷 3 原有（每回合 +5%、5 层封顶）：12020 愈战愈勇、12021 深谋远虑、12022 步步为营；
// 卷 7 新增（交互触发每层 +1%、伤害类 300 层 / 机制类 40~50 层封顶）：12023 承锋蓄势、12024 循隙破甲、12025 蚀骨愈深、12026 磐石道心、12027 厚泽蓄灵、12028 兽威渐盛。
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
constexpr uint32_t kSkillTakeMomentum = 12023; // 承锋蓄势
constexpr uint32_t kSkillPenetration = 12024;  // 循隙破甲
constexpr uint32_t kSkillDotGrowth = 12025;    // 蚀骨愈深
constexpr uint32_t kSkillCtrlResist = 12026;   // 磐石道心
constexpr uint32_t kSkillShieldGrow = 12027;   // 厚泽蓄灵
constexpr uint32_t kSkillPetGrowth = 12028;    // 兽威渐盛
constexpr uint32_t kSkillO01 = 14001;          // 后发制人（全队 OutgoingDamage +5%×3）
constexpr uint32_t kSkillA14 = 10014;          // 铁壁/加防
constexpr uint32_t kSkillA34 = 10034;          // 驱散增益
constexpr uint32_t kSkillA36 = 10036;          // 同袍共济（援护分担）
constexpr uint32_t kSkillA40 = 10040;          // 太清灵盾（独立容量护盾）
constexpr uint32_t kSkillB09 = 11009;          // 游说退敌（劝退敌宠）
constexpr uint32_t kSkillA26 = 10026;          // 生生不息（或使用自建测试复活技）

constexpr uint32_t kBuffGrowingPhys = 120200;
constexpr uint32_t kBuffGrowingMagic = 120210;
constexpr uint32_t kBuffGrowingDef = 120220;
constexpr uint32_t kBuffTakeMomentum = 120230;
constexpr uint32_t kBuffPenetration = 120240;
constexpr uint32_t kBuffDotGrowth = 120250;
constexpr uint32_t kBuffCtrlResist = 120260;
constexpr uint32_t kBuffShieldGrow = 120270;
constexpr uint32_t kBuffPetGrowth = 120280;

constexpr uint32_t kTestIdleSkill = 88000;
constexpr uint32_t kTestPhysStrike = 88001;
constexpr uint32_t kTestMagicStrike = 88002;
constexpr uint32_t kTestKillStrike = 88003;
constexpr uint32_t kTestReviveSkill = 88004;
constexpr uint32_t kTestPoisonSkill = 88005;
constexpr uint32_t kTestSealActiveSkill = 88006;

BattleAction BasicAttack(uint64_t targetId) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_ATTACK);
    action.set_target_id(targetId);
    return action;
}

uint64_t FindShieldCapacity(const BattleActorState& actor, uint32_t skillTableId) {
    for (const auto& buff : actor.buffs()) {
        if (buff.source_skill_id() == skillTableId && buff.remaining_capacity() > 0) {
            return buff.remaining_capacity();
        }
    }
    return 0;
}

uint64_t FirstPeriodicDamageTo(const TurnResultS2C& result, uint64_t sourceId, uint64_t targetId) {
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_DAMAGE &&
            event.source_id() == sourceId && event.target_id() == targetId &&
            event.damage_origin() == static_cast<uint32_t>(turnbattle::BattleDamageOrigin::Periodic)) {
            return event.value();
        }
    }
    return 0;
}

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

        // 纯固定每跳 100 伤害的 100% 必中灼烧技能（用于精准测量蚀骨愈深每层 +6% 持续伤害，且同步验证无 required_tag 覆盖毒/灼烧）。
        {
            auto& s = data->AddSkill(kTestPoisonSkill);
            s.set_battle_model(1); s.set_caster_mask(3); s.set_ability_kind(1);
            s.set_target_team(1); s.set_max_targets(1); s.set_pet_max_targets(1);
            s.set_unlock_level(1); s.set_max_rank(10); s.add_skill_type(1); s.add_effect(kTestPoisonSkill);
            auto& b = data->AddBuff(kTestPoisonSkill);
            b.set_battle_op(static_cast<uint32_t>(BattleEffectOp::PeriodicDamage));
            b.set_state_kind(static_cast<uint32_t>(turnbattle::BattleStateKind::Burn));
            b.set_max_layer(1); b.set_flat_value(100.0); b.set_tick_count(6);
            b.set_duration_rounds(6); b.set_tick_on_apply(1);
            b.set_application_chance(0.0); // 灼烧(kind=8)走必中分支，精准测量每跳数值。
            (*b.mutable_tag())["Debuff"] = true; (*b.mutable_tag())["Burn"] = true;
        }
        // 50% 基础命中率的封主动测试技能（用于验证磐石道心全控抵抗对封主动生效）。
        {
            auto& s = data->AddSkill(kTestSealActiveSkill);
            s.set_battle_model(1); s.set_caster_mask(3); s.set_ability_kind(1);
            s.set_target_team(1); s.set_max_targets(1); s.set_pet_max_targets(1);
            s.set_unlock_level(1); s.set_max_rank(10); s.add_skill_type(1); s.add_effect(kTestSealActiveSkill);
            auto& b = data->AddBuff(kTestSealActiveSkill);
            b.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Control));
            b.set_state_kind(static_cast<uint32_t>(turnbattle::BattleStateKind::SealActive));
            b.set_max_layer(1); b.set_duration_rounds(1); b.set_application_chance(0.5);
            (*b.mutable_tag())["Debuff"] = true;
        }
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

TEST_F(GrowingPassiveSkillTest, TakeMomentumStacksOnEnemyDirectAndShareDamageAndCapsAtFive) {
    ASSERT_EQ(300u, data->FindBuff(kBuffTakeMomentum)->stack_limit());
    auto req = BaseRequest();
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillTakeMomentum] = 1;
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillA36] = 1;
    req.mutable_players(0)->add_skill_table_ids(kSkillA36);
    // 再加 1 名敌方用于验证同一回合承受两次攻击仍严格限叠 1 层。
    auto* enemy2 = req.add_players();
    *enemy2 = req.players(2);
    enemy2->set_player_id(202);
    enemy2->mutable_base_attributes()->set_speed(90);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    // 第 1 回合未受击：不随回合开始自动叠层（维持 0 层，直伤基础 600）。
    engine.SubmitAction(kHero, Cast(kTestPhysStrike, kEnemy));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    engine.SubmitAction(202, Cast(kTestIdleSkill, 202));
    auto res = engine.ResolveCurrentRound();
    EXPECT_EQ(0u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffTakeMomentum));
    EXPECT_EQ(600u, FirstDamageTo(res, kHero, kEnemy));

    // 第 2 回合被两名敌人同时直击：每回合限叠 1 层（0 -> 1）。
    engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestPhysStrike, kHero));
    engine.SubmitAction(202, Cast(kTestPhysStrike, kHero));
    res = engine.ResolveCurrentRound();
    EXPECT_EQ(1u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffTakeMomentum));

    // 第 3 回合 Hero 用 A36 援护 Ally，敌方打 Ally 经援护转伤给 Hero：应正常叠 1 层（1 -> 2）。
    engine.SubmitAction(kHero, Cast(kSkillA36, kAlly));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestPhysStrike, kAlly));
    engine.SubmitAction(202, Cast(kTestIdleSkill, 202));
    res = engine.ResolveCurrentRound();
    EXPECT_EQ(2u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffTakeMomentum));

    // 第 4..8 回合连续受击：第 7 回合出手吃 5 层（+5%=630）并受击叠至 6 层，第 8 回合出手吃 6 层（+6%=636）并叠至 7 层。
    for (int r = 4; r <= 8; ++r) {
        engine.SubmitAction(kHero, Cast(kTestPhysStrike, kEnemy));
        engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
        engine.SubmitAction(kEnemy, Cast(kTestPhysStrike, kHero));
        engine.SubmitAction(202, Cast(kTestIdleSkill, 202));
        res = engine.ResolveCurrentRound();
        if (r == 7) {
            EXPECT_EQ(630u, FirstDamageTo(res, kHero, kEnemy));
            EXPECT_EQ(6u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffTakeMomentum));
        }
        if (r == 8) {
            EXPECT_EQ(636u, FirstDamageTo(res, kHero, kEnemy));
            EXPECT_EQ(7u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffTakeMomentum));
        }
    }
}

TEST_F(GrowingPassiveSkillTest, DefensePenetrationStacksOnDirectAttackAndPiercesHighDefense) {
    ASSERT_EQ(50u, data->FindBuff(kBuffPenetration)->stack_limit());
    auto req = BaseRequest();
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillPenetration] = 1;
    // 给敌方较高防御（2400，未达 60% 封顶线）与高血量，精确观测穿防叠至 50 层（50% 穿防）封顶。
    req.mutable_players(2)->set_defense(2400);
    req.mutable_players(2)->set_max_health(100000);
    req.mutable_players(2)->mutable_base_attributes()->set_health(100000);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    // 第 1 回合待机不攻击：0 层。
    engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    auto res = engine.ResolveCurrentRound();
    EXPECT_EQ(0u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffPenetration));

    // 第 2 回合普攻命中敌方：叠 1 层（验证普攻计入主动攻击命中）。
    engine.SubmitAction(kHero, BasicAttack(kEnemy));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    res = engine.ResolveCurrentRound();
    EXPECT_EQ(1u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffPenetration));

    // 第 3..53 回合用技能攻击敌方：第 3 回合吃 1 层穿防，第 52 回合吃满 50 层（50% 穿防），第 53 回合封顶不变。
    uint64_t r3Dmg = 0;
    uint64_t r52Dmg = 0;
    uint64_t r53Dmg = 0;
    for (int r = 3; r <= 53; ++r) {
        engine.SubmitAction(kHero, Cast(kTestPhysStrike, kEnemy));
        engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
        engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
        res = engine.ResolveCurrentRound();
        const uint64_t dmg = FirstDamageTo(res, kHero, kEnemy);
        if (r == 3) r3Dmg = dmg;
        if (r == 52) r52Dmg = dmg;
        if (r == 53) r53Dmg = dmg;
    }
    EXPECT_EQ(50u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffPenetration));
    EXPECT_GT(r52Dmg, r3Dmg);
    EXPECT_EQ(r52Dmg, r53Dmg);
}

TEST_F(GrowingPassiveSkillTest, PeriodicGrowthStacksOnDotTicksAndScalesSubsequentTicksToFiveCap) {
    ASSERT_EQ(300u, data->FindBuff(kBuffDotGrowth)->stack_limit());
    auto req = BaseRequest();
    (*req.mutable_players(0)->mutable_skill_ranks())[kSkillDotGrowth] = 1;
    (*req.mutable_players(0)->mutable_skill_ranks())[kTestPoisonSkill] = 1;
    req.mutable_players(0)->add_skill_table_ids(kTestPoisonSkill);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));

    // 第 1 回合施加 6 跳固定 100 伤灼烧：第 1 跳（0 层）100 -> 叠至 1 层；第 2..6 跳依次吃 1..5 层（+1%..+5%），并在第 6 跳后叠至 6 层（突破旧 5 层上限）。
    engine.SubmitAction(kHero, Cast(kTestPoisonSkill, kEnemy));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    auto res = engine.ResolveCurrentRound();
    EXPECT_EQ(100u, FirstPeriodicDamageTo(res, kHero, kEnemy));
    EXPECT_EQ(1u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffDotGrowth));

    const std::array<uint64_t, 5> expectedTicks = {101, 102, 103, 104, 105};
    for (int i = 0; i < 5; ++i) {
        engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
        engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
        engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
        res = engine.ResolveCurrentRound();
        EXPECT_EQ(expectedTicks[i], FirstPeriodicDamageTo(res, kHero, kEnemy)) << "tick=" << (i + 2);
        EXPECT_EQ(static_cast<uint32_t>(i + 2), FindBuffStacks(FindActorState(res.state(), kHero), kBuffDotGrowth));
    }
}

TEST_F(GrowingPassiveSkillTest, AllControlResistAndShieldGrowthTriggerInteractivelyToFiveCap) {
    // 1. 验证厚泽蓄灵（12027）：待机不白嫖叠层，主动施放护盾/治疗后每轮叠 1 层（+1% 护盾容量，50 层封顶 +50%）。
    ASSERT_EQ(50u, data->FindBuff(kBuffShieldGrow)->stack_limit());
    ASSERT_EQ(40u, data->FindBuff(kBuffCtrlResist)->stack_limit());
    auto shieldReq = BaseRequest();
    (*shieldReq.mutable_players(0)->mutable_skill_ranks())[kSkillShieldGrow] = 1;
    (*shieldReq.mutable_players(0)->mutable_skill_ranks())[kSkillA40] = 1;
    shieldReq.mutable_players(0)->add_skill_table_ids(kSkillA40);
    auto testData = std::make_shared<MemoryBattleDataProvider>(*data);
    testData->AddSkill(kSkillA40).set_cooldown_rounds(0);
    testData->AddSkill(kSkillA40).set_mana_percent(0.0);

    TurnBattleEngine shieldEngine(testData);
    ASSERT_TRUE(shieldEngine.Initialize(shieldReq));
    // 第 1 回合待机不放辅助技：维持 0 层。
    shieldEngine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    shieldEngine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    shieldEngine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    auto idleRes = shieldEngine.ResolveCurrentRound();
    EXPECT_EQ(0u, FindBuffStacks(FindActorState(idleRes.state(), kHero), kBuffShieldGrow));

    // 第 2..53 回合连续施放 A40：第 2 回合 0 层底值 -> 叠至 1 层；第 52 回合吃满 50 层（+50%）；第 53 回合封顶不变。
    uint64_t r2Shield = 0;
    uint64_t r52Shield = 0;
    uint64_t r53Shield = 0;
    for (int r = 2; r <= 53; ++r) {
        shieldEngine.SubmitAction(kHero, Cast(kSkillA40, kHero));
        shieldEngine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
        shieldEngine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
        const auto res = shieldEngine.ResolveCurrentRound();
        const uint64_t cap = FindShieldCapacity(FindActorState(res.state(), kHero), kSkillA40);
        if (r == 2) r2Shield = cap;
        if (r == 52) r52Shield = cap;
        if (r == 53) r53Shield = cap;
        EXPECT_EQ(std::min<uint32_t>(50u, static_cast<uint32_t>(r - 1)),
                  FindBuffStacks(FindActorState(res.state(), kHero), kBuffShieldGrow));
    }
    EXPECT_GT(r52Shield, r2Shield);
    EXPECT_EQ(r52Shield, r53Shield);

    // 2. 验证磐石道心（12026）：未受敌方控场针对维持 0 层；每回合遭敌方行动控制尝试叠 1 层（+1%），满 40 层（+40%）封顶并显著压低封主动命中率。
    auto runControlTrials = [&](bool withCtrlResist) {
        uint32_t landed = 0;
        for (uint32_t seed = 1; seed <= 40; ++seed) {
            auto req = BaseRequest();
            req.set_seed(9000 + seed);
            req.mutable_players(2)->set_max_health(200000);
            req.mutable_players(2)->mutable_base_attributes()->set_health(200000);
            if (withCtrlResist) (*req.mutable_players(0)->mutable_skill_ranks())[kSkillCtrlResist] = 1;
            (*req.mutable_players(2)->mutable_skill_ranks())[kTestSealActiveSkill] = 1;
            req.mutable_players(2)->add_skill_table_ids(kTestSealActiveSkill);
            TurnBattleEngine eng(data);
            EXPECT_TRUE(eng.Initialize(req));
            // 第 1 回合敌方不控场：磐石道心维持 0 层。
            eng.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
            eng.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
            eng.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
            const auto r1 = eng.ResolveCurrentRound();
            if (withCtrlResist) {
                EXPECT_EQ(0u, FindBuffStacks(FindActorState(r1.state(), kHero), kBuffCtrlResist));
            }
            // 第 2..41 回合敌方连续对 Hero 施放封主动，叠至满 40 层封顶。
            for (int r = 2; r <= 41; ++r) {
                eng.SubmitAction(kHero, BasicAttack(kEnemy));
                eng.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
                eng.SubmitAction(kEnemy, Cast(kTestSealActiveSkill, kHero));
                eng.ResolveCurrentRound();
            }
            // 第 42 回合检验满 40 层（+40% 抗控）封顶与对封主动命中的实际压制。
            eng.SubmitAction(kHero, BasicAttack(kEnemy));
            eng.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
            eng.SubmitAction(kEnemy, Cast(kTestSealActiveSkill, kHero));
            const auto res = eng.ResolveCurrentRound();
            if (withCtrlResist) {
                EXPECT_EQ(40u, FindBuffStacks(FindActorState(res.state(), kHero), kBuffCtrlResist));
            }
            if (HasBuff(FindActorState(res.state(), kHero), kTestSealActiveSkill)) ++landed;
        }
        return landed;
    };
    const uint32_t baselineLanded = runControlTrials(false);
    const uint32_t resistedLanded = runControlTrials(true);
    EXPECT_LT(resistedLanded, baselineLanded);
}

TEST_F(GrowingPassiveSkillTest, PetGrowthIsPetOnlyAndClearsStacksOnDeathOrRetreatThenRestacks) {
    ASSERT_EQ(300u, data->FindBuff(kBuffPetGrowth)->stack_limit());
    // 1. 人物非法挂载 12028 兽威渐盛必须不生效（caster_mask=2 宝宝专属）。
    auto heroReq = BaseRequest();
    (*heroReq.mutable_players(0)->mutable_skill_ranks())[kSkillPetGrowth] = 1;
    TurnBattleEngine heroEngine(data);
    ASSERT_TRUE(heroEngine.Initialize(heroReq));
    EXPECT_FALSE(HasBuff(FindActorState(heroEngine.BuildStateSnapshot(), kHero), kBuffPetGrowth));

    // 2. 宝宝攻击命中叠层、阵亡清零但保留被动本体、复活后从 0 重新叠层。
    auto makePetRequest = [&](uint32_t seed) {
        auto req = BaseRequest();
        req.set_seed(seed);
        auto* pet = req.mutable_players(0)->add_pets();
        pet->set_pet_id(9001);
        pet->set_pet_table_id(1);
        pet->set_level(30);
        pet->set_max_health(20000);
        pet->set_max_mana(5000);
        pet->set_physical_attack(1000);
        pet->set_magic_attack(1000);
        pet->set_defense(0);
        pet->mutable_base_attributes()->set_health(20000);
        pet->mutable_base_attributes()->set_mana(5000);
        pet->mutable_base_attributes()->set_speed(280);
        (*pet->mutable_skill_ranks())[kSkillPetGrowth] = 1;
        (*pet->mutable_skill_ranks())[kTestIdleSkill] = 1;
        (*pet->mutable_skill_ranks())[kTestPhysStrike] = 1;
        pet->add_skill_table_ids(kTestIdleSkill);
        pet->add_skill_table_ids(kTestPhysStrike);
        return req;
    };

    auto deathReq = makePetRequest(20261126);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(deathReq));
    uint64_t petActorId = 0;
    for (const auto& a : engine.BuildStateSnapshot().actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_PET && a.owner_player_id() == kHero) petActorId = a.actor_id();
    }
    ASSERT_NE(petActorId, 0u);

    // 第 1、2 回合宝宝攻击命中敌方 -> 叠到 2 层（第 2 回合吃 1 层 +1% = 606）。
    for (int r = 1; r <= 2; ++r) {
        engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
        engine.SubmitActorAction(kHero, petActorId, Cast(kTestPhysStrike, kEnemy));
        engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
        engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
        const auto res = engine.ResolveCurrentRound();
        EXPECT_EQ(static_cast<uint32_t>(r), FindBuffStacks(FindActorState(res.state(), petActorId), kBuffPetGrowth));
        if (r == 2) EXPECT_EQ(606u, FirstDamageTo(res, petActorId, kEnemy));
    }

    // 第 3 回合宝宝被敌方秒杀：层数清零为 0，但被动本体仍在。
    engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    engine.SubmitActorAction(kHero, petActorId, Cast(kTestIdleSkill, petActorId));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestKillStrike, petActorId));
    auto res = engine.ResolveCurrentRound();
    ASSERT_TRUE(FindActorState(res.state(), petActorId).is_dead());
    EXPECT_TRUE(HasBuff(FindActorState(res.state(), petActorId), kBuffPetGrowth));
    EXPECT_EQ(0u, FindBuffStacks(FindActorState(res.state(), petActorId), kBuffPetGrowth));

    // 第 4 回合友军复活宝宝，第 5 回合宝宝再次攻击：伤害回到基础 600（0 层）并重新叠至 1 层。
    engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    engine.SubmitAction(kAlly, Cast(kTestReviveSkill, petActorId));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    res = engine.ResolveCurrentRound();
    ASSERT_FALSE(FindActorState(res.state(), petActorId).is_dead());
    EXPECT_EQ(0u, FindBuffStacks(FindActorState(res.state(), petActorId), kBuffPetGrowth));

    engine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    engine.SubmitActorAction(kHero, petActorId, Cast(kTestPhysStrike, kEnemy));
    engine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    engine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    res = engine.ResolveCurrentRound();
    EXPECT_EQ(600u, FirstDamageTo(res, petActorId, kEnemy));
    EXPECT_EQ(1u, FindBuffStacks(FindActorState(res.state(), petActorId), kBuffPetGrowth));

    // 3. 宝宝被敌宠 B09 游说退敌劝退时同样清零层数。
    auto retreatData = std::make_shared<MemoryBattleDataProvider>(*data);
    retreatData->AddBuff(110090).set_application_chance(1.0); // 100% 劝退以便确定性验证清零。
    auto retreatReq = makePetRequest(20261127);
    auto* enemyPet = retreatReq.mutable_players(2)->add_pets();
    enemyPet->set_pet_id(9002);
    enemyPet->set_pet_table_id(1);
    enemyPet->set_level(30);
    enemyPet->set_max_health(20000);
    enemyPet->set_max_mana(5000);
    enemyPet->set_physical_attack(1000);
    enemyPet->set_magic_attack(1000);
    enemyPet->mutable_base_attributes()->set_health(20000);
    enemyPet->mutable_base_attributes()->set_mana(5000);
    enemyPet->mutable_base_attributes()->set_speed(120);
    (*enemyPet->mutable_skill_ranks())[kSkillB09] = 1;
    (*enemyPet->mutable_skill_ranks())[kTestIdleSkill] = 1;
    enemyPet->add_skill_table_ids(kSkillB09);
    enemyPet->add_skill_table_ids(kTestIdleSkill);

    TurnBattleEngine retreatEngine(retreatData);
    ASSERT_TRUE(retreatEngine.Initialize(retreatReq));
    uint64_t heroPet2 = 0;
    uint64_t enemyPetId = 0;
    for (const auto& a : retreatEngine.BuildStateSnapshot().actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_PET && a.owner_player_id() == kHero) heroPet2 = a.actor_id();
        if (a.actor_type() == BATTLE_ACTOR_TYPE_PET && a.owner_player_id() == kEnemy) enemyPetId = a.actor_id();
    }
    ASSERT_NE(heroPet2, 0u);
    ASSERT_NE(enemyPetId, 0u);

    // 第 1 回合宝宝攻击叠至 1 层，同回合被敌宠劝退 -> 层数立刻清零为 0。
    retreatEngine.SubmitAction(kHero, Cast(kTestIdleSkill, kHero));
    retreatEngine.SubmitActorAction(kHero, heroPet2, Cast(kTestPhysStrike, kEnemy));
    retreatEngine.SubmitAction(kAlly, Cast(kTestIdleSkill, kAlly));
    retreatEngine.SubmitAction(kEnemy, Cast(kTestIdleSkill, kEnemy));
    retreatEngine.SubmitActorAction(kEnemy, enemyPetId, Cast(kSkillB09, heroPet2));
    res = retreatEngine.ResolveCurrentRound();
    ASSERT_TRUE(FindActorState(res.state(), heroPet2).fled());
    EXPECT_TRUE(HasBuff(FindActorState(res.state(), heroPet2), kBuffPetGrowth));
    EXPECT_EQ(0u, FindBuffStacks(FindActorState(res.state(), heroPet2), kBuffPetGrowth));
}

} // namespace
