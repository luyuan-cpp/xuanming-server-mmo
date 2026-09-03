
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/attributeautoplan_table.pb.h"

// ============================================================
// Per-column ECS components for AttributeAutoPlanTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct AttributeAutoPlanIdComp {
    uint32_t value;
};

struct AttributeAutoPlanClass_idComp {
    uint32_t value;
};

struct AttributeAutoPlanPool_idComp {
    uint32_t value;
};

struct AttributeAutoPlanDescComp {
    std::string_view value;
};

struct AttributeAutoPlanDimensionComp {
    std::span<const uint32_t> values;
};

struct AttributeAutoPlanWeightComp {
    std::span<const uint32_t> values;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline AttributeAutoPlanIdComp MakeAttributeAutoPlanIdComp(const AttributeAutoPlanTable& row) {
    return { row.id() };
}
inline AttributeAutoPlanClass_idComp MakeAttributeAutoPlanClass_idComp(const AttributeAutoPlanTable& row) {
    return { row.class_id() };
}
inline AttributeAutoPlanPool_idComp MakeAttributeAutoPlanPool_idComp(const AttributeAutoPlanTable& row) {
    return { row.pool_id() };
}
inline AttributeAutoPlanDescComp MakeAttributeAutoPlanDescComp(const AttributeAutoPlanTable& row) {
    return { std::string_view(row.desc()) };
}
inline AttributeAutoPlanDimensionComp MakeAttributeAutoPlanDimensionComp(const AttributeAutoPlanTable& row) {
    const auto& rf = row.dimension();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline AttributeAutoPlanWeightComp MakeAttributeAutoPlanWeightComp(const AttributeAutoPlanTable& row) {
    const auto& rf = row.weight();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
