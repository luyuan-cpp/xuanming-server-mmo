#pragma once
#include <vector>
#include "dungeon_table.h"

#include "basescene_table.h"

#include "monster_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for DungeonTable
// ---------------------------------------------------------------------------

/// Resolve Dungeon.scene_id -> BaseScene row.
inline const BaseSceneTable* GetDungeonSceneIdRow(const DungeonTable& row) {
    auto [ptr, _] = BaseSceneTableManager::Instance().FindByIdSilent(row.scene_id());
    return ptr;
}

/// Resolve Dungeon.scene_id -> BaseScene row (by Dungeon id).
inline const BaseSceneTable* GetDungeonSceneIdRow(uint32_t tableId) {
    auto [row, _] = DungeonTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetDungeonSceneIdRow(*row);
}

/// Resolve Dungeon.monster[] -> Monster rows.
inline std::vector<const MonsterTable*> GetDungeonMonsterRows(const DungeonTable& row) {
    std::vector<const MonsterTable*> result;
    for (auto id : row.monster()) {
        auto [ptr, _] = MonsterTableManager::Instance().FindByIdSilent(id);
        if (ptr) result.push_back(ptr);
    }
    return result;
}

/// Resolve Dungeon.monster[] -> Monster rows (by Dungeon id).
inline std::vector<const MonsterTable*> GetDungeonMonsterRows(uint32_t tableId) {
    auto [row, _] = DungeonTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return {};
    return GetDungeonMonsterRows(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

/// Reverse FK: find all Dungeon rows whose scene_id == key.
inline const std::vector<const DungeonTable*>& FindDungeonRowsBySceneId(uint32_t key) {
    return DungeonTableManager::Instance().GetBySceneId(key);
}
