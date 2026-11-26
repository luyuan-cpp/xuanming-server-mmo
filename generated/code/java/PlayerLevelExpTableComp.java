
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for PlayerLevelExpTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class PlayerLevelExpTableComp {

    private PlayerLevelExpTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(PlayerLevelExpTable row) {
            return new Id(row.getId());
        }
    }

    public record Required_total_exp(long value) {
        public static Required_total_exp from(PlayerLevelExpTable row) {
            return new Required_total_exp(row.getRequiredTotalExp());
        }
    }

}