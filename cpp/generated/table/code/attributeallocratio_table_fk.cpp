#include "attributeallocratio_table_fk.h"
#include "attributeallocratio_table.h"
#include "attributedimension_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for AttributeAllocRatioTable
// ---------------------------------------------------------------------------

/// Resolve AttributeAllocRatio.dimension_id -> AttributeDimension row.
const AttributeDimensionTable* GetAttributeAllocRatioDimensionIdRow(const AttributeAllocRatioTable& row) {
    auto [ptr, _] = AttributeDimensionTableManager::Instance().FindByIdSilent(row.dimension_id());
    return ptr;
}

/// Resolve AttributeAllocRatio.dimension_id -> AttributeDimension row (by AttributeAllocRatio id).
const AttributeDimensionTable* GetAttributeAllocRatioDimensionIdRow(uint32_t tableId) {
    auto [row, _] = AttributeAllocRatioTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetAttributeAllocRatioDimensionIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all AttributeAllocRatio rows whose dimension_id == key.
const std::vector<const AttributeAllocRatioTable*>& FindAttributeAllocRatioRowsByDimensionId(uint32_t key) {
    return AttributeAllocRatioTableManager::Instance().GetByDimensionId(key);
}
