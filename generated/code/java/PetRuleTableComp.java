
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for PetRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class PetRuleTableComp {

    private PetRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(PetRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Max_pets(int value) {
        public static Max_pets from(PetRuleTable row) {
            return new Max_pets(row.getMaxPets());
        }
    }

    public record Name_max_len(int value) {
        public static Name_max_len from(PetRuleTable row) {
            return new Name_max_len(row.getNameMaxLen());
        }
    }

    public record Rename_cost_gold(long value) {
        public static Rename_cost_gold from(PetRuleTable row) {
            return new Rename_cost_gold(row.getRenameCostGold());
        }
    }

    public record Summon_cooldown_seconds(int value) {
        public static Summon_cooldown_seconds from(PetRuleTable row) {
            return new Summon_cooldown_seconds(row.getSummonCooldownSeconds());
        }
    }

    public record Desc(String value) {
        public static Desc from(PetRuleTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Entry_config_version(int value) {
        public static Entry_config_version from(PetRuleTable row) {
            return new Entry_config_version(row.getEntryConfigVersion());
        }
    }

    public record Entry_total_points(int value) {
        public static Entry_total_points from(PetRuleTable row) {
            return new Entry_total_points(row.getEntryTotalPoints());
        }
    }

    public record Entry_max_directions(int value) {
        public static Entry_max_directions from(PetRuleTable row) {
            return new Entry_max_directions(row.getEntryMaxDirections());
        }
    }

}