
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/guildwarmilestone_table.pb.h"

// ============================================================
// Per-column ECS components for GuildWarMilestoneTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct GuildWarMilestoneIdComp {
    uint32_t value;
};

struct GuildWarMilestoneRequired_guild_meritComp {
    uint64_t value;
};

struct GuildWarMilestoneReward_item1_idComp {
    uint32_t value;
};

struct GuildWarMilestoneReward_item1_countComp {
    uint32_t value;
};

struct GuildWarMilestoneReward_item2_idComp {
    uint32_t value;
};

struct GuildWarMilestoneReward_item2_countComp {
    uint32_t value;
};

struct GuildWarMilestoneReward_silverComp {
    uint64_t value;
};

struct GuildWarMilestoneReward_contributionComp {
    uint64_t value;
};

struct GuildWarMilestoneReward_previewComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline GuildWarMilestoneIdComp MakeGuildWarMilestoneIdComp(const GuildWarMilestoneTable& row) {
    return { row.id() };
}
inline GuildWarMilestoneRequired_guild_meritComp MakeGuildWarMilestoneRequired_guild_meritComp(const GuildWarMilestoneTable& row) {
    return { row.required_guild_merit() };
}
inline GuildWarMilestoneReward_item1_idComp MakeGuildWarMilestoneReward_item1_idComp(const GuildWarMilestoneTable& row) {
    return { row.reward_item1_id() };
}
inline GuildWarMilestoneReward_item1_countComp MakeGuildWarMilestoneReward_item1_countComp(const GuildWarMilestoneTable& row) {
    return { row.reward_item1_count() };
}
inline GuildWarMilestoneReward_item2_idComp MakeGuildWarMilestoneReward_item2_idComp(const GuildWarMilestoneTable& row) {
    return { row.reward_item2_id() };
}
inline GuildWarMilestoneReward_item2_countComp MakeGuildWarMilestoneReward_item2_countComp(const GuildWarMilestoneTable& row) {
    return { row.reward_item2_count() };
}
inline GuildWarMilestoneReward_silverComp MakeGuildWarMilestoneReward_silverComp(const GuildWarMilestoneTable& row) {
    return { row.reward_silver() };
}
inline GuildWarMilestoneReward_contributionComp MakeGuildWarMilestoneReward_contributionComp(const GuildWarMilestoneTable& row) {
    return { row.reward_contribution() };
}
inline GuildWarMilestoneReward_previewComp MakeGuildWarMilestoneReward_previewComp(const GuildWarMilestoneTable& row) {
    return { std::string_view(row.reward_preview()) };
}
