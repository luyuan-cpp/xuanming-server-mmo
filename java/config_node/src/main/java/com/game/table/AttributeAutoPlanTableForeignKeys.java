package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for AttributeAutoPlanTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class AttributeAutoPlanTableForeignKeys {
    private AttributeAutoPlanTableForeignKeys() {}

    /** Resolve AttributeAutoPlan.pool_id -> AttributePool row. */
    public static AttributePoolTable getPoolIdRow(AttributeAutoPlanTable row) {
        return AttributePoolTableManager.getInstance().findById(row.getPoolId());
    }

    /** Resolve AttributeAutoPlan.pool_id -> AttributePool row (by AttributeAutoPlan id). */
    public static AttributePoolTable getPoolIdRow(int tableId) {
        AttributeAutoPlanTable row = AttributeAutoPlanTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getPoolIdRow(row);
    }

    /** Resolve AttributeAutoPlan.dimension[] -> AttributeDimension rows. */
    public static List<AttributeDimensionTable> getDimensionRows(AttributeAutoPlanTable row) {
        List<AttributeDimensionTable> result = new ArrayList<>();
        for (int id : row.getDimensionList()) {
            AttributeDimensionTable r = AttributeDimensionTableManager.getInstance().findById(id);
            if (r != null) { result.add(r); }
        }
        return result;
    }

    /** Resolve AttributeAutoPlan.dimension[] -> AttributeDimension rows (by AttributeAutoPlan id). */
    public static List<AttributeDimensionTable> getDimensionRows(int tableId) {
        AttributeAutoPlanTable row = AttributeAutoPlanTableManager.getInstance().findById(tableId);
        if (row == null) { return List.of(); }
        return getDimensionRows(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all AttributeAutoPlan rows whose pool_id == key. */
    public static List<AttributeAutoPlanTable> findRowsByPoolId(int key) {
        return AttributeAutoPlanTableManager.getInstance().getByPoolId(key);
    }

}
