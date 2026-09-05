#include "dungeon_table_fk.h"
#include "dungeon_table.h"
#include "basescene_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for DungeonTable
// ---------------------------------------------------------------------------

/// Resolve Dungeon.scene_id -> BaseScene row.
const BaseSceneTable* GetDungeonSceneIdRow(const DungeonTable& row) {
    auto [ptr, _] = BaseSceneTableManager::Instance().FindByIdSilent(row.scene_id());
    return ptr;
}

/// Resolve Dungeon.scene_id -> BaseScene row (by Dungeon id).
const BaseSceneTable* GetDungeonSceneIdRow(uint32_t tableId) {
    auto [row, _] = DungeonTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetDungeonSceneIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all Dungeon rows whose scene_id == key.
const std::vector<const DungeonTable*>& FindDungeonRowsBySceneId(uint32_t key) {
    return DungeonTableManager::Instance().GetBySceneId(key);
}
