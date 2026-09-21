package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for GuildShopTable
// ---------------------------------------------------------------------------

// GetGuildShopItemIdRow resolves GuildShop.item_id -> Item row.
func GetGuildShopItemIdRow(row *pb.GuildShopTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.ItemId)
}

// GetGuildShopItemIdRowById resolves GuildShop.item_id -> Item row (by GuildShop id).
func GetGuildShopItemIdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := GuildShopTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetGuildShopItemIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindGuildShopRowsByItemId returns all GuildShop rows whose item_id == key.
func FindGuildShopRowsByItemId(key uint32) []*pb.GuildShopTable {
    return GuildShopTableManagerInstance.GetByItemId(key)
}
