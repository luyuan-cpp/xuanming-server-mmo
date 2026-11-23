#include "rankedrule_table_fk.h"
#include "rankedrule_table.h"
#include "class_table.h"
#include "dungeon_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for RankedRuleTable
// ---------------------------------------------------------------------------

/// Resolve RankedRule.battle_config_id -> Dungeon row.
const DungeonTable* GetRankedRuleBattleConfigIdRow(const RankedRuleTable& row) {
    auto [ptr, _] = DungeonTableManager::Instance().FindByIdSilent(row.battle_config_id());
    return ptr;
}

/// Resolve RankedRule.battle_config_id -> Dungeon row (by RankedRule id).
const DungeonTable* GetRankedRuleBattleConfigIdRow(uint32_t tableId) {
    auto [row, _] = RankedRuleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetRankedRuleBattleConfigIdRow(*row);
}

/// Resolve RankedRule.reference_class_id -> Class row.
const ClassTable* GetRankedRuleReferenceClassIdRow(const RankedRuleTable& row) {
    auto [ptr, _] = ClassTableManager::Instance().FindByIdSilent(row.reference_class_id());
    return ptr;
}

/// Resolve RankedRule.reference_class_id -> Class row (by RankedRule id).
const ClassTable* GetRankedRuleReferenceClassIdRow(uint32_t tableId) {
    auto [row, _] = RankedRuleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetRankedRuleReferenceClassIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all RankedRule rows whose battle_config_id == key.
const std::vector<const RankedRuleTable*>& FindRankedRuleRowsByBattleConfigId(uint32_t key) {
    return RankedRuleTableManager::Instance().GetByBattleConfigId(key);
}

/// Reverse FK: find all RankedRule rows whose reference_class_id == key.
const std::vector<const RankedRuleTable*>& FindRankedRuleRowsByReferenceClassId(uint32_t key) {
    return RankedRuleTableManager::Instance().GetByReferenceClassId(key);
}
