
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for AfkPassRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class AfkPassRuleTableComp {

    private AfkPassRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(AfkPassRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Period_days(int value) {
        public static Period_days from(AfkPassRuleTable row) {
            return new Period_days(row.getPeriodDays());
        }
    }

    public record Price_currency_type(int value) {
        public static Price_currency_type from(AfkPassRuleTable row) {
            return new Price_currency_type(row.getPriceCurrencyType());
        }
    }

    public record Price(long value) {
        public static Price from(AfkPassRuleTable row) {
            return new Price(row.getPrice());
        }
    }

    public record Max_remaining_days(int value) {
        public static Max_remaining_days from(AfkPassRuleTable row) {
            return new Max_remaining_days(row.getMaxRemainingDays());
        }
    }

    public record Remind_before_hours(int value) {
        public static Remind_before_hours from(AfkPassRuleTable row) {
            return new Remind_before_hours(row.getRemindBeforeHours());
        }
    }

    public record Desc(String value) {
        public static Desc from(AfkPassRuleTable row) {
            return new Desc(row.getDesc());
        }
    }

}