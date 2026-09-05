#include "mirror_table_fk.h"
#include "mirror_table.h"
#include "basescene_table.h"
#include "world_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for MirrorTable
// ---------------------------------------------------------------------------

/// Resolve Mirror.scene_id -> BaseScene row.
const BaseSceneTable* GetMirrorSceneIdRow(const MirrorTable& row) {
    auto [ptr, _] = BaseSceneTableManager::Instance().FindByIdSilent(row.scene_id());
    return ptr;
}

/// Resolve Mirror.scene_id -> BaseScene row (by Mirror id).
const BaseSceneTable* GetMirrorSceneIdRow(uint32_t tableId) {
    auto [row, _] = MirrorTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetMirrorSceneIdRow(*row);
}

/// Resolve Mirror.main_scene_id -> World row.
const WorldTable* GetMirrorMainSceneIdRow(const MirrorTable& row) {
    auto [ptr, _] = WorldTableManager::Instance().FindByIdSilent(row.main_scene_id());
    return ptr;
}

/// Resolve Mirror.main_scene_id -> World row (by Mirror id).
const WorldTable* GetMirrorMainSceneIdRow(uint32_t tableId) {
    auto [row, _] = MirrorTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetMirrorMainSceneIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all Mirror rows whose scene_id == key.
const std::vector<const MirrorTable*>& FindMirrorRowsBySceneId(uint32_t key) {
    return MirrorTableManager::Instance().GetBySceneId(key);
}

/// Reverse FK: find all Mirror rows whose main_scene_id == key.
const std::vector<const MirrorTable*>& FindMirrorRowsByMainSceneId(uint32_t key) {
    return MirrorTableManager::Instance().GetByMainSceneId(key);
}
