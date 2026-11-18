
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for PetEntryTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class PetEntryTableComp {

    private PetEntryTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(PetEntryTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(PetEntryTable row) {
            return new Name(row.getName());
        }
    }

    public record Desc(String value) {
        public static Desc from(PetEntryTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Category(int value) {
        public static Category from(PetEntryTable row) {
            return new Category(row.getCategory());
        }
    }

    public record Channel(int value) {
        public static Channel from(PetEntryTable row) {
            return new Channel(row.getChannel());
        }
    }

    public record State_kind(int value) {
        public static State_kind from(PetEntryTable row) {
            return new State_kind(row.getStateKind());
        }
    }

    public record Cap(int value) {
        public static Cap from(PetEntryTable row) {
            return new Cap(row.getCap());
        }
    }

    public record Per_point_bps(int value) {
        public static Per_point_bps from(PetEntryTable row) {
            return new Per_point_bps(row.getPerPointBps());
        }
    }

    public record Sort(int value) {
        public static Sort from(PetEntryTable row) {
            return new Sort(row.getSort());
        }
    }

    public record Icon(String value) {
        public static Icon from(PetEntryTable row) {
            return new Icon(row.getIcon());
        }
    }

}