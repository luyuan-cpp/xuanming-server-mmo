
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/skill_table.pb.h"

// ============================================================
// Per-column ECS components for SkillTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct SkillIdComp {
    uint32_t value;
};

struct SkillRequire_targetComp {
    uint32_t value;
};

struct SkillTarget_statusComp {
    uint32_t value;
};

struct SkillCast_pointComp {
    double value;
};

struct SkillRecovery_timeComp {
    double value;
};

struct SkillImmediateComp {
    uint32_t value;
};

struct SkillChannel_thinkComp {
    uint32_t value;
};

struct SkillChannel_finishComp {
    uint32_t value;
};

struct SkillThink_intervalComp {
    uint32_t value;
};

struct SkillChannel_timeComp {
    uint32_t value;
};

struct SkillRangeComp {
    double value;
};

struct SkillMax_rangeComp {
    double value;
};

struct SkillMin_rangeComp {
    double value;
};

struct SkillSelf_statusComp {
    uint32_t value;
};

struct SkillRequired_statusComp {
    uint32_t value;
};

struct SkillCooldown_idComp {
    uint32_t value;
};

struct SkillDamageComp {
    std::string_view value;
};

struct SkillDamage_typeComp {
    uint32_t value;
};

struct SkillAttack_multiplierComp {
    double value;
};

struct SkillSkill_typeComp {
    std::span<const uint32_t> values;
};

struct SkillTargeting_modeComp {
    std::span<const uint32_t> values;
};

struct SkillEffectComp {
    std::span<const uint32_t> values;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline SkillIdComp MakeSkillIdComp(const SkillTable& row) {
    return { row.id() };
}
inline SkillRequire_targetComp MakeSkillRequire_targetComp(const SkillTable& row) {
    return { row.require_target() };
}
inline SkillTarget_statusComp MakeSkillTarget_statusComp(const SkillTable& row) {
    return { row.target_status() };
}
inline SkillCast_pointComp MakeSkillCast_pointComp(const SkillTable& row) {
    return { row.cast_point() };
}
inline SkillRecovery_timeComp MakeSkillRecovery_timeComp(const SkillTable& row) {
    return { row.recovery_time() };
}
inline SkillImmediateComp MakeSkillImmediateComp(const SkillTable& row) {
    return { row.immediate() };
}
inline SkillChannel_thinkComp MakeSkillChannel_thinkComp(const SkillTable& row) {
    return { row.channel_think() };
}
inline SkillChannel_finishComp MakeSkillChannel_finishComp(const SkillTable& row) {
    return { row.channel_finish() };
}
inline SkillThink_intervalComp MakeSkillThink_intervalComp(const SkillTable& row) {
    return { row.think_interval() };
}
inline SkillChannel_timeComp MakeSkillChannel_timeComp(const SkillTable& row) {
    return { row.channel_time() };
}
inline SkillRangeComp MakeSkillRangeComp(const SkillTable& row) {
    return { row.range() };
}
inline SkillMax_rangeComp MakeSkillMax_rangeComp(const SkillTable& row) {
    return { row.max_range() };
}
inline SkillMin_rangeComp MakeSkillMin_rangeComp(const SkillTable& row) {
    return { row.min_range() };
}
inline SkillSelf_statusComp MakeSkillSelf_statusComp(const SkillTable& row) {
    return { row.self_status() };
}
inline SkillRequired_statusComp MakeSkillRequired_statusComp(const SkillTable& row) {
    return { row.required_status() };
}
inline SkillCooldown_idComp MakeSkillCooldown_idComp(const SkillTable& row) {
    return { row.cooldown_id() };
}
inline SkillDamageComp MakeSkillDamageComp(const SkillTable& row) {
    return { std::string_view(row.damage()) };
}
inline SkillDamage_typeComp MakeSkillDamage_typeComp(const SkillTable& row) {
    return { row.damage_type() };
}
inline SkillAttack_multiplierComp MakeSkillAttack_multiplierComp(const SkillTable& row) {
    return { row.attack_multiplier() };
}
inline SkillSkill_typeComp MakeSkillSkill_typeComp(const SkillTable& row) {
    const auto& rf = row.skill_type();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline SkillTargeting_modeComp MakeSkillTargeting_modeComp(const SkillTable& row) {
    const auto& rf = row.targeting_mode();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline SkillEffectComp MakeSkillEffectComp(const SkillTable& row) {
    const auto& rf = row.effect();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
