
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for AppearanceRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class AppearanceRuleTableComp {

    private AppearanceRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(AppearanceRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Card_item_id(int value) {
        public static Card_item_id from(AppearanceRuleTable row) {
            return new Card_item_id(row.getCardItemId());
        }
    }

    public record Card_item_count(int value) {
        public static Card_item_count from(AppearanceRuleTable row) {
            return new Card_item_count(row.getCardItemCount());
        }
    }

    public record Trial_duration_seconds(int value) {
        public static Trial_duration_seconds from(AppearanceRuleTable row) {
            return new Trial_duration_seconds(row.getTrialDurationSeconds());
        }
    }

    public record Mall_sku_id(long value) {
        public static Mall_sku_id from(AppearanceRuleTable row) {
            return new Mall_sku_id(row.getMallSkuId());
        }
    }

    public record Mall_price_fen(long value) {
        public static Mall_price_fen from(AppearanceRuleTable row) {
            return new Mall_price_fen(row.getMallPriceFen());
        }
    }

    public record Desc(String value) {
        public static Desc from(AppearanceRuleTable row) {
            return new Desc(row.getDesc());
        }
    }

}