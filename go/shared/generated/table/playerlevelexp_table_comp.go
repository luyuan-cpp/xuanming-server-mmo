
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for PlayerLevelExpTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type PlayerLevelExpIdComp struct {
    Value uint32
}

type PlayerLevelExpRequired_total_expComp struct {
    Value uint64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakePlayerLevelExpIdComp(row *pb.PlayerLevelExpTable) PlayerLevelExpIdComp {
    return PlayerLevelExpIdComp{Value: row.Id}
}

func MakePlayerLevelExpRequired_total_expComp(row *pb.PlayerLevelExpTable) PlayerLevelExpRequired_total_expComp {
    return PlayerLevelExpRequired_total_expComp{Value: row.RequiredTotalExp}
}

