#include "travelpasstier_table_fk.h"
#include "travelpasstier_table.h"
#include "travelpassseason_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for TravelPassTierTable
// ---------------------------------------------------------------------------

/// Resolve TravelPassTier.pass_id -> TravelPassSeason row.
const TravelPassSeasonTable* GetTravelPassTierPassIdRow(const TravelPassTierTable& row) {
    auto [ptr, _] = TravelPassSeasonTableManager::Instance().FindByIdSilent(row.pass_id());
    return ptr;
}

/// Resolve TravelPassTier.pass_id -> TravelPassSeason row (by TravelPassTier id).
const TravelPassSeasonTable* GetTravelPassTierPassIdRow(uint32_t tableId) {
    auto [row, _] = TravelPassTierTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetTravelPassTierPassIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all TravelPassTier rows whose pass_id == key.
const std::vector<const TravelPassTierTable*>& FindTravelPassTierRowsByPassId(uint32_t key) {
    return TravelPassTierTableManager::Instance().GetByPassId(key);
}
