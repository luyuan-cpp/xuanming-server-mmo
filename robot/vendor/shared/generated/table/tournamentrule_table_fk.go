package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for TournamentRuleTable
// ---------------------------------------------------------------------------

// GetTournamentRuleBattleConfigIdRow resolves TournamentRule.battle_config_id -> Dungeon row.
func GetTournamentRuleBattleConfigIdRow(row *pb.TournamentRuleTable) (*pb.DungeonTable, bool) {
    return DungeonTableManagerInstance.FindById(row.BattleConfigId)
}

// GetTournamentRuleBattleConfigIdRowById resolves TournamentRule.battle_config_id -> Dungeon row (by TournamentRule id).
func GetTournamentRuleBattleConfigIdRowById(tableId uint32) (*pb.DungeonTable, bool) {
    row, ok := TournamentRuleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetTournamentRuleBattleConfigIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindTournamentRuleRowsByBattleConfigId returns all TournamentRule rows whose battle_config_id == key.
func FindTournamentRuleRowsByBattleConfigId(key uint32) []*pb.TournamentRuleTable {
    return TournamentRuleTableManagerInstance.GetByBattleConfigId(key)
}
