package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for GuildWarRuleTable
// ---------------------------------------------------------------------------

// GetGuildWarRuleBattleConfigIdRow resolves GuildWarRule.battle_config_id -> Dungeon row.
func GetGuildWarRuleBattleConfigIdRow(row *pb.GuildWarRuleTable) (*pb.DungeonTable, bool) {
    return DungeonTableManagerInstance.FindById(row.BattleConfigId)
}

// GetGuildWarRuleBattleConfigIdRowById resolves GuildWarRule.battle_config_id -> Dungeon row (by GuildWarRule id).
func GetGuildWarRuleBattleConfigIdRowById(tableId uint32) (*pb.DungeonTable, bool) {
    row, ok := GuildWarRuleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetGuildWarRuleBattleConfigIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindGuildWarRuleRowsByBattleConfigId returns all GuildWarRule rows whose battle_config_id == key.
func FindGuildWarRuleRowsByBattleConfigId(key uint32) []*pb.GuildWarRuleTable {
    return GuildWarRuleTableManagerInstance.GetByBattleConfigId(key)
}
