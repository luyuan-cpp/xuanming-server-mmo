package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for TournamentRuleTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class TournamentRuleTableForeignKeys {
    private TournamentRuleTableForeignKeys() {}

    /** Resolve TournamentRule.battle_config_id -> Dungeon row. */
    public static DungeonTable getBattleConfigIdRow(TournamentRuleTable row) {
        return DungeonTableManager.getInstance().findById(row.getBattleConfigId());
    }

    /** Resolve TournamentRule.battle_config_id -> Dungeon row (by TournamentRule id). */
    public static DungeonTable getBattleConfigIdRow(int tableId) {
        TournamentRuleTable row = TournamentRuleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getBattleConfigIdRow(row);
    }

    /** Resolve TournamentRule.salute_item_id -> Item row. */
    public static ItemTable getSaluteItemIdRow(TournamentRuleTable row) {
        return ItemTableManager.getInstance().findById(row.getSaluteItemId());
    }

    /** Resolve TournamentRule.salute_item_id -> Item row (by TournamentRule id). */
    public static ItemTable getSaluteItemIdRow(int tableId) {
        TournamentRuleTable row = TournamentRuleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getSaluteItemIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all TournamentRule rows whose battle_config_id == key. */
    public static List<TournamentRuleTable> findRowsByBattleConfigId(int key) {
        return TournamentRuleTableManager.getInstance().getByBattleConfigId(key);
    }

    /** Reverse FK: find all TournamentRule rows whose salute_item_id == key. */
    public static List<TournamentRuleTable> findRowsBySaluteItemId(int key) {
        return TournamentRuleTableManager.getInstance().getBySaluteItemId(key);
    }

}
