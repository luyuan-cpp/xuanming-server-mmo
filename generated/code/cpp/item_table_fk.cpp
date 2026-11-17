#include "item_table_fk.h"
#include "item_table.h"
#include "equipaffixrule_table.h"
#include "equipartifact_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for ItemTable
// ---------------------------------------------------------------------------

/// Resolve Item.affix_rule -> EquipAffixRule row.
const EquipAffixRuleTable* GetItemAffixRuleRow(const ItemTable& row) {
    auto [ptr, _] = EquipAffixRuleTableManager::Instance().FindByIdSilent(row.affix_rule());
    return ptr;
}

/// Resolve Item.affix_rule -> EquipAffixRule row (by Item id).
const EquipAffixRuleTable* GetItemAffixRuleRow(uint32_t tableId) {
    auto [row, _] = ItemTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetItemAffixRuleRow(*row);
}

/// Resolve Item.forge_material_id -> Item row.
const ItemTable* GetItemForgeMaterialIdRow(const ItemTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.forge_material_id());
    return ptr;
}

/// Resolve Item.forge_material_id -> Item row (by Item id).
const ItemTable* GetItemForgeMaterialIdRow(uint32_t tableId) {
    auto [row, _] = ItemTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetItemForgeMaterialIdRow(*row);
}

/// Resolve Item.artifact_id -> EquipArtifact row.
const EquipArtifactTable* GetItemArtifactIdRow(const ItemTable& row) {
    auto [ptr, _] = EquipArtifactTableManager::Instance().FindByIdSilent(row.artifact_id());
    return ptr;
}

/// Resolve Item.artifact_id -> EquipArtifact row (by Item id).
const EquipArtifactTable* GetItemArtifactIdRow(uint32_t tableId) {
    auto [row, _] = ItemTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetItemArtifactIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all Item rows whose affix_rule == key.
const std::vector<const ItemTable*>& FindItemRowsByAffixRule(uint32_t key) {
    return ItemTableManager::Instance().GetByAffixRule(key);
}

/// Reverse FK: find all Item rows whose forge_material_id == key.
const std::vector<const ItemTable*>& FindItemRowsByForgeMaterialId(uint32_t key) {
    return ItemTableManager::Instance().GetByForgeMaterialId(key);
}

/// Reverse FK: find all Item rows whose artifact_id == key.
const std::vector<const ItemTable*>& FindItemRowsByArtifactId(uint32_t key) {
    return ItemTableManager::Instance().GetByArtifactId(key);
}
