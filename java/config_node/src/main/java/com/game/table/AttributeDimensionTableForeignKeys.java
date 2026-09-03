package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for AttributeDimensionTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class AttributeDimensionTableForeignKeys {
    private AttributeDimensionTableForeignKeys() {}

    /** Resolve AttributeDimension.pool_id -> AttributePool row. */
    public static AttributePoolTable getPoolIdRow(AttributeDimensionTable row) {
        return AttributePoolTableManager.getInstance().findById(row.getPoolId());
    }

    /** Resolve AttributeDimension.pool_id -> AttributePool row (by AttributeDimension id). */
    public static AttributePoolTable getPoolIdRow(int tableId) {
        AttributeDimensionTable row = AttributeDimensionTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getPoolIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all AttributeDimension rows whose pool_id == key. */
    public static List<AttributeDimensionTable> findRowsByPoolId(int key) {
        return AttributeDimensionTableManager.getInstance().getByPoolId(key);
    }

}
