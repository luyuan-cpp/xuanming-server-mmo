#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <map>
#include <random>
#include <set>
#include <string>
#include <utility>
#include <vector>

#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "services/scene/player/system/equip_attribute_rules.h"

// 装备属性纯规则(equip_attribute_rules.h)的单元测试,设计文档 docs/design/equipment-attributes.md §4.2 / §7。
// 重点钉住三件事:
//   1) RollAffixes 的随机数消费顺序 —— 用脚本化随机源逐次核对「第几次掷骰、要的是多大的区间」。
//      顺序一变,所有按种子复现的结果(回放 / 排障)都跟着变,所以改实现时这里必须先红。
//   2) ApplyEffect 对坏表数据(未知 kind / 越界 param)的拒绝,以及加法饱和不回绕。
//   3) CombatStat 的数值 == proto CombatAttributes 的字段号(文件末尾的编译期守卫)。
// 二期(强化 + 镶嵌,设计文档 docs/design/equipment-enhance-inlay.md §4.2 / §7)追加四件:
//   4) ScaleByPermyriad 的取整与饱和 —— 算法在契约里写死,加成、tooltip、robot 冒烟都按它对账;
//   5) RollEnhanceSuccess 的随机数消费:0 不掷、其余恰好掷一次 rand(10000)、严格小于 rate 才成功;
//   6) LevelAfterFailure 不会掉到 0 以下;
//   7) CheckGemFit 的四种结果与写死的判定顺序(同时不满足多条时回哪一个,客户端按它决定要不要重拉)。
// 放在 battle 测试工程:规则是纯 inline 头函数,不需要链接 scene.lib(同 attribute_allocation_rules_test)。

using equiprules::AffixTier;
using equiprules::ApplyEffect;
using equiprules::CheckGemFit;
using equiprules::CombatOf;
using equiprules::CombatStat;
using equiprules::DerivedFlatOf;
using equiprules::DerivedStat;
using equiprules::EffectKind;
using equiprules::EquipBonus;
using equiprules::GemFit;
using equiprules::GemSocketRule;
using equiprules::IsRandomAffixTier;
using equiprules::LevelAfterFailure;
using equiprules::PickLevelTier;
using equiprules::PoolEntry;
using equiprules::PrimaryPointsFor;
using equiprules::RollAffixes;
using equiprules::RolledAffix;
using equiprules::RollEnhanceSuccess;
using equiprules::RollRule;
using equiprules::ScaleByPermyriad;
using equiprules::ToCombatStat;
using equiprules::ToDerivedStat;
using equiprules::ToEffectKind;
using equiprules::ToRaw;
using equiprules::TryToAffixTier;
using equiprules::ValueRange;

namespace {

// EquipAttribute.effect 的五个取值(表里填的数)
constexpr uint32_t kKindPrimaryPoint = 1;
constexpr uint32_t kKindAllPrimaryPoints = 2;
constexpr uint32_t kKindDerivedFlat = 3;
constexpr uint32_t kKindBothAttacks = 4;
constexpr uint32_t kKindCombat = 5;

// AttributeDimension.id
constexpr uint32_t kConstitution = 101;
constexpr uint32_t kStrength = 103;
constexpr uint32_t kAgility = 104;

// 脚本化随机源:按给定序列依次返回,并记下每次被要的区间长度 n。
// 序列用完还被调用 = 被测函数多掷了骰子:记一条失败并返回 0(不抛异常,免得盖住后面的断言)。
// 必须以**左值**传给 RollAffixes(按引用推导),否则记在副本上、这里什么都看不到。
class ScriptedRand {
public:
    explicit ScriptedRand(std::vector<uint64_t> script) : script_(std::move(script)) {}

    uint64_t operator()(uint64_t n) {
        requested_.push_back(n);
        if (next_ >= script_.size()) {
            ADD_FAILURE() << "rand called more times than scripted: call #" << requested_.size() << " n=" << n;
            return 0;
        }
        const uint64_t value = script_[next_++];
        EXPECT_LT(value, n) << "scripted value out of range at call #" << requested_.size();
        return value;
    }

    // 每次调用要的 n,按调用顺序
    const std::vector<uint64_t>& Requested() const { return requested_; }
    // 脚本是否恰好用完(少掷了同样算错)
    bool Exhausted() const { return next_ == script_.size(); }

private:
    std::vector<uint64_t> script_;
    std::size_t next_ = 0;
    std::vector<uint64_t> requested_;
};

using RangeTable = std::map<uint32_t, ValueRange>;

// rangeOf 的测试实现:查不到 = 本档不可用(cap 0)。表按值捕获,传临时表也安全。
auto RangeOf(RangeTable table) {
    return [table = std::move(table)](uint32_t attrId) -> ValueRange {
        const auto it = table.find(attrId);
        if (it == table.end()) {
            return ValueRange{};
        }
        return it->second;
    };
}

// 同上,并按调用顺序把被问到的 attrId 记进 asked(asked 必须活过返回的函数对象)。
auto RecordingRangeOf(RangeTable table, std::vector<uint32_t>& asked) {
    return [table = std::move(table), &asked](uint32_t attrId) -> ValueRange {
        asked.push_back(attrId);
        const auto it = table.find(attrId);
        if (it == table.end()) {
            return ValueRange{};
        }
        return it->second;
    };
}

struct ExpectedAffix {
    uint32_t attrId;
    AffixTier tier;
    uint32_t value;
    uint32_t seq;
};

void ExpectAffixes(const std::vector<RolledAffix>& actual, const std::vector<ExpectedAffix>& expected) {
    ASSERT_EQ(actual.size(), expected.size());
    for (std::size_t i = 0; i < expected.size(); ++i) {
        EXPECT_EQ(actual[i].attrId, expected[i].attrId) << "index=" << i;
        EXPECT_EQ(ToRaw(actual[i].tier), ToRaw(expected[i].tier)) << "index=" << i;
        EXPECT_EQ(actual[i].value, expected[i].value) << "index=" << i;
        EXPECT_EQ(actual[i].seq, expected[i].seq) << "index=" << i;
    }
}

constexpr AffixTier kBlue = AffixTier::kBlue;
constexpr AffixTier kPink = AffixTier::kPink;
constexpr AffixTier kYellow = AffixTier::kYellow;

// 三条属性的小池:权重 10 / 20 / 30(合计 60)。取值区间长度各不相同(8 / 15 / 22),
// 所以从「掷值时要的 n」就能认出上一步抽到的是谁。
std::vector<PoolEntry> ThreeAttrPool() { return {{1, 10}, {2, 20}, {3, 30}}; }
RangeTable ThreeAttrRanges() { return {{1, {3, 10}}, {2, {6, 20}}, {3, {9, 30}}}; }

// 只有一条属性的池:权重 5,取值 [2, 4](区间长度 3)
std::vector<PoolEntry> SingleAttrPool() { return {{7, 5}}; }
RangeTable SingleAttrRanges() { return {{7, {2, 4}}}; }

void ExpectSameBonus(const EquipBonus& actual, const EquipBonus& expected) {
    EXPECT_EQ(actual.primaryPoints, expected.primaryPoints);
    EXPECT_EQ(actual.allPrimaryPoints, expected.allPrimaryPoints);
    EXPECT_EQ(actual.derivedFlat, expected.derivedFlat);
    EXPECT_EQ(actual.combat, expected.combat);
}

// 四类字段都有非零值的加成:验证「被拒绝时 bonus 一个字段都没动」
EquipBonus SeededBonus() {
    EquipBonus bonus;
    bonus.primaryPoints[kStrength] = 7;
    bonus.allPrimaryPoints = 3;
    bonus.derivedFlat[equiprules::Index(DerivedStat::kSpeed)] = 11;
    bonus.combat[equiprules::Index(CombatStat::kComboRate)] = 5;
    return bonus;
}

// 守约的随机源里最「倒霉」的一个:永远掷出区间里的最大值 n - 1。
// 写成普通函数是因为生产侧就是把一个普通函数(player_equip.cpp 的 RandBelow)当随机源传给规则头的。
uint64_t LargestRoll(uint64_t n) { return n - 1; }

// GemFit 只在进程内传递,规则头没给它配 ToRaw。转成数再比:失败信息里能直接看出是哪一种结果
constexpr uint32_t FitCode(GemFit fit) { return static_cast<uint32_t>(fit); }

// 宝石类型(EquipGem.gem_type,设计文档二期 §2.2)。宝石自己的类型从 1 起;0 只出现在孔位定义里,表示「任意」
constexpr uint32_t kAnyGem = 0;
constexpr uint32_t kGemAttack = 1;
constexpr uint32_t kGemDefense = 2;
constexpr uint32_t kGemHealth = 3;
constexpr uint32_t kGemSwift = 4;

// 设计文档二期 §2.3 的武器孔位:0 号攻击孔(只收攻击宝石)、1 号通用孔(任意类型)
std::vector<GemSocketRule> WeaponSockets() { return {{0, kGemAttack}, {1, kAnyGem}}; }

}  // namespace

// ---------------------------------------------------------------------------
// RollAffixes:蓝属性(条数 → 逐条「抽属性、掷值」)
// ---------------------------------------------------------------------------

// 消费顺序逐次钉死:条数 1 次,然后每条蓝属性「抽属性 1 次 + 掷值 1 次」;
// 不放回 —— 剩余权重和 60 → 30 → 10 逐条缩小;seq 从 0 连续递增
TEST(EquipAffixRollTest, BlueDrawFollowsFixedConsumptionOrder) {
    // 条数:rand(3)=2 → 1+2=3 条
    // 第 1 条:rand(60)=35 → 累加 10/30/60,落在属性 3;rand(22)=0 → 9
    // 第 2 条:剩 {1:10, 2:20},rand(30)=10 → 累加 10/30,落在属性 2;rand(15)=14 → 20
    // 第 3 条:剩 {1:10},rand(10)=9 → 属性 1;rand(8)=4 → 7
    ScriptedRand dice({2, 35, 0, 10, 14, 9, 4});
    const auto rolled = RollAffixes(ThreeAttrPool(), RollRule{1, 3, 0, 0}, dice, RangeOf(ThreeAttrRanges()));

    ExpectAffixes(rolled, {{3, kBlue, 9, 0}, {2, kBlue, 20, 1}, {1, kBlue, 7, 2}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{3, 60, 22, 30, 15, 10, 8}));
    EXPECT_TRUE(dice.Exhausted());
}

// 按权重抽的边界:累加权重 10 / 30 / 60,掷出的数恰好等于累加和时归下一个
TEST(EquipAffixRollTest, WeightedPickBoundaries) {
    struct Case {
        uint64_t ticket;
        uint32_t expectedAttr;
    };
    const Case cases[] = {{0, 1}, {9, 1}, {10, 2}, {29, 2}, {30, 3}, {59, 3}};
    for (const auto& c : cases) {
        ScriptedRand dice({0, c.ticket, 0});
        const auto rolled = RollAffixes(ThreeAttrPool(), RollRule{1, 1, 0, 0}, dice, RangeOf(ThreeAttrRanges()));
        ASSERT_EQ(rolled.size(), 1u) << "ticket=" << c.ticket;
        EXPECT_EQ(rolled[0].attrId, c.expectedAttr) << "ticket=" << c.ticket;
        EXPECT_TRUE(dice.Exhausted()) << "ticket=" << c.ticket;
    }
}

// 池小于 blueMin:能掷几条掷几条(条数夹到候选数),不重复、不死循环
TEST(EquipAffixRollTest, BlueCountIsClampedWhenPoolIsSmallerThanBlueMin) {
    // 条数:rand(3)=2 → 3+2=5 → 夹到 2
    // 第 1 条:rand(30)=0 → 属性 1;rand(5)=4 → 5
    // 第 2 条:剩 {2:20},rand(20)=19 → 属性 2;rand(7)=0 → 2
    ScriptedRand dice({2, 0, 4, 19, 0});
    const auto rolled = RollAffixes({{1, 10}, {2, 20}}, RollRule{3, 5, 0, 0}, dice,
                                    RangeOf({{1, {1, 5}}, {2, {2, 8}}}));

    ExpectAffixes(rolled, {{1, kBlue, 5, 0}, {2, kBlue, 2, 1}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{3, 30, 5, 20, 7}));
    EXPECT_TRUE(dice.Exhausted());
}

// 表配反了(blueMax < blueMin)按 blueMin;条数那一掷仍然发生(rand(1)),消费顺序不因区间退化而变
TEST(EquipAffixRollTest, BlueMaxBelowBlueMinUsesBlueMin) {
    // 条数:rand(1)=0 → 2 条
    // 第 1 条:rand(30)=29 → 属性 2;rand(7)=0 → 2
    // 第 2 条:剩 {1:10},rand(10)=9 → 属性 1;rand(5)=0 → 1
    ScriptedRand dice({0, 29, 0, 9, 0});
    const auto rolled = RollAffixes({{1, 10}, {2, 20}}, RollRule{2, 1, 0, 0}, dice,
                                    RangeOf({{1, {1, 5}}, {2, {2, 8}}}));

    ExpectAffixes(rolled, {{2, kBlue, 2, 0}, {1, kBlue, 1, 1}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{1, 30, 7, 10, 5}));
    EXPECT_TRUE(dice.Exhausted());
}

// 掷出 0 条蓝:只消耗条数那一掷
TEST(EquipAffixRollTest, ZeroBlueCountConsumesOnlyTheCountRoll) {
    ScriptedRand fixedZero(std::vector<uint64_t>{0});
    EXPECT_TRUE(RollAffixes(ThreeAttrPool(), RollRule{0, 0, 0, 0}, fixedZero, RangeOf(ThreeAttrRanges())).empty());
    EXPECT_EQ(fixedZero.Requested(), (std::vector<uint64_t>{1}));

    ScriptedRand rolledZero(std::vector<uint64_t>{0});
    EXPECT_TRUE(RollAffixes(ThreeAttrPool(), RollRule{0, 2, 0, 0}, rolledZero, RangeOf(ThreeAttrRanges())).empty());
    EXPECT_EQ(rolledZero.Requested(), (std::vector<uint64_t>{3}));
}

// ---------------------------------------------------------------------------
// RollAffixes:粉 / 黄(概率 → 抽属性 → 掷值)
// ---------------------------------------------------------------------------

// 粉、黄可以与蓝重复(也可以互相重复):池里只有一条属性时三档全是它,各自 seq = 0
TEST(EquipAffixRollTest, PinkAndYellowMayRepeatBlue) {
    // 蓝:rand(1)=0 → 1 条;rand(5)=4 → 属性 7;rand(3)=1 → 3
    // 粉:rand(10000)=2999 < 3000 掷出;rand(5)=0 → 属性 7;rand(3)=2 → 4
    // 黄:rand(10000)=1499 < 1500 掷出;rand(5)=2 → 属性 7;rand(3)=0 → 2
    ScriptedRand dice({0, 4, 1, 2999, 0, 2, 1499, 2, 0});
    const auto rolled = RollAffixes(SingleAttrPool(), RollRule{1, 1, 3000, 1500}, dice, RangeOf(SingleAttrRanges()));

    ExpectAffixes(rolled, {{7, kBlue, 3, 0}, {7, kPink, 4, 0}, {7, kYellow, 2, 0}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{1, 5, 3, 10000, 5, 3, 10000, 5, 3}));
    EXPECT_TRUE(dice.Exhausted());
}

// 粉是放回抽取:蓝把三条属性抽光之后,粉仍然从完整的池(权重和回到 60)里抽
TEST(EquipAffixRollTest, PinkDrawsFromFullPoolAfterBlueExhaustedIt) {
    // 蓝 3 条:rand(1)=0;rand(60)=0 → 属性 1、rand(8)=0 → 3;rand(50)=0 → 属性 2、rand(15)=0 → 6;
    //          rand(30)=0 → 属性 3、rand(22)=0 → 9
    // 粉:rand(10000)=0 掷出;rand(60)=59 → 属性 3;rand(22)=21 → 30
    ScriptedRand dice({0, 0, 0, 0, 0, 0, 0, 0, 59, 21});
    const auto rolled = RollAffixes(ThreeAttrPool(), RollRule{3, 3, 10000, 0}, dice, RangeOf(ThreeAttrRanges()));

    ExpectAffixes(rolled, {{1, kBlue, 3, 0}, {2, kBlue, 6, 1}, {3, kBlue, 9, 2}, {3, kPink, 30, 0}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{1, 60, 8, 50, 15, 30, 22, 10000, 60, 22}));
    EXPECT_TRUE(dice.Exhausted());
}

// 概率边界:掷出的数必须**严格小于** rate 才算掷出;没掷出只消耗那一次概率掷骰
TEST(EquipAffixRollTest, RollEqualToRateMisses) {
    ScriptedRand dice({0, 0, 0, 3000, 1500});
    const auto rolled = RollAffixes(SingleAttrPool(), RollRule{1, 1, 3000, 1500}, dice, RangeOf(SingleAttrRanges()));

    ExpectAffixes(rolled, {{7, kBlue, 2, 0}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{1, 5, 3, 10000, 10000}));
    EXPECT_TRUE(dice.Exhausted());
}

// 粉没掷出不影响黄:黄照常掷概率、抽属性、掷值
TEST(EquipAffixRollTest, YellowIsRolledIndependentlyOfPink) {
    ScriptedRand dice({0, 0, 0, 9999, 0, 0, 0});
    const auto rolled = RollAffixes(SingleAttrPool(), RollRule{1, 1, 3000, 1500}, dice, RangeOf(SingleAttrRanges()));

    ExpectAffixes(rolled, {{7, kBlue, 2, 0}, {7, kYellow, 2, 0}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{1, 5, 3, 10000, 10000, 5, 3}));
    EXPECT_TRUE(dice.Exhausted());
}

// rate = 0:不出,而且**不消耗随机数**(策划把概率填 0 = 关闭,不能因此挪动后面的掷骰)
TEST(EquipAffixRollTest, ZeroRateIsSkippedWithoutConsumingRand) {
    ScriptedRand bothOff({0, 0, 0});
    ExpectAffixes(RollAffixes(SingleAttrPool(), RollRule{1, 1, 0, 0}, bothOff, RangeOf(SingleAttrRanges())),
                  {{7, kBlue, 2, 0}});
    EXPECT_EQ(bothOff.Requested(), (std::vector<uint64_t>{1, 5, 3}));
    EXPECT_TRUE(bothOff.Exhausted());

    // 粉关、黄开:黄的概率掷骰紧跟在蓝后面,中间没有给粉留一掷
    ScriptedRand pinkOff({0, 0, 0, 0, 4, 2});
    ExpectAffixes(RollAffixes(SingleAttrPool(), RollRule{1, 1, 0, 1500}, pinkOff, RangeOf(SingleAttrRanges())),
                  {{7, kBlue, 2, 0}, {7, kYellow, 4, 0}});
    EXPECT_EQ(pinkOff.Requested(), (std::vector<uint64_t>{1, 5, 3, 10000, 5, 3}));
    EXPECT_TRUE(pinkOff.Exhausted());
}

// rate >= 10000 的约定:必出,且**仍然消耗**概率那一掷(要的区间仍是 10000,不是 rate)。
// 9999 是概率掷骰能掷出的最大值,它也必须算掷出
TEST(EquipAffixRollTest, RateAtOrAboveBaseAlwaysHitsButStillConsumesTheRoll) {
    ScriptedRand exact({0, 0, 0, 9999, 0, 0, 9999, 0, 0});
    ExpectAffixes(RollAffixes(SingleAttrPool(), RollRule{1, 1, 10000, 10000}, exact, RangeOf(SingleAttrRanges())),
                  {{7, kBlue, 2, 0}, {7, kPink, 2, 0}, {7, kYellow, 2, 0}});
    EXPECT_EQ(exact.Requested(), (std::vector<uint64_t>{1, 5, 3, 10000, 5, 3, 10000, 5, 3}));
    EXPECT_TRUE(exact.Exhausted());

    // 表里填超了(20000 / UINT32_MAX)与填 10000 完全等价
    ScriptedRand over({0, 0, 0, 9999, 0, 0, 9999, 0, 0});
    ExpectAffixes(RollAffixes(SingleAttrPool(), RollRule{1, 1, 20000, UINT32_MAX}, over, RangeOf(SingleAttrRanges())),
                  {{7, kBlue, 2, 0}, {7, kPink, 2, 0}, {7, kYellow, 2, 0}});
    EXPECT_EQ(over.Requested(), (std::vector<uint64_t>{1, 5, 3, 10000, 5, 3, 10000, 5, 3}));
    EXPECT_TRUE(over.Exhausted());
}

// ---------------------------------------------------------------------------
// RollAffixes:候选过滤与取值区间
// ---------------------------------------------------------------------------

// cap == 0(本装备等级档没有这条属性)与 weight == 0 的条目都不参与:不占权重、不占蓝条数。
// rangeOf 只对「权重 > 0」的属性按池内顺序各问一次,权重 0 的根本不问
TEST(EquipAffixRollTest, UnavailableAndZeroWeightEntriesAreSkipped) {
    // 候选只剩 {1:10, 3:30}(属性 2 查不到区间;属性 9 权重 0,即使有区间也不算)
    // 条数:rand(1)=0 → 3 → 夹到 2
    // 第 1 条:rand(40)=10 → 累加 10/40,落在属性 3;rand(22)=21 → 30
    // 第 2 条:剩 {1:10},rand(10)=9 → 属性 1;rand(8)=7 → 10
    std::vector<uint32_t> asked;
    ScriptedRand dice({0, 10, 21, 9, 7});
    const auto rolled = RollAffixes({{1, 10}, {2, 20}, {9, 0}, {3, 30}}, RollRule{3, 3, 0, 0}, dice,
                                    RecordingRangeOf({{1, {3, 10}}, {3, {9, 30}}, {9, {1, 1}}}, asked));

    ExpectAffixes(rolled, {{3, kBlue, 30, 0}, {1, kBlue, 10, 1}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{1, 40, 22, 10, 8}));
    EXPECT_TRUE(dice.Exhausted());
    EXPECT_EQ(asked, (std::vector<uint32_t>{1, 2, 3}));
}

// 值域夹取:下限为 0 按 1、下限大于上限按上限,恒有 1 <= value <= cap;
// 区间退化成单点时仍掷一次 rand(1);上限取到 UINT32_MAX 也不溢出
TEST(EquipAffixRollTest, ValueIsClampedIntoOneToCap) {
    struct Case {
        ValueRange range;
        uint64_t expectedSpan;  // 掷值那一次要的 n
        uint64_t roll;
        uint32_t expectedValue;
    };
    const Case cases[] = {
        {{0, 5}, 5, 0, 1},                                  // 下限 0 → 按 1,最小掷出 1
        {{0, 5}, 5, 4, 5},                                  // 最大掷出 cap
        {{3, 5}, 3, 0, 3},                                  // 正常区间的两端
        {{3, 5}, 3, 2, 5},
        {{9, 5}, 1, 0, 5},                                  // 下限 > 上限 → 按上限,单点
        {{5, 5}, 1, 0, 5},                                  // 下限 == 上限,单点
        {{1, UINT32_MAX}, UINT32_MAX, UINT32_MAX - 1, UINT32_MAX},  // 区间长度 2^32 - 1,不溢出
        {{0, UINT32_MAX}, UINT32_MAX, 0, 1},
    };
    for (const auto& c : cases) {
        ScriptedRand dice({0, 0, c.roll});
        const auto rolled = RollAffixes({{1, 1}}, RollRule{1, 1, 0, 0}, dice, RangeOf({{1, c.range}}));
        ASSERT_EQ(rolled.size(), 1u) << "min=" << c.range.minValue << " cap=" << c.range.cap;
        EXPECT_EQ(rolled[0].value, c.expectedValue) << "min=" << c.range.minValue << " cap=" << c.range.cap;
        EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{1, 1, c.expectedSpan}))
            << "min=" << c.range.minValue << " cap=" << c.range.cap;
    }
}

// 没有任何可用候选(空池 / 全 0 权重 / 全部本档不可用):返回空,而且一次随机数都不消耗 ——
// 哪怕粉黄概率是 100%。脚本为空,任何一次 rand 调用都会让用例失败
TEST(EquipAffixRollTest, NoUsableCandidateReturnsNothingAndConsumesNothing) {
    const RollRule greedy{1, 3, 10000, 10000};

    ScriptedRand emptyPool(std::vector<uint64_t>{});
    EXPECT_TRUE(RollAffixes({}, greedy, emptyPool, RangeOf(ThreeAttrRanges())).empty());
    EXPECT_TRUE(emptyPool.Requested().empty());

    ScriptedRand zeroWeights(std::vector<uint64_t>{});
    EXPECT_TRUE(RollAffixes({{1, 0}, {2, 0}, {3, 0}}, greedy, zeroWeights, RangeOf(ThreeAttrRanges())).empty());
    EXPECT_TRUE(zeroWeights.Requested().empty());

    ScriptedRand noRanges(std::vector<uint64_t>{});
    EXPECT_TRUE(RollAffixes(ThreeAttrPool(), greedy, noRanges, RangeOf({})).empty());
    EXPECT_TRUE(noRanges.Requested().empty());
}

// 同一属性在池里出现两次(坏表):权重合并成一条,蓝属性仍然互不重复;rangeOf 对它只问一次
TEST(EquipAffixRollTest, DuplicatePoolEntriesAreMergedSoBlueStaysUnique) {
    // 候选 {1:10+30=40, 2:20},合计 60
    // 条数:rand(1)=0 → 2 条
    // 第 1 条:rand(60)=39 → 累加 40,落在属性 1;rand(8)=0 → 3
    // 第 2 条:剩 {2:20},rand(20)=19 → 属性 2;rand(15)=0 → 6
    //   (不合并的话这里还剩一个「属性 1」可抽,要的区间会是 30 而不是 20,蓝属性就可能重复)
    std::vector<uint32_t> asked;
    ScriptedRand dice({0, 39, 0, 19, 0});
    const auto rolled = RollAffixes({{1, 10}, {2, 20}, {1, 30}}, RollRule{2, 2, 0, 0}, dice,
                                    RecordingRangeOf(ThreeAttrRanges(), asked));

    ExpectAffixes(rolled, {{1, kBlue, 3, 0}, {2, kBlue, 6, 1}});
    EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{1, 60, 8, 20, 15}));
    EXPECT_TRUE(dice.Exhausted());
    EXPECT_EQ(asked, (std::vector<uint32_t>{1, 2}));
}

// blueMax 巨大 + 权重巨大:条数区间长度 2^32、权重和 3 × (2^32 - 1) 都超出 32 位,
// 必须按 64 位算 —— 32 位下区间长度会绕成 0(rand(0) 非法)、权重和会绕成一个小数
TEST(EquipAffixRollTest, HugeBlueMaxAndWeightsUse64BitArithmetic) {
    constexpr uint64_t kWeight = UINT32_MAX;  // 4294967295
    // 条数:rand(2^32)=4000000000 → 夹到 3
    // 第 1 条:rand(3W)=2W → 累加 W/2W/3W,落在属性 3;rand(1)=0 → 1
    // 第 2 条:剩 {1:W, 2:W},rand(2W)=W-1 → 属性 1;rand(1)=0 → 1
    // 第 3 条:剩 {2:W},rand(W)=0 → 属性 2;rand(1)=0 → 1
    ScriptedRand dice({4000000000ull, 2 * kWeight, 0, kWeight - 1, 0, 0, 0});
    const auto rolled =
        RollAffixes({{1, UINT32_MAX}, {2, UINT32_MAX}, {3, UINT32_MAX}}, RollRule{0, UINT32_MAX, 0, 0}, dice,
                    RangeOf({{1, {1, 1}}, {2, {1, 1}}, {3, {1, 1}}}));

    ExpectAffixes(rolled, {{3, kBlue, 1, 0}, {1, kBlue, 1, 1}, {2, kBlue, 1, 2}});
    EXPECT_EQ(dice.Requested(),
              (std::vector<uint64_t>{4294967296ull, 3 * kWeight, 1, 2 * kWeight, 1, kWeight, 1}));
    EXPECT_TRUE(dice.Exhausted());
}

// 随机源不守约(永远返回超大值):结果被夹回区间内,不越界、不死循环。
// 夹取后的效果 = 每次都掷出区间里的最大值:条数取上限、抽到剩余候选里的最后一个、值取 cap、概率不中
TEST(EquipAffixRollTest, OutOfContractRandIsClampedNotTrusted) {
    std::size_t calls = 0;
    auto broken = [&calls](uint64_t) {
        ++calls;
        return UINT64_MAX;
    };
    const auto rolled = RollAffixes(ThreeAttrPool(), RollRule{1, 3, 5000, 5000}, broken, RangeOf(ThreeAttrRanges()));

    ExpectAffixes(rolled, {{3, kBlue, 30, 0}, {2, kBlue, 20, 1}, {1, kBlue, 10, 2}});
    EXPECT_EQ(calls, 9u);  // 条数 1 + 蓝 3 × 2 + 粉概率 1 + 黄概率 1
}

// ---------------------------------------------------------------------------
// RollAffixes:真实设计数据抽查(设计文档 §2.3 / §2.4 / §2.6)
// ---------------------------------------------------------------------------

// 武器池 12 条(权重各 100,顺序同 tools/scripts/equip_xlsx_patch.py 里 EQUIP_AFFIX_POOLS 的声明顺序;
// 导出的表行按 id 排,次序与这里不同 —— 本用例只断言不变量,与池内次序无关)+ 规则 (1, 3, 3000, 1500),
// 取值区间抄 §2.6 的 L80 档(min_value = max(1, ceil(cap × 0.3)))。真随机固定种子跑 2000 次,
// 每次都必须满足:蓝 1..3 条且互不重复、seq 连续、粉黄各至多 1 条、顺序蓝 → 粉 → 黄、值在 [min, cap] 内
TEST(EquipAffixRollTest, WeaponPoolWithDesignDataKeepsInvariants) {
    const std::vector<PoolEntry> weaponPool{
        {1, 100},   // 伤害
        {3, 100},   // 力量
        {4, 100},   // 体质
        {5, 100},   // 灵力
        {6, 100},   // 敏捷
        {2, 100},   // 准确
        {14, 100},  // 物理连击率
        {15, 100},  // 反击率
        {12, 100},  // 物理必杀率
        {17, 100},  // 所有技能上升
        {18, 100},  // 忽视所有抗异常
        {13, 100},  // 法术必杀率
    };
    const RangeTable level80Ranges{
        {1, {360, 1200}}, {2, {480, 1600}}, {3, {6, 20}},  {4, {6, 20}},  {5, {6, 20}},  {6, {6, 20}},
        {12, {3, 10}},    {13, {3, 10}},    {14, {3, 10}}, {15, {3, 10}}, {17, {2, 5}},  {18, {6, 20}},
    };
    ASSERT_EQ(weaponPool.size(), 12u);
    ASSERT_EQ(level80Ranges.size(), weaponPool.size());
    const RollRule rule{1, 3, 3000, 1500};
    constexpr int kRounds = 2000;

    std::mt19937_64 engine(20261007);
    auto dice = [&engine](uint64_t n) { return std::uniform_int_distribution<uint64_t>(0, n - 1)(engine); };
    const auto rangeOf = RangeOf(level80Ranges);

    int blueCountSeen[4] = {0, 0, 0, 0};
    int pinkTotal = 0;
    int yellowTotal = 0;
    for (int round = 0; round < kRounds; ++round) {
        const auto rolled = RollAffixes(weaponPool, rule, dice, rangeOf);

        std::set<uint32_t> blueAttrs;
        uint32_t blueCount = 0;
        int pinkCount = 0;
        int yellowCount = 0;
        uint32_t previousTier = ToRaw(kBlue);
        for (const auto& affix : rolled) {
            const auto range = level80Ranges.find(affix.attrId);
            ASSERT_TRUE(range != level80Ranges.end()) << "round=" << round << " attr=" << affix.attrId;
            ASSERT_GE(affix.value, range->second.minValue) << "round=" << round << " attr=" << affix.attrId;
            ASSERT_LE(affix.value, range->second.cap) << "round=" << round << " attr=" << affix.attrId;
            ASSERT_GE(ToRaw(affix.tier), previousTier) << "round=" << round;  // 蓝 → 粉 → 黄,不回头
            previousTier = ToRaw(affix.tier);

            if (affix.tier == kBlue) {
                ASSERT_EQ(affix.seq, blueCount) << "round=" << round;  // seq 从 0 连续
                ASSERT_TRUE(blueAttrs.insert(affix.attrId).second) << "round=" << round << " attr=" << affix.attrId;
                ++blueCount;
            } else if (affix.tier == kPink) {
                ASSERT_EQ(affix.seq, 0u) << "round=" << round;
                ++pinkCount;
            } else if (affix.tier == kYellow) {
                ASSERT_EQ(affix.seq, 0u) << "round=" << round;
                ++yellowCount;
            } else {
                FAIL() << "unexpected tier " << ToRaw(affix.tier) << " round=" << round;
            }
        }
        ASSERT_GE(blueCount, 1u) << "round=" << round;
        ASSERT_LE(blueCount, 3u) << "round=" << round;
        ASSERT_LE(pinkCount, 1) << "round=" << round;
        ASSERT_LE(yellowCount, 1) << "round=" << round;
        ++blueCountSeen[blueCount];
        pinkTotal += pinkCount;
        yellowTotal += yellowCount;
    }

    // 粗粒度的分布检查,只为抓住「万分比当成千分比」「条数恒为上限」这类量级错误。
    // 带宽都在 6 个标准差以外(粉 600 ± 20,黄 300 ± 16,每种条数 667 ± 21),不会因为换平台 / 换种子而抖
    EXPECT_GT(blueCountSeen[1], 400);
    EXPECT_GT(blueCountSeen[2], 400);
    EXPECT_GT(blueCountSeen[3], 400);
    EXPECT_GT(pinkTotal, 400);
    EXPECT_LT(pinkTotal, 800);
    EXPECT_GT(yellowTotal, 150);
    EXPECT_LT(yellowTotal, 450);
}

// ---------------------------------------------------------------------------
// ApplyEffect / PrimaryPointsFor
// ---------------------------------------------------------------------------

// kind 1:按维度累加;param == 0 非法。维度是否真实存在不归规则头管(调用方的表校验负责)
TEST(EquipAttributeRulesTest, PrimaryPointAccumulatesPerDimension) {
    EquipBonus bonus;
    EXPECT_TRUE(ApplyEffect(bonus, kKindPrimaryPoint, kStrength, 5));
    EXPECT_TRUE(ApplyEffect(bonus, kKindPrimaryPoint, kStrength, 7));
    EXPECT_TRUE(ApplyEffect(bonus, kKindPrimaryPoint, kAgility, 2));
    EXPECT_TRUE(ApplyEffect(bonus, kKindPrimaryPoint, 999, 1));

    EquipBonus expected;
    expected.primaryPoints = {{kStrength, 12}, {kAgility, 2}, {999, 1}};
    ExpectSameBonus(bonus, expected);

    auto seeded = SeededBonus();
    EXPECT_FALSE(ApplyEffect(seeded, kKindPrimaryPoint, 0, 5));
    ExpectSameBonus(seeded, SeededBonus());
}

// kind 2:只加到 allPrimaryPoints,不展开成各维度;param 忽略(表里填 0)
TEST(EquipAttributeRulesTest, AllPrimaryPointsIgnoresParam) {
    EquipBonus bonus;
    EXPECT_TRUE(ApplyEffect(bonus, kKindAllPrimaryPoints, 0, 4));
    EXPECT_TRUE(ApplyEffect(bonus, kKindAllPrimaryPoints, 12345, 6));

    EquipBonus expected;
    expected.allPrimaryPoints = 10;
    ExpectSameBonus(bonus, expected);
}

// kind 3:param 必须是 1..6 的 DerivedStat;0、7 与更大的数都拒绝且不改 bonus。
// 262 = 256 + 6:先窄化成 uint8 再判断的实现会把它当成 6 放行
TEST(EquipAttributeRulesTest, DerivedFlatAcceptsOnlyKnownStats) {
    EquipBonus bonus;
    for (uint32_t param = 1; param <= 6; ++param) {
        EXPECT_TRUE(ApplyEffect(bonus, kKindDerivedFlat, param, param * 10)) << "param=" << param;
    }
    EquipBonus expected;
    expected.derivedFlat = {0, 10, 20, 30, 40, 50, 60};
    ExpectSameBonus(bonus, expected);
    EXPECT_EQ(DerivedFlatOf(bonus, DerivedStat::kSpeed), 50u);
    EXPECT_EQ(DerivedFlatOf(bonus, DerivedStat::kDefense), 60u);

    for (const uint32_t param : {0u, 7u, 255u, 262u, UINT32_MAX}) {
        auto seeded = SeededBonus();
        EXPECT_FALSE(ApplyEffect(seeded, kKindDerivedFlat, param, 5)) << "param=" << param;
        ExpectSameBonus(seeded, SeededBonus());
    }
}

// kind 4(伤害):物伤与法伤各加一份,别的不动;param 忽略
TEST(EquipAttributeRulesTest, BothAttacksAddsToPhysicalAndMagic) {
    EquipBonus bonus;
    EXPECT_TRUE(ApplyEffect(bonus, kKindBothAttacks, 0, 50));
    EXPECT_TRUE(ApplyEffect(bonus, kKindBothAttacks, 99, 1));

    EquipBonus expected;
    expected.derivedFlat[equiprules::Index(DerivedStat::kPhysicalAttack)] = 51;
    expected.derivedFlat[equiprules::Index(DerivedStat::kMagicAttack)] = 51;
    ExpectSameBonus(bonus, expected);
}

// kind 5:param 必须是 1..15 的 CombatStat;0、16 与更大的数都拒绝且不改 bonus(271 = 256 + 15,同上)
TEST(EquipAttributeRulesTest, CombatAcceptsOnlyKnownStats) {
    EquipBonus bonus;
    for (uint32_t param = 1; param <= 15; ++param) {
        EXPECT_TRUE(ApplyEffect(bonus, kKindCombat, param, param)) << "param=" << param;
    }
    EquipBonus expected;
    expected.combat = {0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15};
    ExpectSameBonus(bonus, expected);
    EXPECT_EQ(CombatOf(bonus, CombatStat::kPhysicalCritRate), 1u);
    EXPECT_EQ(CombatOf(bonus, CombatStat::kPhysicalResist), 15u);

    for (const uint32_t param : {0u, 16u, 255u, 271u, UINT32_MAX}) {
        auto seeded = SeededBonus();
        EXPECT_FALSE(ApplyEffect(seeded, kKindCombat, param, 5)) << "param=" << param;
        ExpectSameBonus(seeded, SeededBonus());
    }
}

// 未知 kind(0、6、更大的数;257 = 256 + 1 同上)一律拒绝且不改 bonus,哪怕 param 本身合法
TEST(EquipAttributeRulesTest, UnknownKindIsRejectedAndBonusUntouched) {
    for (const uint32_t kind : {0u, 6u, 255u, 257u, UINT32_MAX}) {
        auto seeded = SeededBonus();
        EXPECT_FALSE(ApplyEffect(seeded, kind, kStrength, 5)) << "kind=" << kind;
        EXPECT_FALSE(ApplyEffect(seeded, kind, 1, 5)) << "kind=" << kind;
        ExpectSameBonus(seeded, SeededBonus());
    }
}

// 加法饱和到 uint64 上限,不回绕成小数;五种 kind 与 PrimaryPointsFor 的求和都一样
TEST(EquipAttributeRulesTest, AdditionSaturatesInsteadOfWrapping) {
    constexpr uint64_t kNearMax = UINT64_MAX - 1;  // 再加 5:回绕的话会变成 3

    EquipBonus bonus;
    EXPECT_TRUE(ApplyEffect(bonus, kKindPrimaryPoint, kStrength, kNearMax));
    EXPECT_TRUE(ApplyEffect(bonus, kKindPrimaryPoint, kStrength, 5));
    EXPECT_EQ(bonus.primaryPoints.at(kStrength), UINT64_MAX);

    EXPECT_TRUE(ApplyEffect(bonus, kKindAllPrimaryPoints, 0, kNearMax));
    EXPECT_TRUE(ApplyEffect(bonus, kKindAllPrimaryPoints, 0, 5));
    EXPECT_EQ(bonus.allPrimaryPoints, UINT64_MAX);

    EXPECT_TRUE(ApplyEffect(bonus, kKindDerivedFlat, ToRaw(DerivedStat::kMaxHealth), kNearMax));
    EXPECT_TRUE(ApplyEffect(bonus, kKindDerivedFlat, ToRaw(DerivedStat::kMaxHealth), 5));
    EXPECT_EQ(DerivedFlatOf(bonus, DerivedStat::kMaxHealth), UINT64_MAX);

    EXPECT_TRUE(ApplyEffect(bonus, kKindBothAttacks, 0, kNearMax));
    EXPECT_TRUE(ApplyEffect(bonus, kKindBothAttacks, 0, 5));
    EXPECT_EQ(DerivedFlatOf(bonus, DerivedStat::kPhysicalAttack), UINT64_MAX);
    EXPECT_EQ(DerivedFlatOf(bonus, DerivedStat::kMagicAttack), UINT64_MAX);

    EXPECT_TRUE(ApplyEffect(bonus, kKindCombat, ToRaw(CombatStat::kComboRate), kNearMax));
    EXPECT_TRUE(ApplyEffect(bonus, kKindCombat, ToRaw(CombatStat::kComboRate), 5));
    EXPECT_EQ(CombatOf(bonus, CombatStat::kComboRate), UINT64_MAX);

    // 单项与「所有属性」都已顶到上限:求和同样饱和
    EXPECT_EQ(PrimaryPointsFor(bonus, kStrength), UINT64_MAX);
}

// value == 0 合法但无效果:不为它建一个值为 0 的键(否则「有没有装备加成」无法从 map 是否为空看出来)
TEST(EquipAttributeRulesTest, ZeroValueIsAcceptedWithoutCreatingKey) {
    EquipBonus bonus;
    EXPECT_TRUE(ApplyEffect(bonus, kKindPrimaryPoint, kStrength, 0));
    EXPECT_TRUE(ApplyEffect(bonus, kKindAllPrimaryPoints, 0, 0));
    EXPECT_TRUE(ApplyEffect(bonus, kKindDerivedFlat, ToRaw(DerivedStat::kSpeed), 0));
    EXPECT_TRUE(ApplyEffect(bonus, kKindBothAttacks, 0, 0));
    EXPECT_TRUE(ApplyEffect(bonus, kKindCombat, ToRaw(CombatStat::kReflectRate), 0));
    ExpectSameBonus(bonus, EquipBonus{});
}

// 一级属性维度的装备点 = 单项(没有则 0)+ 所有属性;只读,不会顺手建键
TEST(EquipAttributeRulesTest, PrimaryPointsForAddsSingleAndAll) {
    EquipBonus bonus;
    EXPECT_EQ(PrimaryPointsFor(bonus, kStrength), 0u);

    ASSERT_TRUE(ApplyEffect(bonus, kKindPrimaryPoint, kStrength, 5));
    EXPECT_EQ(PrimaryPointsFor(bonus, kStrength), 5u);
    EXPECT_EQ(PrimaryPointsFor(bonus, kConstitution), 0u);

    ASSERT_TRUE(ApplyEffect(bonus, kKindAllPrimaryPoints, 0, 3));
    EXPECT_EQ(PrimaryPointsFor(bonus, kStrength), 8u);
    EXPECT_EQ(PrimaryPointsFor(bonus, kConstitution), 3u);
    EXPECT_EQ(bonus.primaryPoints.size(), 1u);
}

// 按名字读数组:kNone 恒为 0(ApplyEffect 不会写第 0 格),kCount 越界返回 0 而不是读出界
TEST(EquipAttributeRulesTest, StatReadersReturnZeroOutOfRange) {
    auto bonus = SeededBonus();
    EXPECT_EQ(DerivedFlatOf(bonus, DerivedStat::kSpeed), 11u);
    EXPECT_EQ(DerivedFlatOf(bonus, DerivedStat::kNone), 0u);
    EXPECT_EQ(DerivedFlatOf(bonus, DerivedStat::kCount), 0u);
    EXPECT_EQ(CombatOf(bonus, CombatStat::kComboRate), 5u);
    EXPECT_EQ(CombatOf(bonus, CombatStat::kNone), 0u);
    EXPECT_EQ(CombatOf(bonus, CombatStat::kCount), 0u);
}

// ---------------------------------------------------------------------------
// 原始数 → 枚举
// ---------------------------------------------------------------------------

// 表 / 存档里的数转枚举:越界一律 kNone。256 + n 这组值专门防「先窄化成 uint8 再比较」
TEST(EquipAttributeRulesTest, RawValuesConvertToEnumsOrNone) {
    EXPECT_EQ(ToRaw(ToEffectKind(0)), ToRaw(EffectKind::kNone));
    EXPECT_EQ(ToRaw(ToEffectKind(1)), ToRaw(EffectKind::kPrimaryPoint));
    EXPECT_EQ(ToRaw(ToEffectKind(5)), ToRaw(EffectKind::kCombat));
    EXPECT_EQ(ToRaw(ToEffectKind(6)), ToRaw(EffectKind::kNone));
    EXPECT_EQ(ToRaw(ToEffectKind(257)), ToRaw(EffectKind::kNone));

    EXPECT_EQ(ToRaw(ToDerivedStat(0)), ToRaw(DerivedStat::kNone));
    EXPECT_EQ(ToRaw(ToDerivedStat(5)), ToRaw(DerivedStat::kSpeed));
    EXPECT_EQ(ToRaw(ToDerivedStat(6)), ToRaw(DerivedStat::kDefense));
    EXPECT_EQ(ToRaw(ToDerivedStat(7)), ToRaw(DerivedStat::kNone));
    EXPECT_EQ(ToRaw(ToDerivedStat(262)), ToRaw(DerivedStat::kNone));

    EXPECT_EQ(ToRaw(ToCombatStat(0)), ToRaw(CombatStat::kNone));
    EXPECT_EQ(ToRaw(ToCombatStat(1)), ToRaw(CombatStat::kPhysicalCritRate));
    EXPECT_EQ(ToRaw(ToCombatStat(15)), ToRaw(CombatStat::kPhysicalResist));
    EXPECT_EQ(ToRaw(ToCombatStat(16)), ToRaw(CombatStat::kNone));
    EXPECT_EQ(ToRaw(ToCombatStat(271)), ToRaw(CombatStat::kNone));
    EXPECT_EQ(ToRaw(ToCombatStat(UINT32_MAX)), ToRaw(CombatStat::kNone));
}

// 颜色档:0..4 可转(0 是基础属性,只用于显示行);越界转换失败且不改出参。
// 存档里的随机属性档只认 1..4(4 绿预留,读到按合法处理)
TEST(EquipAttributeRulesTest, AffixTierConversionAndValidity) {
    AffixTier tier = AffixTier::kPink;
    EXPECT_TRUE(TryToAffixTier(0, tier));
    EXPECT_EQ(ToRaw(tier), ToRaw(AffixTier::kBase));
    EXPECT_TRUE(TryToAffixTier(3, tier));
    EXPECT_EQ(ToRaw(tier), ToRaw(AffixTier::kYellow));
    EXPECT_TRUE(TryToAffixTier(4, tier));
    EXPECT_EQ(ToRaw(tier), ToRaw(AffixTier::kGreen));
    for (const uint32_t raw : {5u, 255u, 260u, UINT32_MAX}) {
        EXPECT_FALSE(TryToAffixTier(raw, tier)) << "raw=" << raw;
        EXPECT_EQ(ToRaw(tier), ToRaw(AffixTier::kGreen)) << "raw=" << raw;
    }

    EXPECT_FALSE(IsRandomAffixTier(0));
    EXPECT_TRUE(IsRandomAffixTier(1));
    EXPECT_TRUE(IsRandomAffixTier(2));
    EXPECT_TRUE(IsRandomAffixTier(3));
    EXPECT_TRUE(IsRandomAffixTier(4));
    EXPECT_FALSE(IsRandomAffixTier(5));
    EXPECT_FALSE(IsRandomAffixTier(257));
    EXPECT_FALSE(IsRandomAffixTier(UINT32_MAX));
}

// ---------------------------------------------------------------------------
// PickLevelTier
// ---------------------------------------------------------------------------

// 等级档就是表行的自然顺序,不要求有序:返回的是下标,不是等级
TEST(EquipLevelTierTest, PicksLargestLevelNotAboveEquipLevelFromUnorderedInput) {
    const std::vector<uint32_t> levels{40, 1, 80, 20, 60};
    EXPECT_EQ(PickLevelTier(levels, 1), 1);
    EXPECT_EQ(PickLevelTier(levels, 19), 1);
    EXPECT_EQ(PickLevelTier(levels, 39), 3);
    EXPECT_EQ(PickLevelTier(levels, 59), 0);
    EXPECT_EQ(PickLevelTier(levels, 79), 4);
    EXPECT_EQ(PickLevelTier(levels, 85), 2);
    EXPECT_EQ(PickLevelTier(levels, UINT32_MAX), 2);
}

// 恰好等于档起点时落在这一档(level 是「含」的起点)
TEST(EquipLevelTierTest, ExactLevelBelongsToThatTier) {
    const std::vector<uint32_t> levels{40, 1, 80, 20, 60};
    EXPECT_EQ(PickLevelTier(levels, 20), 3);
    EXPECT_EQ(PickLevelTier(levels, 40), 0);
    EXPECT_EQ(PickLevelTier(levels, 60), 4);
    EXPECT_EQ(PickLevelTier(levels, 80), 2);
}

// 一档都不满足(全都更大 / 空表)= 该属性在这件装备上不可用,返回 -1;并列时取下标最小的
TEST(EquipLevelTierTest, NoTierAtOrBelowReturnsMinusOne) {
    EXPECT_EQ(PickLevelTier({40, 1, 80, 20, 60}, 0), -1);
    EXPECT_EQ(PickLevelTier({20, 40}, 19), -1);
    EXPECT_EQ(PickLevelTier({}, 10), -1);
    EXPECT_EQ(PickLevelTier({20, 20, 10}, 30), 0);
}

// ---------------------------------------------------------------------------
// ScaleByPermyriad(二期:强化按万分比放大基础属性)
// ---------------------------------------------------------------------------

// 0 级严格恒等:bonus == 0 时任何基础值原样返回 —— 未强化的装备与一期逐位一致(设计文档二期 §5 不变量 4)
TEST(EquipEnhanceRulesTest, ScaleWithZeroBonusIsExactIdentity) {
    const uint64_t bases[] = {0, 1, 12, 40, 9999, 10000, 19999, UINT32_MAX, UINT64_MAX - 1, UINT64_MAX};
    for (const uint64_t base : bases) {
        EXPECT_EQ(ScaleByPermyriad(base, 0), base) << "base=" << base;
    }
}

// 契约写死的五个锚点。规则头里有同一组 static_assert;这里是运行期版本,失败时能看到算出来的数
TEST(EquipEnhanceRulesTest, ScaleMatchesContractAnchors) {
    EXPECT_EQ(ScaleByPermyriad(40, 500), 42u);         // 40 × 5% = 2
    EXPECT_EQ(ScaleByPermyriad(12, 500), 12u);         // 12 × 5% = 0.6 → 0:基础值 12 的装备 +1 时数值不变
    EXPECT_EQ(ScaleByPermyriad(19999, 5000), 29998u);  // 9999.5 → 9999(整万部分 5000 + 余数部分 4999)
    EXPECT_EQ(ScaleByPermyriad(UINT64_MAX, 1), UINT64_MAX);
    EXPECT_EQ(ScaleByPermyriad(40, 0), 40u);
}

// 增量向下取整:不足 1 的部分丢掉,恰好够 1 才加 1
TEST(EquipEnhanceRulesTest, ScaleIncrementRoundsDown) {
    struct Case {
        uint64_t base;
        uint32_t bonus;
        uint64_t expected;
    };
    const Case cases[] = {
        {19, 500, 19},       // 0.95 → 0
        {20, 500, 21},       // 恰好 1
        {39, 500, 40},       // 1.95 → 1
        {1, 9999, 1},        // 0.9999 → 0
        {1, 10000, 2},       // +100% 恰好翻倍
        {3, 3333, 3},        // 0.9999 → 0
        {3, 3334, 4},        // 1.0002 → 1
        {9999, 1, 9999},     // 0.9999 → 0
        {10000, 1, 10001},   // 增量全部来自整万部分
        {10001, 1, 10002},   // 1.0001 → 1
        {19999, 1, 20000},   // 1.9999 → 1
        {0, 8000, 0},        // 基础值 0 放大多少都是 0
        {0, UINT32_MAX, 0},
    };
    for (const auto& c : cases) {
        EXPECT_EQ(ScaleByPermyriad(c.base, c.bonus), c.expected) << "base=" << c.base << " bonus=" << c.bonus;
    }
}

// 与直接公式 base + base × bonus / 10000 逐点对照。基础值限制在 32 位以内,乘积到不了 2^64,直接公式本身
// 是精确的 —— 「拆成整万部分 + 余数部分」不许改变其中任何一个结果
TEST(EquipEnhanceRulesTest, ScaleMatchesDirectFormulaWhereProductFitsIn64Bits) {
    const uint64_t bases[] = {0, 1, 7, 12, 40, 1620, 9999, 10000, 10001, 19999, 123456789, UINT32_MAX};
    const uint32_t bonuses[] = {1, 499, 500, 2600, 8000, 9999, 10000, 10001, 65535, UINT32_MAX};
    for (const uint64_t base : bases) {
        for (const uint32_t bonus : bonuses) {
            const uint64_t direct = base + base * bonus / 10000;
            EXPECT_EQ(ScaleByPermyriad(base, bonus), direct) << "base=" << base << " bonus=" << bonus;
        }
    }
}

// base × bonus 超出 64 位、但结果本身放得下:必须算出精确值,不能因为中间乘积溢出就饱和或回绕
TEST(EquipEnhanceRulesTest, ScaleStaysExactWhenBaseTimesBonusExceeds64Bits) {
    constexpr uint64_t kQuintillion = 1000000000000000000ull;  // 10^18;× 5000 = 5 × 10^21,超过 2^64
    EXPECT_EQ(ScaleByPermyriad(kQuintillion, 5000), 1500000000000000000ull);
    // 带余数:增量 = 5 × 10^17 + floor(9999 × 0.5) = 5 × 10^17 + 4999
    EXPECT_EQ(ScaleByPermyriad(kQuintillion + 9999, 5000), 1500000000000014998ull);
    // 贴着上限但没越过:(2^63 - 1) × 2 = 2^64 - 2,不许提前饱和
    EXPECT_EQ(ScaleByPermyriad(UINT64_MAX / 2, 10000), UINT64_MAX - 1);
}

// 饱和不回绕。算法里有三处可能越界,各配一个「回绕版本会算错」的输入
TEST(EquipEnhanceRulesTest, ScaleSaturatesInsteadOfWrapping) {
    // ① base + 增量 越界(契约锚点):回绕的话 UINT64_MAX 会变成一个 10^15 量级的数
    EXPECT_EQ(ScaleByPermyriad(UINT64_MAX, 1), UINT64_MAX);
    // 刚好越过一格:2^63 × 2 = 2^64
    EXPECT_EQ(ScaleByPermyriad(UINT64_MAX / 2 + 1, 10000), UINT64_MAX);

    // ② 整万部分的乘法越界:2^33 × 2^31 = 2^64,回绕成 0 → 增量为 0 → 结果会等于基础值本身
    constexpr uint64_t kWholePartIsTwoPow33 = (uint64_t{1} << 33) * 10000;
    EXPECT_EQ(ScaleByPermyriad(kWholePartIsTwoPow33, uint32_t{1} << 31), UINT64_MAX);

    // ③ 两项相加越界:整万部分 (2^32 + 1) × (2^32 - 1) = 2^64 - 1 恰好放得下(乘法本身没有饱和),
    //    加上余数部分 floor(9999 × (2^32 - 1) / 10000) 才越界;普通加法会让增量回绕成约 4.29 × 10^9
    constexpr uint64_t kWholePartHitsMaxExactly = ((uint64_t{1} << 32) + 1) * 10000 + 9999;
    EXPECT_EQ(ScaleByPermyriad(kWholePartHitsMaxExactly, UINT32_MAX), UINT64_MAX);

    // 两个参数都取最大
    EXPECT_EQ(ScaleByPermyriad(UINT64_MAX, UINT32_MAX), UINT64_MAX);
}

// 真实设计数据抽查:设计文档二期 §2.1 的 12 级 bonus_permyriad × 一期样例装备的两个基础值
// (1101 武器 伤害 40;1401 鞋 防御 / 速度 12)。等级越高数值不减;基础值 12 在 +1(5%)时不变是
// 向下取整的结果(契约锚点),不是缺陷 —— 数值是占位建议,要改只改表
TEST(EquipEnhanceRulesTest, ScaleOfDesignBaseValuesFollowsEnhanceTable) {
    struct Level {
        uint32_t bonusPermyriad;
        uint64_t expectedFor40;
        uint64_t expectedFor12;
    };
    const Level levels[] = {
        {500, 42, 12},  {1000, 44, 13}, {1500, 46, 13}, {2000, 48, 14}, {2600, 50, 15}, {3200, 52, 15},
        {3800, 55, 16}, {4500, 58, 17}, {5200, 60, 18}, {6000, 64, 19}, {7000, 68, 20}, {8000, 72, 21},
    };
    uint64_t previousFor40 = 40;
    uint64_t previousFor12 = 12;
    for (const auto& level : levels) {
        const uint64_t scaled40 = ScaleByPermyriad(40, level.bonusPermyriad);
        const uint64_t scaled12 = ScaleByPermyriad(12, level.bonusPermyriad);
        EXPECT_EQ(scaled40, level.expectedFor40) << "bonus=" << level.bonusPermyriad;
        EXPECT_EQ(scaled12, level.expectedFor12) << "bonus=" << level.bonusPermyriad;
        EXPECT_GE(scaled40, previousFor40) << "bonus=" << level.bonusPermyriad;
        EXPECT_GE(scaled12, previousFor12) << "bonus=" << level.bonusPermyriad;
        previousFor40 = scaled40;
        previousFor12 = scaled12;
    }
}

// ---------------------------------------------------------------------------
// RollEnhanceSuccess(二期:强化判定与它的随机数消费)
// ---------------------------------------------------------------------------

// rate == 0:必败,而且**不消耗随机数**。脚本为空,任何一次 rand 调用都会让用例失败
TEST(EquipEnhanceRulesTest, ZeroRateFailsWithoutConsumingRand) {
    ScriptedRand dice(std::vector<uint64_t>{});
    EXPECT_FALSE(RollEnhanceSuccess(0, dice));
    EXPECT_TRUE(dice.Requested().empty());
}

// rate != 0:不论成败、不论 rate 多大,都恰好掷一次,要的区间恒是 10000(万分比的分母,不是 rate)
TEST(EquipEnhanceRulesTest, NonZeroRateConsumesExactlyOneRollOfPermyriadBase) {
    for (const uint32_t rate : {1u, 2000u, 9999u, 10000u, 10001u, 20000u, UINT32_MAX}) {
        // 掷出 0:rate >= 1 就成功
        ScriptedRand lowest(std::vector<uint64_t>{0});
        EXPECT_TRUE(RollEnhanceSuccess(rate, lowest)) << "rate=" << rate;
        EXPECT_EQ(lowest.Requested(), (std::vector<uint64_t>{10000})) << "rate=" << rate;
        EXPECT_TRUE(lowest.Exhausted()) << "rate=" << rate;

        // 掷出 9999(能掷出的最大值):只有 rate >= 10000 才成功;失败的那一次同样只掷一次
        ScriptedRand highest(std::vector<uint64_t>{9999});
        EXPECT_EQ(RollEnhanceSuccess(rate, highest), rate >= 10000) << "rate=" << rate;
        EXPECT_EQ(highest.Requested(), (std::vector<uint64_t>{10000})) << "rate=" << rate;
        EXPECT_TRUE(highest.Exhausted()) << "rate=" << rate;
    }
}

// 成败的边界:掷出的数必须**严格小于** rate 才成功 —— 掷出 rate - 1 成功,掷出 rate 失败
TEST(EquipEnhanceRulesTest, RollBelowRateSucceedsAndRollEqualToRateFails) {
    const uint32_t rates[] = {1, 2000, 2500, 5000, 9000, 9999};
    for (const uint32_t rate : rates) {
        ScriptedRand justBelow(std::vector<uint64_t>{uint64_t{rate} - 1});
        EXPECT_TRUE(RollEnhanceSuccess(rate, justBelow)) << "rate=" << rate;
        EXPECT_EQ(justBelow.Requested(), (std::vector<uint64_t>{10000})) << "rate=" << rate;
        EXPECT_TRUE(justBelow.Exhausted()) << "rate=" << rate;

        ScriptedRand exactlyRate(std::vector<uint64_t>{uint64_t{rate}});
        EXPECT_FALSE(RollEnhanceSuccess(rate, exactlyRate)) << "rate=" << rate;
        EXPECT_EQ(exactlyRate.Requested(), (std::vector<uint64_t>{10000})) << "rate=" << rate;
        EXPECT_TRUE(exactlyRate.Exhausted()) << "rate=" << rate;
    }
}

// rate >= 10000 的约定:必成,但**照样消耗**这一掷(要的区间仍是 10000,不是 rate)—— 与 RollAffixes 的
// 粉 / 黄同一口径。表里填超了(20000 / UINT32_MAX)与填 10000 完全等价
TEST(EquipEnhanceRulesTest, RateAtOrAboveBaseAlwaysSucceedsButStillConsumesTheRoll) {
    const uint64_t rolls[] = {0, 5000, 9999};
    for (const uint32_t rate : {10000u, 10001u, 20000u, UINT32_MAX}) {
        for (const uint64_t roll : rolls) {
            ScriptedRand dice(std::vector<uint64_t>{roll});
            EXPECT_TRUE(RollEnhanceSuccess(rate, dice)) << "rate=" << rate << " roll=" << roll;
            EXPECT_EQ(dice.Requested(), (std::vector<uint64_t>{10000})) << "rate=" << rate << " roll=" << roll;
            EXPECT_TRUE(dice.Exhausted()) << "rate=" << rate << " roll=" << roll;
        }
    }
}

// 把 0..9999 每个可能掷出的数都过一遍:成功的个数恰好等于 rate(超过 10000 按 10000)—— 成功率就是
// rate / 10000,没有差一,也没有把万分比当成千分比。rate 取设计文档二期 §2.1 里出现过的全部成功率
TEST(EquipEnhanceRulesTest, SuccessCountOverAllPossibleRollsEqualsRate) {
    const uint32_t rates[] = {1, 2000, 2500, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000, 20000};
    for (const uint32_t rate : rates) {
        uint32_t successes = 0;
        for (uint64_t roll = 0; roll < 10000; ++roll) {
            auto fixed = [roll](uint64_t) { return roll; };
            if (RollEnhanceSuccess(rate, fixed)) {
                ++successes;
            }
        }
        EXPECT_EQ(successes, rate < 10000 ? rate : 10000u) << "rate=" << rate;
    }
}

// 随机源不守约(永远返回超大值):被夹成 9999,所以只有必成的 rate 才成功 —— 坏随机源不会白送一次成功,
// 也仍然只被调一次
TEST(EquipEnhanceRulesTest, OutOfContractRandIsClampedIntoAMiss) {
    std::size_t calls = 0;
    auto broken = [&calls](uint64_t) {
        ++calls;
        return UINT64_MAX;
    };
    EXPECT_FALSE(RollEnhanceSuccess(9999, broken));
    EXPECT_EQ(calls, 1u);
    EXPECT_TRUE(RollEnhanceSuccess(10000, broken));
    EXPECT_EQ(calls, 2u);
}

// 随机源的三种传法都要能用:左值函数对象(上面的 ScriptedRand)、普通函数(生产侧的 RandBelow 就是这么传的)、
// 临时 lambda。后两种这里各走一遍 —— 签名改坏的话这条用例先编不过
TEST(EquipEnhanceRulesTest, AcceptsPlainFunctionAndTemporaryLambdaAsRand) {
    EXPECT_FALSE(RollEnhanceSuccess(9999, LargestRoll));  // 掷出 9999,不小于 9999
    EXPECT_TRUE(RollEnhanceSuccess(10000, LargestRoll));
    EXPECT_TRUE(RollEnhanceSuccess(1, [](uint64_t) { return uint64_t{0}; }));
    EXPECT_FALSE(RollEnhanceSuccess(1, [](uint64_t) { return uint64_t{1}; }));
}

// ---------------------------------------------------------------------------
// LevelAfterFailure(二期:强化失败掉级)
// ---------------------------------------------------------------------------

// loss == 0 不掉级(设计默认:fail_level_loss 全填 0);最多掉到 0,不回绕成一个巨大的等级
TEST(EquipEnhanceRulesTest, LevelAfterFailureDropsButNeverBelowZero) {
    EXPECT_EQ(LevelAfterFailure(5, 0), 5u);
    EXPECT_EQ(LevelAfterFailure(5, 1), 4u);
    EXPECT_EQ(LevelAfterFailure(5, 4), 1u);
    EXPECT_EQ(LevelAfterFailure(5, 5), 0u);  // 恰好掉光
    EXPECT_EQ(LevelAfterFailure(5, 6), 0u);  // 要掉的比现有的多:停在 0
    EXPECT_EQ(LevelAfterFailure(0, 0), 0u);
    EXPECT_EQ(LevelAfterFailure(0, 3), 0u);
    EXPECT_EQ(LevelAfterFailure(12, UINT32_MAX), 0u);
    EXPECT_EQ(LevelAfterFailure(UINT32_MAX, 1), UINT32_MAX - 1);
}

// ---------------------------------------------------------------------------
// CheckGemFit(二期:宝石能不能嵌进某个孔)
// ---------------------------------------------------------------------------

// 类型相符的孔能嵌;「任意」孔什么类型都能嵌
TEST(EquipGemFitTest, MatchingTypedSocketAndAnySocketFit) {
    const std::vector<uint32_t> noneTaken;
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), noneTaken, 0, kGemAttack)), FitCode(GemFit::kOk));

    const uint32_t gemTypes[] = {kGemAttack, kGemDefense, kGemHealth, kGemSwift, UINT32_MAX};
    for (const uint32_t gemType : gemTypes) {
        EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), noneTaken, 1, gemType)), FitCode(GemFit::kOk))
            << "gemType=" << gemType;
    }
}

// 这件装备没有这个孔:孔位号不在该部位的定义里(含部位根本没有孔)
TEST(EquipGemFitTest, UndefinedSocketIsInvalid) {
    const std::vector<uint32_t> noneTaken;
    const uint32_t missing[] = {2, 7, equiprules::kMaxGemSockets, UINT32_MAX};
    for (const uint32_t socket : missing) {
        EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), noneTaken, socket, kGemAttack)),
                  FitCode(GemFit::kSocketInvalid))
            << "socket=" << socket;
    }
    EXPECT_EQ(FitCode(CheckGemFit({}, noneTaken, 0, kGemAttack)), FitCode(GemFit::kSocketInvalid));

    // 规则头不要求孔位号连续(那是表校验的事):只定义了 3 号孔的部位,3 号存在、0 号不存在
    const std::vector<GemSocketRule> onlyThird{{3, kGemSwift}};
    EXPECT_EQ(FitCode(CheckGemFit(onlyThird, noneTaken, 3, kGemSwift)), FitCode(GemFit::kOk));
    EXPECT_EQ(FitCode(CheckGemFit(onlyThird, noneTaken, 0, kGemSwift)), FitCode(GemFit::kSocketInvalid));
}

// 孔里已有宝石:拒绝。occupied 是实例上的原始记录 —— 重复、乱序、表里已经不存在的孔都可能出现
TEST(EquipGemFitTest, OccupiedSocketIsRejectedFromRawRecords) {
    // 只占了 0 号:0 号被拒,1 号照常
    const std::vector<uint32_t> zeroTaken{0};
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), zeroTaken, 0, kGemAttack)), FitCode(GemFit::kSocketOccupied));
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), zeroTaken, 1, kGemAttack)), FitCode(GemFit::kOk));

    // 脏数据:同一个孔记了两条、顺序是乱的 —— 照样算占用
    const std::vector<uint32_t> duplicated{1, 1, 0};
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), duplicated, 0, kGemAttack)), FitCode(GemFit::kSocketOccupied));
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), duplicated, 1, kGemAttack)), FitCode(GemFit::kSocketOccupied));

    // 记录里有表里已经删掉的孔(5 号):不影响别的孔;5 号本身仍是「没有这个孔」
    const std::vector<uint32_t> staleRecord{5};
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), staleRecord, 0, kGemAttack)), FitCode(GemFit::kOk));
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), staleRecord, 1, kGemDefense)), FitCode(GemFit::kOk));
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), staleRecord, 5, kGemAttack)), FitCode(GemFit::kSocketInvalid));
}

// 限定了类型的孔只收同类型的宝石
TEST(EquipGemFitTest, TypedSocketRejectsOtherGemTypes) {
    const std::vector<uint32_t> noneTaken;
    const uint32_t others[] = {kGemDefense, kGemHealth, kGemSwift, UINT32_MAX};
    for (const uint32_t gemType : others) {
        EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), noneTaken, 0, gemType)), FitCode(GemFit::kTypeMismatch))
            << "gemType=" << gemType;
    }
}

// gemType == 0 不是合法的宝石类型(0 只在孔位定义里表示「任意」):哪怕目标是「任意」孔也拒绝 ——
// 否则一行坏的 EquipGem(gem_type 填了 0)能嵌进所有通用孔
TEST(EquipGemFitTest, ZeroGemTypeNeverFits) {
    const std::vector<uint32_t> noneTaken;
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), noneTaken, 1, 0)), FitCode(GemFit::kTypeMismatch));
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), noneTaken, 0, 0)), FitCode(GemFit::kTypeMismatch));
}

// 判定顺序写死:没有这个孔 → 已占用 → 类型不符。同时不满足多条时回靠前的那一个
TEST(EquipGemFitTest, VerdictOrderIsInvalidThenOccupiedThenTypeMismatch) {
    const std::vector<uint32_t> noneTaken;
    const std::vector<uint32_t> taken{0, 5};

    // 孔不存在 + 记录里有它 + 类型为 0:三条都不满足,回「没有这个孔」
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), taken, 5, 0)), FitCode(GemFit::kSocketInvalid));
    // 孔不存在 + 类型为 0
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), noneTaken, 5, 0)), FitCode(GemFit::kSocketInvalid));
    // 孔存在 + 已占用 + 类型不符:回「已占用」
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), taken, 0, kGemDefense)), FitCode(GemFit::kSocketOccupied));
    // 孔存在 + 已占用 + 类型为 0:仍回「已占用」
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), taken, 0, 0)), FitCode(GemFit::kSocketOccupied));
    // 只剩类型不符
    EXPECT_EQ(FitCode(CheckGemFit(WeaponSockets(), noneTaken, 0, kGemDefense)), FitCode(GemFit::kTypeMismatch));
}

// 坏表:同一个孔位号有两条定义时只认第一条 —— 不会因为后面那条更宽松就放行,也不会因为后面那条更严就拒绝
TEST(EquipGemFitTest, DuplicateSocketDefinitionsUseTheFirstOne) {
    const std::vector<uint32_t> noneTaken;

    const std::vector<GemSocketRule> typedThenAny{{0, kGemDefense}, {0, kAnyGem}};
    EXPECT_EQ(FitCode(CheckGemFit(typedThenAny, noneTaken, 0, kGemDefense)), FitCode(GemFit::kOk));
    EXPECT_EQ(FitCode(CheckGemFit(typedThenAny, noneTaken, 0, kGemAttack)), FitCode(GemFit::kTypeMismatch));

    const std::vector<GemSocketRule> anyThenTyped{{0, kAnyGem}, {0, kGemDefense}};
    EXPECT_EQ(FitCode(CheckGemFit(anyThenTyped, noneTaken, 0, kGemAttack)), FitCode(GemFit::kOk));
}

// 真实设计数据抽查:设计文档二期 §2.3 的 6 个孔位 × §2.2 的 4 种宝石类型,逐格写明收还是不收
TEST(EquipGemFitTest, DesignSocketTableAcceptsExpectedGemTypes) {
    const std::vector<GemSocketRule> weapon = WeaponSockets();                      // 部位 11
    const std::vector<GemSocketRule> hat{{0, kGemHealth}};                          // 部位 12
    const std::vector<GemSocketRule> clothes{{0, kGemDefense}, {1, kGemHealth}};    // 部位 13
    const std::vector<GemSocketRule> shoes{{0, kGemSwift}};                         // 部位 14
    struct Row {
        std::string label;
        std::vector<GemSocketRule> sockets;
        uint32_t socket;
        bool accepts[4];  // 依次对应 攻击 / 防御 / 气血 / 迅捷
    };
    const Row rows[] = {
        {"weapon socket 0 (attack)", weapon, 0, {true, false, false, false}},
        {"weapon socket 1 (any)", weapon, 1, {true, true, true, true}},
        {"hat socket 0 (health)", hat, 0, {false, false, true, false}},
        {"clothes socket 0 (defense)", clothes, 0, {false, true, false, false}},
        {"clothes socket 1 (health)", clothes, 1, {false, false, true, false}},
        {"shoes socket 0 (swift)", shoes, 0, {false, false, false, true}},
    };
    const uint32_t gemTypes[4] = {kGemAttack, kGemDefense, kGemHealth, kGemSwift};
    const std::vector<uint32_t> noneTaken;
    for (const auto& row : rows) {
        // 孔位号必须小于 kMaxGemSockets(表校验的口径;消费侧也只看实例上前这么多条记录)
        ASSERT_LT(row.socket, equiprules::kMaxGemSockets) << row.label;
        for (std::size_t i = 0; i < 4; ++i) {
            EXPECT_EQ(FitCode(CheckGemFit(row.sockets, noneTaken, row.socket, gemTypes[i])),
                      FitCode(row.accepts[i] ? GemFit::kOk : GemFit::kTypeMismatch))
                << row.label << " gemType=" << gemTypes[i];
        }
    }
}

// ---------------------------------------------------------------------------
// RollEnhanceWithPity(唯一满配突破级 +11：概率提前成 + 满次数必成不掉级)
// ---------------------------------------------------------------------------

TEST(EquipEnhanceRulesTest, RollEnhanceWithPityEarlySuccessResetsPityAndFailIncrementsPity) {
    // 10% 概率提前成功：命中时保底清零且不标记为保底触发。
    ScriptedRand hit(std::vector<uint64_t>{999});
    const auto early = equiprules::RollEnhanceWithPity(1000, 7, 20, hit);
    EXPECT_TRUE(early.success);
    EXPECT_FALSE(early.triggeredByPity);
    EXPECT_EQ(early.nextPityCount, 0u);
    EXPECT_TRUE(hit.Exhausted());

    // 未命中时失败且保底次数 +1。
    ScriptedRand miss(std::vector<uint64_t>{1000});
    const auto failed = equiprules::RollEnhanceWithPity(1000, 7, 20, miss);
    EXPECT_FALSE(failed.success);
    EXPECT_FALSE(failed.triggeredByPity);
    EXPECT_EQ(failed.nextPityCount, 8u);
    EXPECT_TRUE(miss.Exhausted());

    // 第 19 次已失败（下一次即第 20 次）：保底必成且不消耗随机数、保底清零。
    ScriptedRand empty(std::vector<uint64_t>{});
    const auto pitied = equiprules::RollEnhanceWithPity(1000, 19, 20, empty);
    EXPECT_TRUE(pitied.success);
    EXPECT_TRUE(pitied.triggeredByPity);
    EXPECT_EQ(pitied.nextPityCount, 0u);
    EXPECT_TRUE(empty.Requested().empty());

    // pityLimit == 0 时退回普通强化判定，失败后保底计数恒为 0。
    ScriptedRand noPityMiss(std::vector<uint64_t>{9999});
    const auto plainFail = equiprules::RollEnhanceWithPity(1000, 0, 0, noPityMiss);
    EXPECT_FALSE(plainFail.success);
    EXPECT_FALSE(plainFail.triggeredByPity);
    EXPECT_EQ(plainFail.nextPityCount, 0u);
}

// ---------------------------------------------------------------------------
// 编译期守卫:CombatStat 的数值 == proto CombatAttributes 的字段号(设计文档 §3.3「三者不得错位」)。
// proto 改号、或在枚举中间插项,这个文件直接编不过 —— 而不是「抗冰冻」在线上悄悄变成「抗昏睡」。
// 需要 proto-gen 之后的 actor_attribute_state_comp.pb.h(CombatAttributes 是本期新增的消息)。
// ---------------------------------------------------------------------------

namespace {

constexpr bool SameNumber(CombatStat stat, int protoFieldNumber) {
    return static_cast<int>(ToRaw(stat)) == protoFieldNumber;
}

static_assert(SameNumber(CombatStat::kPhysicalCritRate, CombatAttributes::kPhysicalCritRateFieldNumber));
static_assert(SameNumber(CombatStat::kMagicCritRate, CombatAttributes::kMagicCritRateFieldNumber));
static_assert(SameNumber(CombatStat::kComboRate, CombatAttributes::kComboRateFieldNumber));
static_assert(SameNumber(CombatStat::kCounterRate, CombatAttributes::kCounterRateFieldNumber));
static_assert(SameNumber(CombatStat::kReflectRate, CombatAttributes::kReflectRateFieldNumber));
static_assert(SameNumber(CombatStat::kSkillLevelBonus, CombatAttributes::kSkillLevelBonusFieldNumber));
static_assert(SameNumber(CombatStat::kIgnoreAilmentResist, CombatAttributes::kIgnoreAilmentResistFieldNumber));
static_assert(SameNumber(CombatStat::kResistPoison, CombatAttributes::kResistPoisonFieldNumber));
static_assert(SameNumber(CombatStat::kResistFreeze, CombatAttributes::kResistFreezeFieldNumber));
static_assert(SameNumber(CombatStat::kResistSleep, CombatAttributes::kResistSleepFieldNumber));
static_assert(SameNumber(CombatStat::kResistForget, CombatAttributes::kResistForgetFieldNumber));
static_assert(SameNumber(CombatStat::kResistConfusion, CombatAttributes::kResistConfusionFieldNumber));
static_assert(SameNumber(CombatStat::kResistAllAilment, CombatAttributes::kResistAllAilmentFieldNumber));
static_assert(SameNumber(CombatStat::kMagicResist, CombatAttributes::kMagicResistFieldNumber));
static_assert(SameNumber(CombatStat::kPhysicalResist, CombatAttributes::kPhysicalResistFieldNumber));

// 二期:LevelAfterFailure 在契约里是 constexpr(调用方可以拿去算编译期常量),去掉的话这里先编不过。
// ScaleByPermyriad 同样是 constexpr,它的五个锚点已经是规则头里的 static_assert,这里不重复。
static_assert(LevelAfterFailure(5, 2) == 3);
static_assert(LevelAfterFailure(2, 5) == 0);

}  // namespace
