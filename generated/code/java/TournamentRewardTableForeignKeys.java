package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for TournamentRewardTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class TournamentRewardTableForeignKeys {
    private TournamentRewardTableForeignKeys() {}

    /** Resolve TournamentReward.item_ids[] -> Item rows. */
    public static List<ItemTable> getItemIdsRows(TournamentRewardTable row) {
        List<ItemTable> result = new ArrayList<>();
        for (int id : row.getItemIdsList()) {
            ItemTable r = ItemTableManager.getInstance().findById(id);
            if (r != null) { result.add(r); }
        }
        return result;
    }

    /** Resolve TournamentReward.item_ids[] -> Item rows (by TournamentReward id). */
    public static List<ItemTable> getItemIdsRows(int tableId) {
        TournamentRewardTable row = TournamentRewardTableManager.getInstance().findById(tableId);
        if (row == null) { return List.of(); }
        return getItemIdsRows(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

}
