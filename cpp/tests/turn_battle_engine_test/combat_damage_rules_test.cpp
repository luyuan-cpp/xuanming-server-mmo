#include <gtest/gtest.h>

#include <cmath>
#include <cstdint>
#include <limits>

#include "services/battle/system/combat_damage_rules.h"
#include "services/scene/player/system/player_level_rules.h"

// 伤害纯规则(combat_damage_rules.h)的单元测试:回合引擎与实时技能共用这一份公式,
// 引擎侧的接线(普攻吃物伤、技能按 damage_type 选攻击、PVP 系数)由 turn_battle_engine_test 覆盖。
// 放在 battle 测试工程:规则是纯 inline 头函数,不需要链接任何库。

using combatdamage::AttackMultiplier;
using combatdamage::DamageBeforeCritical;
using combatdamage::DamageToHealth;
using combatdamage::kLevelFactorMaxLevel;
using combatdamage::kMagicDamage;
using combatdamage::kMaxPassiveReduction;
using combatdamage::kPhysicalDamage;
using combatdamage::LevelFactor;
using combatdamage::ReceivedRatio;
using combatdamage::SelectAttack;

// 手算一个中间点:力量 20、攻击 100、护甲 10 + 防御 40、抗性 5%、目标 10 级
//   原始伤害 = 10 × (1 + 20 × 0.1) + 100 = 130;等级系数 = 30 + 10 × 10 = 130
//   受伤比例 = 130 ÷ (50 + 130) × 0.95
TEST(CombatDamageRulesTest, FormulaAtHandComputedPoint) {
    const double expectedRatio = 130.0 / 180.0 * 0.95;
    EXPECT_DOUBLE_EQ(ReceivedRatio(10, 40, 5, 10), expectedRatio);
    EXPECT_DOUBLE_EQ(DamageBeforeCritical(10.0, 20, 100, 1.0, 10, 40, 5, 10), 130.0 * expectedRatio);
}

// 防御越高受伤越少;但护甲 / 防御 / 抗性堆多高,常驻减伤都不超过 60%(减法口径下会直接归零)
TEST(CombatDamageRulesTest, ReductionIsMonotonicAndCappedAtSixtyPercent) {
    double previous = 1.0;
    for (const uint64_t defense : {0ull, 50ull, 130ull, 1000ull, 100000ull}) {
        const double ratio = ReceivedRatio(0, defense, 0, 10);
        EXPECT_LE(ratio, previous) << "defense=" << defense;
        EXPECT_GE(ratio, 1.0 - kMaxPassiveReduction) << "defense=" << defense;
        previous = ratio;
    }
    EXPECT_DOUBLE_EQ(ReceivedRatio(0, 0, 0, 10), 1.0);
    EXPECT_DOUBLE_EQ(ReceivedRatio(0, 130, 0, 10), 0.5);                          // 防御 = 等级系数时减半
    EXPECT_DOUBLE_EQ(ReceivedRatio(0, 0, 100, 10), 1.0 - kMaxPassiveReduction);  // 满抗性也只减六成
    EXPECT_DOUBLE_EQ(ReceivedRatio(0, 0, 250, 10), 1.0 - kMaxPassiveReduction);  // 抗性超过 100% 按 100% 算
}

// 两个 uint64 上限相加不能溢出成小数
TEST(CombatDamageRulesTest, HugeDefenseDoesNotOverflow) {
    const uint64_t kMax = std::numeric_limits<uint64_t>::max();
    EXPECT_DOUBLE_EQ(ReceivedRatio(kMax, kMax, 0, 85), 1.0 - kMaxPassiveReduction);
}

// 目标等级夹到 1..85:0 级按 1 级,超上限按上限
TEST(CombatDamageRulesTest, TargetLevelIsClamped) {
    EXPECT_DOUBLE_EQ(LevelFactor(1), 40.0);
    EXPECT_DOUBLE_EQ(LevelFactor(0), LevelFactor(1));
    EXPECT_DOUBLE_EQ(LevelFactor(85), 880.0);
    EXPECT_DOUBLE_EQ(LevelFactor(200), LevelFactor(85));
}

// battle 库不 include scene 头文件,等级上限只能镜像一份;两边一改一漏这里立刻红
TEST(CombatDamageRulesTest, LevelFactorCapMatchesPlayerLevelCap) {
    EXPECT_EQ(kLevelFactorMaxLevel, playerlevel::kMaxLevel);
}

// 基础伤害 0 / 负数 / 非有限数 = 纯辅助技能或坏表:攻击再高也不产生伤害
TEST(CombatDamageRulesTest, NonPositiveOrInvalidBaseDealsNoDamage) {
    for (const double base : {0.0, -5.0, std::nan(""), std::numeric_limits<double>::infinity()}) {
        EXPECT_EQ(DamageBeforeCritical(base, 50, 100000, 1.0, 0, 0, 0, 10), 0.0) << "base=" << base;
    }
}

// 攻击倍率:未配(0)按 1 倍;正常值照乘;负数 / 非有限数是坏表,只丢掉攻击加成,基础伤害照打
TEST(CombatDamageRulesTest, AttackMultiplierDefaultsAndRejectsBadValues) {
    EXPECT_DOUBLE_EQ(AttackMultiplier(0.0), 1.0);
    EXPECT_DOUBLE_EQ(AttackMultiplier(2.5), 2.5);
    EXPECT_DOUBLE_EQ(AttackMultiplier(-1.0), 0.0);
    EXPECT_DOUBLE_EQ(AttackMultiplier(std::nan("")), 0.0);
    // 目标 10 级、无护甲防御抗性 → 受伤比例 1,结果即原始伤害
    EXPECT_DOUBLE_EQ(DamageBeforeCritical(10.0, 0, 100, 0.0, 0, 0, 0, 10), 110.0);
    EXPECT_DOUBLE_EQ(DamageBeforeCritical(10.0, 0, 100, 2.0, 0, 0, 0, 10), 210.0);
    EXPECT_DOUBLE_EQ(DamageBeforeCritical(10.0, 0, 100, -3.0, 0, 0, 0, 10), 10.0);
}

TEST(CombatDamageRulesTest, SelectAttackByDamageType) {
    EXPECT_EQ(SelectAttack(kPhysicalDamage, 300, 700), 300u);
    EXPECT_EQ(SelectAttack(kMagicDamage, 300, 700), 700u);
    EXPECT_EQ(SelectAttack(7, 300, 700), 0u);  // 未知类型不吃任何攻击
}

// 落血:向上取整、不超过当前气血;NaN / 无穷 / 非正数一律 0
TEST(CombatDamageRulesTest, DamageToHealthRoundsUpAndSaturates) {
    EXPECT_EQ(DamageToHealth(10.2, 100), 11u);
    EXPECT_EQ(DamageToHealth(10.0, 100), 10u);
    EXPECT_EQ(DamageToHealth(250.0, 100), 100u);
    EXPECT_EQ(DamageToHealth(1e300, 100), 100u);
    EXPECT_EQ(DamageToHealth(5.0, 0), 0u);
    for (const double bad : {0.0, -1.0, std::nan(""), std::numeric_limits<double>::infinity()}) {
        EXPECT_EQ(DamageToHealth(bad, 100), 0u) << "raw=" << bad;
    }
}
