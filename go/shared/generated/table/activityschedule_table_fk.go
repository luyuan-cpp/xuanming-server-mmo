package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for ActivityScheduleTable
// ---------------------------------------------------------------------------

// GetActivityScheduleIdRow resolves ActivitySchedule.id -> Mission row.
func GetActivityScheduleIdRow(row *pb.ActivityScheduleTable) (*pb.MissionTable, bool) {
    return MissionTableManagerInstance.FindById(row.Id)
}

// GetActivityScheduleIdRowById resolves ActivitySchedule.id -> Mission row (by ActivitySchedule id).
func GetActivityScheduleIdRowById(tableId uint32) (*pb.MissionTable, bool) {
    row, ok := ActivityScheduleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetActivityScheduleIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindActivityScheduleRowsById returns all ActivitySchedule rows whose id == key.
func FindActivityScheduleRowsById(key uint32) []*pb.ActivityScheduleTable {
    return ActivityScheduleTableManagerInstance.GetById(key)
}
