
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/item_table.pb.h"

// ============================================================
// Per-column ECS components for ItemTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct ItemIdComp {
    uint32_t value;
};

struct ItemMax_stack_sizeComp {
    uint32_t value;
};

struct ItemEquip_kindComp {
    uint32_t value;
};

struct ItemBattle_usableComp {
    uint32_t value;
};

struct ItemBattle_heal_hpComp {
    uint64_t value;
};

struct ItemBattle_heal_mpComp {
    uint64_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline ItemIdComp MakeItemIdComp(const ItemTable& row) {
    return { row.id() };
}
inline ItemMax_stack_sizeComp MakeItemMax_stack_sizeComp(const ItemTable& row) {
    return { row.max_stack_size() };
}
inline ItemEquip_kindComp MakeItemEquip_kindComp(const ItemTable& row) {
    return { row.equip_kind() };
}
inline ItemBattle_usableComp MakeItemBattle_usableComp(const ItemTable& row) {
    return { row.battle_usable() };
}
inline ItemBattle_heal_hpComp MakeItemBattle_heal_hpComp(const ItemTable& row) {
    return { row.battle_heal_hp() };
}
inline ItemBattle_heal_mpComp MakeItemBattle_heal_mpComp(const ItemTable& row) {
    return { row.battle_heal_mp() };
}
