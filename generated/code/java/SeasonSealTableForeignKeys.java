package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for SeasonSealTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class SeasonSealTableForeignKeys {
    private SeasonSealTableForeignKeys() {}

    /** Resolve SeasonSeal.season_id -> SeasonRule row. */
    public static SeasonRuleTable getSeasonIdRow(SeasonSealTable row) {
        return SeasonRuleTableManager.getInstance().findById(row.getSeasonId());
    }

    /** Resolve SeasonSeal.season_id -> SeasonRule row (by SeasonSeal id). */
    public static SeasonRuleTable getSeasonIdRow(int tableId) {
        SeasonSealTable row = SeasonSealTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getSeasonIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all SeasonSeal rows whose season_id == key. */
    public static List<SeasonSealTable> findRowsBySeasonId(int key) {
        return SeasonSealTableManager.getInstance().getBySeasonId(key);
    }

}
