package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for GuildWarRuleTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class GuildWarRuleTableForeignKeys {
    private GuildWarRuleTableForeignKeys() {}

    /** Resolve GuildWarRule.battle_config_id -> Dungeon row. */
    public static DungeonTable getBattleConfigIdRow(GuildWarRuleTable row) {
        return DungeonTableManager.getInstance().findById(row.getBattleConfigId());
    }

    /** Resolve GuildWarRule.battle_config_id -> Dungeon row (by GuildWarRule id). */
    public static DungeonTable getBattleConfigIdRow(int tableId) {
        GuildWarRuleTable row = GuildWarRuleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getBattleConfigIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all GuildWarRule rows whose battle_config_id == key. */
    public static List<GuildWarRuleTable> findRowsByBattleConfigId(int key) {
        return GuildWarRuleTableManager.getInstance().getByBattleConfigId(key);
    }

}
