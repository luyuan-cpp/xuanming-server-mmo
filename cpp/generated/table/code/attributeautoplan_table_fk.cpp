#include "attributeautoplan_table_fk.h"
#include "attributeautoplan_table.h"
#include "attributedimension_table.h"
#include "attributepool_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for AttributeAutoPlanTable
// ---------------------------------------------------------------------------

/// Resolve AttributeAutoPlan.pool_id -> AttributePool row.
const AttributePoolTable* GetAttributeAutoPlanPoolIdRow(const AttributeAutoPlanTable& row) {
    auto [ptr, _] = AttributePoolTableManager::Instance().FindByIdSilent(row.pool_id());
    return ptr;
}

/// Resolve AttributeAutoPlan.pool_id -> AttributePool row (by AttributeAutoPlan id).
const AttributePoolTable* GetAttributeAutoPlanPoolIdRow(uint32_t tableId) {
    auto [row, _] = AttributeAutoPlanTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetAttributeAutoPlanPoolIdRow(*row);
}

/// Resolve AttributeAutoPlan.dimension[] -> AttributeDimension rows.
std::vector<const AttributeDimensionTable*> GetAttributeAutoPlanDimensionRows(const AttributeAutoPlanTable& row) {
    std::vector<const AttributeDimensionTable*> result;
    for (auto id : row.dimension()) {
        auto [ptr, _] = AttributeDimensionTableManager::Instance().FindByIdSilent(id);
        if (ptr) result.push_back(ptr);
    }
    return result;
}

/// Resolve AttributeAutoPlan.dimension[] -> AttributeDimension rows (by AttributeAutoPlan id).
std::vector<const AttributeDimensionTable*> GetAttributeAutoPlanDimensionRows(uint32_t tableId) {
    auto [row, _] = AttributeAutoPlanTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return {};
    return GetAttributeAutoPlanDimensionRows(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all AttributeAutoPlan rows whose pool_id == key.
const std::vector<const AttributeAutoPlanTable*>& FindAttributeAutoPlanRowsByPoolId(uint32_t key) {
    return AttributeAutoPlanTableManager::Instance().GetByPoolId(key);
}
