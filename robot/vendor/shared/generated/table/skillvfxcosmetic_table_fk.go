package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for SkillVfxCosmeticTable
// ---------------------------------------------------------------------------

// GetSkillVfxCosmeticSkillTableIdRow resolves SkillVfxCosmetic.skill_table_id -> Skill row.
func GetSkillVfxCosmeticSkillTableIdRow(row *pb.SkillVfxCosmeticTable) (*pb.SkillTable, bool) {
    return SkillTableManagerInstance.FindById(row.SkillTableId)
}

// GetSkillVfxCosmeticSkillTableIdRowById resolves SkillVfxCosmetic.skill_table_id -> Skill row (by SkillVfxCosmetic id).
func GetSkillVfxCosmeticSkillTableIdRowById(tableId uint32) (*pb.SkillTable, bool) {
    row, ok := SkillVfxCosmeticTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetSkillVfxCosmeticSkillTableIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindSkillVfxCosmeticRowsBySkillTableId returns all SkillVfxCosmetic rows whose skill_table_id == key.
func FindSkillVfxCosmeticRowsBySkillTableId(key uint32) []*pb.SkillVfxCosmeticTable {
    return SkillVfxCosmeticTableManagerInstance.GetBySkillTableId(key)
}
