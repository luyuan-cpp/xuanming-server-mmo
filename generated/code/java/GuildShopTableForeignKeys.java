package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for GuildShopTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class GuildShopTableForeignKeys {
    private GuildShopTableForeignKeys() {}

    /** Resolve GuildShop.item_id -> Item row. */
    public static ItemTable getItemIdRow(GuildShopTable row) {
        return ItemTableManager.getInstance().findById(row.getItemId());
    }

    /** Resolve GuildShop.item_id -> Item row (by GuildShop id). */
    public static ItemTable getItemIdRow(int tableId) {
        GuildShopTable row = GuildShopTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getItemIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all GuildShop rows whose item_id == key. */
    public static List<GuildShopTable> findRowsByItemId(int key) {
        return GuildShopTableManager.getInstance().getByItemId(key);
    }

}
