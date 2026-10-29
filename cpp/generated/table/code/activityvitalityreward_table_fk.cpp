#include "activityvitalityreward_table_fk.h"
#include "activityvitalityreward_table.h"
#include "activityvitalityrule_table.h"
#include "reward_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for ActivityVitalityRewardTable
// ---------------------------------------------------------------------------

/// Resolve ActivityVitalityReward.rule_id -> ActivityVitalityRule row.
const ActivityVitalityRuleTable* GetActivityVitalityRewardRuleIdRow(const ActivityVitalityRewardTable& row) {
    auto [ptr, _] = ActivityVitalityRuleTableManager::Instance().FindByIdSilent(row.rule_id());
    return ptr;
}

/// Resolve ActivityVitalityReward.rule_id -> ActivityVitalityRule row (by ActivityVitalityReward id).
const ActivityVitalityRuleTable* GetActivityVitalityRewardRuleIdRow(uint32_t tableId) {
    auto [row, _] = ActivityVitalityRewardTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetActivityVitalityRewardRuleIdRow(*row);
}

/// Resolve ActivityVitalityReward.reward_id -> Reward row.
const RewardTable* GetActivityVitalityRewardRewardIdRow(const ActivityVitalityRewardTable& row) {
    auto [ptr, _] = RewardTableManager::Instance().FindByIdSilent(row.reward_id());
    return ptr;
}

/// Resolve ActivityVitalityReward.reward_id -> Reward row (by ActivityVitalityReward id).
const RewardTable* GetActivityVitalityRewardRewardIdRow(uint32_t tableId) {
    auto [row, _] = ActivityVitalityRewardTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetActivityVitalityRewardRewardIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all ActivityVitalityReward rows whose rule_id == key.
const std::vector<const ActivityVitalityRewardTable*>& FindActivityVitalityRewardRowsByRuleId(uint32_t key) {
    return ActivityVitalityRewardTableManager::Instance().GetByRuleId(key);
}

/// Reverse FK: find all ActivityVitalityReward rows whose reward_id == key.
const std::vector<const ActivityVitalityRewardTable*>& FindActivityVitalityRewardRowsByRewardId(uint32_t key) {
    return ActivityVitalityRewardTableManager::Instance().GetByRewardId(key);
}
