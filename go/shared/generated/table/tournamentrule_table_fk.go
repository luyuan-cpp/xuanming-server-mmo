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

// GetTournamentRuleSaluteItemIdRow resolves TournamentRule.salute_item_id -> Item row.
func GetTournamentRuleSaluteItemIdRow(row *pb.TournamentRuleTable) (*pb.ItemTable, bool) {
    return ItemTableManagerInstance.FindById(row.SaluteItemId)
}

// GetTournamentRuleSaluteItemIdRowById resolves TournamentRule.salute_item_id -> Item row (by TournamentRule id).
func GetTournamentRuleSaluteItemIdRowById(tableId uint32) (*pb.ItemTable, bool) {
    row, ok := TournamentRuleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetTournamentRuleSaluteItemIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindTournamentRuleRowsByBattleConfigId returns all TournamentRule rows whose battle_config_id == key.
func FindTournamentRuleRowsByBattleConfigId(key uint32) []*pb.TournamentRuleTable {
    return TournamentRuleTableManagerInstance.GetByBattleConfigId(key)
}

// FindTournamentRuleRowsBySaluteItemId returns all TournamentRule rows whose salute_item_id == key.
func FindTournamentRuleRowsBySaluteItemId(key uint32) []*pb.TournamentRuleTable {
    return TournamentRuleTableManagerInstance.GetBySaluteItemId(key)
}
