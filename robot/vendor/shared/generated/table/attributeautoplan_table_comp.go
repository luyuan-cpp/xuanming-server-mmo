
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for AttributeAutoPlanTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type AttributeAutoPlanIdComp struct {
    Value uint32
}

type AttributeAutoPlanClass_idComp struct {
    Value uint32
}

type AttributeAutoPlanPool_idComp struct {
    Value uint32
}

type AttributeAutoPlanDescComp struct {
    Value string
}

type AttributeAutoPlanDimensionComp struct {
    Values []uint32
}

type AttributeAutoPlanWeightComp struct {
    Values []uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeAttributeAutoPlanIdComp(row *pb.AttributeAutoPlanTable) AttributeAutoPlanIdComp {
    return AttributeAutoPlanIdComp{Value: row.Id}
}

func MakeAttributeAutoPlanClass_idComp(row *pb.AttributeAutoPlanTable) AttributeAutoPlanClass_idComp {
    return AttributeAutoPlanClass_idComp{Value: row.ClassId}
}

func MakeAttributeAutoPlanPool_idComp(row *pb.AttributeAutoPlanTable) AttributeAutoPlanPool_idComp {
    return AttributeAutoPlanPool_idComp{Value: row.PoolId}
}

func MakeAttributeAutoPlanDescComp(row *pb.AttributeAutoPlanTable) AttributeAutoPlanDescComp {
    return AttributeAutoPlanDescComp{Value: row.Desc}
}

func MakeAttributeAutoPlanDimensionComp(row *pb.AttributeAutoPlanTable) AttributeAutoPlanDimensionComp {
    return AttributeAutoPlanDimensionComp{Values: row.Dimension}
}

func MakeAttributeAutoPlanWeightComp(row *pb.AttributeAutoPlanTable) AttributeAutoPlanWeightComp {
    return AttributeAutoPlanWeightComp{Values: row.Weight}
}

