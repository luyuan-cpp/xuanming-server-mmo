
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for PetEntryTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type PetEntryIdComp struct {
    Value uint32
}

type PetEntryNameComp struct {
    Value string
}

type PetEntryDescComp struct {
    Value string
}

type PetEntryCategoryComp struct {
    Value uint32
}

type PetEntryChannelComp struct {
    Value uint32
}

type PetEntryState_kindComp struct {
    Value uint32
}

type PetEntryCapComp struct {
    Value uint32
}

type PetEntryPer_point_bpsComp struct {
    Value uint32
}

type PetEntrySortComp struct {
    Value uint32
}

type PetEntryIconComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakePetEntryIdComp(row *pb.PetEntryTable) PetEntryIdComp {
    return PetEntryIdComp{Value: row.Id}
}

func MakePetEntryNameComp(row *pb.PetEntryTable) PetEntryNameComp {
    return PetEntryNameComp{Value: row.Name}
}

func MakePetEntryDescComp(row *pb.PetEntryTable) PetEntryDescComp {
    return PetEntryDescComp{Value: row.Desc}
}

func MakePetEntryCategoryComp(row *pb.PetEntryTable) PetEntryCategoryComp {
    return PetEntryCategoryComp{Value: row.Category}
}

func MakePetEntryChannelComp(row *pb.PetEntryTable) PetEntryChannelComp {
    return PetEntryChannelComp{Value: row.Channel}
}

func MakePetEntryState_kindComp(row *pb.PetEntryTable) PetEntryState_kindComp {
    return PetEntryState_kindComp{Value: row.StateKind}
}

func MakePetEntryCapComp(row *pb.PetEntryTable) PetEntryCapComp {
    return PetEntryCapComp{Value: row.Cap}
}

func MakePetEntryPer_point_bpsComp(row *pb.PetEntryTable) PetEntryPer_point_bpsComp {
    return PetEntryPer_point_bpsComp{Value: row.PerPointBps}
}

func MakePetEntrySortComp(row *pb.PetEntryTable) PetEntrySortComp {
    return PetEntrySortComp{Value: row.Sort}
}

func MakePetEntryIconComp(row *pb.PetEntryTable) PetEntryIconComp {
    return PetEntryIconComp{Value: row.Icon}
}

