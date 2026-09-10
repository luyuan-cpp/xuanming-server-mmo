
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

}