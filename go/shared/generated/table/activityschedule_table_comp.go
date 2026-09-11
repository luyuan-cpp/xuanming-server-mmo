
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

