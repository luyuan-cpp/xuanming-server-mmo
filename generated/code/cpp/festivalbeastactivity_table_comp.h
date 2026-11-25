
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/festivalbeastactivity_table.pb.h"

// ============================================================
// Per-column ECS components for FestivalBeastActivityTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct FestivalBeastActivityIdComp {
    uint32_t value;
};

struct FestivalBeastActivityEnabledComp {
    bool value;
};

struct FestivalBeastActivityConfig_revisionComp {
    uint32_t value;
};

struct FestivalBeastActivityActivity_nameComp {
    std::string_view value;
};

struct FestivalBeastActivityBoss_nameComp {
    std::string_view value;
};

struct FestivalBeastActivityBanner_asset_keyComp {
    std::string_view value;
};

struct FestivalBeastActivityIcon_keyComp {
    std::string_view value;
};

struct FestivalBeastActivityEffective_start_at_msComp {
    uint64_t value;
};

struct FestivalBeastActivityEffective_end_at_msComp {
    uint64_t value;
};

struct FestivalBeastActivityClaim_showcase_daysComp {
    uint32_t value;
};

struct FestivalBeastActivityBattle_config_idComp {
    uint32_t value;
};

struct FestivalBeastActivityRequired_winsComp {
    uint32_t value;
};

struct FestivalBeastActivityReward_identityComp {
    std::string_view value;
};

struct FestivalBeastActivityReward_definition_versionComp {
    uint32_t value;
};

struct FestivalBeastActivityReward_pet_table_idComp {
    uint32_t value;
};

struct FestivalBeastActivityRound_limitComp {
    uint32_t value;
};

struct FestivalBeastActivityCycle_roundsComp {
    uint32_t value;
};

struct FestivalBeastActivityFirst_heavy_roundComp {
    uint32_t value;
};

struct FestivalBeastActivityTelegraph_lead_roundsComp {
    uint32_t value;
};

struct FestivalBeastActivityHeavy_damage_percentComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline FestivalBeastActivityIdComp MakeFestivalBeastActivityIdComp(const FestivalBeastActivityTable& row) {
    return { row.id() };
}
inline FestivalBeastActivityEnabledComp MakeFestivalBeastActivityEnabledComp(const FestivalBeastActivityTable& row) {
    return { row.enabled() };
}
inline FestivalBeastActivityConfig_revisionComp MakeFestivalBeastActivityConfig_revisionComp(const FestivalBeastActivityTable& row) {
    return { row.config_revision() };
}
inline FestivalBeastActivityActivity_nameComp MakeFestivalBeastActivityActivity_nameComp(const FestivalBeastActivityTable& row) {
    return { std::string_view(row.activity_name()) };
}
inline FestivalBeastActivityBoss_nameComp MakeFestivalBeastActivityBoss_nameComp(const FestivalBeastActivityTable& row) {
    return { std::string_view(row.boss_name()) };
}
inline FestivalBeastActivityBanner_asset_keyComp MakeFestivalBeastActivityBanner_asset_keyComp(const FestivalBeastActivityTable& row) {
    return { std::string_view(row.banner_asset_key()) };
}
inline FestivalBeastActivityIcon_keyComp MakeFestivalBeastActivityIcon_keyComp(const FestivalBeastActivityTable& row) {
    return { std::string_view(row.icon_key()) };
}
inline FestivalBeastActivityEffective_start_at_msComp MakeFestivalBeastActivityEffective_start_at_msComp(const FestivalBeastActivityTable& row) {
    return { row.effective_start_at_ms() };
}
inline FestivalBeastActivityEffective_end_at_msComp MakeFestivalBeastActivityEffective_end_at_msComp(const FestivalBeastActivityTable& row) {
    return { row.effective_end_at_ms() };
}
inline FestivalBeastActivityClaim_showcase_daysComp MakeFestivalBeastActivityClaim_showcase_daysComp(const FestivalBeastActivityTable& row) {
    return { row.claim_showcase_days() };
}
inline FestivalBeastActivityBattle_config_idComp MakeFestivalBeastActivityBattle_config_idComp(const FestivalBeastActivityTable& row) {
    return { row.battle_config_id() };
}
inline FestivalBeastActivityRequired_winsComp MakeFestivalBeastActivityRequired_winsComp(const FestivalBeastActivityTable& row) {
    return { row.required_wins() };
}
inline FestivalBeastActivityReward_identityComp MakeFestivalBeastActivityReward_identityComp(const FestivalBeastActivityTable& row) {
    return { std::string_view(row.reward_identity()) };
}
inline FestivalBeastActivityReward_definition_versionComp MakeFestivalBeastActivityReward_definition_versionComp(const FestivalBeastActivityTable& row) {
    return { row.reward_definition_version() };
}
inline FestivalBeastActivityReward_pet_table_idComp MakeFestivalBeastActivityReward_pet_table_idComp(const FestivalBeastActivityTable& row) {
    return { row.reward_pet_table_id() };
}
inline FestivalBeastActivityRound_limitComp MakeFestivalBeastActivityRound_limitComp(const FestivalBeastActivityTable& row) {
    return { row.round_limit() };
}
inline FestivalBeastActivityCycle_roundsComp MakeFestivalBeastActivityCycle_roundsComp(const FestivalBeastActivityTable& row) {
    return { row.cycle_rounds() };
}
inline FestivalBeastActivityFirst_heavy_roundComp MakeFestivalBeastActivityFirst_heavy_roundComp(const FestivalBeastActivityTable& row) {
    return { row.first_heavy_round() };
}
inline FestivalBeastActivityTelegraph_lead_roundsComp MakeFestivalBeastActivityTelegraph_lead_roundsComp(const FestivalBeastActivityTable& row) {
    return { row.telegraph_lead_rounds() };
}
inline FestivalBeastActivityHeavy_damage_percentComp MakeFestivalBeastActivityHeavy_damage_percentComp(const FestivalBeastActivityTable& row) {
    return { row.heavy_damage_percent() };
}
