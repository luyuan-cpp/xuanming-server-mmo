#include "jianghurule_table_fk.h"
#include "jianghurule_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for JianghuRuleTable
// ---------------------------------------------------------------------------

/// Resolve JianghuRule.gold_writ_item_id -> Item row.
const ItemTable* GetJianghuRuleGoldWritItemIdRow(const JianghuRuleTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.gold_writ_item_id());
    return ptr;
}

/// Resolve JianghuRule.gold_writ_item_id -> Item row (by JianghuRule id).
const ItemTable* GetJianghuRuleGoldWritItemIdRow(uint32_t tableId) {
    auto [row, _] = JianghuRuleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetJianghuRuleGoldWritItemIdRow(*row);
}

/// Resolve JianghuRule.clear_item_id -> Item row.
const ItemTable* GetJianghuRuleClearItemIdRow(const JianghuRuleTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.clear_item_id());
    return ptr;
}

/// Resolve JianghuRule.clear_item_id -> Item row (by JianghuRule id).
const ItemTable* GetJianghuRuleClearItemIdRow(uint32_t tableId) {
    auto [row, _] = JianghuRuleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetJianghuRuleClearItemIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all JianghuRule rows whose gold_writ_item_id == key.
const std::vector<const JianghuRuleTable*>& FindJianghuRuleRowsByGoldWritItemId(uint32_t key) {
    return JianghuRuleTableManager::Instance().GetByGoldWritItemId(key);
}

/// Reverse FK: find all JianghuRule rows whose clear_item_id == key.
const std::vector<const JianghuRuleTable*>& FindJianghuRuleRowsByClearItemId(uint32_t key) {
    return JianghuRuleTableManager::Instance().GetByClearItemId(key);
}
