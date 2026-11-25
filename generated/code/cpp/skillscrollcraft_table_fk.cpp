#include "skillscrollcraft_table_fk.h"
#include "skillscrollcraft_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for SkillScrollCraftTable
// ---------------------------------------------------------------------------

/// Resolve SkillScrollCraft.fragment_item_id -> Item row.
const ItemTable* GetSkillScrollCraftFragmentItemIdRow(const SkillScrollCraftTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.fragment_item_id());
    return ptr;
}

/// Resolve SkillScrollCraft.fragment_item_id -> Item row (by SkillScrollCraft id).
const ItemTable* GetSkillScrollCraftFragmentItemIdRow(uint32_t tableId) {
    auto [row, _] = SkillScrollCraftTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetSkillScrollCraftFragmentItemIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all SkillScrollCraft rows whose fragment_item_id == key.
const std::vector<const SkillScrollCraftTable*>& FindSkillScrollCraftRowsByFragmentItemId(uint32_t key) {
    return SkillScrollCraftTableManager::Instance().GetByFragmentItemId(key);
}
