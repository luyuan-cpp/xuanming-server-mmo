package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for AppearanceRuleTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class AppearanceRuleTableForeignKeys {
    private AppearanceRuleTableForeignKeys() {}

    /** Resolve AppearanceRule.card_item_id -> Item row. */
    public static ItemTable getCardItemIdRow(AppearanceRuleTable row) {
        return ItemTableManager.getInstance().findById(row.getCardItemId());
    }

    /** Resolve AppearanceRule.card_item_id -> Item row (by AppearanceRule id). */
    public static ItemTable getCardItemIdRow(int tableId) {
        AppearanceRuleTable row = AppearanceRuleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getCardItemIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all AppearanceRule rows whose card_item_id == key. */
    public static List<AppearanceRuleTable> findRowsByCardItemId(int key) {
        return AppearanceRuleTableManager.getInstance().getByCardItemId(key);
    }

}
