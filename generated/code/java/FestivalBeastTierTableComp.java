
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for FestivalBeastTierTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class FestivalBeastTierTableComp {

    private FestivalBeastTierTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(FestivalBeastTierTable row) {
            return new Id(row.getId());
        }
    }

    public record Min_level(int value) {
        public static Min_level from(FestivalBeastTierTable row) {
            return new Min_level(row.getMinLevel());
        }
    }

    public record Max_level(int value) {
        public static Max_level from(FestivalBeastTierTable row) {
            return new Max_level(row.getMaxLevel());
        }
    }

    public record Boss_health(long value) {
        public static Boss_health from(FestivalBeastTierTable row) {
            return new Boss_health(row.getBossHealth());
        }
    }

    public record Boss_physical_attack(long value) {
        public static Boss_physical_attack from(FestivalBeastTierTable row) {
            return new Boss_physical_attack(row.getBossPhysicalAttack());
        }
    }

    public record Boss_defense(long value) {
        public static Boss_defense from(FestivalBeastTierTable row) {
            return new Boss_defense(row.getBossDefense());
        }
    }

    public record Boss_resistance(long value) {
        public static Boss_resistance from(FestivalBeastTierTable row) {
            return new Boss_resistance(row.getBossResistance());
        }
    }

    public record Boss_speed(long value) {
        public static Boss_speed from(FestivalBeastTierTable row) {
            return new Boss_speed(row.getBossSpeed());
        }
    }

}