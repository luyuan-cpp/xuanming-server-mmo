
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/equipenhance_table.pb.h"

// ============================================================
// Per-column ECS components for EquipEnhanceTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct EquipEnhanceIdComp {
    uint32_t value;
};

struct EquipEnhanceBonus_permyriadComp {
    uint32_t value;
};

struct EquipEnhanceSuccess_rateComp {
    uint32_t value;
};

struct EquipEnhanceFail_level_lossComp {
    uint32_t value;
};

struct EquipEnhanceCost_goldComp {
    uint64_t value;
};

struct EquipEnhanceMaterial_idComp {
    uint32_t value;
};

struct EquipEnhanceMaterial_countComp {
    uint32_t value;
};

struct EquipEnhanceProfile_idComp {
    uint32_t value;
};

struct EquipEnhanceLevelComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline EquipEnhanceIdComp MakeEquipEnhanceIdComp(const EquipEnhanceTable& row) {
    return { row.id() };
}
inline EquipEnhanceBonus_permyriadComp MakeEquipEnhanceBonus_permyriadComp(const EquipEnhanceTable& row) {
    return { row.bonus_permyriad() };
}
inline EquipEnhanceSuccess_rateComp MakeEquipEnhanceSuccess_rateComp(const EquipEnhanceTable& row) {
    return { row.success_rate() };
}
inline EquipEnhanceFail_level_lossComp MakeEquipEnhanceFail_level_lossComp(const EquipEnhanceTable& row) {
    return { row.fail_level_loss() };
}
inline EquipEnhanceCost_goldComp MakeEquipEnhanceCost_goldComp(const EquipEnhanceTable& row) {
    return { row.cost_gold() };
}
inline EquipEnhanceMaterial_idComp MakeEquipEnhanceMaterial_idComp(const EquipEnhanceTable& row) {
    return { row.material_id() };
}
inline EquipEnhanceMaterial_countComp MakeEquipEnhanceMaterial_countComp(const EquipEnhanceTable& row) {
    return { row.material_count() };
}
inline EquipEnhanceProfile_idComp MakeEquipEnhanceProfile_idComp(const EquipEnhanceTable& row) {
    return { row.profile_id() };
}
inline EquipEnhanceLevelComp MakeEquipEnhanceLevelComp(const EquipEnhanceTable& row) {
    return { row.level() };
}
