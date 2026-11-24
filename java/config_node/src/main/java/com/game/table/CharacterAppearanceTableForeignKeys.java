package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for CharacterAppearanceTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class CharacterAppearanceTableForeignKeys {
    private CharacterAppearanceTableForeignKeys() {}

    /** Resolve CharacterAppearance.series_id -> AppearanceSeries row. */
    public static AppearanceSeriesTable getSeriesIdRow(CharacterAppearanceTable row) {
        return AppearanceSeriesTableManager.getInstance().findById(row.getSeriesId());
    }

    /** Resolve CharacterAppearance.series_id -> AppearanceSeries row (by CharacterAppearance id). */
    public static AppearanceSeriesTable getSeriesIdRow(int tableId) {
        CharacterAppearanceTable row = CharacterAppearanceTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getSeriesIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all CharacterAppearance rows whose series_id == key. */
    public static List<CharacterAppearanceTable> findRowsBySeriesId(int key) {
        return CharacterAppearanceTableManager.getInstance().getBySeriesId(key);
    }

}
