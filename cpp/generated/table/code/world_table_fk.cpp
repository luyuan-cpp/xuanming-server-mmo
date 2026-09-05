#include "world_table_fk.h"
#include "world_table.h"
#include "basescene_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for WorldTable
// ---------------------------------------------------------------------------

/// Resolve World.scene_id -> BaseScene row.
const BaseSceneTable* GetWorldSceneIdRow(const WorldTable& row) {
    auto [ptr, _] = BaseSceneTableManager::Instance().FindByIdSilent(row.scene_id());
    return ptr;
}

/// Resolve World.scene_id -> BaseScene row (by World id).
const BaseSceneTable* GetWorldSceneIdRow(uint32_t tableId) {
    auto [row, _] = WorldTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetWorldSceneIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all World rows whose scene_id == key.
const std::vector<const WorldTable*>& FindWorldRowsBySceneId(uint32_t key) {
    return WorldTableManager::Instance().GetBySceneId(key);
}
