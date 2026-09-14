
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/attributeallocratio_table.pb.h"

// ============================================================
// Per-column ECS components for AttributeAllocRatioTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct AttributeAllocRatioIdComp {
    uint32_t value;
};

struct AttributeAllocRatioDimension_idComp {
    uint32_t value;
};

struct AttributeAllocRatioClass_idComp {
    uint32_t value;
};

struct AttributeAllocRatioMax_healthComp {
    double value;
};

struct AttributeAllocRatioMax_manaComp {
    double value;
};

struct AttributeAllocRatioPhysical_attackComp {
    double value;
};

struct AttributeAllocRatioMagic_attackComp {
    double value;
};

struct AttributeAllocRatioSpeedComp {
    double value;
};

struct AttributeAllocRatioDefenseComp {
    double value;
};

struct AttributeAllocRatioDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline AttributeAllocRatioIdComp MakeAttributeAllocRatioIdComp(const AttributeAllocRatioTable& row) {
    return { row.id() };
}
inline AttributeAllocRatioDimension_idComp MakeAttributeAllocRatioDimension_idComp(const AttributeAllocRatioTable& row) {
    return { row.dimension_id() };
}
inline AttributeAllocRatioClass_idComp MakeAttributeAllocRatioClass_idComp(const AttributeAllocRatioTable& row) {
    return { row.class_id() };
}
inline AttributeAllocRatioMax_healthComp MakeAttributeAllocRatioMax_healthComp(const AttributeAllocRatioTable& row) {
    return { row.max_health() };
}
inline AttributeAllocRatioMax_manaComp MakeAttributeAllocRatioMax_manaComp(const AttributeAllocRatioTable& row) {
    return { row.max_mana() };
}
inline AttributeAllocRatioPhysical_attackComp MakeAttributeAllocRatioPhysical_attackComp(const AttributeAllocRatioTable& row) {
    return { row.physical_attack() };
}
inline AttributeAllocRatioMagic_attackComp MakeAttributeAllocRatioMagic_attackComp(const AttributeAllocRatioTable& row) {
    return { row.magic_attack() };
}
inline AttributeAllocRatioSpeedComp MakeAttributeAllocRatioSpeedComp(const AttributeAllocRatioTable& row) {
    return { row.speed() };
}
inline AttributeAllocRatioDefenseComp MakeAttributeAllocRatioDefenseComp(const AttributeAllocRatioTable& row) {
    return { row.defense() };
}
inline AttributeAllocRatioDescComp MakeAttributeAllocRatioDescComp(const AttributeAllocRatioTable& row) {
    return { std::string_view(row.desc()) };
}
