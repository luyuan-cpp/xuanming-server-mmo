package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for PlayModeTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class PlayModeTableForeignKeys {
    private PlayModeTableForeignKeys() {}

    /** Resolve PlayMode.battle_config_id -> Dungeon row. */
    public static DungeonTable getBattleConfigIdRow(PlayModeTable row) {
        return DungeonTableManager.getInstance().findById(row.getBattleConfigId());
    }

    /** Resolve PlayMode.battle_config_id -> Dungeon row (by PlayMode id). */
    public static DungeonTable getBattleConfigIdRow(int tableId) {
        PlayModeTable row = PlayModeTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getBattleConfigIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all PlayMode rows whose battle_config_id == key. */
    public static List<PlayModeTable> findRowsByBattleConfigId(int key) {
        return PlayModeTableManager.getInstance().getByBattleConfigId(key);
    }

}
