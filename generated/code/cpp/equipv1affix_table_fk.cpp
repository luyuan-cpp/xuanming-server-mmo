#include "equipv1affix_table_fk.h"
#include "equipv1affix_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for EquipV1AffixTable
// ---------------------------------------------------------------------------

/// Resolve EquipV1Affix.material_id -> Item row.
const ItemTable* GetEquipV1AffixMaterialIdRow(const EquipV1AffixTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.material_id());
    return ptr;
}

/// Resolve EquipV1Affix.material_id -> Item row (by EquipV1Affix id).
const ItemTable* GetEquipV1AffixMaterialIdRow(uint32_t tableId) {
    auto [row, _] = EquipV1AffixTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetEquipV1AffixMaterialIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all EquipV1Affix rows whose material_id == key.
const std::vector<const EquipV1AffixTable*>& FindEquipV1AffixRowsByMaterialId(uint32_t key) {
    return EquipV1AffixTableManager::Instance().GetByMaterialId(key);
}
