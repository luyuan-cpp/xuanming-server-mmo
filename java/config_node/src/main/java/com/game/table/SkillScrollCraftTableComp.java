
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for SkillScrollCraftTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class SkillScrollCraftTableComp {

    private SkillScrollCraftTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(SkillScrollCraftTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(SkillScrollCraftTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(SkillScrollCraftTable row) {
            return new Description(row.getDescription());
        }
    }

    public record Rarity_tier(int value) {
        public static Rarity_tier from(SkillScrollCraftTable row) {
            return new Rarity_tier(row.getRarityTier());
        }
    }

    public record Fragment_item_id(int value) {
        public static Fragment_item_id from(SkillScrollCraftTable row) {
            return new Fragment_item_id(row.getFragmentItemId());
        }
    }

    public record Random_fragment_count(int value) {
        public static Random_fragment_count from(SkillScrollCraftTable row) {
            return new Random_fragment_count(row.getRandomFragmentCount());
        }
    }

    public record Random_gold_cost(long value) {
        public static Random_gold_cost from(SkillScrollCraftTable row) {
            return new Random_gold_cost(row.getRandomGoldCost());
        }
    }

    public record Targeted_fragment_count(int value) {
        public static Targeted_fragment_count from(SkillScrollCraftTable row) {
            return new Targeted_fragment_count(row.getTargetedFragmentCount());
        }
    }

    public record Targeted_gold_cost(long value) {
        public static Targeted_gold_cost from(SkillScrollCraftTable row) {
            return new Targeted_gold_cost(row.getTargetedGoldCost());
        }
    }

    public record Dismantle_fragment_count(int value) {
        public static Dismantle_fragment_count from(SkillScrollCraftTable row) {
            return new Dismantle_fragment_count(row.getDismantleFragmentCount());
        }
    }

    public record Rare_scroll_item_id(int value) {
        public static Rare_scroll_item_id from(SkillScrollCraftTable row) {
            return new Rare_scroll_item_id(row.getRareScrollItemId());
        }
    }

    public record Rare_scroll_count(int value) {
        public static Rare_scroll_count from(SkillScrollCraftTable row) {
            return new Rare_scroll_count(row.getRareScrollCount());
        }
    }

    public record Rare_ink_item_id(int value) {
        public static Rare_ink_item_id from(SkillScrollCraftTable row) {
            return new Rare_ink_item_id(row.getRareInkItemId());
        }
    }

    public record Rare_ink_count(int value) {
        public static Rare_ink_count from(SkillScrollCraftTable row) {
            return new Rare_ink_count(row.getRareInkCount());
        }
    }

    public record Rare_gold_cost(long value) {
        public static Rare_gold_cost from(SkillScrollCraftTable row) {
            return new Rare_gold_cost(row.getRareGoldCost());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(SkillScrollCraftTable row) {
            return new Enabled(row.getEnabled());
        }
    }

}