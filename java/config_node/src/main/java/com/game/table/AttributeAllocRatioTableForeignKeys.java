package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for AttributeAllocRatioTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class AttributeAllocRatioTableForeignKeys {
    private AttributeAllocRatioTableForeignKeys() {}

    /** Resolve AttributeAllocRatio.dimension_id -> AttributeDimension row. */
    public static AttributeDimensionTable getDimensionIdRow(AttributeAllocRatioTable row) {
        return AttributeDimensionTableManager.getInstance().findById(row.getDimensionId());
    }

    /** Resolve AttributeAllocRatio.dimension_id -> AttributeDimension row (by AttributeAllocRatio id). */
    public static AttributeDimensionTable getDimensionIdRow(int tableId) {
        AttributeAllocRatioTable row = AttributeAllocRatioTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getDimensionIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all AttributeAllocRatio rows whose dimension_id == key. */
    public static List<AttributeAllocRatioTable> findRowsByDimensionId(int key) {
        return AttributeAllocRatioTableManager.getInstance().getByDimensionId(key);
    }

}
