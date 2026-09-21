#include "guildshop_table_fk.h"
#include "guildshop_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for GuildShopTable
// ---------------------------------------------------------------------------

/// Resolve GuildShop.item_id -> Item row.
const ItemTable* GetGuildShopItemIdRow(const GuildShopTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.item_id());
    return ptr;
}

/// Resolve GuildShop.item_id -> Item row (by GuildShop id).
const ItemTable* GetGuildShopItemIdRow(uint32_t tableId) {
    auto [row, _] = GuildShopTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetGuildShopItemIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all GuildShop rows whose item_id == key.
const std::vector<const GuildShopTable*>& FindGuildShopRowsByItemId(uint32_t key) {
    return GuildShopTableManager::Instance().GetByItemId(key);
}
