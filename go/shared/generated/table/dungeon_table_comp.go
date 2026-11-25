
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

type DungeonAllow_pet_captureComp struct {
    Value uint32
}

type DungeonBoss_trial_kindComp struct {
    Value uint32
}

type DungeonDifficultyComp struct {
    Value uint32
}

type DungeonRound_limitComp struct {
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

func MakeDungeonAllow_pet_captureComp(row *pb.DungeonTable) DungeonAllow_pet_captureComp {
    return DungeonAllow_pet_captureComp{Value: row.AllowPetCapture}
}

func MakeDungeonBoss_trial_kindComp(row *pb.DungeonTable) DungeonBoss_trial_kindComp {
    return DungeonBoss_trial_kindComp{Value: row.BossTrialKind}
}

func MakeDungeonDifficultyComp(row *pb.DungeonTable) DungeonDifficultyComp {
    return DungeonDifficultyComp{Value: row.Difficulty}
}

func MakeDungeonRound_limitComp(row *pb.DungeonTable) DungeonRound_limitComp {
    return DungeonRound_limitComp{Value: row.RoundLimit}
}

func MakeDungeonMonsterComp(row *pb.DungeonTable) DungeonMonsterComp {
    return DungeonMonsterComp{Values: row.Monster}
}

