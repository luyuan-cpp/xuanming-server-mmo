
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for AttributeAllocRatioTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class AttributeAllocRatioTableComp {

    private AttributeAllocRatioTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(AttributeAllocRatioTable row) {
            return new Id(row.getId());
        }
    }

    public record Dimension_id(int value) {
        public static Dimension_id from(AttributeAllocRatioTable row) {
            return new Dimension_id(row.getDimensionId());
        }
    }

    public record Class_id(int value) {
        public static Class_id from(AttributeAllocRatioTable row) {
            return new Class_id(row.getClassId());
        }
    }

    public record Max_health(double value) {
        public static Max_health from(AttributeAllocRatioTable row) {
            return new Max_health(row.getMaxHealth());
        }
    }

    public record Max_mana(double value) {
        public static Max_mana from(AttributeAllocRatioTable row) {
            return new Max_mana(row.getMaxMana());
        }
    }

    public record Physical_attack(double value) {
        public static Physical_attack from(AttributeAllocRatioTable row) {
            return new Physical_attack(row.getPhysicalAttack());
        }
    }

    public record Magic_attack(double value) {
        public static Magic_attack from(AttributeAllocRatioTable row) {
            return new Magic_attack(row.getMagicAttack());
        }
    }

    public record Speed(double value) {
        public static Speed from(AttributeAllocRatioTable row) {
            return new Speed(row.getSpeed());
        }
    }

    public record Defense(double value) {
        public static Defense from(AttributeAllocRatioTable row) {
            return new Defense(row.getDefense());
        }
    }

    public record Desc(String value) {
        public static Desc from(AttributeAllocRatioTable row) {
            return new Desc(row.getDesc());
        }
    }

}