package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for SpiritShopGoodsTable
// ---------------------------------------------------------------------------

// GetSpiritShopGoodsItemId_1Row resolves SpiritShopGoods.item_id_1 -> Item row.
func GetSpiritShopGoodsItemId_1Row(row *pb.SpiritShopGoodsTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.ItemId_1)
}

// GetSpiritShopGoodsItemId_1RowById resolves SpiritShopGoods.item_id_1 -> Item row (by SpiritShopGoods id).
func GetSpiritShopGoodsItemId_1RowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := SpiritShopGoodsTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetSpiritShopGoodsItemId_1Row(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindSpiritShopGoodsRowsByItemId_1 returns all SpiritShopGoods rows whose item_id_1 == key.
func FindSpiritShopGoodsRowsByItemId_1(key uint32) []*pb.SpiritShopGoodsTable {
    return SpiritShopGoodsTableManagerInstance.GetByItemId_1(key)
}
