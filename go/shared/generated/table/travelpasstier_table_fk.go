package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for TravelPassTierTable
// ---------------------------------------------------------------------------

// GetTravelPassTierPassIdRow resolves TravelPassTier.pass_id -> TravelPassSeason row.
func GetTravelPassTierPassIdRow(row *pb.TravelPassTierTable) (*pb.TravelPassSeasonTable, bool) {
    return TravelPassSeasonTableManagerInstance.FindById(row.PassId)
}

// GetTravelPassTierPassIdRowById resolves TravelPassTier.pass_id -> TravelPassSeason row (by TravelPassTier id).
func GetTravelPassTierPassIdRowById(tableId uint32) (*pb.TravelPassSeasonTable, bool) {
    row, ok := TravelPassTierTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetTravelPassTierPassIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindTravelPassTierRowsByPassId returns all TravelPassTier rows whose pass_id == key.
func FindTravelPassTierRowsByPassId(key uint32) []*pb.TravelPassTierTable {
    return TravelPassTierTableManagerInstance.GetByPassId(key)
}
