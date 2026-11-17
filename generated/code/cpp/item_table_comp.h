
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

struct ItemNameComp {
    std::string_view value;
};

struct ItemDescriptionComp {
    std::string_view value;
};

struct ItemIcon_keyComp {
    std::string_view value;
};

struct ItemEquip_levelComp {
    uint32_t value;
};

struct ItemEquip_classComp {
    uint32_t value;
};

struct ItemAffix_poolComp {
    uint32_t value;
};

struct ItemAffix_ruleComp {
    uint32_t value;
};

struct ItemEquip_profileComp {
    uint32_t value;
};

struct ItemForge_material_idComp {
    uint32_t value;
};

struct ItemForge_material_countComp {
    uint32_t value;
};

struct ItemArtifact_idComp {
    uint32_t value;
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
inline ItemNameComp MakeItemNameComp(const ItemTable& row) {
    return { std::string_view(row.name()) };
}
inline ItemDescriptionComp MakeItemDescriptionComp(const ItemTable& row) {
    return { std::string_view(row.description()) };
}
inline ItemIcon_keyComp MakeItemIcon_keyComp(const ItemTable& row) {
    return { std::string_view(row.icon_key()) };
}
inline ItemEquip_levelComp MakeItemEquip_levelComp(const ItemTable& row) {
    return { row.equip_level() };
}
inline ItemEquip_classComp MakeItemEquip_classComp(const ItemTable& row) {
    return { row.equip_class() };
}
inline ItemAffix_poolComp MakeItemAffix_poolComp(const ItemTable& row) {
    return { row.affix_pool() };
}
inline ItemAffix_ruleComp MakeItemAffix_ruleComp(const ItemTable& row) {
    return { row.affix_rule() };
}
inline ItemEquip_profileComp MakeItemEquip_profileComp(const ItemTable& row) {
    return { row.equip_profile() };
}
inline ItemForge_material_idComp MakeItemForge_material_idComp(const ItemTable& row) {
    return { row.forge_material_id() };
}
inline ItemForge_material_countComp MakeItemForge_material_countComp(const ItemTable& row) {
    return { row.forge_material_count() };
}
inline ItemArtifact_idComp MakeItemArtifact_idComp(const ItemTable& row) {
    return { row.artifact_id() };
}
