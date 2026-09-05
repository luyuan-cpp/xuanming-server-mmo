
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for DungeonTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type DungeonIdComp struct {
    Value uint32
}

type DungeonScene_idComp struct {
    Value uint32
}

type DungeonMax_team_sizeComp struct {
    Value uint32
}

type DungeonTime_limitComp struct {
    Value uint32
}

type DungeonMonsterComp struct {
    Values []uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeDungeonIdComp(row *pb.DungeonTable) DungeonIdComp {
    return DungeonIdComp{Value: row.Id}
}

func MakeDungeonScene_idComp(row *pb.DungeonTable) DungeonScene_idComp {
    return DungeonScene_idComp{Value: row.SceneId}
}

func MakeDungeonMax_team_sizeComp(row *pb.DungeonTable) DungeonMax_team_sizeComp {
    return DungeonMax_team_sizeComp{Value: row.MaxTeamSize}
}

func MakeDungeonTime_limitComp(row *pb.DungeonTable) DungeonTime_limitComp {
    return DungeonTime_limitComp{Value: row.TimeLimit}
}

func MakeDungeonMonsterComp(row *pb.DungeonTable) DungeonMonsterComp {
    return DungeonMonsterComp{Values: row.Monster}
}

