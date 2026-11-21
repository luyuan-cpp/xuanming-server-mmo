package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for AppearanceRuleTable
// ---------------------------------------------------------------------------

// GetAppearanceRuleCardItemIdRow resolves AppearanceRule.card_item_id -> Item row.
func GetAppearanceRuleCardItemIdRow(row *pb.AppearanceRuleTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.CardItemId)
}

// GetAppearanceRuleCardItemIdRowById resolves AppearanceRule.card_item_id -> Item row (by AppearanceRule id).
func GetAppearanceRuleCardItemIdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := AppearanceRuleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetAppearanceRuleCardItemIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindAppearanceRuleRowsByCardItemId returns all AppearanceRule rows whose card_item_id == key.
func FindAppearanceRuleRowsByCardItemId(key uint32) []*pb.AppearanceRuleTable {
    return AppearanceRuleTableManagerInstance.GetByCardItemId(key)
}
