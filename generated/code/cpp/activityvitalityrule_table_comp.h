
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/activityvitalityrule_table.pb.h"

// ============================================================
// Per-column ECS components for ActivityVitalityRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct ActivityVitalityRuleIdComp {
    uint32_t value;
};

struct ActivityVitalityRuleEnabledComp {
    bool value;
};

struct ActivityVitalityRuleConfiguration_revisionComp {
    uint64_t value;
};

struct ActivityVitalityRuleFirst_period_starts_at_msComp {
    uint64_t value;
};

struct ActivityVitalityRulePeriod_duration_msComp {
    uint64_t value;
};

struct ActivityVitalityRulePoints_limitComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline ActivityVitalityRuleIdComp MakeActivityVitalityRuleIdComp(const ActivityVitalityRuleTable& row) {
    return { row.id() };
}
inline ActivityVitalityRuleEnabledComp MakeActivityVitalityRuleEnabledComp(const ActivityVitalityRuleTable& row) {
    return { row.enabled() };
}
inline ActivityVitalityRuleConfiguration_revisionComp MakeActivityVitalityRuleConfiguration_revisionComp(const ActivityVitalityRuleTable& row) {
    return { row.configuration_revision() };
}
inline ActivityVitalityRuleFirst_period_starts_at_msComp MakeActivityVitalityRuleFirst_period_starts_at_msComp(const ActivityVitalityRuleTable& row) {
    return { row.first_period_starts_at_ms() };
}
inline ActivityVitalityRulePeriod_duration_msComp MakeActivityVitalityRulePeriod_duration_msComp(const ActivityVitalityRuleTable& row) {
    return { row.period_duration_ms() };
}
inline ActivityVitalityRulePoints_limitComp MakeActivityVitalityRulePoints_limitComp(const ActivityVitalityRuleTable& row) {
    return { row.points_limit() };
}
