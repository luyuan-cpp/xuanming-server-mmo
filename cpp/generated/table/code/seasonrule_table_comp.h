
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/seasonrule_table.pb.h"

// ============================================================
// Per-column ECS components for SeasonRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct SeasonRuleIdComp {
    uint32_t value;
};

struct SeasonRuleNameComp {
    std::string_view value;
};

struct SeasonRuleStarts_at_msComp {
    uint64_t value;
};

struct SeasonRuleEnds_at_msComp {
    uint64_t value;
};

struct SeasonRuleEnabledComp {
    bool value;
};

struct SeasonRuleSeal_max_pointsComp {
    uint32_t value;
};

struct SeasonRulePve_win_pointsComp {
    uint32_t value;
};

struct SeasonRulePvp_win_pointsComp {
    uint32_t value;
};

struct SeasonRulePvp_other_pointsComp {
    uint32_t value;
};

struct SeasonRuleMax_health_bps_capComp {
    uint32_t value;
};

struct SeasonRuleDefense_bps_capComp {
    uint32_t value;
};

struct SeasonRuleCatchup_currency_typeComp {
    uint32_t value;
};

struct SeasonRuleCatchup_price_per_pointComp {
    uint64_t value;
};

struct SeasonRuleDefault_seal_idComp {
    uint32_t value;
};

struct SeasonRuleDescComp {
    std::string_view value;
};

struct SeasonRuleHonor_title_nameComp {
    std::string_view value;
};

struct SeasonRuleHonor_badge_icon_keyComp {
    std::string_view value;
};

struct SeasonRuleCycle_orderComp {
    uint32_t value;
};

struct SeasonRuleCycle_duration_daysComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline SeasonRuleIdComp MakeSeasonRuleIdComp(const SeasonRuleTable& row) {
    return { row.id() };
}
inline SeasonRuleNameComp MakeSeasonRuleNameComp(const SeasonRuleTable& row) {
    return { std::string_view(row.name()) };
}
inline SeasonRuleStarts_at_msComp MakeSeasonRuleStarts_at_msComp(const SeasonRuleTable& row) {
    return { row.starts_at_ms() };
}
inline SeasonRuleEnds_at_msComp MakeSeasonRuleEnds_at_msComp(const SeasonRuleTable& row) {
    return { row.ends_at_ms() };
}
inline SeasonRuleEnabledComp MakeSeasonRuleEnabledComp(const SeasonRuleTable& row) {
    return { row.enabled() };
}
inline SeasonRuleSeal_max_pointsComp MakeSeasonRuleSeal_max_pointsComp(const SeasonRuleTable& row) {
    return { row.seal_max_points() };
}
inline SeasonRulePve_win_pointsComp MakeSeasonRulePve_win_pointsComp(const SeasonRuleTable& row) {
    return { row.pve_win_points() };
}
inline SeasonRulePvp_win_pointsComp MakeSeasonRulePvp_win_pointsComp(const SeasonRuleTable& row) {
    return { row.pvp_win_points() };
}
inline SeasonRulePvp_other_pointsComp MakeSeasonRulePvp_other_pointsComp(const SeasonRuleTable& row) {
    return { row.pvp_other_points() };
}
inline SeasonRuleMax_health_bps_capComp MakeSeasonRuleMax_health_bps_capComp(const SeasonRuleTable& row) {
    return { row.max_health_bps_cap() };
}
inline SeasonRuleDefense_bps_capComp MakeSeasonRuleDefense_bps_capComp(const SeasonRuleTable& row) {
    return { row.defense_bps_cap() };
}
inline SeasonRuleCatchup_currency_typeComp MakeSeasonRuleCatchup_currency_typeComp(const SeasonRuleTable& row) {
    return { row.catchup_currency_type() };
}
inline SeasonRuleCatchup_price_per_pointComp MakeSeasonRuleCatchup_price_per_pointComp(const SeasonRuleTable& row) {
    return { row.catchup_price_per_point() };
}
inline SeasonRuleDefault_seal_idComp MakeSeasonRuleDefault_seal_idComp(const SeasonRuleTable& row) {
    return { row.default_seal_id() };
}
inline SeasonRuleDescComp MakeSeasonRuleDescComp(const SeasonRuleTable& row) {
    return { std::string_view(row.desc()) };
}
inline SeasonRuleHonor_title_nameComp MakeSeasonRuleHonor_title_nameComp(const SeasonRuleTable& row) {
    return { std::string_view(row.honor_title_name()) };
}
inline SeasonRuleHonor_badge_icon_keyComp MakeSeasonRuleHonor_badge_icon_keyComp(const SeasonRuleTable& row) {
    return { std::string_view(row.honor_badge_icon_key()) };
}
inline SeasonRuleCycle_orderComp MakeSeasonRuleCycle_orderComp(const SeasonRuleTable& row) {
    return { row.cycle_order() };
}
inline SeasonRuleCycle_duration_daysComp MakeSeasonRuleCycle_duration_daysComp(const SeasonRuleTable& row) {
    return { row.cycle_duration_days() };
}
