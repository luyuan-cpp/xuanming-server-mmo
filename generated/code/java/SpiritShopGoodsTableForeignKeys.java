package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for SpiritShopGoodsTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class SpiritShopGoodsTableForeignKeys {
    private SpiritShopGoodsTableForeignKeys() {}

    /** Resolve SpiritShopGoods.item_id_1 -> Item row. */
    public static ItemTable getItemId1Row(SpiritShopGoodsTable row) {
        return ItemTableManager.getInstance().findById(row.getItemId1());
    }

    /** Resolve SpiritShopGoods.item_id_1 -> Item row (by SpiritShopGoods id). */
    public static ItemTable getItemId1Row(int tableId) {
        SpiritShopGoodsTable row = SpiritShopGoodsTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getItemId1Row(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all SpiritShopGoods rows whose item_id_1 == key. */
    public static List<SpiritShopGoodsTable> findRowsByItemId1(int key) {
        return SpiritShopGoodsTableManager.getInstance().getByItemId1(key);
    }

}
