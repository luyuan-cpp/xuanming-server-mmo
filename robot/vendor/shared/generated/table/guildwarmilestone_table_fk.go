package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for GuildWarMilestoneTable
// ---------------------------------------------------------------------------

// GetGuildWarMilestoneRewardItem1IdRow resolves GuildWarMilestone.reward_item1_id -> Item row.
func GetGuildWarMilestoneRewardItem1IdRow(row *pb.GuildWarMilestoneTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.RewardItem1Id)
}

// GetGuildWarMilestoneRewardItem1IdRowById resolves GuildWarMilestone.reward_item1_id -> Item row (by GuildWarMilestone id).
func GetGuildWarMilestoneRewardItem1IdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := GuildWarMilestoneTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetGuildWarMilestoneRewardItem1IdRow(row)
}

// GetGuildWarMilestoneRewardItem2IdRow resolves GuildWarMilestone.reward_item2_id -> Item row.
func GetGuildWarMilestoneRewardItem2IdRow(row *pb.GuildWarMilestoneTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.RewardItem2Id)
}

// GetGuildWarMilestoneRewardItem2IdRowById resolves GuildWarMilestone.reward_item2_id -> Item row (by GuildWarMilestone id).
func GetGuildWarMilestoneRewardItem2IdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := GuildWarMilestoneTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetGuildWarMilestoneRewardItem2IdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindGuildWarMilestoneRowsByRewardItem1Id returns all GuildWarMilestone rows whose reward_item1_id == key.
func FindGuildWarMilestoneRowsByRewardItem1Id(key uint32) []*pb.GuildWarMilestoneTable {
    return GuildWarMilestoneTableManagerInstance.GetByRewardItem1Id(key)
}

// FindGuildWarMilestoneRowsByRewardItem2Id returns all GuildWarMilestone rows whose reward_item2_id == key.
func FindGuildWarMilestoneRowsByRewardItem2Id(key uint32) []*pb.GuildWarMilestoneTable {
    return GuildWarMilestoneTableManagerInstance.GetByRewardItem2Id(key)
}
