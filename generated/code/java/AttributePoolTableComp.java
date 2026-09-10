
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for AttributePoolTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class AttributePoolTableComp {

    private AttributePoolTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(AttributePoolTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(AttributePoolTable row) {
            return new Name(row.getName());
        }
    }

    public record Unlock_level(int value) {
        public static Unlock_level from(AttributePoolTable row) {
            return new Unlock_level(row.getUnlockLevel());
        }
    }

    public record Points_per_level(int value) {
        public static Points_per_level from(AttributePoolTable row) {
            return new Points_per_level(row.getPointsPerLevel());
        }
    }

    public record Base_points(int value) {
        public static Base_points from(AttributePoolTable row) {
            return new Base_points(row.getBasePoints());
        }
    }

    public record Dimension_cap(int value) {
        public static Dimension_cap from(AttributePoolTable row) {
            return new Dimension_cap(row.getDimensionCap());
        }
    }

    public record Reset_cost_gold(long value) {
        public static Reset_cost_gold from(AttributePoolTable row) {
            return new Reset_cost_gold(row.getResetCostGold());
        }
    }

    public record Reset_free_below_level(int value) {
        public static Reset_free_below_level from(AttributePoolTable row) {
            return new Reset_free_below_level(row.getResetFreeBelowLevel());
        }
    }

    public record Desc(String value) {
        public static Desc from(AttributePoolTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Owner_type(int value) {
        public static Owner_type from(AttributePoolTable row) {
            return new Owner_type(row.getOwnerType());
        }
    }

}