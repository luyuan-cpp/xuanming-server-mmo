
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/guildactivity_table.pb.h"

// ============================================================
// Per-column ECS components for GuildActivityTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct GuildActivityIdComp {
    uint32_t value;
};

struct GuildActivityNameComp {
    std::string_view value;
};

struct GuildActivityTypeComp {
    uint32_t value;
};

struct GuildActivityEnabledComp {
    bool value;
};

struct GuildActivityStart_at_msComp {
    uint64_t value;
};

struct GuildActivityEnd_at_msComp {
    uint64_t value;
};

struct GuildActivityMin_guild_levelComp {
    uint32_t value;
};

struct GuildActivityPersonal_contributionComp {
    uint64_t value;
};

struct GuildActivityGuild_fundsComp {
    uint64_t value;
};

struct GuildActivityGuild_thresholdComp {
    uint32_t value;
};

struct GuildActivityReward_idComp {
    uint32_t value;
};

struct GuildActivityDungeon_idComp {
    uint32_t value;
};

struct GuildActivityTeam_size_minComp {
    uint32_t value;
};

struct GuildActivityTeam_size_maxComp {
    uint32_t value;
};

struct GuildActivityDaily_limitComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline GuildActivityIdComp MakeGuildActivityIdComp(const GuildActivityTable& row) {
    return { row.id() };
}
inline GuildActivityNameComp MakeGuildActivityNameComp(const GuildActivityTable& row) {
    return { std::string_view(row.name()) };
}
inline GuildActivityTypeComp MakeGuildActivityTypeComp(const GuildActivityTable& row) {
    return { row.type() };
}
inline GuildActivityEnabledComp MakeGuildActivityEnabledComp(const GuildActivityTable& row) {
    return { row.enabled() };
}
inline GuildActivityStart_at_msComp MakeGuildActivityStart_at_msComp(const GuildActivityTable& row) {
    return { row.start_at_ms() };
}
inline GuildActivityEnd_at_msComp MakeGuildActivityEnd_at_msComp(const GuildActivityTable& row) {
    return { row.end_at_ms() };
}
inline GuildActivityMin_guild_levelComp MakeGuildActivityMin_guild_levelComp(const GuildActivityTable& row) {
    return { row.min_guild_level() };
}
inline GuildActivityPersonal_contributionComp MakeGuildActivityPersonal_contributionComp(const GuildActivityTable& row) {
    return { row.personal_contribution() };
}
inline GuildActivityGuild_fundsComp MakeGuildActivityGuild_fundsComp(const GuildActivityTable& row) {
    return { row.guild_funds() };
}
inline GuildActivityGuild_thresholdComp MakeGuildActivityGuild_thresholdComp(const GuildActivityTable& row) {
    return { row.guild_threshold() };
}
inline GuildActivityReward_idComp MakeGuildActivityReward_idComp(const GuildActivityTable& row) {
    return { row.reward_id() };
}
inline GuildActivityDungeon_idComp MakeGuildActivityDungeon_idComp(const GuildActivityTable& row) {
    return { row.dungeon_id() };
}
inline GuildActivityTeam_size_minComp MakeGuildActivityTeam_size_minComp(const GuildActivityTable& row) {
    return { row.team_size_min() };
}
inline GuildActivityTeam_size_maxComp MakeGuildActivityTeam_size_maxComp(const GuildActivityTable& row) {
    return { row.team_size_max() };
}
inline GuildActivityDaily_limitComp MakeGuildActivityDaily_limitComp(const GuildActivityTable& row) {
    return { row.daily_limit() };
}
