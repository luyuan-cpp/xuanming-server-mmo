package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for SkillScrollCraftTable
// ---------------------------------------------------------------------------

// GetSkillScrollCraftFragmentItemIdRow resolves SkillScrollCraft.fragment_item_id -> Item row.
func GetSkillScrollCraftFragmentItemIdRow(row *pb.SkillScrollCraftTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.FragmentItemId)
}

// GetSkillScrollCraftFragmentItemIdRowById resolves SkillScrollCraft.fragment_item_id -> Item row (by SkillScrollCraft id).
func GetSkillScrollCraftFragmentItemIdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := SkillScrollCraftTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetSkillScrollCraftFragmentItemIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindSkillScrollCraftRowsByFragmentItemId returns all SkillScrollCraft rows whose fragment_item_id == key.
func FindSkillScrollCraftRowsByFragmentItemId(key uint32) []*pb.SkillScrollCraftTable {
    return SkillScrollCraftTableManagerInstance.GetByFragmentItemId(key)
}
