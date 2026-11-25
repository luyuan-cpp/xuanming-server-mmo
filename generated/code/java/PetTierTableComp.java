
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for PetTierTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class PetTierTableComp {

    private PetTierTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(PetTierTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(PetTierTable row) {
            return new Name(row.getName());
        }
    }

    public record Art_key(String value) {
        public static Art_key from(PetTierTable row) {
            return new Art_key(row.getArtKey());
        }
    }

    public record Desc(String value) {
        public static Desc from(PetTierTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Ascend_cost_item_id(int value) {
        public static Ascend_cost_item_id from(PetTierTable row) {
            return new Ascend_cost_item_id(row.getAscendCostItemId());
        }
    }

    public record Ascend_cost_item_count(int value) {
        public static Ascend_cost_item_count from(PetTierTable row) {
            return new Ascend_cost_item_count(row.getAscendCostItemCount());
        }
    }

    public record Ascend_cost_gold(long value) {
        public static Ascend_cost_gold from(PetTierTable row) {
            return new Ascend_cost_gold(row.getAscendCostGold());
        }
    }

    public record Ascend_min_pet_level(int value) {
        public static Ascend_min_pet_level from(PetTierTable row) {
            return new Ascend_min_pet_level(row.getAscendMinPetLevel());
        }
    }

    public record Bonus_entry_points(int value) {
        public static Bonus_entry_points from(PetTierTable row) {
            return new Bonus_entry_points(row.getBonusEntryPoints());
        }
    }

}