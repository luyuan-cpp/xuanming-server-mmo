
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for EquipEnhanceTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class EquipEnhanceTableComp {

    private EquipEnhanceTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(EquipEnhanceTable row) {
            return new Id(row.getId());
        }
    }

    public record Bonus_permyriad(int value) {
        public static Bonus_permyriad from(EquipEnhanceTable row) {
            return new Bonus_permyriad(row.getBonusPermyriad());
        }
    }

    public record Success_rate(int value) {
        public static Success_rate from(EquipEnhanceTable row) {
            return new Success_rate(row.getSuccessRate());
        }
    }

    public record Fail_level_loss(int value) {
        public static Fail_level_loss from(EquipEnhanceTable row) {
            return new Fail_level_loss(row.getFailLevelLoss());
        }
    }

    public record Cost_gold(long value) {
        public static Cost_gold from(EquipEnhanceTable row) {
            return new Cost_gold(row.getCostGold());
        }
    }

    public record Material_id(int value) {
        public static Material_id from(EquipEnhanceTable row) {
            return new Material_id(row.getMaterialId());
        }
    }

    public record Material_count(int value) {
        public static Material_count from(EquipEnhanceTable row) {
            return new Material_count(row.getMaterialCount());
        }
    }

    public record Profile_id(int value) {
        public static Profile_id from(EquipEnhanceTable row) {
            return new Profile_id(row.getProfileId());
        }
    }

    public record Level(int value) {
        public static Level from(EquipEnhanceTable row) {
            return new Level(row.getLevel());
        }
    }

    public record Pity_limit(int value) {
        public static Pity_limit from(EquipEnhanceTable row) {
            return new Pity_limit(row.getPityLimit());
        }
    }

}