#pragma once

// 属性加点纯规则(零 ECS / 零表 / 零 proto 依赖,可单测),设计文档
// docs/design/player-attribute-allocation.md §3。
//
// PlayerAttributeSystem 负责把表行 / 组件翻译成这里的输入,这里只回答这几个问题:
//   1) 某池在某等级一共有多少点;
//   2) 一份"目标已分配"是否合法(只增不减 / 单项上限 / 总量不超剩余);
//   3) 自动加点怎么把剩余点分下去(有上限池按优先序灌满,无上限池按权重比例);
//   4) 角色属性点里"玩家分配的那部分"换算成多少二级属性(百分比 + 集中投资公式,2026-09-13);
//   5) 一份存储加点在本版本是否生效、还剩多少点(加点统一规则,2026-10-23,见下方 StoredPoolAllocation)。
//
// 单测:cpp/tests/turn_battle_engine_test/attribute_allocation_rules_test.cpp(纯头文件,不需要链接 scene.lib)。

#include <cmath>
#include <cstdint>
#include <map>
#include <vector>

namespace attributerules {

struct PoolRule {
    uint32_t poolId = 0;
    uint32_t unlockLevel = 1;
    uint32_t pointsPerLevel = 0;
    uint32_t basePoints = 0;
    uint32_t dimensionCap = 0;  // 0 = 不限
};

enum class AllocError : uint32_t {
    kOk = 0,
    kPoolLocked,
    kDimensionNotInPool,
    kCannotDecrease,
    kCapExceeded,
    kNotEnoughPoints,
    kNothingToChange,
};

inline bool IsUnlocked(const PoolRule& rule, uint32_t level) {
    return level >= rule.unlockLevel && rule.unlockLevel > 0;
}

// 总点数 = 解锁时一次性给点 + 每级点数 × (等级 - 解锁等级 + 1) + 额外点;未解锁 = 0。
inline uint32_t TotalPoints(const PoolRule& rule, uint32_t level, uint32_t bonusPoints) {
    if (!IsUnlocked(rule, level)) {
        return 0;
    }
    const uint64_t levels = static_cast<uint64_t>(level - rule.unlockLevel + 1);
    const uint64_t total = static_cast<uint64_t>(rule.basePoints) +
                           static_cast<uint64_t>(rule.pointsPerLevel) * levels +
                           static_cast<uint64_t>(bonusPoints);
    return total > UINT32_MAX ? UINT32_MAX : static_cast<uint32_t>(total);
}

// 当前值(HP/MP)随上限变化时按比例保持:活着的至少留 1。一升一降往返不净得;唯一例外是极低血量时
// "至少留 1"会让往返回升到 floor(oldMax/newMax),有上界,再往返也不再增长。
// 角色(加载/加点/切方案/洗点)与宝宝(加载/加点/洗点)共用;两边的升级(kLevelChanged)都走
// "按绝对增量补"分支,不经过本函数。避免"改属性当治疗"的白嫖路径
// (评审 2026-09-04,player-attribute-allocation.md §3.1)。
inline uint64_t RescaleCurrent(uint64_t current, uint64_t oldMax, uint64_t newMax) {
    if (current == 0 || newMax == 0) {
        return 0;
    }
    if (oldMax == 0 || oldMax == newMax) {
        return current < newMax ? current : newMax;
    }
    const auto scaled = static_cast<uint64_t>(static_cast<double>(current) *
                                              static_cast<double>(newMax) /
                                              static_cast<double>(oldMax));
    if (scaled < 1) {
        return 1;
    }
    return scaled < newMax ? scaled : newMax;
}

inline uint32_t SumPoints(const std::map<uint32_t, uint32_t>& allocated) {
    uint64_t sum = 0;
    for (const auto& [_, v] : allocated) {
        sum += v;
    }
    return sum > UINT32_MAX ? UINT32_MAX : static_cast<uint32_t>(sum);
}

// ---- 存储加点的生效判定(docs/design/mixed-version-persist-field-protection.md §12.1 D1–D4,2026-10-23) ----
//
// 加点统一规则(角色与宝宝共用;这里是与纯规则相关的部分,按表归类与写入口在 PlayerAttributeSystem / PetSystem):
//   1) 加载与重算永不改写、不返还已分配点,只有玩家主动的加点 / 洗点与 GM 会改。存储值原样携带,生效值在运行期
//      按本版本配置算出;本版本解释不了的内容只做保守处理(不生效、不让它变强),绝不先删掉再算。
//      以前加载时删掉查不到表的维度(D1 / D3)、整池清空超额的分配(D2 / D4),回滚一次就删数据且升回也恢复不了。
//   2) "已知维度" = 本版本表里属于该池、且属于该归属方(角色 / 宝宝)的维度;已分配里其余的键都是"未知键"
//      (新版本加的维度,或本版本归不到该方的键)。调用方按表归类后填进 StoredPoolAllocation。
//   3) 池生效 ⇔ Σ已知 ≤ 本版本总量 且(单项不限,或每个已知维度 ≤ 单项上限)。不生效时该池全部已知维度按 0 参与
//      二级属性、面板 value 与战斗快照;未知键永不生效。不生效的来源:改表缩点、等级降低(GM / 回档 / 超上限的
//      存储等级按上限读)、新版本扩点后回滚或混版本跨 zone。
//   4) 剩余点 = 生效 ? max(0, 总量 − Σ已知 − Σ未知) : 0。未知键归不到任何池,保守地对该方每个池都扣:
//      否则旧版本里玩家能把这些点再加到别的维度上,升回新版本后正好超额。
//   5) 加点的目标仍只能是本池已知维度(ValidateAllocation),剩余点按 4);不生效时剩余为 0,自然回"点数不足"。
//   6) 洗点只清本池已知维度,未知键原样保留;Σ已知 == 0 回"无可洗"且不扣费;池不生效时免费。
//   7) HP/MP 的旧上限 / 新上限都按生效值算:从不生效状态洗点,上限不变,HP/MP 也不变,不制造往返回血。
//   (第 8 条 —— 不给 AttributeScheme / PetInstance 本层未知字段加守卫 —— 写在两个系统的归类函数旁。)
// 求和一律 64 位且饱和,判定时不先把两个和相加:存储值再大也不会回绕成"看起来合法"的小数。

// 一份存储加点在某池上的汇总。纯规则不认识表,归类(哪些键是本池已知维度)由调用方按本版本表做。
struct StoredPoolAllocation {
    uint64_t knownUsed = 0;     // 本池已知维度的已分配点之和
    uint32_t knownLargest = 0;  // 本池已知维度里最大的单项(只用于单项上限判定)
    uint64_t foreignUsed = 0;   // 未知键的已分配点之和:永不生效,但计入已用(规则 4)

    void AddKnown(uint32_t points) {
        knownUsed = SaturatingAddPoints(knownUsed, points);
        knownLargest = points > knownLargest ? points : knownLargest;
    }
    void AddForeign(uint32_t points) { foreignUsed = SaturatingAddPoints(foreignUsed, points); }

    static uint64_t SaturatingAddPoints(uint64_t sum, uint32_t points) {
        return sum > UINT64_MAX - points ? UINT64_MAX : sum + points;
    }
};

// 规则 3:这份存储加点在本池是否生效。total 是本版本按生效等级算出的总量(TotalPoints)。
// 未解锁的池总量为 0:没有加点照样"生效"(剩余 0),有加点则不生效。
inline bool PoolAllocationEffective(const PoolRule& rule, uint32_t total, const StoredPoolAllocation& stored) {
    if (stored.knownUsed > total) {
        return false;
    }
    return rule.dimensionCap == 0 || stored.knownLargest <= rule.dimensionCap;
}

// 规则 4:本池剩余可分配点。不生效为 0;生效时饱和到 0,结果恒 ≤ total。
inline uint32_t RemainingPoints(const PoolRule& rule, uint32_t total, const StoredPoolAllocation& stored) {
    if (!PoolAllocationEffective(rule, total, stored)) {
        return 0;
    }
    // 生效时 knownUsed ≤ total,这里的减法不会回绕;再拿 foreignUsed 与差值比较,不做 knownUsed + foreignUsed
    // (foreignUsed 饱和到 UINT64_MAX 时相加会绕回一个小数)。
    const uint64_t left = static_cast<uint64_t>(total) - stored.knownUsed;
    return stored.foreignUsed >= left ? 0 : static_cast<uint32_t>(left - stored.foreignUsed);
}

// 校验"目标已分配"(target 只含本池维度;缺省维度视为不变)。
//   current   当前方案里本池各维度的已分配点(池内全部维度都要有键,值可为 0)
//   remaining 本池剩余点
//   deltaOut  通过校验时的增量总和
inline AllocError ValidateAllocation(const PoolRule& rule, uint32_t level,
                                     const std::map<uint32_t, uint32_t>& current,
                                     const std::map<uint32_t, uint32_t>& target,
                                     uint32_t remaining, uint32_t& deltaOut) {
    deltaOut = 0;
    if (!IsUnlocked(rule, level)) {
        return AllocError::kPoolLocked;
    }
    // 必须 64 位累加:目标值来自客户端,单项最大 UINT32_MAX(外挂发的负数也会绕成超大正数),
    // 32 位累加时 UINT32_MAX + 10 会绕成 9,骗过下面的剩余点校验
    uint64_t delta = 0;
    for (const auto& [dimensionId, want] : target) {
        const auto it = current.find(dimensionId);
        if (it == current.end()) {
            return AllocError::kDimensionNotInPool;
        }
        if (want < it->second) {
            return AllocError::kCannotDecrease;
        }
        if (rule.dimensionCap > 0 && want > rule.dimensionCap) {
            return AllocError::kCapExceeded;
        }
        delta += want - it->second;
    }
    if (delta == 0) {
        return AllocError::kNothingToChange;
    }
    if (delta > remaining) {
        return AllocError::kNotEnoughPoints;
    }
    deltaOut = static_cast<uint32_t>(delta);
    return AllocError::kOk;
}

// 自动分配:把 remaining 点分到 allocated(in-out,含已分配)上。
//   order   维度优先序(表 AttributeAutoPlan.dimension)
//   weights 对应权重(缺省视为 1;0 = 不分)
// 有上限池:按 order 逐个灌到 cap;无上限池:按权重比例分配,余数按 order 先后逐点补齐,
// 结果确定(不依赖 map 迭代序以外的任何随机)。
inline void DistributePoints(const PoolRule& rule, const std::vector<uint32_t>& order,
                             const std::vector<uint32_t>& weights, uint32_t remaining,
                             std::map<uint32_t, uint32_t>& allocated) {
    if (remaining == 0 || order.empty()) {
        return;
    }
    if (rule.dimensionCap > 0) {
        for (const auto dimensionId : order) {
            if (remaining == 0) {
                break;
            }
            auto& cur = allocated[dimensionId];
            if (cur >= rule.dimensionCap) {
                continue;
            }
            const uint32_t room = rule.dimensionCap - cur;
            const uint32_t give = room < remaining ? room : remaining;
            cur += give;
            remaining -= give;
        }
        return;
    }

    uint64_t weightSum = 0;
    std::vector<uint32_t> effectiveWeights(order.size(), 1);
    for (size_t i = 0; i < order.size(); ++i) {
        effectiveWeights[i] = i < weights.size() ? weights[i] : 1;
        weightSum += effectiveWeights[i];
    }
    if (weightSum == 0) {
        return;
    }
    uint32_t distributed = 0;
    for (size_t i = 0; i < order.size(); ++i) {
        const auto give = static_cast<uint32_t>(
            static_cast<uint64_t>(remaining) * effectiveWeights[i] / weightSum);
        allocated[order[i]] += give;
        distributed += give;
    }
    // 余数按优先序逐点补齐(只补权重 > 0 的维度)
    uint32_t left = remaining - distributed;
    for (size_t i = 0; left > 0 && i < order.size(); ++i) {
        if (effectiveWeights[i] == 0) {
            continue;
        }
        allocated[order[i]] += 1;
        --left;
    }
}

// ---- 角色属性点的加点收益(2026-09-13 策划公式,设计文档 §3.1) ----
//
//   有效点数 E(n) = n × (1 + bonus × n ÷ scale)
//   属性增量     = 标准基础属性 × 比例 × E(n) ÷ E(scale)
//
// scale = 该维度满级时最多能分到的点数(角色属性点池 = 85 × 5 = 425),E(scale) = scale × (1 + bonus)
// (= 510,即策划公式里的除数)。所以满投正好拿到 AttributeAllocRatio 表定的比例,分散投则按 E(n) 打折
// —— 这就是"集中投资"的来源:同样 425 点,全投一项有效点 510,四项各 106 点合计约 445.15,
// 集中投比分散投多约 14.6%(反过来说分散投少约 12.7%)。
//
// scale 与除数**不进表,由池表与等级上限现算**(评审 2026-09-13):它们完全由 AttributePool 的
// points_per_level / dimension_cap 与 playerlevel::kMaxLevel 决定。手抄一份到表里就是第二份真相 ——
// 改等级上限或每级点数时忘了同步,满投会悄悄超过表定比例,且零报错。表里只配 bonus。
// "标准基础属性"= 职业初值 + 85 级自然成长(不含加点、不含装备),由调用方按表算好传进来。

inline constexpr double kDefaultEfficiencyBonus = 0.20;

struct AllocFormulaRule {
    double efficiencyBonus = kDefaultEfficiencyBonus;
    uint32_t fullInvestmentPoints = 425;  // scale:该维度满级时最多能分到的点数
};

// 该池单个维度在 maxLevel 时最多能分到多少点:不限单项 → 满级总点数;限单项 → min(单项上限, 满级总点数)。
// 不计 bonus_points:额外点目前没有写入方,有了之后 n 可能超过 scale、收益超过表定比例(设计文档 §8)。
inline uint32_t FullInvestmentPoints(const PoolRule& rule, uint32_t maxLevel) {
    const uint32_t total = TotalPoints(rule, maxLevel, 0);
    return (rule.dimensionCap > 0 && rule.dimensionCap < total) ? rule.dimensionCap : total;
}

// bonus 取表值:0 是合法值(纯线性,没有集中投资倾向);负数 / 非有限数是坏表,退回策划默认 0.20
inline AllocFormulaRule MakeFormulaRule(double tableBonus, uint32_t fullInvestmentPoints) {
    AllocFormulaRule rule;
    if (std::isfinite(tableBonus) && tableBonus >= 0.0) {
        rule.efficiencyBonus = tableBonus;
    }
    rule.fullInvestmentPoints = fullInvestmentPoints;
    return rule;
}

inline double EffectivePoints(uint32_t allocated, const AllocFormulaRule& rule) {
    const double n = static_cast<double>(allocated);
    if (rule.fullInvestmentPoints == 0) {
        return n;  // 该池满级也分不到点(解锁等级高于上限),不会有已分配;防御除零
    }
    return n * (1.0 + rule.efficiencyBonus * n / static_cast<double>(rule.fullInvestmentPoints));
}

// 满投时的有效点数 E(scale) = scale × (1 + bonus),即公式里的除数
inline double FullInvestmentEffectivePoints(const AllocFormulaRule& rule) {
    return static_cast<double>(rule.fullInvestmentPoints) * (1.0 + rule.efficiencyBonus);
}

// 一个维度对一项二级属性的增量。ratio 是该维度对该属性的满投比例(AttributeAllocRatio 表);
// 比例为 0 的属性直接得 0,不做任何除法。
inline double AllocatedIncrement(double standardBase, double ratio, uint32_t allocated,
                                 const AllocFormulaRule& rule) {
    const double divisor = FullInvestmentEffectivePoints(rule);
    if (ratio <= 0.0 || allocated == 0 || standardBase <= 0.0 || divisor <= 0.0) {
        return 0.0;
    }
    return standardBase * ratio * EffectivePoints(allocated, rule) / divisor;
}

}  // namespace attributerules
