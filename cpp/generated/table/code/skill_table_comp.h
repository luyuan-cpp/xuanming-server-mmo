
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

struct SkillClass_idComp {
    uint32_t value;
};

struct SkillNameComp {
    std::string_view value;
};

struct SkillDescriptionComp {
    std::string_view value;
};

struct SkillIcon_keyComp {
    std::string_view value;
};

struct SkillCategoryComp {
    std::string_view value;
};

struct SkillUnlock_levelComp {
    uint32_t value;
};

struct SkillMax_rankComp {
    uint32_t value;
};

struct SkillTarget_teamComp {
    uint32_t value;
};

struct SkillMax_targetsComp {
    uint32_t value;
};

struct SkillMana_percentComp {
    double value;
};

struct SkillCooldown_roundsComp {
    uint32_t value;
};

struct SkillHeal_multiplierComp {
    double value;
};

struct SkillHeal_per_levelComp {
    double value;
};

struct SkillExecute_heal_thresholdComp {
    double value;
};

struct SkillExecute_heal_bonusComp {
    double value;
};

struct SkillRestore_mana_percentComp {
    double value;
};

struct SkillDispel_countComp {
    uint32_t value;
};

struct SkillInitiativeComp {
    int32_t value;
};

struct SkillPassive_statComp {
    std::string_view value;
};

struct SkillPassive_percentComp {
    double value;
};

struct SkillBuff_percentComp {
    double value;
};

struct SkillBattle_modelComp {
    uint32_t value;
};

struct SkillCaster_maskComp {
    uint32_t value;
};

struct SkillAbility_kindComp {
    uint32_t value;
};

struct SkillPet_max_targetsComp {
    uint32_t value;
};

struct SkillPrototype_codeComp {
    std::string_view value;
};

struct SkillMax_battle_usesComp {
    uint32_t value;
};

struct SkillHealth_cost_modeComp {
    uint32_t value;
};

struct SkillAction_tagsComp {
    uint32_t value;
};

struct SkillAllow_dead_targetComp {
    uint32_t value;
};

struct SkillForbid_copyComp {
    uint32_t value;
};

struct SkillSeason_idComp {
    uint32_t value;
};

struct SkillScroll_idComp {
    uint32_t value;
};

struct SkillRarity_tierComp {
    uint32_t value;
};

struct SkillSecondary_target_policyComp {
    uint32_t value;
};

struct SkillExclusive_groupComp {
    uint32_t value;
};

struct SkillBan_pool_categoryComp {
    uint32_t value;
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

struct SkillInit_effectComp {
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
inline SkillClass_idComp MakeSkillClass_idComp(const SkillTable& row) {
    return { row.class_id() };
}
inline SkillNameComp MakeSkillNameComp(const SkillTable& row) {
    return { std::string_view(row.name()) };
}
inline SkillDescriptionComp MakeSkillDescriptionComp(const SkillTable& row) {
    return { std::string_view(row.description()) };
}
inline SkillIcon_keyComp MakeSkillIcon_keyComp(const SkillTable& row) {
    return { std::string_view(row.icon_key()) };
}
inline SkillCategoryComp MakeSkillCategoryComp(const SkillTable& row) {
    return { std::string_view(row.category()) };
}
inline SkillUnlock_levelComp MakeSkillUnlock_levelComp(const SkillTable& row) {
    return { row.unlock_level() };
}
inline SkillMax_rankComp MakeSkillMax_rankComp(const SkillTable& row) {
    return { row.max_rank() };
}
inline SkillTarget_teamComp MakeSkillTarget_teamComp(const SkillTable& row) {
    return { row.target_team() };
}
inline SkillMax_targetsComp MakeSkillMax_targetsComp(const SkillTable& row) {
    return { row.max_targets() };
}
inline SkillMana_percentComp MakeSkillMana_percentComp(const SkillTable& row) {
    return { row.mana_percent() };
}
inline SkillCooldown_roundsComp MakeSkillCooldown_roundsComp(const SkillTable& row) {
    return { row.cooldown_rounds() };
}
inline SkillHeal_multiplierComp MakeSkillHeal_multiplierComp(const SkillTable& row) {
    return { row.heal_multiplier() };
}
inline SkillHeal_per_levelComp MakeSkillHeal_per_levelComp(const SkillTable& row) {
    return { row.heal_per_level() };
}
inline SkillExecute_heal_thresholdComp MakeSkillExecute_heal_thresholdComp(const SkillTable& row) {
    return { row.execute_heal_threshold() };
}
inline SkillExecute_heal_bonusComp MakeSkillExecute_heal_bonusComp(const SkillTable& row) {
    return { row.execute_heal_bonus() };
}
inline SkillRestore_mana_percentComp MakeSkillRestore_mana_percentComp(const SkillTable& row) {
    return { row.restore_mana_percent() };
}
inline SkillDispel_countComp MakeSkillDispel_countComp(const SkillTable& row) {
    return { row.dispel_count() };
}
inline SkillInitiativeComp MakeSkillInitiativeComp(const SkillTable& row) {
    return { row.initiative() };
}
inline SkillPassive_statComp MakeSkillPassive_statComp(const SkillTable& row) {
    return { std::string_view(row.passive_stat()) };
}
inline SkillPassive_percentComp MakeSkillPassive_percentComp(const SkillTable& row) {
    return { row.passive_percent() };
}
inline SkillBuff_percentComp MakeSkillBuff_percentComp(const SkillTable& row) {
    return { row.buff_percent() };
}
inline SkillBattle_modelComp MakeSkillBattle_modelComp(const SkillTable& row) {
    return { row.battle_model() };
}
inline SkillCaster_maskComp MakeSkillCaster_maskComp(const SkillTable& row) {
    return { row.caster_mask() };
}
inline SkillAbility_kindComp MakeSkillAbility_kindComp(const SkillTable& row) {
    return { row.ability_kind() };
}
inline SkillPet_max_targetsComp MakeSkillPet_max_targetsComp(const SkillTable& row) {
    return { row.pet_max_targets() };
}
inline SkillPrototype_codeComp MakeSkillPrototype_codeComp(const SkillTable& row) {
    return { std::string_view(row.prototype_code()) };
}
inline SkillMax_battle_usesComp MakeSkillMax_battle_usesComp(const SkillTable& row) {
    return { row.max_battle_uses() };
}
inline SkillHealth_cost_modeComp MakeSkillHealth_cost_modeComp(const SkillTable& row) {
    return { row.health_cost_mode() };
}
inline SkillAction_tagsComp MakeSkillAction_tagsComp(const SkillTable& row) {
    return { row.action_tags() };
}
inline SkillAllow_dead_targetComp MakeSkillAllow_dead_targetComp(const SkillTable& row) {
    return { row.allow_dead_target() };
}
inline SkillForbid_copyComp MakeSkillForbid_copyComp(const SkillTable& row) {
    return { row.forbid_copy() };
}
inline SkillSeason_idComp MakeSkillSeason_idComp(const SkillTable& row) {
    return { row.season_id() };
}
inline SkillScroll_idComp MakeSkillScroll_idComp(const SkillTable& row) {
    return { row.scroll_id() };
}
inline SkillRarity_tierComp MakeSkillRarity_tierComp(const SkillTable& row) {
    return { row.rarity_tier() };
}
inline SkillSecondary_target_policyComp MakeSkillSecondary_target_policyComp(const SkillTable& row) {
    return { row.secondary_target_policy() };
}
inline SkillExclusive_groupComp MakeSkillExclusive_groupComp(const SkillTable& row) {
    return { row.exclusive_group() };
}
inline SkillBan_pool_categoryComp MakeSkillBan_pool_categoryComp(const SkillTable& row) {
    return { row.ban_pool_category() };
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
inline SkillInit_effectComp MakeSkillInit_effectComp(const SkillTable& row) {
    const auto& rf = row.init_effect();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
