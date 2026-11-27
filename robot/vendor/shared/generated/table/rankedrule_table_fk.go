package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for RankedRuleTable
// ---------------------------------------------------------------------------

// GetRankedRuleBattleConfigIdRow resolves RankedRule.battle_config_id -> Dungeon row.
func GetRankedRuleBattleConfigIdRow(row *pb.RankedRuleTable) (*pb.DungeonTable, bool) {
    return DungeonTableManagerInstance.FindById(row.BattleConfigId)
}

// GetRankedRuleBattleConfigIdRowById resolves RankedRule.battle_config_id -> Dungeon row (by RankedRule id).
func GetRankedRuleBattleConfigIdRowById(tableId uint32) (*pb.DungeonTable, bool) {
    row, ok := RankedRuleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetRankedRuleBattleConfigIdRow(row)
}

// GetRankedRuleReferenceClassIdRow resolves RankedRule.reference_class_id -> Class row.
func GetRankedRuleReferenceClassIdRow(row *pb.RankedRuleTable) (*pb.ClassTable, bool) {
    return ClassTableManagerInstance.FindById(row.ReferenceClassId)
}

// GetRankedRuleReferenceClassIdRowById resolves RankedRule.reference_class_id -> Class row (by RankedRule id).
func GetRankedRuleReferenceClassIdRowById(tableId uint32) (*pb.ClassTable, bool) {
    row, ok := RankedRuleTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetRankedRuleReferenceClassIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindRankedRuleRowsByBattleConfigId returns all RankedRule rows whose battle_config_id == key.
func FindRankedRuleRowsByBattleConfigId(key uint32) []*pb.RankedRuleTable {
    return RankedRuleTableManagerInstance.GetByBattleConfigId(key)
}

// FindRankedRuleRowsByReferenceClassId returns all RankedRule rows whose reference_class_id == key.
func FindRankedRuleRowsByReferenceClassId(key uint32) []*pb.RankedRuleTable {
    return RankedRuleTableManagerInstance.GetByReferenceClassId(key)
}
