package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for GuildActivityTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class GuildActivityTableForeignKeys {
    private GuildActivityTableForeignKeys() {}

    /** Resolve GuildActivity.reward_id -> Reward row. */
    public static RewardTable getRewardIdRow(GuildActivityTable row) {
        return RewardTableManager.getInstance().findById(row.getRewardId());
    }

    /** Resolve GuildActivity.reward_id -> Reward row (by GuildActivity id). */
    public static RewardTable getRewardIdRow(int tableId) {
        GuildActivityTable row = GuildActivityTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getRewardIdRow(row);
    }

    /** Resolve GuildActivity.dungeon_id -> Dungeon row. */
    public static DungeonTable getDungeonIdRow(GuildActivityTable row) {
        return DungeonTableManager.getInstance().findById(row.getDungeonId());
    }

    /** Resolve GuildActivity.dungeon_id -> Dungeon row (by GuildActivity id). */
    public static DungeonTable getDungeonIdRow(int tableId) {
        GuildActivityTable row = GuildActivityTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getDungeonIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all GuildActivity rows whose reward_id == key. */
    public static List<GuildActivityTable> findRowsByRewardId(int key) {
        return GuildActivityTableManager.getInstance().getByRewardId(key);
    }

    /** Reverse FK: find all GuildActivity rows whose dungeon_id == key. */
    public static List<GuildActivityTable> findRowsByDungeonId(int key) {
        return GuildActivityTableManager.getInstance().getByDungeonId(key);
    }

}
