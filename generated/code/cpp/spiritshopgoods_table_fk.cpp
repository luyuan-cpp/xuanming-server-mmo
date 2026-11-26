#include "spiritshopgoods_table_fk.h"
#include "spiritshopgoods_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for SpiritShopGoodsTable
// ---------------------------------------------------------------------------

/// Resolve SpiritShopGoods.item_id_1 -> Item row.
const ItemTable* GetSpiritShopGoodsItemId1Row(const SpiritShopGoodsTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.item_id_1());
    return ptr;
}

/// Resolve SpiritShopGoods.item_id_1 -> Item row (by SpiritShopGoods id).
const ItemTable* GetSpiritShopGoodsItemId1Row(uint32_t tableId) {
    auto [row, _] = SpiritShopGoodsTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetSpiritShopGoodsItemId1Row(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all SpiritShopGoods rows whose item_id_1 == key.
const std::vector<const SpiritShopGoodsTable*>& FindSpiritShopGoodsRowsByItemId1(uint32_t key) {
    return SpiritShopGoodsTableManager::Instance().GetByItemId1(key);
}
