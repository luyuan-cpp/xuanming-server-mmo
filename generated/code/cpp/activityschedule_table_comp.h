
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/activityschedule_table.pb.h"

// ============================================================
// Per-column ECS components for ActivityScheduleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct ActivityScheduleIdComp {
    uint32_t value;
};

struct ActivityScheduleEnabledComp {
    bool value;
};

struct ActivityScheduleBaseline_start_at_msComp {
    uint64_t value;
};

struct ActivityScheduleBaseline_end_at_msComp {
    uint64_t value;
};

struct ActivityScheduleDefinition_keyComp {
    std::string_view value;
};

struct ActivityScheduleConfiguration_revisionComp {
    uint64_t value;
};

struct ActivityScheduleCategoryComp {
    uint32_t value;
};

struct ActivityScheduleNameComp {
    std::string_view value;
};

struct ActivityScheduleDescriptionComp {
    std::string_view value;
};

struct ActivityScheduleIcon_keyComp {
    std::string_view value;
};

struct ActivityScheduleVitality_pointsComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline ActivityScheduleIdComp MakeActivityScheduleIdComp(const ActivityScheduleTable& row) {
    return { row.id() };
}
inline ActivityScheduleEnabledComp MakeActivityScheduleEnabledComp(const ActivityScheduleTable& row) {
    return { row.enabled() };
}
inline ActivityScheduleBaseline_start_at_msComp MakeActivityScheduleBaseline_start_at_msComp(const ActivityScheduleTable& row) {
    return { row.baseline_start_at_ms() };
}
inline ActivityScheduleBaseline_end_at_msComp MakeActivityScheduleBaseline_end_at_msComp(const ActivityScheduleTable& row) {
    return { row.baseline_end_at_ms() };
}
inline ActivityScheduleDefinition_keyComp MakeActivityScheduleDefinition_keyComp(const ActivityScheduleTable& row) {
    return { std::string_view(row.definition_key()) };
}
inline ActivityScheduleConfiguration_revisionComp MakeActivityScheduleConfiguration_revisionComp(const ActivityScheduleTable& row) {
    return { row.configuration_revision() };
}
inline ActivityScheduleCategoryComp MakeActivityScheduleCategoryComp(const ActivityScheduleTable& row) {
    return { row.category() };
}
inline ActivityScheduleNameComp MakeActivityScheduleNameComp(const ActivityScheduleTable& row) {
    return { std::string_view(row.name()) };
}
inline ActivityScheduleDescriptionComp MakeActivityScheduleDescriptionComp(const ActivityScheduleTable& row) {
    return { std::string_view(row.description()) };
}
inline ActivityScheduleIcon_keyComp MakeActivityScheduleIcon_keyComp(const ActivityScheduleTable& row) {
    return { std::string_view(row.icon_key()) };
}
inline ActivityScheduleVitality_pointsComp MakeActivityScheduleVitality_pointsComp(const ActivityScheduleTable& row) {
    return { row.vitality_points() };
}
