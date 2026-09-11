
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
