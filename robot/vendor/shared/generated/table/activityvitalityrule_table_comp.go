
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for ActivityVitalityRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type ActivityVitalityRuleIdComp struct {
    Value uint32
}

type ActivityVitalityRuleEnabledComp struct {
    Value bool
}

type ActivityVitalityRuleConfiguration_revisionComp struct {
    Value uint64
}

type ActivityVitalityRuleFirst_period_starts_at_msComp struct {
    Value uint64
}

type ActivityVitalityRulePeriod_duration_msComp struct {
    Value uint64
}

type ActivityVitalityRulePoints_limitComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeActivityVitalityRuleIdComp(row *pb.ActivityVitalityRuleTable) ActivityVitalityRuleIdComp {
    return ActivityVitalityRuleIdComp{Value: row.Id}
}

func MakeActivityVitalityRuleEnabledComp(row *pb.ActivityVitalityRuleTable) ActivityVitalityRuleEnabledComp {
    return ActivityVitalityRuleEnabledComp{Value: row.Enabled}
}

func MakeActivityVitalityRuleConfiguration_revisionComp(row *pb.ActivityVitalityRuleTable) ActivityVitalityRuleConfiguration_revisionComp {
    return ActivityVitalityRuleConfiguration_revisionComp{Value: row.ConfigurationRevision}
}

func MakeActivityVitalityRuleFirst_period_starts_at_msComp(row *pb.ActivityVitalityRuleTable) ActivityVitalityRuleFirst_period_starts_at_msComp {
    return ActivityVitalityRuleFirst_period_starts_at_msComp{Value: row.FirstPeriodStartsAtMs}
}

func MakeActivityVitalityRulePeriod_duration_msComp(row *pb.ActivityVitalityRuleTable) ActivityVitalityRulePeriod_duration_msComp {
    return ActivityVitalityRulePeriod_duration_msComp{Value: row.PeriodDurationMs}
}

func MakeActivityVitalityRulePoints_limitComp(row *pb.ActivityVitalityRuleTable) ActivityVitalityRulePoints_limitComp {
    return ActivityVitalityRulePoints_limitComp{Value: row.PointsLimit}
}

