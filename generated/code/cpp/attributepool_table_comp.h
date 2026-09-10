
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/attributepool_table.pb.h"

// ============================================================
// Per-column ECS components for AttributePoolTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct AttributePoolIdComp {
    uint32_t value;
};

struct AttributePoolNameComp {
    std::string_view value;
};

struct AttributePoolUnlock_levelComp {
    uint32_t value;
};

struct AttributePoolPoints_per_levelComp {
    uint32_t value;
};

struct AttributePoolBase_pointsComp {
    uint32_t value;
};

struct AttributePoolDimension_capComp {
    uint32_t value;
};

struct AttributePoolReset_cost_goldComp {
    uint64_t value;
};

struct AttributePoolReset_free_below_levelComp {
    uint32_t value;
};

struct AttributePoolDescComp {
    std::string_view value;
};

struct AttributePoolOwner_typeComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline AttributePoolIdComp MakeAttributePoolIdComp(const AttributePoolTable& row) {
    return { row.id() };
}
inline AttributePoolNameComp MakeAttributePoolNameComp(const AttributePoolTable& row) {
    return { std::string_view(row.name()) };
}
inline AttributePoolUnlock_levelComp MakeAttributePoolUnlock_levelComp(const AttributePoolTable& row) {
    return { row.unlock_level() };
}
inline AttributePoolPoints_per_levelComp MakeAttributePoolPoints_per_levelComp(const AttributePoolTable& row) {
    return { row.points_per_level() };
}
inline AttributePoolBase_pointsComp MakeAttributePoolBase_pointsComp(const AttributePoolTable& row) {
    return { row.base_points() };
}
inline AttributePoolDimension_capComp MakeAttributePoolDimension_capComp(const AttributePoolTable& row) {
    return { row.dimension_cap() };
}
inline AttributePoolReset_cost_goldComp MakeAttributePoolReset_cost_goldComp(const AttributePoolTable& row) {
    return { row.reset_cost_gold() };
}
inline AttributePoolReset_free_below_levelComp MakeAttributePoolReset_free_below_levelComp(const AttributePoolTable& row) {
    return { row.reset_free_below_level() };
}
inline AttributePoolDescComp MakeAttributePoolDescComp(const AttributePoolTable& row) {
    return { std::string_view(row.desc()) };
}
inline AttributePoolOwner_typeComp MakeAttributePoolOwner_typeComp(const AttributePoolTable& row) {
    return { row.owner_type() };
}
