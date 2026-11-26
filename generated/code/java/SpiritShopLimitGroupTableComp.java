
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for SpiritShopLimitGroupTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class SpiritShopLimitGroupTableComp {

    private SpiritShopLimitGroupTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(SpiritShopLimitGroupTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(SpiritShopLimitGroupTable row) {
            return new Name(row.getName());
        }
    }

    public record Reset_kind(int value) {
        public static Reset_kind from(SpiritShopLimitGroupTable row) {
            return new Reset_kind(row.getResetKind());
        }
    }

    public record Max_weight(int value) {
        public static Max_weight from(SpiritShopLimitGroupTable row) {
            return new Max_weight(row.getMaxWeight());
        }
    }

}