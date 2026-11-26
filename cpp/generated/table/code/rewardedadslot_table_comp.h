
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/rewardedadslot_table.pb.h"

// ============================================================
// Per-column ECS components for RewardedAdSlotTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct RewardedAdSlotIdComp {
    uint32_t value;
};

struct RewardedAdSlotEnabledComp {
    uint32_t value;
};

struct RewardedAdSlotMin_levelComp {
    uint32_t value;
};

struct RewardedAdSlotDaily_limitComp {
    uint32_t value;
};

struct RewardedAdSlotCooldown_secComp {
    uint32_t value;
};

struct RewardedAdSlotMin_watch_secComp {
    uint32_t value;
};

struct RewardedAdSlotSession_ttl_secComp {
    uint32_t value;
};

struct RewardedAdSlotCurrency_typeComp {
    uint32_t value;
};

struct RewardedAdSlotReward_amountComp {
    uint64_t value;
};

struct RewardedAdSlotRequire_ssvComp {
    uint32_t value;
};

struct RewardedAdSlotPlatform_maskComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline RewardedAdSlotIdComp MakeRewardedAdSlotIdComp(const RewardedAdSlotTable& row) {
    return { row.id() };
}
inline RewardedAdSlotEnabledComp MakeRewardedAdSlotEnabledComp(const RewardedAdSlotTable& row) {
    return { row.enabled() };
}
inline RewardedAdSlotMin_levelComp MakeRewardedAdSlotMin_levelComp(const RewardedAdSlotTable& row) {
    return { row.min_level() };
}
inline RewardedAdSlotDaily_limitComp MakeRewardedAdSlotDaily_limitComp(const RewardedAdSlotTable& row) {
    return { row.daily_limit() };
}
inline RewardedAdSlotCooldown_secComp MakeRewardedAdSlotCooldown_secComp(const RewardedAdSlotTable& row) {
    return { row.cooldown_sec() };
}
inline RewardedAdSlotMin_watch_secComp MakeRewardedAdSlotMin_watch_secComp(const RewardedAdSlotTable& row) {
    return { row.min_watch_sec() };
}
inline RewardedAdSlotSession_ttl_secComp MakeRewardedAdSlotSession_ttl_secComp(const RewardedAdSlotTable& row) {
    return { row.session_ttl_sec() };
}
inline RewardedAdSlotCurrency_typeComp MakeRewardedAdSlotCurrency_typeComp(const RewardedAdSlotTable& row) {
    return { row.currency_type() };
}
inline RewardedAdSlotReward_amountComp MakeRewardedAdSlotReward_amountComp(const RewardedAdSlotTable& row) {
    return { row.reward_amount() };
}
inline RewardedAdSlotRequire_ssvComp MakeRewardedAdSlotRequire_ssvComp(const RewardedAdSlotTable& row) {
    return { row.require_ssv() };
}
inline RewardedAdSlotPlatform_maskComp MakeRewardedAdSlotPlatform_maskComp(const RewardedAdSlotTable& row) {
    return { row.platform_mask() };
}
