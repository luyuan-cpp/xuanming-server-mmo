#include "activityschedule_table_fk.h"
#include "activityschedule_table.h"
#include "mission_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for ActivityScheduleTable
// ---------------------------------------------------------------------------

/// Resolve ActivitySchedule.id -> Mission row.
const MissionTable* GetActivityScheduleIdRow(const ActivityScheduleTable& row) {
    auto [ptr, _] = MissionTableManager::Instance().FindByIdSilent(row.id());
    return ptr;
}

/// Resolve ActivitySchedule.id -> Mission row (by ActivitySchedule id).
const MissionTable* GetActivityScheduleIdRow(uint32_t tableId) {
    auto [row, _] = ActivityScheduleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetActivityScheduleIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all ActivitySchedule rows whose id == key.
const std::vector<const ActivityScheduleTable*>& FindActivityScheduleRowsById(uint32_t key) {
    return ActivityScheduleTableManager::Instance().GetById(key);
}
