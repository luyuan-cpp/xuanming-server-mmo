
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ItemTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ItemTableComp {

    private ItemTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ItemTable row) {
            return new Id(row.getId());
        }
    }

    public record Max_stack_size(int value) {
        public static Max_stack_size from(ItemTable row) {
            return new Max_stack_size(row.getMaxStackSize());
        }
    }

    public record Equip_kind(int value) {
        public static Equip_kind from(ItemTable row) {
            return new Equip_kind(row.getEquipKind());
        }
    }

}