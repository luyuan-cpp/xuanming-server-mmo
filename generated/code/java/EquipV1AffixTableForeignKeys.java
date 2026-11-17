package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for EquipV1AffixTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class EquipV1AffixTableForeignKeys {
    private EquipV1AffixTableForeignKeys() {}

    /** Resolve EquipV1Affix.material_id -> Item row. */
    public static ItemTable getMaterialIdRow(EquipV1AffixTable row) {
        return ItemTableManager.getInstance().findById(row.getMaterialId());
    }

    /** Resolve EquipV1Affix.material_id -> Item row (by EquipV1Affix id). */
    public static ItemTable getMaterialIdRow(int tableId) {
        EquipV1AffixTable row = EquipV1AffixTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getMaterialIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all EquipV1Affix rows whose material_id == key. */
    public static List<EquipV1AffixTable> findRowsByMaterialId(int key) {
        return EquipV1AffixTableManager.getInstance().getByMaterialId(key);
    }

}
