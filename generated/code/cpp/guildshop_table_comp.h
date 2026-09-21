
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/guildshop_table.pb.h"

// ============================================================
// Per-column ECS components for GuildShopTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct GuildShopIdComp {
    uint32_t value;
};

struct GuildShopNameComp {
    std::string_view value;
};

struct GuildShopCategoryComp {
    uint32_t value;
};

struct GuildShopItem_idComp {
    uint32_t value;
};

struct GuildShopItem_countComp {
    uint32_t value;
};

struct GuildShopCost_contributionComp {
    uint64_t value;
};

struct GuildShopRequired_guild_levelComp {
    uint32_t value;
};

struct GuildShopLimit_periodComp {
    uint32_t value;
};

struct GuildShopLimit_countComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline GuildShopIdComp MakeGuildShopIdComp(const GuildShopTable& row) {
    return { row.id() };
}
inline GuildShopNameComp MakeGuildShopNameComp(const GuildShopTable& row) {
    return { std::string_view(row.name()) };
}
inline GuildShopCategoryComp MakeGuildShopCategoryComp(const GuildShopTable& row) {
    return { row.category() };
}
inline GuildShopItem_idComp MakeGuildShopItem_idComp(const GuildShopTable& row) {
    return { row.item_id() };
}
inline GuildShopItem_countComp MakeGuildShopItem_countComp(const GuildShopTable& row) {
    return { row.item_count() };
}
inline GuildShopCost_contributionComp MakeGuildShopCost_contributionComp(const GuildShopTable& row) {
    return { row.cost_contribution() };
}
inline GuildShopRequired_guild_levelComp MakeGuildShopRequired_guild_levelComp(const GuildShopTable& row) {
    return { row.required_guild_level() };
}
inline GuildShopLimit_periodComp MakeGuildShopLimit_periodComp(const GuildShopTable& row) {
    return { row.limit_period() };
}
inline GuildShopLimit_countComp MakeGuildShopLimit_countComp(const GuildShopTable& row) {
    return { row.limit_count() };
}
