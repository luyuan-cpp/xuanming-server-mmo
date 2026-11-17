
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/equipv1affix_table.pb.h"

// ============================================================
// Per-column ECS components for EquipV1AffixTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct EquipV1AffixIdComp {
    uint32_t value;
};

struct EquipV1AffixPoolComp {
    uint32_t value;
};

struct EquipV1AffixEffectComp {
    uint32_t value;
};

struct EquipV1AffixEffect_paramComp {
    uint32_t value;
};

struct EquipV1AffixValue_permyriadComp {
    uint32_t value;
};

struct EquipV1AffixSource_capComp {
    uint32_t value;
};

struct EquipV1AffixNameComp {
    std::string_view value;
};

struct EquipV1AffixDescriptionComp {
    std::string_view value;
};

struct EquipV1AffixMaterial_idComp {
    uint32_t value;
};

struct EquipV1AffixMaterial_countComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline EquipV1AffixIdComp MakeEquipV1AffixIdComp(const EquipV1AffixTable& row) {
    return { row.id() };
}
inline EquipV1AffixPoolComp MakeEquipV1AffixPoolComp(const EquipV1AffixTable& row) {
    return { row.pool() };
}
inline EquipV1AffixEffectComp MakeEquipV1AffixEffectComp(const EquipV1AffixTable& row) {
    return { row.effect() };
}
inline EquipV1AffixEffect_paramComp MakeEquipV1AffixEffect_paramComp(const EquipV1AffixTable& row) {
    return { row.effect_param() };
}
inline EquipV1AffixValue_permyriadComp MakeEquipV1AffixValue_permyriadComp(const EquipV1AffixTable& row) {
    return { row.value_permyriad() };
}
inline EquipV1AffixSource_capComp MakeEquipV1AffixSource_capComp(const EquipV1AffixTable& row) {
    return { row.source_cap() };
}
inline EquipV1AffixNameComp MakeEquipV1AffixNameComp(const EquipV1AffixTable& row) {
    return { std::string_view(row.name()) };
}
inline EquipV1AffixDescriptionComp MakeEquipV1AffixDescriptionComp(const EquipV1AffixTable& row) {
    return { std::string_view(row.description()) };
}
inline EquipV1AffixMaterial_idComp MakeEquipV1AffixMaterial_idComp(const EquipV1AffixTable& row) {
    return { row.material_id() };
}
inline EquipV1AffixMaterial_countComp MakeEquipV1AffixMaterial_countComp(const EquipV1AffixTable& row) {
    return { row.material_count() };
}
