package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for GuildWarMilestoneTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class GuildWarMilestoneTableForeignKeys {
    private GuildWarMilestoneTableForeignKeys() {}

    /** Resolve GuildWarMilestone.reward_item1_id -> Item row. */
    public static ItemTable getRewardItem1IdRow(GuildWarMilestoneTable row) {
        return ItemTableManager.getInstance().findById(row.getRewardItem1Id());
    }

    /** Resolve GuildWarMilestone.reward_item1_id -> Item row (by GuildWarMilestone id). */
    public static ItemTable getRewardItem1IdRow(int tableId) {
        GuildWarMilestoneTable row = GuildWarMilestoneTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getRewardItem1IdRow(row);
    }

    /** Resolve GuildWarMilestone.reward_item2_id -> Item row. */
    public static ItemTable getRewardItem2IdRow(GuildWarMilestoneTable row) {
        return ItemTableManager.getInstance().findById(row.getRewardItem2Id());
    }

    /** Resolve GuildWarMilestone.reward_item2_id -> Item row (by GuildWarMilestone id). */
    public static ItemTable getRewardItem2IdRow(int tableId) {
        GuildWarMilestoneTable row = GuildWarMilestoneTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getRewardItem2IdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all GuildWarMilestone rows whose reward_item1_id == key. */
    public static List<GuildWarMilestoneTable> findRowsByRewardItem1Id(int key) {
        return GuildWarMilestoneTableManager.getInstance().getByRewardItem1Id(key);
    }

    /** Reverse FK: find all GuildWarMilestone rows whose reward_item2_id == key. */
    public static List<GuildWarMilestoneTable> findRowsByRewardItem2Id(int key) {
        return GuildWarMilestoneTableManager.getInstance().getByRewardItem2Id(key);
    }

}
