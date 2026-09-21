
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/guilddonate_table.pb.h"

// ============================================================
// Per-column ECS components for GuildDonateTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct GuildDonateIdComp {
    uint32_t value;
};

struct GuildDonateNameComp {
    std::string_view value;
};

struct GuildDonateCurrency_typeComp {
    uint32_t value;
};

struct GuildDonateCost_amountComp {
    uint64_t value;
};

struct GuildDonateContribution_gainComp {
    uint64_t value;
};

struct GuildDonateFunds_gainComp {
    uint64_t value;
};

struct GuildDonateDaily_limitComp {
    uint32_t value;
};

struct GuildDonateMin_guild_levelComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline GuildDonateIdComp MakeGuildDonateIdComp(const GuildDonateTable& row) {
    return { row.id() };
}
inline GuildDonateNameComp MakeGuildDonateNameComp(const GuildDonateTable& row) {
    return { std::string_view(row.name()) };
}
inline GuildDonateCurrency_typeComp MakeGuildDonateCurrency_typeComp(const GuildDonateTable& row) {
    return { row.currency_type() };
}
inline GuildDonateCost_amountComp MakeGuildDonateCost_amountComp(const GuildDonateTable& row) {
    return { row.cost_amount() };
}
inline GuildDonateContribution_gainComp MakeGuildDonateContribution_gainComp(const GuildDonateTable& row) {
    return { row.contribution_gain() };
}
inline GuildDonateFunds_gainComp MakeGuildDonateFunds_gainComp(const GuildDonateTable& row) {
    return { row.funds_gain() };
}
inline GuildDonateDaily_limitComp MakeGuildDonateDaily_limitComp(const GuildDonateTable& row) {
    return { row.daily_limit() };
}
inline GuildDonateMin_guild_levelComp MakeGuildDonateMin_guild_levelComp(const GuildDonateTable& row) {
    return { row.min_guild_level() };
}
