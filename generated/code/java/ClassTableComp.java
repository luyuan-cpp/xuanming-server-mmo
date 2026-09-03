
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ClassTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ClassTableComp {

    private ClassTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ClassTable row) {
            return new Id(row.getId());
        }
    }

    public record Init_health(long value) {
        public static Init_health from(ClassTable row) {
            return new Init_health(row.getInitHealth());
        }
    }

    public record Init_mana(long value) {
        public static Init_mana from(ClassTable row) {
            return new Init_mana(row.getInitMana());
        }
    }

    public record Init_strength(long value) {
        public static Init_strength from(ClassTable row) {
            return new Init_strength(row.getInitStrength());
        }
    }

    public record Init_armor(long value) {
        public static Init_armor from(ClassTable row) {
            return new Init_armor(row.getInitArmor());
        }
    }

    public record Init_resistance(long value) {
        public static Init_resistance from(ClassTable row) {
            return new Init_resistance(row.getInitResistance());
        }
    }

    public record Init_critchance(long value) {
        public static Init_critchance from(ClassTable row) {
            return new Init_critchance(row.getInitCritchance());
        }
    }

    public record Init_speed(long value) {
        public static Init_speed from(ClassTable row) {
            return new Init_speed(row.getInitSpeed());
        }
    }

    public record Skill(List<Integer> values) {
        public static Skill from(ClassTable row) {
            return new Skill(row.getSkillList());
        }
    }

}