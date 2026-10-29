
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/activityvitalityreward_table.pb.h"

// ============================================================
// Per-column ECS components for ActivityVitalityRewardTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct ActivityVitalityRewardIdComp {
    uint32_t value;
};

struct ActivityVitalityRewardRule_idComp {
    uint32_t value;
};

struct ActivityVitalityRewardRequired_pointsComp {
    uint32_t value;
};

struct ActivityVitalityRewardReward_idComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline ActivityVitalityRewardIdComp MakeActivityVitalityRewardIdComp(const ActivityVitalityRewardTable& row) {
    return { row.id() };
}
inline ActivityVitalityRewardRule_idComp MakeActivityVitalityRewardRule_idComp(const ActivityVitalityRewardTable& row) {
    return { row.rule_id() };
}
inline ActivityVitalityRewardRequired_pointsComp MakeActivityVitalityRewardRequired_pointsComp(const ActivityVitalityRewardTable& row) {
    return { row.required_points() };
}
inline ActivityVitalityRewardReward_idComp MakeActivityVitalityRewardReward_idComp(const ActivityVitalityRewardTable& row) {
    return { row.reward_id() };
}
