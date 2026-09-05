
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for AttributeRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class AttributeRuleTableComp {

    private AttributeRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(AttributeRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Max_schemes(int value) {
        public static Max_schemes from(AttributeRuleTable row) {
            return new Max_schemes(row.getMaxSchemes());
        }
    }

    public record Free_scheme_count(int value) {
        public static Free_scheme_count from(AttributeRuleTable row) {
            return new Free_scheme_count(row.getFreeSchemeCount());
        }
    }

    public record Create_scheme_cost_gold(long value) {
        public static Create_scheme_cost_gold from(AttributeRuleTable row) {
            return new Create_scheme_cost_gold(row.getCreateSchemeCostGold());
        }
    }

    public record Switch_cooldown_seconds(int value) {
        public static Switch_cooldown_seconds from(AttributeRuleTable row) {
            return new Switch_cooldown_seconds(row.getSwitchCooldownSeconds());
        }
    }

    public record Scheme_name_max_len(int value) {
        public static Scheme_name_max_len from(AttributeRuleTable row) {
            return new Scheme_name_max_len(row.getSchemeNameMaxLen());
        }
    }

}