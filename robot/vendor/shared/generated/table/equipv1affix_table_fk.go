package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for EquipV1AffixTable
// ---------------------------------------------------------------------------

// GetEquipV1AffixMaterialIdRow resolves EquipV1Affix.material_id -> Item row.
func GetEquipV1AffixMaterialIdRow(row *pb.EquipV1AffixTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.MaterialId)
}

// GetEquipV1AffixMaterialIdRowById resolves EquipV1Affix.material_id -> Item row (by EquipV1Affix id).
func GetEquipV1AffixMaterialIdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := EquipV1AffixTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetEquipV1AffixMaterialIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindEquipV1AffixRowsByMaterialId returns all EquipV1Affix rows whose material_id == key.
func FindEquipV1AffixRowsByMaterialId(key uint32) []*pb.EquipV1AffixTable {
    return EquipV1AffixTableManagerInstance.GetByMaterialId(key)
}
