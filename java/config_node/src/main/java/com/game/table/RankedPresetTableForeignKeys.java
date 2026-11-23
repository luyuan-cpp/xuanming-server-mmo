package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for RankedPresetTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class RankedPresetTableForeignKeys {
    private RankedPresetTableForeignKeys() {}

    /** Resolve RankedPreset.pet_table_id -> Pet row. */
    public static PetTable getPetTableIdRow(RankedPresetTable row) {
        return PetTableManager.getInstance().findById(row.getPetTableId());
    }

    /** Resolve RankedPreset.pet_table_id -> Pet row (by RankedPreset id). */
    public static PetTable getPetTableIdRow(int tableId) {
        RankedPresetTable row = RankedPresetTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getPetTableIdRow(row);
    }

    /** Resolve RankedPreset.skill_ids[] -> Skill rows. */
    public static List<SkillTable> getSkillIdsRows(RankedPresetTable row) {
        List<SkillTable> result = new ArrayList<>();
        for (int id : row.getSkillIdsList()) {
            SkillTable r = SkillTableManager.getInstance().findById(id);
            if (r != null) { result.add(r); }
        }
        return result;
    }

    /** Resolve RankedPreset.skill_ids[] -> Skill rows (by RankedPreset id). */
    public static List<SkillTable> getSkillIdsRows(int tableId) {
        RankedPresetTable row = RankedPresetTableManager.getInstance().findById(tableId);
        if (row == null) { return List.of(); }
        return getSkillIdsRows(row);
    }

    /** Resolve RankedPreset.pet_entry_ids[] -> PetEntry rows. */
    public static List<PetEntryTable> getPetEntryIdsRows(RankedPresetTable row) {
        List<PetEntryTable> result = new ArrayList<>();
        for (int id : row.getPetEntryIdsList()) {
            PetEntryTable r = PetEntryTableManager.getInstance().findById(id);
            if (r != null) { result.add(r); }
        }
        return result;
    }

    /** Resolve RankedPreset.pet_entry_ids[] -> PetEntry rows (by RankedPreset id). */
    public static List<PetEntryTable> getPetEntryIdsRows(int tableId) {
        RankedPresetTable row = RankedPresetTableManager.getInstance().findById(tableId);
        if (row == null) { return List.of(); }
        return getPetEntryIdsRows(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all RankedPreset rows whose pet_table_id == key. */
    public static List<RankedPresetTable> findRowsByPetTableId(int key) {
        return RankedPresetTableManager.getInstance().getByPetTableId(key);
    }

}
