package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for ActivityScheduleTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class ActivityScheduleTableForeignKeys {
    private ActivityScheduleTableForeignKeys() {}

    /** Resolve ActivitySchedule.id -> Mission row. */
    public static MissionTable getIdRow(ActivityScheduleTable row) {
        return MissionTableManager.getInstance().findById(row.getId());
    }

    /** Resolve ActivitySchedule.id -> Mission row (by ActivitySchedule id). */
    public static MissionTable getIdRow(int tableId) {
        ActivityScheduleTable row = ActivityScheduleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all ActivitySchedule rows whose id == key. */
    public static List<ActivityScheduleTable> findRowsById(int key) {
        return ActivityScheduleTableManager.getInstance().getById(key);
    }

}
