package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for SkillVfxCosmeticTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class SkillVfxCosmeticTableForeignKeys {
    private SkillVfxCosmeticTableForeignKeys() {}

    /** Resolve SkillVfxCosmetic.skill_table_id -> Skill row. */
    public static SkillTable getSkillTableIdRow(SkillVfxCosmeticTable row) {
        return SkillTableManager.getInstance().findById(row.getSkillTableId());
    }

    /** Resolve SkillVfxCosmetic.skill_table_id -> Skill row (by SkillVfxCosmetic id). */
    public static SkillTable getSkillTableIdRow(int tableId) {
        SkillVfxCosmeticTable row = SkillVfxCosmeticTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getSkillTableIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all SkillVfxCosmetic rows whose skill_table_id == key. */
    public static List<SkillVfxCosmeticTable> findRowsBySkillTableId(int key) {
        return SkillVfxCosmeticTableManager.getInstance().getBySkillTableId(key);
    }

}
