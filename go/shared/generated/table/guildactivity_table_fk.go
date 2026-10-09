package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for GuildActivityTable
// ---------------------------------------------------------------------------

// GetGuildActivityRewardIdRow resolves GuildActivity.reward_id -> Reward row.
func GetGuildActivityRewardIdRow(row *pb.GuildActivityTable) (*pb.RewardTable, bool) {
    return RewardTableManagerInstance.FindById(row.RewardId)
}

// GetGuildActivityRewardIdRowById resolves GuildActivity.reward_id -> Reward row (by GuildActivity id).
func GetGuildActivityRewardIdRowById(tableId uint32) (*pb.RewardTable, bool) {
    row, ok := GuildActivityTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetGuildActivityRewardIdRow(row)
}

// GetGuildActivityDungeonIdRow resolves GuildActivity.dungeon_id -> Dungeon row.
func GetGuildActivityDungeonIdRow(row *pb.GuildActivityTable) (*pb.DungeonTable, bool) {
    return DungeonTableManagerInstance.FindById(row.DungeonId)
}

// GetGuildActivityDungeonIdRowById resolves GuildActivity.dungeon_id -> Dungeon row (by GuildActivity id).
func GetGuildActivityDungeonIdRowById(tableId uint32) (*pb.DungeonTable, bool) {
    row, ok := GuildActivityTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetGuildActivityDungeonIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindGuildActivityRowsByRewardId returns all GuildActivity rows whose reward_id == key.
func FindGuildActivityRowsByRewardId(key uint32) []*pb.GuildActivityTable {
    return GuildActivityTableManagerInstance.GetByRewardId(key)
}

// FindGuildActivityRowsByDungeonId returns all GuildActivity rows whose dungeon_id == key.
func FindGuildActivityRowsByDungeonId(key uint32) []*pb.GuildActivityTable {
    return GuildActivityTableManagerInstance.GetByDungeonId(key)
}
