
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/attributedimension_table.pb.h"

// ============================================================
// Per-column ECS components for AttributeDimensionTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct AttributeDimensionIdComp {
    uint32_t value;
};

struct AttributeDimensionPool_idComp {
    uint32_t value;
};

struct AttributeDimensionNameComp {
    std::string_view value;
};

struct AttributeDimensionDescComp {
    std::string_view value;
};

struct AttributeDimensionSortComp {
    uint32_t value;
};

struct AttributeDimensionBase_per_levelComp {
    uint32_t value;
};

struct AttributeDimensionMax_healthComp {
    double value;
};

struct AttributeDimensionMax_manaComp {
    double value;
};

struct AttributeDimensionPhysical_attackComp {
    double value;
};

struct AttributeDimensionMagic_attackComp {
    double value;
};

struct AttributeDimensionSpeedComp {
    double value;
};

struct AttributeDimensionDefenseComp {
    double value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline AttributeDimensionIdComp MakeAttributeDimensionIdComp(const AttributeDimensionTable& row) {
    return { row.id() };
}
inline AttributeDimensionPool_idComp MakeAttributeDimensionPool_idComp(const AttributeDimensionTable& row) {
    return { row.pool_id() };
}
inline AttributeDimensionNameComp MakeAttributeDimensionNameComp(const AttributeDimensionTable& row) {
    return { std::string_view(row.name()) };
}
inline AttributeDimensionDescComp MakeAttributeDimensionDescComp(const AttributeDimensionTable& row) {
    return { std::string_view(row.desc()) };
}
inline AttributeDimensionSortComp MakeAttributeDimensionSortComp(const AttributeDimensionTable& row) {
    return { row.sort() };
}
inline AttributeDimensionBase_per_levelComp MakeAttributeDimensionBase_per_levelComp(const AttributeDimensionTable& row) {
    return { row.base_per_level() };
}
inline AttributeDimensionMax_healthComp MakeAttributeDimensionMax_healthComp(const AttributeDimensionTable& row) {
    return { row.max_health() };
}
inline AttributeDimensionMax_manaComp MakeAttributeDimensionMax_manaComp(const AttributeDimensionTable& row) {
    return { row.max_mana() };
}
inline AttributeDimensionPhysical_attackComp MakeAttributeDimensionPhysical_attackComp(const AttributeDimensionTable& row) {
    return { row.physical_attack() };
}
inline AttributeDimensionMagic_attackComp MakeAttributeDimensionMagic_attackComp(const AttributeDimensionTable& row) {
    return { row.magic_attack() };
}
inline AttributeDimensionSpeedComp MakeAttributeDimensionSpeedComp(const AttributeDimensionTable& row) {
    return { row.speed() };
}
inline AttributeDimensionDefenseComp MakeAttributeDimensionDefenseComp(const AttributeDimensionTable& row) {
    return { row.defense() };
}
