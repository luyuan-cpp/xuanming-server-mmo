package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for ActivityVitalityRewardTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class ActivityVitalityRewardTableForeignKeys {
    private ActivityVitalityRewardTableForeignKeys() {}

    /** Resolve ActivityVitalityReward.rule_id -> ActivityVitalityRule row. */
    public static ActivityVitalityRuleTable getRuleIdRow(ActivityVitalityRewardTable row) {
        return ActivityVitalityRuleTableManager.getInstance().findById(row.getRuleId());
    }

    /** Resolve ActivityVitalityReward.rule_id -> ActivityVitalityRule row (by ActivityVitalityReward id). */
    public static ActivityVitalityRuleTable getRuleIdRow(int tableId) {
        ActivityVitalityRewardTable row = ActivityVitalityRewardTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getRuleIdRow(row);
    }

    /** Resolve ActivityVitalityReward.reward_id -> Reward row. */
    public static RewardTable getRewardIdRow(ActivityVitalityRewardTable row) {
        return RewardTableManager.getInstance().findById(row.getRewardId());
    }

    /** Resolve ActivityVitalityReward.reward_id -> Reward row (by ActivityVitalityReward id). */
    public static RewardTable getRewardIdRow(int tableId) {
        ActivityVitalityRewardTable row = ActivityVitalityRewardTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getRewardIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all ActivityVitalityReward rows whose rule_id == key. */
    public static List<ActivityVitalityRewardTable> findRowsByRuleId(int key) {
        return ActivityVitalityRewardTableManager.getInstance().getByRuleId(key);
    }

    /** Reverse FK: find all ActivityVitalityReward rows whose reward_id == key. */
    public static List<ActivityVitalityRewardTable> findRowsByRewardId(int key) {
        return ActivityVitalityRewardTableManager.getInstance().getByRewardId(key);
    }

}
