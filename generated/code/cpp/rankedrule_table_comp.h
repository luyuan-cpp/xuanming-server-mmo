
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/rankedrule_table.pb.h"

// ============================================================
// Per-column ECS components for RankedRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct RankedRuleIdComp {
    uint32_t value;
};

struct RankedRuleRule_versionComp {
    uint32_t value;
};

struct RankedRuleBattle_config_idComp {
    uint32_t value;
};

struct RankedRuleReference_class_idComp {
    uint32_t value;
};

struct RankedRuleCompetitive_levelComp {
    uint32_t value;
};

struct RankedRuleDefault_character_preset_idComp {
    uint32_t value;
};

struct RankedRuleDefault_pet_preset_idComp {
    uint32_t value;
};

struct RankedRuleSeason_duration_daysComp {
    uint32_t value;
};

struct RankedRuleWait_prompt_secondsComp {
    uint32_t value;
};

struct RankedRuleSpread_tier1_secondsComp {
    uint32_t value;
};

struct RankedRuleSpread_tier1_pointsComp {
    uint32_t value;
};

struct RankedRuleSpread_tier2_secondsComp {
    uint32_t value;
};

struct RankedRuleSpread_tier2_pointsComp {
    uint32_t value;
};

struct RankedRuleSpread_tier3_pointsComp {
    uint32_t value;
};

struct RankedRuleMilestone_avatar_matchesComp {
    uint32_t value;
};

struct RankedRuleMilestone_card_matchesComp {
    uint32_t value;
};

struct RankedRuleMilestone_talisman_matchesComp {
    uint32_t value;
};

struct RankedRuleTier_qingfeng_centiComp {
    uint32_t value;
};

struct RankedRuleTier_yunzhang_centiComp {
    uint32_t value;
};

struct RankedRuleTier_tianshu_centiComp {
    uint32_t value;
};

struct RankedRuleTier_wushuang_centiComp {
    uint32_t value;
};

struct RankedRuleDescComp {
    std::string_view value;
};

struct RankedRuleMilestone_win_matchesComp {
    uint32_t value;
};

struct RankedRuleSkill_ban_slots_per_teamComp {
    uint32_t value;
};

struct RankedRuleSkill_ban_min_rating_centiComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline RankedRuleIdComp MakeRankedRuleIdComp(const RankedRuleTable& row) {
    return { row.id() };
}
inline RankedRuleRule_versionComp MakeRankedRuleRule_versionComp(const RankedRuleTable& row) {
    return { row.rule_version() };
}
inline RankedRuleBattle_config_idComp MakeRankedRuleBattle_config_idComp(const RankedRuleTable& row) {
    return { row.battle_config_id() };
}
inline RankedRuleReference_class_idComp MakeRankedRuleReference_class_idComp(const RankedRuleTable& row) {
    return { row.reference_class_id() };
}
inline RankedRuleCompetitive_levelComp MakeRankedRuleCompetitive_levelComp(const RankedRuleTable& row) {
    return { row.competitive_level() };
}
inline RankedRuleDefault_character_preset_idComp MakeRankedRuleDefault_character_preset_idComp(const RankedRuleTable& row) {
    return { row.default_character_preset_id() };
}
inline RankedRuleDefault_pet_preset_idComp MakeRankedRuleDefault_pet_preset_idComp(const RankedRuleTable& row) {
    return { row.default_pet_preset_id() };
}
inline RankedRuleSeason_duration_daysComp MakeRankedRuleSeason_duration_daysComp(const RankedRuleTable& row) {
    return { row.season_duration_days() };
}
inline RankedRuleWait_prompt_secondsComp MakeRankedRuleWait_prompt_secondsComp(const RankedRuleTable& row) {
    return { row.wait_prompt_seconds() };
}
inline RankedRuleSpread_tier1_secondsComp MakeRankedRuleSpread_tier1_secondsComp(const RankedRuleTable& row) {
    return { row.spread_tier1_seconds() };
}
inline RankedRuleSpread_tier1_pointsComp MakeRankedRuleSpread_tier1_pointsComp(const RankedRuleTable& row) {
    return { row.spread_tier1_points() };
}
inline RankedRuleSpread_tier2_secondsComp MakeRankedRuleSpread_tier2_secondsComp(const RankedRuleTable& row) {
    return { row.spread_tier2_seconds() };
}
inline RankedRuleSpread_tier2_pointsComp MakeRankedRuleSpread_tier2_pointsComp(const RankedRuleTable& row) {
    return { row.spread_tier2_points() };
}
inline RankedRuleSpread_tier3_pointsComp MakeRankedRuleSpread_tier3_pointsComp(const RankedRuleTable& row) {
    return { row.spread_tier3_points() };
}
inline RankedRuleMilestone_avatar_matchesComp MakeRankedRuleMilestone_avatar_matchesComp(const RankedRuleTable& row) {
    return { row.milestone_avatar_matches() };
}
inline RankedRuleMilestone_card_matchesComp MakeRankedRuleMilestone_card_matchesComp(const RankedRuleTable& row) {
    return { row.milestone_card_matches() };
}
inline RankedRuleMilestone_talisman_matchesComp MakeRankedRuleMilestone_talisman_matchesComp(const RankedRuleTable& row) {
    return { row.milestone_talisman_matches() };
}
inline RankedRuleTier_qingfeng_centiComp MakeRankedRuleTier_qingfeng_centiComp(const RankedRuleTable& row) {
    return { row.tier_qingfeng_centi() };
}
inline RankedRuleTier_yunzhang_centiComp MakeRankedRuleTier_yunzhang_centiComp(const RankedRuleTable& row) {
    return { row.tier_yunzhang_centi() };
}
inline RankedRuleTier_tianshu_centiComp MakeRankedRuleTier_tianshu_centiComp(const RankedRuleTable& row) {
    return { row.tier_tianshu_centi() };
}
inline RankedRuleTier_wushuang_centiComp MakeRankedRuleTier_wushuang_centiComp(const RankedRuleTable& row) {
    return { row.tier_wushuang_centi() };
}
inline RankedRuleDescComp MakeRankedRuleDescComp(const RankedRuleTable& row) {
    return { std::string_view(row.desc()) };
}
inline RankedRuleMilestone_win_matchesComp MakeRankedRuleMilestone_win_matchesComp(const RankedRuleTable& row) {
    return { row.milestone_win_matches() };
}
inline RankedRuleSkill_ban_slots_per_teamComp MakeRankedRuleSkill_ban_slots_per_teamComp(const RankedRuleTable& row) {
    return { row.skill_ban_slots_per_team() };
}
inline RankedRuleSkill_ban_min_rating_centiComp MakeRankedRuleSkill_ban_min_rating_centiComp(const RankedRuleTable& row) {
    return { row.skill_ban_min_rating_centi() };
}
