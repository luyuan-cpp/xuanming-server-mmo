#pragma once

#include <algorithm>
#include <cmath>
#include <cstdint>
#include "combat_damage_rules.h"
#include "../constants/battle_effect_constants.h"

// 装备计算纯规则：不查表、不掷骰、不修改状态。调用方负责来源资格与实际收益后的次数提交。
namespace equipbattle {
// 伤害效果优先，其余类别按配置效果顺序判定；场景预览与实际扣蓝共用。
template<class Skill, class FindBuff>
uint32_t ManaCategory(const Skill& skill, FindBuff findBuff) {
    using turnbattle::BattleEffectOp;
    if (skill.battle_model() != 1) return 0;
    for (const auto id : skill.effect()) {
        const auto* row = findBuff(id);
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Damage))
            return skill.damage_type() == combatdamage::kPhysicalDamage ? 1u : 2u;
    }
    bool restoreMana = false;
    for (const auto id : skill.effect()) {
        const auto* row = findBuff(id); if (row == nullptr) continue;
        switch (static_cast<BattleEffectOp>(row->battle_op())) {
        case BattleEffectOp::Heal: case BattleEffectOp::ReviveMark: return 3;
        case BattleEffectOp::Shield: return 4;
        case BattleEffectOp::Control:
            if (row->state_kind() >= 1 && row->state_kind() <= 7) return row->state_kind() + 4;
            break;
        case BattleEffectOp::PeriodicDamage:
            if (row->state_kind() == static_cast<uint32_t>(turnbattle::BattleStateKind::Poison)) return 11;
            break;
        case BattleEffectOp::ConsumePeriodic: case BattleEffectOp::ExtendPeriodic: return 11;
        case BattleEffectOp::RestoreMana: restoreMana = true; break;
        default: break;
        }
    }
    return restoreMana ? 0 : 12;
}

constexpr uint32_t MaximumSourceBps(uint32_t effect) {
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

// 控辅强化公平受益后的战斗快照硬顶：治疗/护盾（effect 5/6）在 +11 按 50% 强化份额放大至 3584 bps，
// 障碍命中（effect 11）按 25% 强化份额放大至 1696 bps（且受 AilmentChance 85%/95% 终值硬顶约束）；
// 速度（effect 4）与其余通道严格保持表上限不变。
constexpr uint32_t MaximumEffectiveSourceBps(uint32_t effect) {
    switch (effect) {
    case 5: case 6: return 3584;
    case 11: return 1696;
    default: return MaximumSourceBps(effect);
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
