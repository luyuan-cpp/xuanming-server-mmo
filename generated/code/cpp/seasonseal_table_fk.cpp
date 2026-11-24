#include "seasonseal_table_fk.h"
#include "seasonseal_table.h"
#include "seasonrule_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for SeasonSealTable
// ---------------------------------------------------------------------------

/// Resolve SeasonSeal.season_id -> SeasonRule row.
const SeasonRuleTable* GetSeasonSealSeasonIdRow(const SeasonSealTable& row) {
    auto [ptr, _] = SeasonRuleTableManager::Instance().FindByIdSilent(row.season_id());
    return ptr;
}

/// Resolve SeasonSeal.season_id -> SeasonRule row (by SeasonSeal id).
const SeasonRuleTable* GetSeasonSealSeasonIdRow(uint32_t tableId) {
    auto [row, _] = SeasonSealTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetSeasonSealSeasonIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all SeasonSeal rows whose season_id == key.
const std::vector<const SeasonSealTable*>& FindSeasonSealRowsBySeasonId(uint32_t key) {
    return SeasonSealTableManager::Instance().GetBySeasonId(key);
}
