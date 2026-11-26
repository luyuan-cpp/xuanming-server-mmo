package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for JianghuRuleTable
// ---------------------------------------------------------------------------

// GetJianghuRuleGoldWritItemIdRow resolves JianghuRule.gold_writ_item_id -> Item row.
func GetJianghuRuleGoldWritItemIdRow(row *pb.JianghuRuleTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.GoldWritItemId)
}

// GetJianghuRuleGoldWritItemIdRowById resolves JianghuRule.gold_writ_item_id -> Item row (by JianghuRule id).
func GetJianghuRuleGoldWritItemIdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := JianghuRuleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetJianghuRuleGoldWritItemIdRow(row)
}

// GetJianghuRuleClearItemIdRow resolves JianghuRule.clear_item_id -> Item row.
func GetJianghuRuleClearItemIdRow(row *pb.JianghuRuleTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.ClearItemId)
}

// GetJianghuRuleClearItemIdRowById resolves JianghuRule.clear_item_id -> Item row (by JianghuRule id).
func GetJianghuRuleClearItemIdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := JianghuRuleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetJianghuRuleClearItemIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindJianghuRuleRowsByGoldWritItemId returns all JianghuRule rows whose gold_writ_item_id == key.
func FindJianghuRuleRowsByGoldWritItemId(key uint32) []*pb.JianghuRuleTable {
    return JianghuRuleTableManagerInstance.GetByGoldWritItemId(key)
}

// FindJianghuRuleRowsByClearItemId returns all JianghuRule rows whose clear_item_id == key.
func FindJianghuRuleRowsByClearItemId(key uint32) []*pb.JianghuRuleTable {
    return JianghuRuleTableManagerInstance.GetByClearItemId(key)
}
