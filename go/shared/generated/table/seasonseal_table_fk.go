package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for SeasonSealTable
// ---------------------------------------------------------------------------

// GetSeasonSealSeasonIdRow resolves SeasonSeal.season_id -> SeasonRule row.
func GetSeasonSealSeasonIdRow(row *pb.SeasonSealTable) (*pb.SeasonRuleTable, bool) {
    return SeasonRuleTableManagerInstance.FindById(row.SeasonId)
}

// GetSeasonSealSeasonIdRowById resolves SeasonSeal.season_id -> SeasonRule row (by SeasonSeal id).
func GetSeasonSealSeasonIdRowById(tableId uint32) (*pb.SeasonRuleTable, bool) {
    row, ok := SeasonSealTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetSeasonSealSeasonIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindSeasonSealRowsBySeasonId returns all SeasonSeal rows whose season_id == key.
func FindSeasonSealRowsBySeasonId(key uint32) []*pb.SeasonSealTable {
    return SeasonSealTableManagerInstance.GetBySeasonId(key)
}
