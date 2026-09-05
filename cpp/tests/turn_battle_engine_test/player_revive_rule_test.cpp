#include <gtest/gtest.h>

#include "services/scene/player/system/player_revive.h"

// 玩家基础属性「初始化 / 阵亡复活」规则(player_revive.h)的单元测试。
// 规则被登录加载(player_database_loader.cpp)与战斗结算(player_battle.cpp)共用,
// 2026-09-02 冒烟实测的两类事故都靠它兜底:新号属性全 0 开局即死、阵亡后 0 血还能再排队。
// 放在 battle 测试工程:规则是纯 inline 头函数,不需要链接 scene.lib。

namespace {

constexpr uint64_t kInitHealth = 500;
constexpr uint64_t kInitMana = 200;

ClassTable MakeClassRow() {
    ClassTable cls;
    cls.set_id(1);
    cls.set_init_health(kInitHealth);
    cls.set_init_mana(kInitMana);
    cls.set_init_strength(20);
    cls.set_init_armor(10);
    cls.set_init_resistance(5);
    cls.set_init_critchance(10);
    cls.set_init_speed(20);
    return cls;
}

}  // namespace

// 全新号(全 0)→ 全套初始属性 + 回满
TEST(PlayerReviveRuleTest, FreshAccountGetsFullInitialAttributes) {
    BaseAttributesComp attrs;
    ASSERT_TRUE(IsUninitializedBaseAttributes(attrs));

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow()),
              PlayerReviveOutcome::kInitialized);
    EXPECT_EQ(attrs.health(), kInitHealth);
    EXPECT_EQ(attrs.mana(), kInitMana);
    EXPECT_EQ(attrs.strength(), 20u);
    EXPECT_EQ(attrs.armor(), 10u);
    EXPECT_EQ(attrs.resistance(), 5u);
    EXPECT_EQ(attrs.critchance(), 10u);
    EXPECT_EQ(attrs.speed(), 20u);
}

// 阵亡(health=0,成长属性在)→ 只回满 HP/MP,成长属性一个都不能动
TEST(PlayerReviveRuleTest, DeadPlayerRevivesWithoutTouchingGrowth) {
    BaseAttributesComp attrs;
    attrs.set_strength(77);
    attrs.set_armor(33);
    attrs.set_resistance(9);
    attrs.set_critchance(4);
    attrs.set_speed(41);
    attrs.set_health(0);
    attrs.set_mana(13);
    ASSERT_FALSE(IsUninitializedBaseAttributes(attrs));

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow()),
              PlayerReviveOutcome::kRevived);
    EXPECT_EQ(attrs.health(), kInitHealth);
    EXPECT_EQ(attrs.mana(), kInitMana);
    EXPECT_EQ(attrs.strength(), 77u);
    EXPECT_EQ(attrs.armor(), 33u);
    EXPECT_EQ(attrs.resistance(), 9u);
    EXPECT_EQ(attrs.critchance(), 4u);
    EXPECT_EQ(attrs.speed(), 41u);
}

// 传了真实上限就回到真实上限,不是职业 1 级初值。
// 这条是等级/加点成长不被吞掉的保证:20 级玩家 max_health≈1100,只回 500 等于扣掉一半血上限,
// 而登录路径的 Recalculate 只向下夹不向上补,差额永远补不回来。
TEST(PlayerReviveRuleTest, RevivesToDerivedMaxWhenProvided) {
    BaseAttributesComp attrs;
    attrs.set_strength(77);
    attrs.set_speed(41);
    attrs.set_health(0);

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow(), 1100, 400),
              PlayerReviveOutcome::kRevived);
    EXPECT_EQ(attrs.health(), 1100u);
    EXPECT_EQ(attrs.mana(), 400u);
    EXPECT_EQ(attrs.strength(), 77u) << "成长属性仍然不动";
}

// 新号同样按真实上限回满(成长属性走职业初值,HP/MP 走上限)
TEST(PlayerReviveRuleTest, FreshAccountAlsoFillsToDerivedMax) {
    BaseAttributesComp attrs;

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow(), 640, 260),
              PlayerReviveOutcome::kInitialized);
    EXPECT_EQ(attrs.health(), 640u);
    EXPECT_EQ(attrs.mana(), 260u);
    EXPECT_EQ(attrs.strength(), 20u) << "成长属性仍取职业初值";
}

// 上限传 0 = 暂不可知(登录时二级属性还没算)→ 退回职业初值,而不是把人复活成 0 血
TEST(PlayerReviveRuleTest, ZeroMaxFallsBackToClassInitials) {
    BaseAttributesComp attrs;
    attrs.set_strength(77);
    attrs.set_speed(41);
    attrs.set_health(0);

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow(), 0, 0),
              PlayerReviveOutcome::kRevived);
    EXPECT_EQ(attrs.health(), kInitHealth);
    EXPECT_EQ(attrs.mana(), kInitMana);
}

// 活着 → 完全不动:残血带出战斗(设计 D4)不能被「复活」抹掉,给了上限也不能顶满
TEST(PlayerReviveRuleTest, AlivePlayerIsUntouchedEvenWithMaxProvided) {
    BaseAttributesComp attrs;
    attrs.set_health(123);
    attrs.set_mana(7);
    attrs.set_strength(77);
    attrs.set_speed(41);

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow(), 1100, 400),
              PlayerReviveOutcome::kUntouched);
    EXPECT_EQ(attrs.health(), 123u);
    EXPECT_EQ(attrs.mana(), 7u);
    EXPECT_EQ(attrs.strength(), 77u);
    EXPECT_EQ(attrs.speed(), 41u);
}

// 0 血但任一成长属性非 0:算阵亡不算新号 → 成长属性保持,不被初值覆盖
TEST(PlayerReviveRuleTest, ZeroHealthWithAnyGrowthCountsAsDeadNotFresh) {
    BaseAttributesComp attrs;
    attrs.set_speed(15);
    ASSERT_FALSE(IsUninitializedBaseAttributes(attrs));

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow()),
              PlayerReviveOutcome::kRevived);
    EXPECT_EQ(attrs.health(), kInitHealth);
    EXPECT_EQ(attrs.mana(), kInitMana);
    EXPECT_EQ(attrs.speed(), 15u);
    EXPECT_EQ(attrs.strength(), 0u);
}

// 活着但 strength/speed 恰好为 0:活着就不动,不会被误判成新号重置
TEST(PlayerReviveRuleTest, AliveWithZeroGrowthIsNotReinitialized) {
    BaseAttributesComp attrs;
    attrs.set_health(1);
    ASSERT_FALSE(IsUninitializedBaseAttributes(attrs));

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow()),
              PlayerReviveOutcome::kUntouched);
    EXPECT_EQ(attrs.health(), 1u);
    EXPECT_EQ(attrs.strength(), 0u);
}

// 幂等:初始化后再套一次不再改动(登录 → 结算 → 再登录会重复走这条路)
TEST(PlayerReviveRuleTest, ApplyingTwiceIsIdempotent) {
    BaseAttributesComp attrs;
    ASSERT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow()),
              PlayerReviveOutcome::kInitialized);
    const BaseAttributesComp after = attrs;

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, MakeClassRow()),
              PlayerReviveOutcome::kUntouched);
    EXPECT_EQ(attrs.SerializeAsString(), after.SerializeAsString());
}

// 空表行(全 0 的 ClassTable)不该把活人打成 0 血:活着仍然不动
TEST(PlayerReviveRuleTest, EmptyClassRowDoesNotHarmAlivePlayer) {
    BaseAttributesComp attrs;
    attrs.set_health(50);
    attrs.set_strength(3);

    EXPECT_EQ(ApplyClassInitialAttributesOrRevive(attrs, ClassTable{}),
              PlayerReviveOutcome::kUntouched);
    EXPECT_EQ(attrs.health(), 50u);
}
