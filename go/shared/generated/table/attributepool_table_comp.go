
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for AttributePoolTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type AttributePoolIdComp struct {
    Value uint32
}

type AttributePoolNameComp struct {
    Value string
}

type AttributePoolUnlock_levelComp struct {
    Value uint32
}

type AttributePoolPoints_per_levelComp struct {
    Value uint32
}

type AttributePoolBase_pointsComp struct {
    Value uint32
}

type AttributePoolDimension_capComp struct {
    Value uint32
}

type AttributePoolReset_cost_goldComp struct {
    Value uint64
}

type AttributePoolReset_free_below_levelComp struct {
    Value uint32
}

type AttributePoolDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeAttributePoolIdComp(row *pb.AttributePoolTable) AttributePoolIdComp {
    return AttributePoolIdComp{Value: row.Id}
}

func MakeAttributePoolNameComp(row *pb.AttributePoolTable) AttributePoolNameComp {
    return AttributePoolNameComp{Value: row.Name}
}

func MakeAttributePoolUnlock_levelComp(row *pb.AttributePoolTable) AttributePoolUnlock_levelComp {
    return AttributePoolUnlock_levelComp{Value: row.UnlockLevel}
}

func MakeAttributePoolPoints_per_levelComp(row *pb.AttributePoolTable) AttributePoolPoints_per_levelComp {
    return AttributePoolPoints_per_levelComp{Value: row.PointsPerLevel}
}

func MakeAttributePoolBase_pointsComp(row *pb.AttributePoolTable) AttributePoolBase_pointsComp {
    return AttributePoolBase_pointsComp{Value: row.BasePoints}
}

func MakeAttributePoolDimension_capComp(row *pb.AttributePoolTable) AttributePoolDimension_capComp {
    return AttributePoolDimension_capComp{Value: row.DimensionCap}
}

func MakeAttributePoolReset_cost_goldComp(row *pb.AttributePoolTable) AttributePoolReset_cost_goldComp {
    return AttributePoolReset_cost_goldComp{Value: row.ResetCostGold}
}

func MakeAttributePoolReset_free_below_levelComp(row *pb.AttributePoolTable) AttributePoolReset_free_below_levelComp {
    return AttributePoolReset_free_below_levelComp{Value: row.ResetFreeBelowLevel}
}

func MakeAttributePoolDescComp(row *pb.AttributePoolTable) AttributePoolDescComp {
    return AttributePoolDescComp{Value: row.Desc}
}

