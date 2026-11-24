package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for CharacterAppearanceTable
// ---------------------------------------------------------------------------

// GetCharacterAppearanceSeriesIdRow resolves CharacterAppearance.series_id -> AppearanceSeries row.
func GetCharacterAppearanceSeriesIdRow(row *pb.CharacterAppearanceTable) (*pb.AppearanceSeriesTable, bool) {
    return AppearanceSeriesTableManagerInstance.FindById(row.SeriesId)
}

// GetCharacterAppearanceSeriesIdRowById resolves CharacterAppearance.series_id -> AppearanceSeries row (by CharacterAppearance id).
func GetCharacterAppearanceSeriesIdRowById(tableId uint32) (*pb.AppearanceSeriesTable, bool) {
    row, ok := CharacterAppearanceTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetCharacterAppearanceSeriesIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindCharacterAppearanceRowsBySeriesId returns all CharacterAppearance rows whose series_id == key.
func FindCharacterAppearanceRowsBySeriesId(key uint32) []*pb.CharacterAppearanceTable {
    return CharacterAppearanceTableManagerInstance.GetBySeriesId(key)
}
