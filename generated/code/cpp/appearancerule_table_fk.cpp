#include "appearancerule_table_fk.h"
#include "appearancerule_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for AppearanceRuleTable
// ---------------------------------------------------------------------------

/// Resolve AppearanceRule.card_item_id -> Item row.
const ItemTable* GetAppearanceRuleCardItemIdRow(const AppearanceRuleTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.card_item_id());
    return ptr;
}

/// Resolve AppearanceRule.card_item_id -> Item row (by AppearanceRule id).
const ItemTable* GetAppearanceRuleCardItemIdRow(uint32_t tableId) {
    auto [row, _] = AppearanceRuleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetAppearanceRuleCardItemIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all AppearanceRule rows whose card_item_id == key.
const std::vector<const AppearanceRuleTable*>& FindAppearanceRuleRowsByCardItemId(uint32_t key) {
    return AppearanceRuleTableManager::Instance().GetByCardItemId(key);
}
