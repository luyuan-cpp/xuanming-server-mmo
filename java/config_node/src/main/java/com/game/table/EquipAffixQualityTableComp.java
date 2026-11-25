
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for EquipAffixQualityTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class EquipAffixQualityTableComp {

    private EquipAffixQualityTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(EquipAffixQualityTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(EquipAffixQualityTable row) {
            return new Name(row.getName());
        }
    }

    public record Ratio_pct(int value) {
        public static Ratio_pct from(EquipAffixQualityTable row) {
            return new Ratio_pct(row.getRatioPct());
        }
    }

    public record Apprentice_weight(int value) {
        public static Apprentice_weight from(EquipAffixQualityTable row) {
            return new Apprentice_weight(row.getApprenticeWeight());
        }
    }

    public record Artisan_weight(int value) {
        public static Artisan_weight from(EquipAffixQualityTable row) {
            return new Artisan_weight(row.getArtisanWeight());
        }
    }

    public record Master_weight(int value) {
        public static Master_weight from(EquipAffixQualityTable row) {
            return new Master_weight(row.getMasterWeight());
        }
    }

}