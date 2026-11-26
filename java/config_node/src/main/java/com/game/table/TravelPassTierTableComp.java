
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for TravelPassTierTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class TravelPassTierTableComp {

    private TravelPassTierTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(TravelPassTierTable row) {
            return new Id(row.getId());
        }
    }

    public record Pass_id(int value) {
        public static Pass_id from(TravelPassTierTable row) {
            return new Pass_id(row.getPassId());
        }
    }

    public record Tier_index(int value) {
        public static Tier_index from(TravelPassTierTable row) {
            return new Tier_index(row.getTierIndex());
        }
    }

    public record Required_points(int value) {
        public static Required_points from(TravelPassTierTable row) {
            return new Required_points(row.getRequiredPoints());
        }
    }

    public record Free_item_id(int value) {
        public static Free_item_id from(TravelPassTierTable row) {
            return new Free_item_id(row.getFreeItemId());
        }
    }

    public record Free_item_count(int value) {
        public static Free_item_count from(TravelPassTierTable row) {
            return new Free_item_count(row.getFreeItemCount());
        }
    }

    public record Free_gold(long value) {
        public static Free_gold from(TravelPassTierTable row) {
            return new Free_gold(row.getFreeGold());
        }
    }

    public record Free_cosmetic_key(String value) {
        public static Free_cosmetic_key from(TravelPassTierTable row) {
            return new Free_cosmetic_key(row.getFreeCosmeticKey());
        }
    }

    public record Free_cosmetic_name(String value) {
        public static Free_cosmetic_name from(TravelPassTierTable row) {
            return new Free_cosmetic_name(row.getFreeCosmeticName());
        }
    }

    public record Premium_item_id(int value) {
        public static Premium_item_id from(TravelPassTierTable row) {
            return new Premium_item_id(row.getPremiumItemId());
        }
    }

    public record Premium_item_count(int value) {
        public static Premium_item_count from(TravelPassTierTable row) {
            return new Premium_item_count(row.getPremiumItemCount());
        }
    }

    public record Premium_gold(long value) {
        public static Premium_gold from(TravelPassTierTable row) {
            return new Premium_gold(row.getPremiumGold());
        }
    }

    public record Premium_cosmetic_key(String value) {
        public static Premium_cosmetic_key from(TravelPassTierTable row) {
            return new Premium_cosmetic_key(row.getPremiumCosmeticKey());
        }
    }

    public record Premium_cosmetic_name(String value) {
        public static Premium_cosmetic_name from(TravelPassTierTable row) {
            return new Premium_cosmetic_name(row.getPremiumCosmeticName());
        }
    }

}