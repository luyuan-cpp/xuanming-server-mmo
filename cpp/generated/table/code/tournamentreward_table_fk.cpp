#include "tournamentreward_table_fk.h"
#include "tournamentreward_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for TournamentRewardTable
// ---------------------------------------------------------------------------

/// Resolve TournamentReward.item_ids[] -> Item rows.
std::vector<const ItemTable*> GetTournamentRewardItemIdsRows(const TournamentRewardTable& row) {
    std::vector<const ItemTable*> result;
    for (auto id : row.item_ids()) {
        auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(id);
        if (ptr) result.push_back(ptr);
    }
    return result;
}

/// Resolve TournamentReward.item_ids[] -> Item rows (by TournamentReward id).
std::vector<const ItemTable*> GetTournamentRewardItemIdsRows(uint32_t tableId) {
    auto [row, _] = TournamentRewardTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return {};
    return GetTournamentRewardItemIdsRows(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------
