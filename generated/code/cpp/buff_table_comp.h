
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/buff_table.pb.h"

// ============================================================
// Per-column ECS components for BuffTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct BuffIdComp {
    uint32_t value;
};

struct BuffNo_casterComp {
    uint32_t value;
};

struct BuffBuff_typeComp {
    uint32_t value;
};

struct BuffLevelComp {
    uint32_t value;
};

struct BuffMax_layerComp {
    uint32_t value;
};

struct BuffInfinite_durationComp {
    uint32_t value;
};

struct BuffDurationComp {
    double value;
};

struct BuffForce_interruptComp {
    uint32_t value;
};

struct BuffIntervalComp {
    double value;
};

struct BuffInterval_countComp {
    uint32_t value;
};

struct BuffMovement_speed_boostComp {
    double value;
};

struct BuffMovement_speed_reductionComp {
    double value;
};

struct BuffHealth_regenerationComp {
    std::string_view value;
};

struct BuffCombat_idle_secondsComp {
    double value;
};

struct BuffTimeComp {
    uint32_t value;
};

struct BuffBonus_damageComp {
    std::string_view value;
};

struct BuffBattle_opComp {
    uint32_t value;
};

struct BuffTrigger_eventComp {
    uint32_t value;
};

struct BuffEffect_targetComp {
    uint32_t value;
};

struct BuffFlat_valueComp {
    double value;
};

struct BuffPer_levelComp {
    double value;
};

struct BuffRatioComp {
    double value;
};

struct BuffValue_basisComp {
    uint32_t value;
};

struct BuffDamage_filterComp {
    uint32_t value;
};

struct BuffDuration_roundsComp {
    uint32_t value;
};

struct BuffTick_countComp {
    uint32_t value;
};

struct BuffTrigger_chanceComp {
    double value;
};

struct BuffPer_round_limitComp {
    uint32_t value;
};

struct BuffHit_limitComp {
    uint32_t value;
};

struct BuffModifier_statComp {
    uint32_t value;
};

struct BuffState_kindComp {
    uint32_t value;
};

struct BuffApplication_chanceComp {
    double value;
};

struct BuffRequired_tagComp {
    std::string_view value;
};

struct BuffSecond_required_tagComp {
    std::string_view value;
};

struct BuffConditional_bonusComp {
    double value;
};

struct BuffSecondary_ratioComp {
    double value;
};

struct BuffEffect_countComp {
    uint32_t value;
};

struct BuffTarget_countComp {
    uint32_t value;
};

struct BuffPet_target_countComp {
    uint32_t value;
};

struct BuffStack_keyComp {
    uint32_t value;
};

struct BuffRefresh_policyComp {
    uint32_t value;
};

struct BuffKeep_on_deathComp {
    uint32_t value;
};

struct BuffEffect_priorityComp {
    uint32_t value;
};

struct BuffStart_roundComp {
    uint32_t value;
};

struct BuffStack_limitComp {
    uint32_t value;
};

struct BuffRequire_living_casterComp {
    uint32_t value;
};

struct BuffTick_on_applyComp {
    uint32_t value;
};

struct BuffKeep_on_logoutComp {
    uint32_t value;
};

struct BuffInterval_effectComp {
    std::span<const double> values;
};

struct BuffSub_buffComp {
    std::span<const uint32_t> values;
};

struct BuffTarget_sub_buffComp {
    std::span<const uint32_t> values;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline BuffIdComp MakeBuffIdComp(const BuffTable& row) {
    return { row.id() };
}
inline BuffNo_casterComp MakeBuffNo_casterComp(const BuffTable& row) {
    return { row.no_caster() };
}
inline BuffBuff_typeComp MakeBuffBuff_typeComp(const BuffTable& row) {
    return { row.buff_type() };
}
inline BuffLevelComp MakeBuffLevelComp(const BuffTable& row) {
    return { row.level() };
}
inline BuffMax_layerComp MakeBuffMax_layerComp(const BuffTable& row) {
    return { row.max_layer() };
}
inline BuffInfinite_durationComp MakeBuffInfinite_durationComp(const BuffTable& row) {
    return { row.infinite_duration() };
}
inline BuffDurationComp MakeBuffDurationComp(const BuffTable& row) {
    return { row.duration() };
}
inline BuffForce_interruptComp MakeBuffForce_interruptComp(const BuffTable& row) {
    return { row.force_interrupt() };
}
inline BuffIntervalComp MakeBuffIntervalComp(const BuffTable& row) {
    return { row.interval() };
}
inline BuffInterval_countComp MakeBuffInterval_countComp(const BuffTable& row) {
    return { row.interval_count() };
}
inline BuffMovement_speed_boostComp MakeBuffMovement_speed_boostComp(const BuffTable& row) {
    return { row.movement_speed_boost() };
}
inline BuffMovement_speed_reductionComp MakeBuffMovement_speed_reductionComp(const BuffTable& row) {
    return { row.movement_speed_reduction() };
}
inline BuffHealth_regenerationComp MakeBuffHealth_regenerationComp(const BuffTable& row) {
    return { std::string_view(row.health_regeneration()) };
}
inline BuffCombat_idle_secondsComp MakeBuffCombat_idle_secondsComp(const BuffTable& row) {
    return { row.combat_idle_seconds() };
}
inline BuffTimeComp MakeBuffTimeComp(const BuffTable& row) {
    return { row.time() };
}
inline BuffBonus_damageComp MakeBuffBonus_damageComp(const BuffTable& row) {
    return { std::string_view(row.bonus_damage()) };
}
inline BuffBattle_opComp MakeBuffBattle_opComp(const BuffTable& row) {
    return { row.battle_op() };
}
inline BuffTrigger_eventComp MakeBuffTrigger_eventComp(const BuffTable& row) {
    return { row.trigger_event() };
}
inline BuffEffect_targetComp MakeBuffEffect_targetComp(const BuffTable& row) {
    return { row.effect_target() };
}
inline BuffFlat_valueComp MakeBuffFlat_valueComp(const BuffTable& row) {
    return { row.flat_value() };
}
inline BuffPer_levelComp MakeBuffPer_levelComp(const BuffTable& row) {
    return { row.per_level() };
}
inline BuffRatioComp MakeBuffRatioComp(const BuffTable& row) {
    return { row.ratio() };
}
inline BuffValue_basisComp MakeBuffValue_basisComp(const BuffTable& row) {
    return { row.value_basis() };
}
inline BuffDamage_filterComp MakeBuffDamage_filterComp(const BuffTable& row) {
    return { row.damage_filter() };
}
inline BuffDuration_roundsComp MakeBuffDuration_roundsComp(const BuffTable& row) {
    return { row.duration_rounds() };
}
inline BuffTick_countComp MakeBuffTick_countComp(const BuffTable& row) {
    return { row.tick_count() };
}
inline BuffTrigger_chanceComp MakeBuffTrigger_chanceComp(const BuffTable& row) {
    return { row.trigger_chance() };
}
inline BuffPer_round_limitComp MakeBuffPer_round_limitComp(const BuffTable& row) {
    return { row.per_round_limit() };
}
inline BuffHit_limitComp MakeBuffHit_limitComp(const BuffTable& row) {
    return { row.hit_limit() };
}
inline BuffModifier_statComp MakeBuffModifier_statComp(const BuffTable& row) {
    return { row.modifier_stat() };
}
inline BuffState_kindComp MakeBuffState_kindComp(const BuffTable& row) {
    return { row.state_kind() };
}
inline BuffApplication_chanceComp MakeBuffApplication_chanceComp(const BuffTable& row) {
    return { row.application_chance() };
}
inline BuffRequired_tagComp MakeBuffRequired_tagComp(const BuffTable& row) {
    return { std::string_view(row.required_tag()) };
}
inline BuffSecond_required_tagComp MakeBuffSecond_required_tagComp(const BuffTable& row) {
    return { std::string_view(row.second_required_tag()) };
}
inline BuffConditional_bonusComp MakeBuffConditional_bonusComp(const BuffTable& row) {
    return { row.conditional_bonus() };
}
inline BuffSecondary_ratioComp MakeBuffSecondary_ratioComp(const BuffTable& row) {
    return { row.secondary_ratio() };
}
inline BuffEffect_countComp MakeBuffEffect_countComp(const BuffTable& row) {
    return { row.effect_count() };
}
inline BuffTarget_countComp MakeBuffTarget_countComp(const BuffTable& row) {
    return { row.target_count() };
}
inline BuffPet_target_countComp MakeBuffPet_target_countComp(const BuffTable& row) {
    return { row.pet_target_count() };
}
inline BuffStack_keyComp MakeBuffStack_keyComp(const BuffTable& row) {
    return { row.stack_key() };
}
inline BuffRefresh_policyComp MakeBuffRefresh_policyComp(const BuffTable& row) {
    return { row.refresh_policy() };
}
inline BuffKeep_on_deathComp MakeBuffKeep_on_deathComp(const BuffTable& row) {
    return { row.keep_on_death() };
}
inline BuffEffect_priorityComp MakeBuffEffect_priorityComp(const BuffTable& row) {
    return { row.effect_priority() };
}
inline BuffStart_roundComp MakeBuffStart_roundComp(const BuffTable& row) {
    return { row.start_round() };
}
inline BuffStack_limitComp MakeBuffStack_limitComp(const BuffTable& row) {
    return { row.stack_limit() };
}
inline BuffRequire_living_casterComp MakeBuffRequire_living_casterComp(const BuffTable& row) {
    return { row.require_living_caster() };
}
inline BuffTick_on_applyComp MakeBuffTick_on_applyComp(const BuffTable& row) {
    return { row.tick_on_apply() };
}
inline BuffKeep_on_logoutComp MakeBuffKeep_on_logoutComp(const BuffTable& row) {
    return { row.keep_on_logout() };
}
inline BuffInterval_effectComp MakeBuffInterval_effectComp(const BuffTable& row) {
    const auto& rf = row.interval_effect();
    return { std::span<const double>(rf.data(), rf.size()) };
}
inline BuffSub_buffComp MakeBuffSub_buffComp(const BuffTable& row) {
    const auto& rf = row.sub_buff();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline BuffTarget_sub_buffComp MakeBuffTarget_sub_buffComp(const BuffTable& row) {
    const auto& rf = row.target_sub_buff();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
