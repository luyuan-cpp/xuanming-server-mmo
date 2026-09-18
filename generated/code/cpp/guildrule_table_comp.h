
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/guildrule_table.pb.h"

// ============================================================
// Per-column ECS components for GuildRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct GuildRuleIdComp {
    uint32_t value;
};

struct GuildRuleApplication_expire_hoursComp {
    uint32_t value;
};

struct GuildRuleMax_pending_applications_per_playerComp {
    uint32_t value;
};

struct GuildRuleMax_pending_applications_per_guildComp {
    uint32_t value;
};

struct GuildRuleAsset_op_deadline_secondsComp {
    uint32_t value;
};

struct GuildRuleAsset_op_retry_base_msComp {
    uint32_t value;
};

struct GuildRuleReunion_min_online_membersComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline GuildRuleIdComp MakeGuildRuleIdComp(const GuildRuleTable& row) {
    return { row.id() };
}
inline GuildRuleApplication_expire_hoursComp MakeGuildRuleApplication_expire_hoursComp(const GuildRuleTable& row) {
    return { row.application_expire_hours() };
}
inline GuildRuleMax_pending_applications_per_playerComp MakeGuildRuleMax_pending_applications_per_playerComp(const GuildRuleTable& row) {
    return { row.max_pending_applications_per_player() };
}
inline GuildRuleMax_pending_applications_per_guildComp MakeGuildRuleMax_pending_applications_per_guildComp(const GuildRuleTable& row) {
    return { row.max_pending_applications_per_guild() };
}
inline GuildRuleAsset_op_deadline_secondsComp MakeGuildRuleAsset_op_deadline_secondsComp(const GuildRuleTable& row) {
    return { row.asset_op_deadline_seconds() };
}
inline GuildRuleAsset_op_retry_base_msComp MakeGuildRuleAsset_op_retry_base_msComp(const GuildRuleTable& row) {
    return { row.asset_op_retry_base_ms() };
}
inline GuildRuleReunion_min_online_membersComp MakeGuildRuleReunion_min_online_membersComp(const GuildRuleTable& row) {
    return { row.reunion_min_online_members() };
}
