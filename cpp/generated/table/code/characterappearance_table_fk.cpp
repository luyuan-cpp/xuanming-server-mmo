#include "characterappearance_table_fk.h"
#include "characterappearance_table.h"
#include "appearanceseries_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for CharacterAppearanceTable
// ---------------------------------------------------------------------------

/// Resolve CharacterAppearance.series_id -> AppearanceSeries row.
const AppearanceSeriesTable* GetCharacterAppearanceSeriesIdRow(const CharacterAppearanceTable& row) {
    auto [ptr, _] = AppearanceSeriesTableManager::Instance().FindByIdSilent(row.series_id());
    return ptr;
}

/// Resolve CharacterAppearance.series_id -> AppearanceSeries row (by CharacterAppearance id).
const AppearanceSeriesTable* GetCharacterAppearanceSeriesIdRow(uint32_t tableId) {
    auto [row, _] = CharacterAppearanceTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetCharacterAppearanceSeriesIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all CharacterAppearance rows whose series_id == key.
const std::vector<const CharacterAppearanceTable*>& FindCharacterAppearanceRowsBySeriesId(uint32_t key) {
    return CharacterAppearanceTableManager::Instance().GetBySeriesId(key);
}
