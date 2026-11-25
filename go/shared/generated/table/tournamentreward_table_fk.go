package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for TournamentRewardTable
// ---------------------------------------------------------------------------

// GetTournamentRewardItemIdsRows resolves TournamentReward.item_ids[] -> Item rows.
func GetTournamentRewardItemIdsRows(row *pb.TournamentRewardTable) []*pb.ItemTable {
    var result []*pb.ItemTable
    for _, id := range row.ItemIds {
        if r, ok := ItemTableManagerInstance.FindById(id); ok {
            result = append(result, r)
        }
    }
    return result
}

// GetTournamentRewardItemIdsRowsById resolves TournamentReward.item_ids[] -> Item rows (by TournamentReward id).
func GetTournamentRewardItemIdsRowsById(tableId uint32) []*pb.ItemTable {
    row, ok := TournamentRewardTableManagerInstance.FindById(tableId)
    if !ok {
        return nil
    }
    return GetTournamentRewardItemIdsRows(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------
