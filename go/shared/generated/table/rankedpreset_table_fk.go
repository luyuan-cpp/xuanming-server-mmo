package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for RankedPresetTable
// ---------------------------------------------------------------------------

// GetRankedPresetPetTableIdRow resolves RankedPreset.pet_table_id -> Pet row.
func GetRankedPresetPetTableIdRow(row *pb.RankedPresetTable) (*pb.PetTable, bool) {
    return PetTableManagerInstance.FindById(row.PetTableId)
}

// GetRankedPresetPetTableIdRowById resolves RankedPreset.pet_table_id -> Pet row (by RankedPreset id).
func GetRankedPresetPetTableIdRowById(tableId uint32) (*pb.PetTable, bool) {
    row, ok := RankedPresetTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetRankedPresetPetTableIdRow(row)
}

// GetRankedPresetSkillIdsRows resolves RankedPreset.skill_ids[] -> Skill rows.
func GetRankedPresetSkillIdsRows(row *pb.RankedPresetTable) []*pb.SkillTable {
    var result []*pb.SkillTable
    for _, id := range row.SkillIds {
        if r, ok := SkillTableManagerInstance.FindById(id); ok {
            result = append(result, r)
        }
    }
    return result
}

// GetRankedPresetSkillIdsRowsById resolves RankedPreset.skill_ids[] -> Skill rows (by RankedPreset id).
func GetRankedPresetSkillIdsRowsById(tableId uint32) []*pb.SkillTable {
    row, ok := RankedPresetTableManagerInstance.FindById(tableId)
    if !ok {
        return nil
    }
    return GetRankedPresetSkillIdsRows(row)
}

// GetRankedPresetPetEntryIdsRows resolves RankedPreset.pet_entry_ids[] -> PetEntry rows.
func GetRankedPresetPetEntryIdsRows(row *pb.RankedPresetTable) []*pb.PetEntryTable {
    var result []*pb.PetEntryTable
    for _, id := range row.PetEntryIds {
        if r, ok := PetEntryTableManagerInstance.FindById(id); ok {
            result = append(result, r)
        }
    }
    return result
}

// GetRankedPresetPetEntryIdsRowsById resolves RankedPreset.pet_entry_ids[] -> PetEntry rows (by RankedPreset id).
func GetRankedPresetPetEntryIdsRowsById(tableId uint32) []*pb.PetEntryTable {
    row, ok := RankedPresetTableManagerInstance.FindById(tableId)
    if !ok {
        return nil
    }
    return GetRankedPresetPetEntryIdsRows(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindRankedPresetRowsByPetTableId returns all RankedPreset rows whose pet_table_id == key.
func FindRankedPresetRowsByPetTableId(key uint32) []*pb.RankedPresetTable {
    return RankedPresetTableManagerInstance.GetByPetTableId(key)
}
