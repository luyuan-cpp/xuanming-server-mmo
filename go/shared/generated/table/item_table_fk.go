package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for ItemTable
// ---------------------------------------------------------------------------

// GetItemAffixRuleRow resolves Item.affix_rule -> EquipAffixRule row.
func GetItemAffixRuleRow(row *pb.ItemTable) (*pb.EquipAffixRuleTable, bool) {
    return EquipAffixRuleTableManagerInstance.FindById(row.AffixRule)
}

// GetItemAffixRuleRowById resolves Item.affix_rule -> EquipAffixRule row (by Item id).
func GetItemAffixRuleRowById(tableId uint32) (*pb.EquipAffixRuleTable, bool) {
    row, ok := ItemTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetItemAffixRuleRow(row)
}

// GetItemForgeMaterialIdRow resolves Item.forge_material_id -> Item row.
func GetItemForgeMaterialIdRow(row *pb.ItemTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.ForgeMaterialId)
}

// GetItemForgeMaterialIdRowById resolves Item.forge_material_id -> Item row (by Item id).
func GetItemForgeMaterialIdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := ItemTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetItemForgeMaterialIdRow(row)
}

// GetItemArtifactIdRow resolves Item.artifact_id -> EquipArtifact row.
func GetItemArtifactIdRow(row *pb.ItemTable) (*pb.EquipArtifactTable, bool) {
    return EquipArtifactTableManagerInstance.FindById(row.ArtifactId)
}

// GetItemArtifactIdRowById resolves Item.artifact_id -> EquipArtifact row (by Item id).
func GetItemArtifactIdRowById(tableId uint32) (*pb.EquipArtifactTable, bool) {
    row, ok := ItemTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetItemArtifactIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindItemRowsByAffixRule returns all Item rows whose affix_rule == key.
func FindItemRowsByAffixRule(key uint32) []*pb.ItemTable {
    return ItemTableManagerInstance.GetByAffixRule(key)
}

// FindItemRowsByForgeMaterialId returns all Item rows whose forge_material_id == key.
func FindItemRowsByForgeMaterialId(key uint32) []*pb.ItemTable {
    return ItemTableManagerInstance.GetByForgeMaterialId(key)
}

// FindItemRowsByArtifactId returns all Item rows whose artifact_id == key.
func FindItemRowsByArtifactId(key uint32) []*pb.ItemTable {
    return ItemTableManagerInstance.GetByArtifactId(key)
}
