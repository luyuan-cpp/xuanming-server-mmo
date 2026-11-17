package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for MonsterTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class MonsterTableForeignKeys {
    private MonsterTableForeignKeys() {}

    /** Resolve Monster.capture_pet_table_id -> Pet row. */
    public static PetTable getCapturePetTableIdRow(MonsterTable row) {
        return PetTableManager.getInstance().findById(row.getCapturePetTableId());
    }

    /** Resolve Monster.capture_pet_table_id -> Pet row (by Monster id). */
    public static PetTable getCapturePetTableIdRow(int tableId) {
        MonsterTable row = MonsterTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getCapturePetTableIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all Monster rows whose capture_pet_table_id == key. */
    public static List<MonsterTable> findRowsByCapturePetTableId(int key) {
        return MonsterTableManager.getInstance().getByCapturePetTableId(key);
    }

}
