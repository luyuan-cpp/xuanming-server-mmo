
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/guildlevel_table.pb.h"

// ============================================================
// Per-column ECS components for GuildLevelTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct GuildLevelIdComp {
    uint32_t value;
};

struct GuildLevelMax_membersComp {
    uint32_t value;
};

struct GuildLevelMax_officersComp {
    uint32_t value;
};

struct GuildLevelUpgrade_cost_fundsComp {
    uint64_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline GuildLevelIdComp MakeGuildLevelIdComp(const GuildLevelTable& row) {
    return { row.id() };
}
inline GuildLevelMax_membersComp MakeGuildLevelMax_membersComp(const GuildLevelTable& row) {
    return { row.max_members() };
}
inline GuildLevelMax_officersComp MakeGuildLevelMax_officersComp(const GuildLevelTable& row) {
    return { row.max_officers() };
}
inline GuildLevelUpgrade_cost_fundsComp MakeGuildLevelUpgrade_cost_fundsComp(const GuildLevelTable& row) {
    return { row.upgrade_cost_funds() };
}
