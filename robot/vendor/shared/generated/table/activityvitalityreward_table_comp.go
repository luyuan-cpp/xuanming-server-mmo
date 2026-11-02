
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for ActivityVitalityRewardTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type ActivityVitalityRewardIdComp struct {
    Value uint32
}

type ActivityVitalityRewardRule_idComp struct {
    Value uint32
}

type ActivityVitalityRewardRequired_pointsComp struct {
    Value uint32
}

type ActivityVitalityRewardReward_idComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeActivityVitalityRewardIdComp(row *pb.ActivityVitalityRewardTable) ActivityVitalityRewardIdComp {
    return ActivityVitalityRewardIdComp{Value: row.Id}
}

func MakeActivityVitalityRewardRule_idComp(row *pb.ActivityVitalityRewardTable) ActivityVitalityRewardRule_idComp {
    return ActivityVitalityRewardRule_idComp{Value: row.RuleId}
}

func MakeActivityVitalityRewardRequired_pointsComp(row *pb.ActivityVitalityRewardTable) ActivityVitalityRewardRequired_pointsComp {
    return ActivityVitalityRewardRequired_pointsComp{Value: row.RequiredPoints}
}

func MakeActivityVitalityRewardReward_idComp(row *pb.ActivityVitalityRewardTable) ActivityVitalityRewardReward_idComp {
    return ActivityVitalityRewardReward_idComp{Value: row.RewardId}
}

