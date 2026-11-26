
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for BattleProgressionRewardTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type BattleProgressionRewardIdComp struct {
    Value uint32
}

type BattleProgressionRewardMatch_modeComp struct {
    Value uint32
}

type BattleProgressionRewardDifficultyComp struct {
    Value uint32
}

type BattleProgressionRewardOutcome_tierComp struct {
    Value uint32
}

type BattleProgressionRewardExp_gainComp struct {
    Value uint64
}

type BattleProgressionRewardSilver_gainComp struct {
    Value uint64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeBattleProgressionRewardIdComp(row *pb.BattleProgressionRewardTable) BattleProgressionRewardIdComp {
    return BattleProgressionRewardIdComp{Value: row.Id}
}

func MakeBattleProgressionRewardMatch_modeComp(row *pb.BattleProgressionRewardTable) BattleProgressionRewardMatch_modeComp {
    return BattleProgressionRewardMatch_modeComp{Value: row.MatchMode}
}

func MakeBattleProgressionRewardDifficultyComp(row *pb.BattleProgressionRewardTable) BattleProgressionRewardDifficultyComp {
    return BattleProgressionRewardDifficultyComp{Value: row.Difficulty}
}

func MakeBattleProgressionRewardOutcome_tierComp(row *pb.BattleProgressionRewardTable) BattleProgressionRewardOutcome_tierComp {
    return BattleProgressionRewardOutcome_tierComp{Value: row.OutcomeTier}
}

func MakeBattleProgressionRewardExp_gainComp(row *pb.BattleProgressionRewardTable) BattleProgressionRewardExp_gainComp {
    return BattleProgressionRewardExp_gainComp{Value: row.ExpGain}
}

func MakeBattleProgressionRewardSilver_gainComp(row *pb.BattleProgressionRewardTable) BattleProgressionRewardSilver_gainComp {
    return BattleProgressionRewardSilver_gainComp{Value: row.SilverGain}
}

