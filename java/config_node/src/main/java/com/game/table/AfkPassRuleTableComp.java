
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

    public record Bonus_item_id(int value) {
        public static Bonus_item_id from(AfkPassRuleTable row) {
            return new Bonus_item_id(row.getBonusItemId());
        }
    }

    public record Bonus_item_count(int value) {
        public static Bonus_item_count from(AfkPassRuleTable row) {
            return new Bonus_item_count(row.getBonusItemCount());
        }
    }

    public record Bonus_frame_key(String value) {
        public static Bonus_frame_key from(AfkPassRuleTable row) {
            return new Bonus_frame_key(row.getBonusFrameKey());
        }
    }

    public record Plan_name(String value) {
        public static Plan_name from(AfkPassRuleTable row) {
            return new Plan_name(row.getPlanName());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(AfkPassRuleTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

}