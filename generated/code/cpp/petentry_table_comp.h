
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/petentry_table.pb.h"

// ============================================================
// Per-column ECS components for PetEntryTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct PetEntryIdComp {
    uint32_t value;
};

struct PetEntryNameComp {
    std::string_view value;
};

struct PetEntryDescComp {
    std::string_view value;
};

struct PetEntryCategoryComp {
    uint32_t value;
};

struct PetEntryChannelComp {
    uint32_t value;
};

struct PetEntryState_kindComp {
    uint32_t value;
};

struct PetEntryCapComp {
    uint32_t value;
};

struct PetEntryPer_point_bpsComp {
    uint32_t value;
};

struct PetEntrySortComp {
    uint32_t value;
};

struct PetEntryIconComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline PetEntryIdComp MakePetEntryIdComp(const PetEntryTable& row) {
    return { row.id() };
}
inline PetEntryNameComp MakePetEntryNameComp(const PetEntryTable& row) {
    return { std::string_view(row.name()) };
}
inline PetEntryDescComp MakePetEntryDescComp(const PetEntryTable& row) {
    return { std::string_view(row.desc()) };
}
inline PetEntryCategoryComp MakePetEntryCategoryComp(const PetEntryTable& row) {
    return { row.category() };
}
inline PetEntryChannelComp MakePetEntryChannelComp(const PetEntryTable& row) {
    return { row.channel() };
}
inline PetEntryState_kindComp MakePetEntryState_kindComp(const PetEntryTable& row) {
    return { row.state_kind() };
}
inline PetEntryCapComp MakePetEntryCapComp(const PetEntryTable& row) {
    return { row.cap() };
}
inline PetEntryPer_point_bpsComp MakePetEntryPer_point_bpsComp(const PetEntryTable& row) {
    return { row.per_point_bps() };
}
inline PetEntrySortComp MakePetEntrySortComp(const PetEntryTable& row) {
    return { row.sort() };
}
inline PetEntryIconComp MakePetEntryIconComp(const PetEntryTable& row) {
    return { std::string_view(row.icon()) };
}
