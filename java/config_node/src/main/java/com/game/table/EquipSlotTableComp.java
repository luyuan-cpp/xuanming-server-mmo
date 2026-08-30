
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for EquipSlotTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class EquipSlotTableComp {

    private EquipSlotTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(EquipSlotTable row) {
            return new Id(row.getId());
        }
    }

    public record Equip_kind(int value) {
        public static Equip_kind from(EquipSlotTable row) {
            return new Equip_kind(row.getEquipKind());
        }
    }

}