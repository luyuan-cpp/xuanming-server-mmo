#include "mission_table_fk.h"
#include "mission_table.h"
#include "condition_table.h"
#include "reward_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for MissionTable
// ---------------------------------------------------------------------------

/// Resolve Mission.reward_id -> Reward row.
const RewardTable* GetMissionRewardIdRow(const MissionTable& row) {
    auto [ptr, _] = RewardTableManager::Instance().FindByIdSilent(row.reward_id());
    return ptr;
}

/// Resolve Mission.reward_id -> Reward row (by Mission id).
const RewardTable* GetMissionRewardIdRow(uint32_t tableId) {
    auto [row, _] = MissionTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetMissionRewardIdRow(*row);
}

/// Resolve Mission.condition_id[] -> Condition rows.
std::vector<const ConditionTable*> GetMissionConditionIdRows(const MissionTable& row) {
    std::vector<const ConditionTable*> result;
    for (auto id : row.condition_id()) {
        auto [ptr, _] = ConditionTableManager::Instance().FindByIdSilent(id);
        if (ptr) result.push_back(ptr);
    }
    return result;
}

/// Resolve Mission.condition_id[] -> Condition rows (by Mission id).
std::vector<const ConditionTable*> GetMissionConditionIdRows(uint32_t tableId) {
    auto [row, _] = MissionTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return {};
    return GetMissionConditionIdRows(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all Mission rows whose reward_id == key.
const std::vector<const MissionTable*>& FindMissionRowsByRewardId(uint32_t key) {
    return MissionTableManager::Instance().GetByRewardId(key);
}
