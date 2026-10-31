#include "playmode_table_fk.h"
#include "playmode_table.h"
#include "dungeon_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for PlayModeTable
// ---------------------------------------------------------------------------

/// Resolve PlayMode.battle_config_id -> Dungeon row.
const DungeonTable* GetPlayModeBattleConfigIdRow(const PlayModeTable& row) {
    auto [ptr, _] = DungeonTableManager::Instance().FindByIdSilent(row.battle_config_id());
    return ptr;
}

/// Resolve PlayMode.battle_config_id -> Dungeon row (by PlayMode id).
const DungeonTable* GetPlayModeBattleConfigIdRow(uint32_t tableId) {
    auto [row, _] = PlayModeTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetPlayModeBattleConfigIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all PlayMode rows whose battle_config_id == key.
const std::vector<const PlayModeTable*>& FindPlayModeRowsByBattleConfigId(uint32_t key) {
    return PlayModeTableManager::Instance().GetByBattleConfigId(key);
}
