package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for TravelPassTierTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class TravelPassTierTableForeignKeys {
    private TravelPassTierTableForeignKeys() {}

    /** Resolve TravelPassTier.pass_id -> TravelPassSeason row. */
    public static TravelPassSeasonTable getPassIdRow(TravelPassTierTable row) {
        return TravelPassSeasonTableManager.getInstance().findById(row.getPassId());
    }

    /** Resolve TravelPassTier.pass_id -> TravelPassSeason row (by TravelPassTier id). */
    public static TravelPassSeasonTable getPassIdRow(int tableId) {
        TravelPassTierTable row = TravelPassTierTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getPassIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all TravelPassTier rows whose pass_id == key. */
    public static List<TravelPassTierTable> findRowsByPassId(int key) {
        return TravelPassTierTableManager.getInstance().getByPassId(key);
    }

}
