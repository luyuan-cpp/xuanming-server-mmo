#pragma once
#include <vector>
#include "attributedimension_table.h"

#include "attributepool_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for AttributeDimensionTable
// ---------------------------------------------------------------------------

/// Resolve AttributeDimension.pool_id -> AttributePool row.
inline const AttributePoolTable* GetAttributeDimensionPoolIdRow(const AttributeDimensionTable& row) {
    auto [ptr, _] = AttributePoolTableManager::Instance().FindByIdSilent(row.pool_id());
    return ptr;
}

/// Resolve AttributeDimension.pool_id -> AttributePool row (by AttributeDimension id).
inline const AttributePoolTable* GetAttributeDimensionPoolIdRow(uint32_t tableId) {
    auto [row, _] = AttributeDimensionTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetAttributeDimensionPoolIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

/// Reverse FK: find all AttributeDimension rows whose pool_id == key.
inline const std::vector<const AttributeDimensionTable*>& FindAttributeDimensionRowsByPoolId(uint32_t key) {
    return AttributeDimensionTableManager::Instance().GetByPoolId(key);
}
