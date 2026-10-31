
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/zonegroup_table.pb.h"

// ============================================================
// Per-column ECS components for ZoneGroupTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct ZoneGroupIdComp {
    uint32_t value;
};

struct ZoneGroupGroup_idComp {
    uint32_t value;
};

struct ZoneGroupZone_beginComp {
    uint32_t value;
};

struct ZoneGroupZone_endComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline ZoneGroupIdComp MakeZoneGroupIdComp(const ZoneGroupTable& row) {
    return { row.id() };
}
inline ZoneGroupGroup_idComp MakeZoneGroupGroup_idComp(const ZoneGroupTable& row) {
    return { row.group_id() };
}
inline ZoneGroupZone_beginComp MakeZoneGroupZone_beginComp(const ZoneGroupTable& row) {
    return { row.zone_begin() };
}
inline ZoneGroupZone_endComp MakeZoneGroupZone_endComp(const ZoneGroupTable& row) {
    return { row.zone_end() };
}
