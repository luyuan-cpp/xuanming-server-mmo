
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/equipslot_table.pb.h"

// ============================================================
// Per-column ECS components for EquipSlotTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct EquipSlotIdComp {
    uint32_t value;
};

struct EquipSlotEquip_kindComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline EquipSlotIdComp MakeEquipSlotIdComp(const EquipSlotTable& row) {
    return { row.id() };
}
inline EquipSlotEquip_kindComp MakeEquipSlotEquip_kindComp(const EquipSlotTable& row) {
    return { row.equip_kind() };
}
