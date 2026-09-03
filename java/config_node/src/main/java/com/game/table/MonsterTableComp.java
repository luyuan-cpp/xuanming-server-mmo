
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for MonsterTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class MonsterTableComp {

    private MonsterTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(MonsterTable row) {
            return new Id(row.getId());
        }
    }

    public record Health(long value) {
        public static Health from(MonsterTable row) {
            return new Health(row.getHealth());
        }
    }

    public record Strength(long value) {
        public static Strength from(MonsterTable row) {
            return new Strength(row.getStrength());
        }
    }

    public record Armor(long value) {
        public static Armor from(MonsterTable row) {
            return new Armor(row.getArmor());
        }
    }

    public record Resistance(long value) {
        public static Resistance from(MonsterTable row) {
            return new Resistance(row.getResistance());
        }
    }

    public record Critchance(long value) {
        public static Critchance from(MonsterTable row) {
            return new Critchance(row.getCritchance());
        }
    }

    public record Speed(long value) {
        public static Speed from(MonsterTable row) {
            return new Speed(row.getSpeed());
        }
    }

    public record Exp_reward(long value) {
        public static Exp_reward from(MonsterTable row) {
            return new Exp_reward(row.getExpReward());
        }
    }

    public record Gold_reward(long value) {
        public static Gold_reward from(MonsterTable row) {
            return new Gold_reward(row.getGoldReward());
        }
    }

}