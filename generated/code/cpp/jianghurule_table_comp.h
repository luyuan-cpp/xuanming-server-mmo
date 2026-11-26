
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/jianghurule_table.pb.h"

// ============================================================
// Per-column ECS components for JianghuRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct JianghuRuleIdComp {
    uint32_t value;
};

struct JianghuRuleEnabledComp {
    uint32_t value;
};

struct JianghuRuleSilver_writ_costComp {
    uint64_t value;
};

struct JianghuRuleGold_writ_item_idComp {
    uint32_t value;
};

struct JianghuRuleInvite_ttl_secComp {
    uint32_t value;
};

struct JianghuRuleTeam_confirm_secComp {
    uint32_t value;
};

struct JianghuRuleSame_target_cooldown_secComp {
    uint32_t value;
};

struct JianghuRuleDaily_unmatched_invite_limitComp {
    uint32_t value;
};

struct JianghuRuleDuel_round_limitComp {
    uint32_t value;
};

struct JianghuRuleDuel_timeout_secComp {
    uint32_t value;
};

struct JianghuRuleDefeat_mark_duration_secComp {
    uint32_t value;
};

struct JianghuRuleSilver_clear_costComp {
    uint64_t value;
};

struct JianghuRuleClear_item_idComp {
    uint32_t value;
};

struct JianghuRuleClear_badge_duration_secComp {
    uint32_t value;
};

struct JianghuRuleGrudge_max_rivalsComp {
    uint32_t value;
};

struct JianghuRuleGrudge_max_logs_per_rivalComp {
    uint32_t value;
};

struct JianghuRuleGuild_wall_max_recordsComp {
    uint32_t value;
};

struct JianghuRuleStreak_highlight_thresholdComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline JianghuRuleIdComp MakeJianghuRuleIdComp(const JianghuRuleTable& row) {
    return { row.id() };
}
inline JianghuRuleEnabledComp MakeJianghuRuleEnabledComp(const JianghuRuleTable& row) {
    return { row.enabled() };
}
inline JianghuRuleSilver_writ_costComp MakeJianghuRuleSilver_writ_costComp(const JianghuRuleTable& row) {
    return { row.silver_writ_cost() };
}
inline JianghuRuleGold_writ_item_idComp MakeJianghuRuleGold_writ_item_idComp(const JianghuRuleTable& row) {
    return { row.gold_writ_item_id() };
}
inline JianghuRuleInvite_ttl_secComp MakeJianghuRuleInvite_ttl_secComp(const JianghuRuleTable& row) {
    return { row.invite_ttl_sec() };
}
inline JianghuRuleTeam_confirm_secComp MakeJianghuRuleTeam_confirm_secComp(const JianghuRuleTable& row) {
    return { row.team_confirm_sec() };
}
inline JianghuRuleSame_target_cooldown_secComp MakeJianghuRuleSame_target_cooldown_secComp(const JianghuRuleTable& row) {
    return { row.same_target_cooldown_sec() };
}
inline JianghuRuleDaily_unmatched_invite_limitComp MakeJianghuRuleDaily_unmatched_invite_limitComp(const JianghuRuleTable& row) {
    return { row.daily_unmatched_invite_limit() };
}
inline JianghuRuleDuel_round_limitComp MakeJianghuRuleDuel_round_limitComp(const JianghuRuleTable& row) {
    return { row.duel_round_limit() };
}
inline JianghuRuleDuel_timeout_secComp MakeJianghuRuleDuel_timeout_secComp(const JianghuRuleTable& row) {
    return { row.duel_timeout_sec() };
}
inline JianghuRuleDefeat_mark_duration_secComp MakeJianghuRuleDefeat_mark_duration_secComp(const JianghuRuleTable& row) {
    return { row.defeat_mark_duration_sec() };
}
inline JianghuRuleSilver_clear_costComp MakeJianghuRuleSilver_clear_costComp(const JianghuRuleTable& row) {
    return { row.silver_clear_cost() };
}
inline JianghuRuleClear_item_idComp MakeJianghuRuleClear_item_idComp(const JianghuRuleTable& row) {
    return { row.clear_item_id() };
}
inline JianghuRuleClear_badge_duration_secComp MakeJianghuRuleClear_badge_duration_secComp(const JianghuRuleTable& row) {
    return { row.clear_badge_duration_sec() };
}
inline JianghuRuleGrudge_max_rivalsComp MakeJianghuRuleGrudge_max_rivalsComp(const JianghuRuleTable& row) {
    return { row.grudge_max_rivals() };
}
inline JianghuRuleGrudge_max_logs_per_rivalComp MakeJianghuRuleGrudge_max_logs_per_rivalComp(const JianghuRuleTable& row) {
    return { row.grudge_max_logs_per_rival() };
}
inline JianghuRuleGuild_wall_max_recordsComp MakeJianghuRuleGuild_wall_max_recordsComp(const JianghuRuleTable& row) {
    return { row.guild_wall_max_records() };
}
inline JianghuRuleStreak_highlight_thresholdComp MakeJianghuRuleStreak_highlight_thresholdComp(const JianghuRuleTable& row) {
    return { row.streak_highlight_threshold() };
}
