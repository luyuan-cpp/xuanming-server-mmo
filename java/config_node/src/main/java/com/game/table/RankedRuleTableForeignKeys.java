package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for RankedRuleTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class RankedRuleTableForeignKeys {
    private RankedRuleTableForeignKeys() {}

    /** Resolve RankedRule.battle_config_id -> Dungeon row. */
    public static DungeonTable getBattleConfigIdRow(RankedRuleTable row) {
        return DungeonTableManager.getInstance().findById(row.getBattleConfigId());
    }

    /** Resolve RankedRule.battle_config_id -> Dungeon row (by RankedRule id). */
    public static DungeonTable getBattleConfigIdRow(int tableId) {
        RankedRuleTable row = RankedRuleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getBattleConfigIdRow(row);
    }

    /** Resolve RankedRule.reference_class_id -> Class row. */
    public static ClassTable getReferenceClassIdRow(RankedRuleTable row) {
        return ClassTableManager.getInstance().findById(row.getReferenceClassId());
    }

    /** Resolve RankedRule.reference_class_id -> Class row (by RankedRule id). */
    public static ClassTable getReferenceClassIdRow(int tableId) {
        RankedRuleTable row = RankedRuleTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getReferenceClassIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all RankedRule rows whose battle_config_id == key. */
    public static List<RankedRuleTable> findRowsByBattleConfigId(int key) {
        return RankedRuleTableManager.getInstance().getByBattleConfigId(key);
    }

    /** Reverse FK: find all RankedRule rows whose reference_class_id == key. */
    public static List<RankedRuleTable> findRowsByReferenceClassId(int key) {
        return RankedRuleTableManager.getInstance().getByReferenceClassId(key);
    }

}
