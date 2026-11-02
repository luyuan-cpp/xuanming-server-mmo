package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for PlayModeTable
// ---------------------------------------------------------------------------

// GetPlayModeBattleConfigIdRow resolves PlayMode.battle_config_id -> Dungeon row.
func GetPlayModeBattleConfigIdRow(row *pb.PlayModeTable) (*pb.DungeonTable, bool) {
    return DungeonTableManagerInstance.FindById(row.BattleConfigId)
}

// GetPlayModeBattleConfigIdRowById resolves PlayMode.battle_config_id -> Dungeon row (by PlayMode id).
func GetPlayModeBattleConfigIdRowById(tableId uint32) (*pb.DungeonTable, bool) {
    row, ok := PlayModeTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetPlayModeBattleConfigIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindPlayModeRowsByBattleConfigId returns all PlayMode rows whose battle_config_id == key.
func FindPlayModeRowsByBattleConfigId(key uint32) []*pb.PlayModeTable {
    return PlayModeTableManagerInstance.GetByBattleConfigId(key)
}
