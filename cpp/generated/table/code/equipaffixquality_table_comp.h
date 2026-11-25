
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/equipaffixquality_table.pb.h"

// ============================================================
// Per-column ECS components for EquipAffixQualityTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct EquipAffixQualityIdComp {
    uint32_t value;
};

struct EquipAffixQualityNameComp {
    std::string_view value;
};

struct EquipAffixQualityRatio_pctComp {
    uint32_t value;
};

struct EquipAffixQualityApprentice_weightComp {
    uint32_t value;
};

struct EquipAffixQualityArtisan_weightComp {
    uint32_t value;
};

struct EquipAffixQualityMaster_weightComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline EquipAffixQualityIdComp MakeEquipAffixQualityIdComp(const EquipAffixQualityTable& row) {
    return { row.id() };
}
inline EquipAffixQualityNameComp MakeEquipAffixQualityNameComp(const EquipAffixQualityTable& row) {
    return { std::string_view(row.name()) };
}
inline EquipAffixQualityRatio_pctComp MakeEquipAffixQualityRatio_pctComp(const EquipAffixQualityTable& row) {
    return { row.ratio_pct() };
}
inline EquipAffixQualityApprentice_weightComp MakeEquipAffixQualityApprentice_weightComp(const EquipAffixQualityTable& row) {
    return { row.apprentice_weight() };
}
inline EquipAffixQualityArtisan_weightComp MakeEquipAffixQualityArtisan_weightComp(const EquipAffixQualityTable& row) {
    return { row.artisan_weight() };
}
inline EquipAffixQualityMaster_weightComp MakeEquipAffixQualityMaster_weightComp(const EquipAffixQualityTable& row) {
    return { row.master_weight() };
}
