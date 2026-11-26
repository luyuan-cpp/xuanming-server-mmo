package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for JianghuRuleTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class JianghuRuleTableForeignKeys {
    private JianghuRuleTableForeignKeys() {}

    /** Resolve JianghuRule.gold_writ_item_id -> Item row. */
    public static ItemTable getGoldWritItemIdRow(JianghuRuleTable row) {
        return ItemTableManager.getInstance().findById(row.getGoldWritItemId());
    }

    /** Resolve JianghuRule.gold_writ_item_id -> Item row (by JianghuRule id). */
    public static ItemTable getGoldWritItemIdRow(int tableId) {
        JianghuRuleTable row = JianghuRuleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getGoldWritItemIdRow(row);
    }

    /** Resolve JianghuRule.clear_item_id -> Item row. */
    public static ItemTable getClearItemIdRow(JianghuRuleTable row) {
        return ItemTableManager.getInstance().findById(row.getClearItemId());
    }

    /** Resolve JianghuRule.clear_item_id -> Item row (by JianghuRule id). */
    public static ItemTable getClearItemIdRow(int tableId) {
        JianghuRuleTable row = JianghuRuleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getClearItemIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all JianghuRule rows whose gold_writ_item_id == key. */
    public static List<JianghuRuleTable> findRowsByGoldWritItemId(int key) {
        return JianghuRuleTableManager.getInstance().getByGoldWritItemId(key);
    }

    /** Reverse FK: find all JianghuRule rows whose clear_item_id == key. */
    public static List<JianghuRuleTable> findRowsByClearItemId(int key) {
        return JianghuRuleTableManager.getInstance().getByClearItemId(key);
    }

}
