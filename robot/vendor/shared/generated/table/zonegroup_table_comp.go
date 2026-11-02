
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for ZoneGroupTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type ZoneGroupIdComp struct {
    Value uint32
}

type ZoneGroupGroup_idComp struct {
    Value uint32
}

type ZoneGroupZone_beginComp struct {
    Value uint32
}

type ZoneGroupZone_endComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeZoneGroupIdComp(row *pb.ZoneGroupTable) ZoneGroupIdComp {
    return ZoneGroupIdComp{Value: row.Id}
}

func MakeZoneGroupGroup_idComp(row *pb.ZoneGroupTable) ZoneGroupGroup_idComp {
    return ZoneGroupGroup_idComp{Value: row.GroupId}
}

func MakeZoneGroupZone_beginComp(row *pb.ZoneGroupTable) ZoneGroupZone_beginComp {
    return ZoneGroupZone_beginComp{Value: row.ZoneBegin}
}

func MakeZoneGroupZone_endComp(row *pb.ZoneGroupTable) ZoneGroupZone_endComp {
    return ZoneGroupZone_endComp{Value: row.ZoneEnd}
}

