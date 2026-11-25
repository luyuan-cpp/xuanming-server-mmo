package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for SkillScrollCraftTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class SkillScrollCraftTableForeignKeys {
    private SkillScrollCraftTableForeignKeys() {}

    /** Resolve SkillScrollCraft.fragment_item_id -> Item row. */
    public static ItemTable getFragmentItemIdRow(SkillScrollCraftTable row) {
        return ItemTableManager.getInstance().findById(row.getFragmentItemId());
    }

    /** Resolve SkillScrollCraft.fragment_item_id -> Item row (by SkillScrollCraft id). */
    public static ItemTable getFragmentItemIdRow(int tableId) {
        SkillScrollCraftTable row = SkillScrollCraftTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getFragmentItemIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all SkillScrollCraft rows whose fragment_item_id == key. */
    public static List<SkillScrollCraftTable> findRowsByFragmentItemId(int key) {
        return SkillScrollCraftTableManager.getInstance().getByFragmentItemId(key);
    }

}
