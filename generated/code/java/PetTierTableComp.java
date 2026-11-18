
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for PetTierTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class PetTierTableComp {

    private PetTierTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(PetTierTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(PetTierTable row) {
            return new Name(row.getName());
        }
    }

    public record Art_key(String value) {
        public static Art_key from(PetTierTable row) {
            return new Art_key(row.getArtKey());
        }
    }

    public record Desc(String value) {
        public static Desc from(PetTierTable row) {
            return new Desc(row.getDesc());
        }
    }

}