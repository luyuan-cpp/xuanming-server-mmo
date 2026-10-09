#pragma once

// 装备属性纯规则(零 ECS / 零表 / 零 proto 依赖,可单测),设计文档
// docs/design/equipment-attributes.md §4.2。
//
// PlayerEquipSystem 负责把表行 / 实例翻译成这里的输入,这里只回答四个问题:
//   1) 一条属性(EquipAttribute 表的 effect / effect_param + 数值)怎么累加进一份装备加成;
//   2) 某个一级属性维度一共从装备拿到多少点(单项 + 所有属性);
//   3) 一件新装备掷出哪几条随机属性、各是多少(蓝 / 粉 / 黄);
//   4) 一件装备的等级落在上限表的哪一档。
//
// 随机源与取值区间都由调用方显式传入(AGENTS §11.2 显式依赖):生产传 tlsRandom 与表查询,
// 单测传脚本化序列。炼化(重掷)以后直接复用 RollAffixes,不需要再认识一遍表。
//
// 只依赖 std,且**不得**出现 "player/system/..." 这类 scene 库相对包含:本头被 battle 测试工程
// 直接包含(它的 include 目录里没有 libs/services/scene/)。
//
// 单测:cpp/tests/turn_battle_engine_test/equip_attribute_rules_test.cpp(纯头文件,不需要链接 scene.lib)。

#include <array>
#include <cstddef>
#include <cstdint>
#include <map>
#include <vector>

namespace equiprules {

// ---------------------------------------------------------------------------
// 枚举:数值都是「表 / 协议 / 存档里填的那个数」,一经使用不得改号;末项 kCount 兼作数组长度。
// ---------------------------------------------------------------------------

// 颜色档。数值 == EquipAffix.tier(存档)== EquipAttrLineInfo.tier(下发客户端)。
enum class AffixTier : uint8_t {
    kBase = 0,    // 基础属性:只用于显示行,不会出现在存档的随机属性里
    kBlue = 1,
    kPink = 2,
    kYellow = 3,
    kGreen = 4,   // 套装 / 绿色属性:预留,本期不掷、不显示
    kCount
};

// 效果种类。数值 == EquipAttribute.effect。C++ 只按它分支,不按属性 id 分支。
enum class EffectKind : uint8_t {
    kNone = 0,
    kPrimaryPoint = 1,      // 一级属性点;effect_param = AttributeDimension.id
    kAllPrimaryPoints = 2,  // 所有一级属性点(角色池各维度各 +N);effect_param 不用
    kDerivedFlat = 3,       // 二级属性平加;effect_param = DerivedStat
    kBothAttacks = 4,       // 伤害:物伤与法伤各 +N;effect_param 不用
    kCombat = 5,            // 战斗类属性;effect_param = CombatStat
    kCount
};

// 二级属性。数值 == EquipAttribute.effect_param(effect = 3 时)。
//
// **注意它不等于 proto DerivedAttributesComp 的字段号**:那边是 defense = 5、speed = 6,
// 这里是 kSpeed = 5、kDefense = 6(跟的是配表)。消费方必须按名字逐项取
// (DerivedFlatOf(bonus, DerivedStat::kSpeed) → 加到 speed),绝不能按号对拷。
enum class DerivedStat : uint8_t {
    kNone = 0,
    kMaxHealth = 1,
    kMaxMana = 2,
    kPhysicalAttack = 3,
    kMagicAttack = 4,
    kSpeed = 5,
    kDefense = 6,
    kCount
};

// 战斗类属性。**数值 == proto CombatAttributes 的字段号 == EquipAttribute.effect_param(effect = 5 时)**,
// 三者不得错位:表里填错号 / proto 改号 / 这里插一项,都会让「抗冰冻」悄悄变成「抗昏睡」且零报错。
// 与 proto 的对齐由单测里的 static_assert 机械守住;与表的对齐由启动表校验(ToCombatStat 非 kNone)守住。
// 百分比类一律是整数百分点(10 = 10%),是否百分比看 EquipAttribute.percent,这里不区分。
enum class CombatStat : uint8_t {
    kNone = 0,
    kPhysicalCritRate = 1,
    kMagicCritRate = 2,
    kComboRate = 3,
    kCounterRate = 4,
    kReflectRate = 5,
    kSkillLevelBonus = 6,
    kIgnoreAilmentResist = 7,
    kResistPoison = 8,
    kResistFreeze = 9,
    kResistSleep = 10,
    kResistForget = 11,
    kResistConfusion = 12,
    kResistAllAilment = 13,
    kMagicResist = 14,
    kPhysicalResist = 15,
    kCount
};

// 在枚举中间插项会让 kCount 变化、后面的号全部后移 —— 存档与表里的数就指向了别的属性。
static_assert(static_cast<uint32_t>(AffixTier::kCount) == 5, "AffixTier values are a persistence contract");
static_assert(static_cast<uint32_t>(EffectKind::kCount) == 6, "EffectKind values mirror EquipAttribute.effect");
static_assert(static_cast<uint32_t>(DerivedStat::kCount) == 7, "DerivedStat values mirror EquipAttribute.effect_param");
static_assert(static_cast<uint32_t>(CombatStat::kCount) == 16,
              "CombatStat values mirror CombatAttributes field numbers and EquipAttribute.effect_param");

// ---------------------------------------------------------------------------
// 原始数 <-> 枚举。表 / 协议 / 存档里的字段都是 uint32,统一走这里转,调用方不要到处 static_cast:
// 越界值直接 static_cast 成枚举再拿去当数组下标,就是一次越界写。
// ---------------------------------------------------------------------------

constexpr uint32_t ToRaw(AffixTier tier) { return static_cast<uint32_t>(tier); }
constexpr uint32_t ToRaw(EffectKind kind) { return static_cast<uint32_t>(kind); }
constexpr uint32_t ToRaw(DerivedStat stat) { return static_cast<uint32_t>(stat); }
constexpr uint32_t ToRaw(CombatStat stat) { return static_cast<uint32_t>(stat); }

// 越界(含 0 与 >= kCount)一律返回 kNone。
constexpr EffectKind ToEffectKind(uint32_t raw) {
    return raw > 0 && raw < ToRaw(EffectKind::kCount) ? static_cast<EffectKind>(raw) : EffectKind::kNone;
}
constexpr DerivedStat ToDerivedStat(uint32_t raw) {
    return raw > 0 && raw < ToRaw(DerivedStat::kCount) ? static_cast<DerivedStat>(raw) : DerivedStat::kNone;
}
constexpr CombatStat ToCombatStat(uint32_t raw) {
    return raw > 0 && raw < ToRaw(CombatStat::kCount) ? static_cast<CombatStat>(raw) : CombatStat::kNone;
}

// AffixTier 没有 kNone(0 是合法的「基础属性」),所以用返回值表达成败:
// raw 在 0..4 内返回 true 并写 out;越界返回 false 且不改 out。
constexpr bool TryToAffixTier(uint32_t raw, AffixTier& out) {
    if (raw >= ToRaw(AffixTier::kCount)) {
        return false;
    }
    out = static_cast<AffixTier>(raw);
    return true;
}

// 存档里一条随机属性(EquipAffix.tier)的颜色档是否合法:1 蓝 / 2 粉 / 3 黄 / 4 绿。
// 绿是预留档:本期不产出,但读到了按合法处理(以后上套装时存量数据不用洗);0(基础属性)与越界不合法。
constexpr bool IsRandomAffixTier(uint32_t rawTier) {
    return rawTier >= ToRaw(AffixTier::kBlue) && rawTier < ToRaw(AffixTier::kCount);
}

// EquipBonus 里两个定长数组的下标。
constexpr std::size_t Index(DerivedStat stat) { return static_cast<std::size_t>(stat); }
constexpr std::size_t Index(CombatStat stat) { return static_cast<std::size_t>(stat); }

// ---------------------------------------------------------------------------
// 装备加成的累加
// ---------------------------------------------------------------------------

// 一个角色身上全部装备的加成汇总(基础属性 + 随机属性)。运行时现算,不落库(设计文档 §5 不变量 3)。
// 两个数组的第 0 格(kNone)永远是 0:ApplyEffect 不会写它。
struct EquipBonus {
    std::map<uint32_t, uint64_t> primaryPoints;   // dimension id -> 点数
    uint64_t allPrimaryPoints{0};
    std::array<uint64_t, static_cast<std::size_t>(DerivedStat::kCount)> derivedFlat{};
    std::array<uint64_t, static_cast<std::size_t>(CombatStat::kCount)> combat{};
};

namespace detail {

// 加法饱和到 uint64 上限。装备数值远到不了这里,但回绕会把一个巨大的加成变成接近 0 的数且零报错。
constexpr uint64_t SaturatingAdd(uint64_t a, uint64_t b) {
    return b > UINT64_MAX - a ? UINT64_MAX : a + b;
}

}  // namespace detail

// 把一条属性累加进 bonus。effectKind / effectParam 是 EquipAttribute 表的原始列值,value 是这条属性的数值。
//   kind 1 一级属性点:   primaryPoints[param] += value;param == 0 非法(维度是否真实存在由调用方的表校验负责)
//   kind 2 所有一级属性: allPrimaryPoints += value;param 忽略
//   kind 3 二级属性平加: derivedFlat[param] += value;param 必须是 1..6 的 DerivedStat
//   kind 4 伤害:        物伤与法伤各 += value;param 忽略
//   kind 5 战斗类属性:   combat[param] += value;param 必须是 1..15 的 CombatStat
// 未知 kind / 越界 param 返回 false 且**不改 bonus**(调用方记 ERROR 并跳过这一条)。
// 加法饱和,不回绕。value == 0 合法但无效果(kind 1 不会为它建一个值为 0 的键)。
inline bool ApplyEffect(EquipBonus& bonus, uint32_t effectKind, uint32_t effectParam, uint64_t value) {
    switch (ToEffectKind(effectKind)) {
    case EffectKind::kPrimaryPoint: {
        if (effectParam == 0) {
            return false;
        }
        if (value == 0) {
            return true;
        }
        auto& points = bonus.primaryPoints[effectParam];
        points = detail::SaturatingAdd(points, value);
        return true;
    }
    case EffectKind::kAllPrimaryPoints:
        bonus.allPrimaryPoints = detail::SaturatingAdd(bonus.allPrimaryPoints, value);
        return true;
    case EffectKind::kDerivedFlat: {
        const DerivedStat stat = ToDerivedStat(effectParam);
        if (stat == DerivedStat::kNone) {
            return false;
        }
        auto& flat = bonus.derivedFlat[Index(stat)];
        flat = detail::SaturatingAdd(flat, value);
        return true;
    }
    case EffectKind::kBothAttacks: {
        auto& physical = bonus.derivedFlat[Index(DerivedStat::kPhysicalAttack)];
        auto& magic = bonus.derivedFlat[Index(DerivedStat::kMagicAttack)];
        physical = detail::SaturatingAdd(physical, value);
        magic = detail::SaturatingAdd(magic, value);
        return true;
    }
    case EffectKind::kCombat: {
        const CombatStat stat = ToCombatStat(effectParam);
        if (stat == CombatStat::kNone) {
            return false;
        }
        auto& slot = bonus.combat[Index(stat)];
        slot = detail::SaturatingAdd(slot, value);
        return true;
    }
    case EffectKind::kNone:
    case EffectKind::kCount:
        break;
    }
    return false;
}

// 某个一级属性维度从装备拿到的总点数 = 单项(没有则 0)+ 所有属性;饱和。
// 「所有属性」只该加到角色池的维度上 —— 这里不认识池,由调用方只对角色池维度调用。
inline uint64_t PrimaryPointsFor(const EquipBonus& bonus, uint32_t dimensionId) {
    const auto it = bonus.primaryPoints.find(dimensionId);
    const uint64_t single = it != bonus.primaryPoints.end() ? it->second : 0;
    return detail::SaturatingAdd(single, bonus.allPrimaryPoints);
}

// 按名字读两个数组;kNone / kCount 等越界项返回 0。
inline uint64_t DerivedFlatOf(const EquipBonus& bonus, DerivedStat stat) {
    return Index(stat) < bonus.derivedFlat.size() ? bonus.derivedFlat[Index(stat)] : 0;
}
inline uint64_t CombatOf(const EquipBonus& bonus, CombatStat stat) {
    return Index(stat) < bonus.combat.size() ? bonus.combat[Index(stat)] : 0;
}

// ---------------------------------------------------------------------------
// 掷随机属性
// ---------------------------------------------------------------------------

// 概率的分母:万分比。
inline constexpr uint32_t kPermyriadBase = 10000;

struct PoolEntry { uint32_t attrId{0}; uint32_t weight{0}; };
struct ValueRange { uint32_t minValue{0}; uint32_t cap{0}; };   // cap == 0 = 本档不可用
struct RollRule { uint32_t blueMin{0}; uint32_t blueMax{0}; uint32_t pinkRatePermyriad{0}; uint32_t yellowRatePermyriad{0}; };
struct RolledAffix { uint32_t attrId{0}; AffixTier tier{AffixTier::kBlue}; uint32_t value{0}; uint32_t seq{0}; };

namespace detail {

// 过滤后的一个候选属性。
struct Candidate {
    uint32_t attrId{0};
    uint64_t weight{0};  // 同一属性在池里出现多次时权重合并,所以比表列宽
    ValueRange range;
};

// 调一次 rand(n) 并把结果夹进 [0, n)。rand 守约时这是恒等;不守约(返回 >= n,或有符号类型的负数)
// 时不让它把下标带出界。n 由调用点保证 >= 1。
template <class RandFn>
uint64_t RollBelow(RandFn& rand, uint64_t n) {
    const auto rolled = static_cast<uint64_t>(rand(n));
    return rolled < n ? rolled : n - 1;
}

// 「按权重抽」:order 是 candidates 的下标(保持池内顺序),依次累加权重,第一个使
// 「累加和 > ticket」的中签;返回它在 order 里的位置。order 非空由调用点保证。
inline std::size_t PickByTicket(const std::vector<Candidate>& candidates, const std::vector<std::size_t>& order,
                                uint64_t ticket) {
    uint64_t accumulated = 0;
    for (std::size_t position = 0; position < order.size(); ++position) {
        accumulated += candidates[order[position]].weight;
        if (ticket < accumulated) {
            return position;
        }
    }
    return order.size() - 1;  // ticket 已被 RollBelow 夹在权重和以内,走不到这里;留着只为不越界
}

// 「掷值」:value = low + rand(cap - low + 1),low = minValue 夹到 [1, cap]。cap >= 1 由候选过滤保证。
template <class RandFn>
uint32_t RollValue(RandFn& rand, const ValueRange& range) {
    uint32_t low = range.minValue == 0 ? 1 : range.minValue;
    if (low > range.cap) {
        low = range.cap;
    }
    const uint64_t span = static_cast<uint64_t>(range.cap - low) + 1;
    return low + static_cast<uint32_t>(RollBelow(rand, span));
}

// 粉 / 黄共用:按概率至多掷出 1 条,按权重放回抽取。
template <class RandFn>
void RollChanceAffix(const std::vector<Candidate>& candidates, const std::vector<std::size_t>& order,
                     uint64_t totalWeight, uint32_t ratePermyriad, AffixTier tier, RandFn& rand,
                     std::vector<RolledAffix>& rolled) {
    if (ratePermyriad == 0) {
        return;
    }
    if (RollBelow(rand, kPermyriadBase) >= ratePermyriad) {
        return;
    }
    const uint64_t ticket = RollBelow(rand, totalWeight);
    const Candidate& picked = candidates[order[PickByTicket(candidates, order, ticket)]];
    RolledAffix affix;
    affix.attrId = picked.attrId;
    affix.tier = tier;
    affix.value = RollValue(rand, picked.range);
    affix.seq = 0;
    rolled.push_back(affix);
}

}  // namespace detail

// 掷一件新装备的随机属性。
//
// rand(n):   返回 [0, n) 的均匀整数,n >= 1。**形参必须能无损接收 uint64_t** —— 池的权重和按 64 位
//            累加,可能超过 32 位。生产写法:[](uint64_t n) { return tlsRandom.Rand<uint64_t>(0, n - 1); }。
//            返回值越出 [0, n)(随机源不守约)会被夹回区间内,不会越界。
// rangeOf(attrId):返回该属性在本装备等级档的取值区间(ValueRange);cap == 0 表示本档不可用。
//            对每个「权重 > 0 的不同 attrId」按池内首次出现的顺序恰好调用一次。
//
// 步骤固定 —— 它决定随机数的消费顺序,单测逐次钉住;改顺序 = 改掉所有按种子复现的结果:
//   0. 候选:按池内顺序留下 weight > 0 且 rangeOf(attr).cap > 0 的属性;同一 attrId 在池里出现多次时
//      权重合并成一条(「蓝属性互不重复」因此对任何输入都成立)。候选为空 → 返回空,**不消耗随机数**。
//   1. 蓝:
//      1.1 条数 n = blueMin + rand(blueMax - blueMin + 1);blueMax <= blueMin 时 n = blueMin(仍掷一次
//          rand(1))。再把 n 夹到候选数:池里可用属性不够时能掷几条掷几条。
//      1.2 重复 n 次,每条依次两掷:rand(剩余候选权重和) 按权重**不放回**抽属性 → 掷值。
//          tier = kBlue,seq 从 0 递增。
//   2. 粉:pinkRatePermyriad == 0 → 跳过,不消耗随机数。否则掷 rand(10000),小于 rate 即掷出
//      (rate >= 10000 必出,**仍消耗这一次** —— 规则只有「走到的掷骰步骤恰好消耗一次」一条,不为必出
//      另开分支);掷出后依次两掷:rand(全部候选权重和) 按权重**放回**抽属性(可与蓝重复)→ 掷值。
//      tier = kPink,seq = 0。
//   3. 黄:同粉,用 yellowRatePermyriad,可与蓝 / 粉重复。tier = kYellow,seq = 0。
//   「按权重抽」:候选按池内顺序依次累加权重,第一个使「累加和 > 掷出的数」的中签。
//   「掷值」:    value = low + rand(cap - low + 1),low = minValue 夹到 [1, cap](minValue 为 0 按 1,
//               大于 cap 按 cap),所以恒有 1 <= value <= cap;区间退化成单点时仍掷一次 rand(1)。
//
// 返回顺序:蓝(seq 升序)→ 粉 → 黄。
// 随机数消耗次数 = 1 + 2 × 蓝条数 + 粉(0 / 1 / 3)+ 黄(0 / 1 / 3);候选为空时为 0。
// 任何输入(空池、全 0 权重、rate 超 10000、blueMax 巨大)都不抛异常、不死循环、不越界。
// 开销 O(池大小 × log 池大小 + 蓝条数 × 候选数),池是几十行的配表。
template <class RandFn, class RangeFn>
std::vector<RolledAffix> RollAffixes(const std::vector<PoolEntry>& pool, const RollRule& rule,
                                     RandFn&& rand, RangeFn&& rangeOf) {
    std::vector<RolledAffix> rolled;

    // 0. 候选。seen: attrId -> candidates 下标;kRejected = 已问过 rangeOf、本档不可用(不再问第二次)。
    const std::size_t kRejected = SIZE_MAX;
    std::vector<detail::Candidate> candidates;
    std::map<uint32_t, std::size_t> seen;
    // 权重和用 64 位累加:单条至多 2^32 - 1,条目数受内存限制远小于 2^32,不会溢出。
    uint64_t totalWeight = 0;
    for (const PoolEntry& entry : pool) {
        if (entry.weight == 0) {
            continue;
        }
        const auto it = seen.find(entry.attrId);
        if (it != seen.end()) {
            if (it->second != kRejected) {
                candidates[it->second].weight += entry.weight;
                totalWeight += entry.weight;
            }
            continue;
        }
        const ValueRange range = rangeOf(entry.attrId);
        if (range.cap == 0) {
            seen.emplace(entry.attrId, kRejected);
            continue;
        }
        seen.emplace(entry.attrId, candidates.size());
        detail::Candidate candidate;
        candidate.attrId = entry.attrId;
        candidate.weight = entry.weight;
        candidate.range = range;
        candidates.push_back(candidate);
        totalWeight += entry.weight;
    }
    if (candidates.empty()) {
        return rolled;
    }

    // 全部候选的下标(池内顺序):粉 / 黄放回抽取用它,蓝的不放回抽取从它的一份拷贝里逐个剔除。
    std::vector<std::size_t> order(candidates.size());
    for (std::size_t i = 0; i < order.size(); ++i) {
        order[i] = i;
    }

    // 1. 蓝。条数区间用 64 位算:blueMin = 0、blueMax = UINT32_MAX 时区间长度是 2^32,32 位会绕成 0。
    const uint64_t countSpan =
        rule.blueMax > rule.blueMin ? static_cast<uint64_t>(rule.blueMax - rule.blueMin) + 1 : 1;
    uint64_t blueCount = static_cast<uint64_t>(rule.blueMin) + detail::RollBelow(rand, countSpan);
    if (blueCount > candidates.size()) {
        blueCount = candidates.size();
    }
    rolled.reserve(static_cast<std::size_t>(blueCount) + 2);

    std::vector<std::size_t> remaining = order;
    uint64_t remainingWeight = totalWeight;
    for (uint64_t seq = 0; seq < blueCount; ++seq) {
        // 循环不变量:remaining 非空(blueCount <= 候选数),且每个候选权重 >= 1,所以 remainingWeight >= 1。
        const uint64_t ticket = detail::RollBelow(rand, remainingWeight);
        const std::size_t position = detail::PickByTicket(candidates, remaining, ticket);
        const detail::Candidate& picked = candidates[remaining[position]];
        RolledAffix affix;
        affix.attrId = picked.attrId;
        affix.tier = AffixTier::kBlue;
        affix.value = detail::RollValue(rand, picked.range);
        affix.seq = static_cast<uint32_t>(seq);
        rolled.push_back(affix);
        remainingWeight -= picked.weight;
        // 保序删除:剩余候选仍按池内顺序排,「掷出的数 → 哪个属性」的映射才是确定的。
        remaining.erase(remaining.begin() + static_cast<std::ptrdiff_t>(position));
    }

    // 2. 粉  3. 黄
    detail::RollChanceAffix(candidates, order, totalWeight, rule.pinkRatePermyriad, AffixTier::kPink, rand, rolled);
    detail::RollChanceAffix(candidates, order, totalWeight, rule.yellowRatePermyriad, AffixTier::kYellow, rand,
                            rolled);
    return rolled;
}

// ---------------------------------------------------------------------------
// 上限表的等级档
// ---------------------------------------------------------------------------

// levels 里 <= equipLevel 的最大者的**下标**;一档都不满足返回 -1(该属性在这件装备上不可用)。
// levels 不要求有序(就是表行的自然顺序);有并列时取下标最小的那个。
inline int PickLevelTier(const std::vector<uint32_t>& levels, uint32_t equipLevel) {
    int best = -1;
    for (std::size_t i = 0; i < levels.size(); ++i) {
        if (levels[i] > equipLevel) {
            continue;
        }
        if (best < 0 || levels[i] > levels[static_cast<std::size_t>(best)]) {
            best = static_cast<int>(i);
        }
    }
    return best;
}

}  // namespace equiprules
