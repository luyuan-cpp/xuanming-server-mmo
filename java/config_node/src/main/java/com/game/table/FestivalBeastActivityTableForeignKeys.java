package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for FestivalBeastActivityTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class FestivalBeastActivityTableForeignKeys {
    private FestivalBeastActivityTableForeignKeys() {}

    /** Resolve FestivalBeastActivity.battle_config_id -> Dungeon row. */
    public static DungeonTable getBattleConfigIdRow(FestivalBeastActivityTable row) {
        return DungeonTableManager.getInstance().findById(row.getBattleConfigId());
    }

    /** Resolve FestivalBeastActivity.battle_config_id -> Dungeon row (by FestivalBeastActivity id). */
    public static DungeonTable getBattleConfigIdRow(int tableId) {
        FestivalBeastActivityTable row = FestivalBeastActivityTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getBattleConfigIdRow(row);
    }

    /** Resolve FestivalBeastActivity.reward_pet_table_id -> Pet row. */
    public static PetTable getRewardPetTableIdRow(FestivalBeastActivityTable row) {
        return PetTableManager.getInstance().findById(row.getRewardPetTableId());
    }

    /** Resolve FestivalBeastActivity.reward_pet_table_id -> Pet row (by FestivalBeastActivity id). */
    public static PetTable getRewardPetTableIdRow(int tableId) {
        FestivalBeastActivityTable row = FestivalBeastActivityTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getRewardPetTableIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all FestivalBeastActivity rows whose battle_config_id == key. */
    public static List<FestivalBeastActivityTable> findRowsByBattleConfigId(int key) {
        return FestivalBeastActivityTableManager.getInstance().getByBattleConfigId(key);
    }

    /** Reverse FK: find all FestivalBeastActivity rows whose reward_pet_table_id == key. */
    public static List<FestivalBeastActivityTable> findRowsByRewardPetTableId(int key) {
        return FestivalBeastActivityTableManager.getInstance().getByRewardPetTableId(key);
    }

}
