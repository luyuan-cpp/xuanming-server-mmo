
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for AttributeDimensionTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class AttributeDimensionTableComp {

    private AttributeDimensionTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(AttributeDimensionTable row) {
            return new Id(row.getId());
        }
    }

    public record Pool_id(int value) {
        public static Pool_id from(AttributeDimensionTable row) {
            return new Pool_id(row.getPoolId());
        }
    }

    public record Name(String value) {
        public static Name from(AttributeDimensionTable row) {
            return new Name(row.getName());
        }
    }

    public record Desc(String value) {
        public static Desc from(AttributeDimensionTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Sort(int value) {
        public static Sort from(AttributeDimensionTable row) {
            return new Sort(row.getSort());
        }
    }

    public record Base_per_level(int value) {
        public static Base_per_level from(AttributeDimensionTable row) {
            return new Base_per_level(row.getBasePerLevel());
        }
    }

    public record Max_health(double value) {
        public static Max_health from(AttributeDimensionTable row) {
            return new Max_health(row.getMaxHealth());
        }
    }

    public record Max_mana(double value) {
        public static Max_mana from(AttributeDimensionTable row) {
            return new Max_mana(row.getMaxMana());
        }
    }

    public record Physical_attack(double value) {
        public static Physical_attack from(AttributeDimensionTable row) {
            return new Physical_attack(row.getPhysicalAttack());
        }
    }

    public record Magic_attack(double value) {
        public static Magic_attack from(AttributeDimensionTable row) {
            return new Magic_attack(row.getMagicAttack());
        }
    }

    public record Speed(double value) {
        public static Speed from(AttributeDimensionTable row) {
            return new Speed(row.getSpeed());
        }
    }

    public record Defense(double value) {
        public static Defense from(AttributeDimensionTable row) {
            return new Defense(row.getDefense());
        }
    }

}