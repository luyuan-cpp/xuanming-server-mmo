#pragma once

// 属性加点纯规则(零 ECS / 零表 / 零 proto 依赖,可单测),设计文档
// docs/design/player-attribute-allocation.md §3。
//
// PlayerAttributeSystem 负责把表行 / 组件翻译成这里的输入,这里只回答三个问题:
//   1) 某池在某等级一共有多少点;
//   2) 一份"目标已分配"是否合法(只增不减 / 单项上限 / 总量不超剩余);
//   3) 自动加点怎么把剩余点分下去(有上限池按优先序灌满,无上限池按权重比例)。

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

inline uint32_t SumPoints(const std::map<uint32_t, uint32_t>& allocated) {
    uint64_t sum = 0;
    for (const auto& [_, v] : allocated) {
        sum += v;
    }
    return sum > UINT32_MAX ? UINT32_MAX : static_cast<uint32_t>(sum);
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

}  // namespace attributerules
