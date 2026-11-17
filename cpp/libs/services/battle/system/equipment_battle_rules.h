#pragma once

#include <algorithm>
#include <cmath>
#include <cstdint>

// 装备计算纯规则：不查表、不掷骰、不修改状态。调用方负责来源资格与实际收益后的次数提交。
namespace equipbattle {
inline uint64_t FloorBps(uint64_t value, uint32_t bps) {
    // 此处比例只用于限额与减免，最大 10000；拆商余避免 uint64 乘法溢出。
    const uint64_t bounded = std::min(bps, 10000u);
    return value / 10000 * bounded + value % 10000 * bounded / 10000;
}

inline uint64_t DiscountMana(uint64_t original, double reductionBps) {
    if (original == 0) return 0;
    if (!std::isfinite(reductionBps)) reductionBps = 0;
    const uint32_t reduction = static_cast<uint32_t>(std::clamp(reductionBps, 0.0, 3000.0));
    // ceil(C0 * (1-r)) = C0 - floor(C0*r)，C0 已按技能原规则取整。
    return std::max<uint64_t>(1, original - FloorBps(original, reduction));
}

inline double AilmentChance(double base, double hitBps, double typedResistBps,
                            double legacyResistBps, double ignoreBps, bool poison) {
    if (!std::isfinite(base) || base <= 0) return 0;
    const double resistance = std::max(0.0, typedResistBps + legacyResistBps - ignoreBps);
    return std::clamp(base + (hitBps - resistance) / 10000, 0.05, poison ? 0.95 : 0.85);
}

inline double ReceivedRatio(uint64_t armor, uint64_t defense, double resistanceBps,
                             uint32_t level) {
    const double coefficient = 360.0 + 120.0 * std::clamp(level, 1u, 85u);
    return std::max(0.4, coefficient / (static_cast<double>(armor) + defense + coefficient) *
        (1 - std::clamp(resistanceBps / 10000, 0.0, 1.0)));
}
} // namespace equipbattle
