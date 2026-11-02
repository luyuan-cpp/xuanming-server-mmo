
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for ActivityScheduleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type ActivityScheduleIdComp struct {
    Value uint32
}

type ActivityScheduleEnabledComp struct {
    Value bool
}

type ActivityScheduleBaseline_start_at_msComp struct {
    Value uint64
}

type ActivityScheduleBaseline_end_at_msComp struct {
    Value uint64
}

type ActivityScheduleDefinition_keyComp struct {
    Value string
}

type ActivityScheduleConfiguration_revisionComp struct {
    Value uint64
}

type ActivityScheduleCategoryComp struct {
    Value uint32
}

type ActivityScheduleNameComp struct {
    Value string
}

type ActivityScheduleDescriptionComp struct {
    Value string
}

type ActivityScheduleIcon_keyComp struct {
    Value string
}

type ActivityScheduleVitality_pointsComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeActivityScheduleIdComp(row *pb.ActivityScheduleTable) ActivityScheduleIdComp {
    return ActivityScheduleIdComp{Value: row.Id}
}

func MakeActivityScheduleEnabledComp(row *pb.ActivityScheduleTable) ActivityScheduleEnabledComp {
    return ActivityScheduleEnabledComp{Value: row.Enabled}
}

func MakeActivityScheduleBaseline_start_at_msComp(row *pb.ActivityScheduleTable) ActivityScheduleBaseline_start_at_msComp {
    return ActivityScheduleBaseline_start_at_msComp{Value: row.BaselineStartAtMs}
}

func MakeActivityScheduleBaseline_end_at_msComp(row *pb.ActivityScheduleTable) ActivityScheduleBaseline_end_at_msComp {
    return ActivityScheduleBaseline_end_at_msComp{Value: row.BaselineEndAtMs}
}

func MakeActivityScheduleDefinition_keyComp(row *pb.ActivityScheduleTable) ActivityScheduleDefinition_keyComp {
    return ActivityScheduleDefinition_keyComp{Value: row.DefinitionKey}
}

func MakeActivityScheduleConfiguration_revisionComp(row *pb.ActivityScheduleTable) ActivityScheduleConfiguration_revisionComp {
    return ActivityScheduleConfiguration_revisionComp{Value: row.ConfigurationRevision}
}

func MakeActivityScheduleCategoryComp(row *pb.ActivityScheduleTable) ActivityScheduleCategoryComp {
    return ActivityScheduleCategoryComp{Value: row.Category}
}

func MakeActivityScheduleNameComp(row *pb.ActivityScheduleTable) ActivityScheduleNameComp {
    return ActivityScheduleNameComp{Value: row.Name}
}

func MakeActivityScheduleDescriptionComp(row *pb.ActivityScheduleTable) ActivityScheduleDescriptionComp {
    return ActivityScheduleDescriptionComp{Value: row.Description}
}

func MakeActivityScheduleIcon_keyComp(row *pb.ActivityScheduleTable) ActivityScheduleIcon_keyComp {
    return ActivityScheduleIcon_keyComp{Value: row.IconKey}
}

func MakeActivityScheduleVitality_pointsComp(row *pb.ActivityScheduleTable) ActivityScheduleVitality_pointsComp {
    return ActivityScheduleVitality_pointsComp{Value: row.VitalityPoints}
}

