
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ActivityVitalityRewardTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ActivityVitalityRewardTableComp {

    private ActivityVitalityRewardTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ActivityVitalityRewardTable row) {
            return new Id(row.getId());
        }
    }

    public record Rule_id(int value) {
        public static Rule_id from(ActivityVitalityRewardTable row) {
            return new Rule_id(row.getRuleId());
        }
    }

    public record Required_points(int value) {
        public static Required_points from(ActivityVitalityRewardTable row) {
            return new Required_points(row.getRequiredPoints());
        }
    }

    public record Reward_id(int value) {
        public static Reward_id from(ActivityVitalityRewardTable row) {
            return new Reward_id(row.getRewardId());
        }
    }

}