package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for ItemTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class ItemTableForeignKeys {
    private ItemTableForeignKeys() {}

    /** Resolve Item.affix_rule -> EquipAffixRule row. */
    public static EquipAffixRuleTable getAffixRuleRow(ItemTable row) {
        return EquipAffixRuleTableManager.getInstance().findById(row.getAffixRule());
    }

    /** Resolve Item.affix_rule -> EquipAffixRule row (by Item id). */
    public static EquipAffixRuleTable getAffixRuleRow(int tableId) {
        ItemTable row = ItemTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getAffixRuleRow(row);
    }

    /** Resolve Item.forge_material_id -> Item row. */
    public static ItemTable getForgeMaterialIdRow(ItemTable row) {
        return ItemTableManager.getInstance().findById(row.getForgeMaterialId());
    }

    /** Resolve Item.forge_material_id -> Item row (by Item id). */
    public static ItemTable getForgeMaterialIdRow(int tableId) {
        ItemTable row = ItemTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getForgeMaterialIdRow(row);
    }

    /** Resolve Item.artifact_id -> EquipArtifact row. */
    public static EquipArtifactTable getArtifactIdRow(ItemTable row) {
        return EquipArtifactTableManager.getInstance().findById(row.getArtifactId());
    }

    /** Resolve Item.artifact_id -> EquipArtifact row (by Item id). */
    public static EquipArtifactTable getArtifactIdRow(int tableId) {
        ItemTable row = ItemTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getArtifactIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all Item rows whose affix_rule == key. */
    public static List<ItemTable> findRowsByAffixRule(int key) {
        return ItemTableManager.getInstance().getByAffixRule(key);
    }

    /** Reverse FK: find all Item rows whose forge_material_id == key. */
    public static List<ItemTable> findRowsByForgeMaterialId(int key) {
        return ItemTableManager.getInstance().getByForgeMaterialId(key);
    }

    /** Reverse FK: find all Item rows whose artifact_id == key. */
    public static List<ItemTable> findRowsByArtifactId(int key) {
        return ItemTableManager.getInstance().getByArtifactId(key);
    }

}
