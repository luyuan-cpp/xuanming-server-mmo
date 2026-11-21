
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for CharacterAppearanceTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class CharacterAppearanceTableComp {

    private CharacterAppearanceTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(CharacterAppearanceTable row) {
            return new Id(row.getId());
        }
    }

    public record Appearance_id(String value) {
        public static Appearance_id from(CharacterAppearanceTable row) {
            return new Appearance_id(row.getAppearanceId());
        }
    }

    public record Name(String value) {
        public static Name from(CharacterAppearanceTable row) {
            return new Name(row.getName());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(CharacterAppearanceTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(CharacterAppearanceTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Desc(String value) {
        public static Desc from(CharacterAppearanceTable row) {
            return new Desc(row.getDesc());
        }
    }

}