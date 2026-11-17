#pragma once

#include <algorithm>
#include <cmath>
#include <cstdint>

// 装备计算纯规则：不查表、不掷骰、不修改状态。调用方负责来源资格与实际收益后的次数提交。
namespace equipbattle {
inline uint32_t MaximumSourceBps(uint32_t effect) {
    switch (effect) {
    case 1: case 2: case 5: case 6: return 3200;
    case 3: case 7: case 8: return 800;
    case 4: return 200;
    case 9: case 10: return 600;
    case 11: case 12: return 1600;
    case 13: return 1200;
    default: return 0;
    }
}

inline bool ValidArtifactDefinition(uint32_t effectId, uint32_t bonusBps, uint32_t capBps,
                                     uint32_t perRoundLimit, uint32_t battleLimit,
                                     uint32_t activeRounds, uint32_t healthThresholdBps) {
    if (effectId < 1 || effectId > 8 || bonusBps == 0 || bonusBps > 10000 ||
        capBps > 10000 || healthThresholdBps > 10000 || perRoundLimit > 1 || battleLimit > 3 || activeRounds > 2) return false;
    if (effectId != 6 && perRoundLimit == 0) return false;
    if ((effectId == 3 || effectId >= 6) && battleLimit == 0) return false;
    if (effectId == 6 && (capBps == 0 || activeRounds == 0)) return false;
    return effectId != 4 || healthThresholdBps != 0;
}

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
