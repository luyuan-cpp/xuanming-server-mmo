#include <gtest/gtest.h>

#include <cstdint>
#include <map>
#include <vector>

#include "services/scene/player/system/attribute_allocation_rules.h"
#include "services/scene/player/system/player_level_rules.h"

// 属性加点纯规则(attribute_allocation_rules.h)与角色等级范围(player_level_rules.h)的单元测试,
// 设计文档 docs/design/player-attribute-allocation.md §2.2 / §5。
// 重点守住"客户端数据不可信":请求里的点数是 uint32,外挂塞进来的负数到服务器会绕成超大正数
// (-1 → 4294967295);多个超大值相加若用 32 位累加,会绕回一个小数骗过"剩余点"校验。
// 放在 battle 测试工程:规则是纯 inline 头函数,不需要链接 scene.lib(同 player_revive_rule_test)。

using attributerules::AllocError;
using attributerules::DistributePoints;
using attributerules::PoolRule;
using attributerules::RescaleCurrent;
using attributerules::TotalPoints;
using attributerules::ValidateAllocation;

namespace {

constexpr uint32_t kConstitution = 101;
constexpr uint32_t kSpirit = 102;
constexpr uint32_t kStrength = 103;
constexpr uint32_t kAgility = 104;

constexpr uint32_t kLevel = 30;
constexpr uint32_t kGarbage = 123;  // 预置脏值:验证被拒绝时 deltaOut 一定清零

// 镜像 AttributePool.xlsx 行 1(属性点):1 级解锁、每级 5 点、单项不限
PoolRule MakeAttributePool() {
    PoolRule rule;
    rule.poolId = 1;
    rule.unlockLevel = 1;
    rule.pointsPerLevel = 5;
    rule.dimensionCap = 0;
    return rule;
}

// 镜像行 2(相性点):1 级解锁、每级 1 点、单项上限 50
PoolRule MakeAffinityPool() {
    PoolRule rule;
    rule.poolId = 2;
    rule.unlockLevel = 1;
    rule.pointsPerLevel = 1;
    rule.dimensionCap = 50;
    return rule;
}

// 镜像行 3(仙魔点):60 级解锁、每级 1 点
PoolRule MakeImmortalPool() {
    PoolRule rule;
    rule.poolId = 3;
    rule.unlockLevel = 60;
    rule.pointsPerLevel = 1;
    rule.dimensionCap = 0;
    return rule;
}

// 属性点池四个维度的当前已分配(ValidateAllocation 契约:池内全部维度都要有键)
std::map<uint32_t, uint32_t> AttributeCurrent(uint32_t constitution, uint32_t spirit = 0,
                                               uint32_t strength = 0, uint32_t agility = 0) {
    return {{kConstitution, constitution}, {kSpirit, spirit}, {kStrength, strength}, {kAgility, agility}};
}

std::map<uint32_t, uint32_t> AffinityCurrent(uint32_t metal) {
    return {{201, metal}, {202, 0}, {203, 0}, {204, 0}, {205, 0}};
}

}  // namespace

// ---------------------------------------------------------------------------
// 客户端坏数据:负数 / 超大值 / 溢出
// ---------------------------------------------------------------------------

// 客户端发来的"负数"在 uint32 字段里绕成超大正数,必须按点数不足拒绝,且增量不外泄
TEST(AttributeAllocationRulesTest, WrappedNegativeTargetIsRejectedAsNotEnoughPoints) {
    const auto current = AttributeCurrent(20);
    for (const int32_t negative : {-1, -5, -100, INT32_MIN}) {
        const auto wrapped = static_cast<uint32_t>(negative);
        uint32_t delta = kGarbage;
        EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, current, {{kConstitution, wrapped}}, 10, delta),
                  AllocError::kNotEnoughPoints)
            << "negative=" << negative;
        EXPECT_EQ(delta, 0u) << "negative=" << negative;
    }
}

// 两项增量之和在 32 位下会绕回:UINT32_MAX + 10 → 9 ≤ 剩余 10,窄累加就会被放行,
// 结果体质被写进约 43 亿点。守住 ValidateAllocation 的 64 位累加,谁把 delta 改窄这里立刻红。
TEST(AttributeAllocationRulesTest, IncrementSumThatWouldWrapIn32BitsIsRejected) {
    ASSERT_EQ(static_cast<uint32_t>(uint64_t{UINT32_MAX} + 10), 9u);  // 攻击算术本身:32 位下确实绕成 9

    const std::map<uint32_t, uint32_t> target{{kConstitution, UINT32_MAX}, {kSpirit, 10}};
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, AttributeCurrent(0), target, 10, delta),
              AllocError::kNotEnoughPoints);
    EXPECT_EQ(delta, 0u);
}

// 剩余点给到最大也吸收不了多个超大目标:四项各 UINT32_MAX,64 位和远超 UINT32_MAX
TEST(AttributeAllocationRulesTest, MultipleHugeTargetsExceedEvenMaxRemaining) {
    const std::map<uint32_t, uint32_t> target{
        {kConstitution, UINT32_MAX}, {kSpirit, UINT32_MAX}, {kStrength, UINT32_MAX}, {kAgility, UINT32_MAX}};
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, AttributeCurrent(0), target, UINT32_MAX, delta),
              AllocError::kNotEnoughPoints);
    EXPECT_EQ(delta, 0u);
}

// 有上限池:超单项上限拒绝,超大值同样先撞上限;恰好到上限放行
TEST(AttributeAllocationRulesTest, CapIsEnforcedPerDimension) {
    const auto current = AffinityCurrent(48);
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAffinityPool(), kLevel, current, {{201, 51}}, 100, delta),
              AllocError::kCapExceeded);
    EXPECT_EQ(delta, 0u);

    delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAffinityPool(), kLevel, current, {{201, UINT32_MAX}}, 100, delta),
              AllocError::kCapExceeded);
    EXPECT_EQ(delta, 0u);

    EXPECT_EQ(ValidateAllocation(MakeAffinityPool(), kLevel, current, {{201, 50}}, 100, delta), AllocError::kOk);
    EXPECT_EQ(delta, 2u);
}

// 不属于本池的维度(别的池 / 表里没有 / 0)一律拒绝,客户端不能往请求里塞任意键
TEST(AttributeAllocationRulesTest, DimensionOutsidePoolIsRejected) {
    for (const uint32_t dimensionId : {201u, 999u, 0u}) {
        uint32_t delta = kGarbage;
        EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, AttributeCurrent(0), {{dimensionId, 1}}, 10, delta),
                  AllocError::kDimensionNotInPool)
            << "dimension=" << dimensionId;
        EXPECT_EQ(delta, 0u) << "dimension=" << dimensionId;
    }
}

// ---------------------------------------------------------------------------
// 业务规则:只增不减 / 剩余点边界 / 幂等 / 解锁
// ---------------------------------------------------------------------------

// 只增不减:减点唯一通道是收费洗点
TEST(AttributeAllocationRulesTest, DecreaseIsRejected) {
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, AttributeCurrent(20), {{kConstitution, 19}}, 10, delta),
              AllocError::kCannotDecrease);
    EXPECT_EQ(delta, 0u);
}

// 一项加、一项减 = 把点从一项挪到另一项,等于免费洗点,同样拒绝
TEST(AttributeAllocationRulesTest, MovingPointsBetweenDimensionsIsRejected) {
    const auto current = AttributeCurrent(20, 5);
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, current, {{kConstitution, 25}, {kSpirit, 0}}, 10, delta),
              AllocError::kCannotDecrease);
    EXPECT_EQ(delta, 0u);
}

// 剩余点边界:恰好用完放行,多 1 点拒绝
TEST(AttributeAllocationRulesTest, RemainingPointsBoundary) {
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, AttributeCurrent(20), {{kConstitution, 30}}, 10, delta),
              AllocError::kOk);
    EXPECT_EQ(delta, 10u);

    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, AttributeCurrent(20), {{kConstitution, 31}}, 10, delta),
              AllocError::kNotEnoughPoints);
    EXPECT_EQ(delta, 0u);
}

// 多维度同时加:各项增量合计后再和剩余点比
TEST(AttributeAllocationRulesTest, MultiDimensionIncrementsAreSummed) {
    const auto current = AttributeCurrent(20, 5);
    const std::map<uint32_t, uint32_t> target{{kConstitution, 23}, {kSpirit, 8}, {kAgility, 4}};  // 3 + 3 + 4
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, current, target, 10, delta), AllocError::kOk);
    EXPECT_EQ(delta, 10u);

    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, current, target, 9, delta),
              AllocError::kNotEnoughPoints);
    EXPECT_EQ(delta, 0u);
}

// 目标与当前相同 / 空目标:无变化,不当成功。协议是"目标值"语义,重发同一请求不会重复扣点
TEST(AttributeAllocationRulesTest, NothingToChangeIsRejected) {
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, AttributeCurrent(20), {{kConstitution, 20}}, 10, delta),
              AllocError::kNothingToChange);
    EXPECT_EQ(delta, 0u);

    delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, AttributeCurrent(20), {}, 10, delta),
              AllocError::kNothingToChange);
    EXPECT_EQ(delta, 0u);
}

// 未解锁池拒绝(仙魔点 60 级解锁)
TEST(AttributeAllocationRulesTest, LockedPoolIsRejected) {
    const std::map<uint32_t, uint32_t> current{{301, 0}, {302, 0}, {303, 0}, {304, 0}};
    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeImmortalPool(), 59, current, {{301, 1}}, 10, delta), AllocError::kPoolLocked);
    EXPECT_EQ(delta, 0u);

    EXPECT_EQ(ValidateAllocation(MakeImmortalPool(), 60, current, {{301, 1}}, 10, delta), AllocError::kOk);
    EXPECT_EQ(delta, 1u);
}

// ---------------------------------------------------------------------------
// 总量换算
// ---------------------------------------------------------------------------

// 满级总量:属性点 425、相性点 85、仙魔点 26(60 级起每级 1 点)。
// 上限 85 是策划定的硬约束(2026-09-10);改它要同步设计文档 §2.2 与经验表,所以这里钉死
TEST(AttributeAllocationRulesTest, TotalPointsAtLevelCap) {
    ASSERT_EQ(playerlevel::kMaxLevel, 85u);
    ASSERT_GE(playerlevel::kMaxLevel, MakeImmortalPool().unlockLevel);  // 满级前仙魔池必须能解锁

    EXPECT_EQ(TotalPoints(MakeAttributePool(), playerlevel::kMaxLevel, 0), 425u);
    EXPECT_EQ(TotalPoints(MakeAffinityPool(), playerlevel::kMaxLevel, 0), 85u);
    EXPECT_EQ(TotalPoints(MakeImmortalPool(), playerlevel::kMaxLevel, 0), 26u);

    EXPECT_EQ(TotalPoints(MakeAttributePool(), 1, 0), 5u);
    EXPECT_EQ(TotalPoints(MakeImmortalPool(), 59, 0), 0u);
    EXPECT_EQ(TotalPoints(MakeImmortalPool(), 60, 0), 1u);
}

// 额外点极大时总量饱和到 UINT32_MAX,不绕回小数
TEST(AttributeAllocationRulesTest, TotalPointsSaturatesInsteadOfWrapping) {
    EXPECT_EQ(TotalPoints(MakeAttributePool(), playerlevel::kMaxLevel, UINT32_MAX), UINT32_MAX);
}

// 表配错 unlock_level=0 视为未解锁,不发点
TEST(AttributeAllocationRulesTest, ZeroUnlockLevelMeansLocked) {
    auto rule = MakeAttributePool();
    rule.unlockLevel = 0;
    EXPECT_EQ(TotalPoints(rule, playerlevel::kMaxLevel, 0), 0u);
}

// ---------------------------------------------------------------------------
// 角色等级范围(player_level_rules.h)
// ---------------------------------------------------------------------------

// GM 设等级只接受 1..85;0 与超上限一律拒绝
TEST(PlayerLevelRulesTest, OnlyOneThroughMaxLevelIsWritable) {
    EXPECT_FALSE(playerlevel::IsValidLevel(0));
    EXPECT_TRUE(playerlevel::IsValidLevel(1));
    EXPECT_TRUE(playerlevel::IsValidLevel(playerlevel::kMaxLevel));
    EXPECT_FALSE(playerlevel::IsValidLevel(playerlevel::kMaxLevel + 1));
    EXPECT_FALSE(playerlevel::IsValidLevel(200));
    EXPECT_FALSE(playerlevel::IsValidLevel(UINT32_MAX));
}

// 上限下调前留下的超限存档(GM 设过 86~200 级 / 回档到这类快照)加载时压回上限;合法等级与 0 原样不动
TEST(PlayerLevelRulesTest, StoredLevelAboveCapIsClampedDown) {
    EXPECT_EQ(playerlevel::ClampToMaxLevel(200), playerlevel::kMaxLevel);
    EXPECT_EQ(playerlevel::ClampToMaxLevel(playerlevel::kMaxLevel + 1), playerlevel::kMaxLevel);
    EXPECT_EQ(playerlevel::ClampToMaxLevel(playerlevel::kMaxLevel), playerlevel::kMaxLevel);
    EXPECT_EQ(playerlevel::ClampToMaxLevel(30), 30u);
    EXPECT_EQ(playerlevel::ClampToMaxLevel(0), 0u);
}

// ---------------------------------------------------------------------------
// 自动加点
// ---------------------------------------------------------------------------

// 自动加点的建议必须能原样通过校验(客户端拿建议直接提交),且恰好用完剩余点。
// 逐维钉住:按权重比例取整(7×3/5=4、7×1/5=1、7×1/5=1),余 1 点按优先序补给第一位
TEST(AttributeAllocationRulesTest, DistributedSuggestionPassesValidation) {
    const auto current = AttributeCurrent(20, 5);
    auto suggested = current;
    DistributePoints(MakeAttributePool(), {kStrength, kAgility, kConstitution}, {3, 1, 1}, 7, suggested);
    EXPECT_EQ(suggested.at(kStrength), 5u);
    EXPECT_EQ(suggested.at(kAgility), 1u);
    EXPECT_EQ(suggested.at(kConstitution), 21u);
    EXPECT_EQ(suggested.at(kSpirit), 5u);  // 不在方案优先序里的维度不动

    uint32_t delta = kGarbage;
    EXPECT_EQ(ValidateAllocation(MakeAttributePool(), kLevel, current, suggested, 7, delta), AllocError::kOk);
    EXPECT_EQ(delta, 7u);
}

// 权重 0 = 策划配的"不分":比例分配和余数补齐都要跳过它
TEST(AttributeAllocationRulesTest, ZeroWeightDimensionGetsNoRemainder) {
    auto allocated = AttributeCurrent(0);
    DistributePoints(MakeAttributePool(), {kConstitution, kSpirit, kStrength}, {0, 1, 1}, 3, allocated);
    EXPECT_EQ(allocated.at(kConstitution), 0u);
    EXPECT_EQ(allocated.at(kSpirit), 2u);
    EXPECT_EQ(allocated.at(kStrength), 1u);
}

// 用 AttributeAutoPlan 通用行(class_id=0,池 1)做输入:优先序 力量/体质/敏捷/灵力,表里只配了 3 个权重,
// 缺的第 4 个按 1。1 级 5 点:比例 5×3/6=2、其余取整为 0,余 3 点按优先序补给力量/体质/敏捷
TEST(AttributeAllocationRulesTest, GenericAutoPlanRowDistribution) {
    auto allocated = AttributeCurrent(0);
    DistributePoints(MakeAttributePool(), {kStrength, kConstitution, kAgility, kSpirit}, {3, 1, 1}, 5, allocated);
    EXPECT_EQ(allocated.at(kStrength), 3u);
    EXPECT_EQ(allocated.at(kConstitution), 1u);
    EXPECT_EQ(allocated.at(kAgility), 1u);
    EXPECT_EQ(allocated.at(kSpirit), 0u);
}

// 有上限池:按优先序灌到上限,不超上限、不超剩余
TEST(AttributeAllocationRulesTest, CappedPoolDistributionRespectsCapAndRemaining) {
    auto allocated = AffinityCurrent(49);
    DistributePoints(MakeAffinityPool(), {201, 202, 203}, {}, 3, allocated);
    EXPECT_EQ(allocated.at(201), 50u);
    EXPECT_EQ(allocated.at(202), 2u);
    EXPECT_EQ(allocated.at(203), 0u);
}

// 权重全 0 / 剩余 0:什么都不分
TEST(AttributeAllocationRulesTest, NothingDistributedWithoutPointsOrWeights) {
    auto allocated = AttributeCurrent(1, 2, 3, 4);
    const auto before = allocated;
    DistributePoints(MakeAttributePool(), {kConstitution, kSpirit}, {0, 0}, 10, allocated);
    EXPECT_EQ(allocated, before);

    DistributePoints(MakeAttributePool(), {kConstitution}, {1}, 0, allocated);
    EXPECT_EQ(allocated, before);
}

// ---------------------------------------------------------------------------
// 当前气血/法力随上限变化
// ---------------------------------------------------------------------------

// 改属性不能当治疗:上限一降一升往返,正常血量不会比原来多(2026-09-04 评审修掉的无限回血)。
// 唯一例外来自"活着至少留 1":极低血量往返最多回到 floor(oldMax/newMax),有上界,再往返也不再增长
TEST(AttributeAllocationRulesTest, RescaleRoundTripHealingIsBounded) {
    const uint64_t back = RescaleCurrent(RescaleCurrent(200, 3950, 1100), 1100, 3950);  // 200 → 55 → 197
    EXPECT_GE(back, 1u);
    EXPECT_LE(back, 200u);

    // 1 血:降到 1100 时 0.28 被抬成 1;升回 3950 时 1×3950/1100=3.59 → 3 = floor(3950/1100)
    const uint64_t lowDown = RescaleCurrent(1, 3950, 1100);
    EXPECT_EQ(lowDown, 1u);
    const uint64_t lowBack = RescaleCurrent(lowDown, 1100, 3950);
    EXPECT_EQ(lowBack, 3u);
    // 再往返一次不再增长:不能靠反复切方案累积回血
    EXPECT_EQ(RescaleCurrent(RescaleCurrent(lowBack, 3950, 1100), 1100, 3950), lowBack);
}

// 活人至少留 1、死人保持 0、新上限为 0 归零、旧上限未知只夹不补
TEST(AttributeAllocationRulesTest, RescaleKeepsAliveAndDeadStates) {
    EXPECT_EQ(RescaleCurrent(1, 1000, 10), 1u);
    EXPECT_EQ(RescaleCurrent(0, 1000, 5000), 0u);
    EXPECT_EQ(RescaleCurrent(500, 1000, 0), 0u);
    EXPECT_EQ(RescaleCurrent(800, 0, 500), 500u);
    EXPECT_EQ(RescaleCurrent(300, 1000, 1000), 300u);
}
