#include "tournamentrule_table_fk.h"
#include "tournamentrule_table.h"
#include "dungeon_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for TournamentRuleTable
// ---------------------------------------------------------------------------

/// Resolve TournamentRule.battle_config_id -> Dungeon row.
const DungeonTable* GetTournamentRuleBattleConfigIdRow(const TournamentRuleTable& row) {
    auto [ptr, _] = DungeonTableManager::Instance().FindByIdSilent(row.battle_config_id());
    return ptr;
}

/// Resolve TournamentRule.battle_config_id -> Dungeon row (by TournamentRule id).
const DungeonTable* GetTournamentRuleBattleConfigIdRow(uint32_t tableId) {
    auto [row, _] = TournamentRuleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetTournamentRuleBattleConfigIdRow(*row);
}

/// Resolve TournamentRule.salute_item_id -> Item row.
const ItemTable* GetTournamentRuleSaluteItemIdRow(const TournamentRuleTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.salute_item_id());
    return ptr;
}

/// Resolve TournamentRule.salute_item_id -> Item row (by TournamentRule id).
const ItemTable* GetTournamentRuleSaluteItemIdRow(uint32_t tableId) {
    auto [row, _] = TournamentRuleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetTournamentRuleSaluteItemIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all TournamentRule rows whose battle_config_id == key.
const std::vector<const TournamentRuleTable*>& FindTournamentRuleRowsByBattleConfigId(uint32_t key) {
    return TournamentRuleTableManager::Instance().GetByBattleConfigId(key);
}

/// Reverse FK: find all TournamentRule rows whose salute_item_id == key.
const std::vector<const TournamentRuleTable*>& FindTournamentRuleRowsBySaluteItemId(uint32_t key) {
    return TournamentRuleTableManager::Instance().GetBySaluteItemId(key);
}
