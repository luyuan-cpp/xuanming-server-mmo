#include <gtest/gtest.h>

#include <cstdint>
#include <limits>

#include "services/scene/player/system/attribute_unit_migration.h"

// 存档数值单位迁移(attribute_unit_migration.h)的单测:2026-09-14 法力单位 ×4,旧存档的当前法力只换算一次。
// 放在 battle 测试工程:规则是纯 inline 头函数,只依赖 proto 消息(本工程已链接 proto.lib)。

using attributeunit::kCurrentVersion;
using attributeunit::ManaToCurrentUnit;
using attributeunit::MigrateLoadedAttributeUnits;
using attributeunit::SaturatingScale;

namespace {

PlayerPetComp PetsWithMana(std::initializer_list<uint64_t> manas) {
    PlayerPetComp comp;
    for (const auto mana : manas) {
        comp.add_pets()->set_mana(mana);
    }
    return comp;
}

}  // namespace

// 旧存档(版本 0):角色与每只宝宝的当前法力都 ×4,盖上当前版本;第二次加载什么都不动
TEST(AttributeUnitMigrationTest, OldSaveScalesManaOnceAndStamps) {
    BaseAttributesComp base;
    base.set_mana(200);
    base.set_armor(10);  // 护甲不归迁移管(Recalculate 按职业表直写)
    PlayerAttributeComp attribute;
    auto pets = PetsWithMana({80, 0});

    ASSERT_TRUE(MigrateLoadedAttributeUnits(base, attribute, pets, /*scalePlayerMana*/ true));
    EXPECT_EQ(base.mana(), 800u);
    EXPECT_EQ(base.armor(), 10u);
    EXPECT_EQ(pets.pets(0).mana(), 320u);
    EXPECT_EQ(pets.pets(1).mana(), 0u);
    EXPECT_EQ(attribute.attribute_unit_version(), kCurrentVersion);

    EXPECT_FALSE(MigrateLoadedAttributeUnits(base, attribute, pets, true));
    EXPECT_EQ(base.mana(), 800u);
    EXPECT_EQ(pets.pets(0).mana(), 320u);
}

// 新号 / 阵亡复活的号:本人法力随后会被顶满,不乘;宝宝照乘;照样盖戳
TEST(AttributeUnitMigrationTest, FreshOrRevivedPlayerStampsButStillScalesPets) {
    BaseAttributesComp base;
    base.set_mana(800);
    PlayerAttributeComp attribute;
    auto pets = PetsWithMana({300});

    ASSERT_TRUE(MigrateLoadedAttributeUnits(base, attribute, pets, /*scalePlayerMana*/ false));
    EXPECT_EQ(base.mana(), 800u);
    EXPECT_EQ(pets.pets(0).mana(), 1200u);
    EXPECT_EQ(attribute.attribute_unit_version(), kCurrentVersion);
}

// 已是当前版本或更新版本(比如回滚到旧二进制前被新二进制存过):一律不动
TEST(AttributeUnitMigrationTest, CurrentOrNewerVersionIsUntouched) {
    for (const uint32_t version : {kCurrentVersion, kCurrentVersion + 1}) {
        BaseAttributesComp base;
        base.set_mana(123);
        PlayerAttributeComp attribute;
        attribute.set_attribute_unit_version(version);
        auto pets = PetsWithMana({7});

        EXPECT_FALSE(MigrateLoadedAttributeUnits(base, attribute, pets, true));
        EXPECT_EQ(base.mana(), 123u);
        EXPECT_EQ(pets.pets(0).mana(), 7u);
        EXPECT_EQ(attribute.attribute_unit_version(), version);
    }
}

// 坏存档里的超大值:饱和到上限,不能乘溢出成小数
TEST(AttributeUnitMigrationTest, ScalingSaturatesInsteadOfOverflowing) {
    const uint64_t kMax = std::numeric_limits<uint64_t>::max();
    EXPECT_EQ(SaturatingScale(kMax / 4, 4), (kMax / 4) * 4);
    EXPECT_EQ(SaturatingScale(kMax / 4 + 1, 4), kMax);
    EXPECT_EQ(SaturatingScale(kMax, 4), kMax);
    EXPECT_EQ(SaturatingScale(5, 0), 0u);
}

// 结算应用前的换算(旧 battle 二进制产出的结算没有版本戳 = 0):只换算旧版本,当前 / 更新版本原样
TEST(AttributeUnitMigrationTest, ManaToCurrentUnitConvertsOnlyLegacyValues) {
    EXPECT_EQ(kCurrentVersion, turnbattle::kAttributeUnitVersion);
    EXPECT_EQ(ManaToCurrentUnit(190, 0), 760u);
    EXPECT_EQ(ManaToCurrentUnit(190, kCurrentVersion), 190u);
    EXPECT_EQ(ManaToCurrentUnit(190, kCurrentVersion + 1), 190u);
    EXPECT_EQ(ManaToCurrentUnit(0, 0), 0u);
    EXPECT_EQ(ManaToCurrentUnit(std::numeric_limits<uint64_t>::max(), 0), std::numeric_limits<uint64_t>::max());
}

