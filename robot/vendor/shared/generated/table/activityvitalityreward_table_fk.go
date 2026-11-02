package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for ActivityVitalityRewardTable
// ---------------------------------------------------------------------------

// GetActivityVitalityRewardRuleIdRow resolves ActivityVitalityReward.rule_id -> ActivityVitalityRule row.
func GetActivityVitalityRewardRuleIdRow(row *pb.ActivityVitalityRewardTable) (*pb.ActivityVitalityRuleTable, bool) {
    return ActivityVitalityRuleTableManagerInstance.FindById(row.RuleId)
}

// GetActivityVitalityRewardRuleIdRowById resolves ActivityVitalityReward.rule_id -> ActivityVitalityRule row (by ActivityVitalityReward id).
func GetActivityVitalityRewardRuleIdRowById(tableId uint32) (*pb.ActivityVitalityRuleTable, bool) {
    row, ok := ActivityVitalityRewardTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetActivityVitalityRewardRuleIdRow(row)
}

// GetActivityVitalityRewardRewardIdRow resolves ActivityVitalityReward.reward_id -> Reward row.
func GetActivityVitalityRewardRewardIdRow(row *pb.ActivityVitalityRewardTable) (*pb.RewardTable, bool) {
    return RewardTableManagerInstance.FindById(row.RewardId)
}

// GetActivityVitalityRewardRewardIdRowById resolves ActivityVitalityReward.reward_id -> Reward row (by ActivityVitalityReward id).
func GetActivityVitalityRewardRewardIdRowById(tableId uint32) (*pb.RewardTable, bool) {
    row, ok := ActivityVitalityRewardTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetActivityVitalityRewardRewardIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindActivityVitalityRewardRowsByRuleId returns all ActivityVitalityReward rows whose rule_id == key.
func FindActivityVitalityRewardRowsByRuleId(key uint32) []*pb.ActivityVitalityRewardTable {
    return ActivityVitalityRewardTableManagerInstance.GetByRuleId(key)
}

// FindActivityVitalityRewardRowsByRewardId returns all ActivityVitalityReward rows whose reward_id == key.
func FindActivityVitalityRewardRowsByRewardId(key uint32) []*pb.ActivityVitalityRewardTable {
    return ActivityVitalityRewardTableManagerInstance.GetByRewardId(key)
}
