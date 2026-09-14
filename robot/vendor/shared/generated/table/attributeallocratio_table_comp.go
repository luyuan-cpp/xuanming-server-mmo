
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for AttributeAllocRatioTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type AttributeAllocRatioIdComp struct {
    Value uint32
}

type AttributeAllocRatioDimension_idComp struct {
    Value uint32
}

type AttributeAllocRatioClass_idComp struct {
    Value uint32
}

type AttributeAllocRatioMax_healthComp struct {
    Value float64
}

type AttributeAllocRatioMax_manaComp struct {
    Value float64
}

type AttributeAllocRatioPhysical_attackComp struct {
    Value float64
}

type AttributeAllocRatioMagic_attackComp struct {
    Value float64
}

type AttributeAllocRatioSpeedComp struct {
    Value float64
}

type AttributeAllocRatioDefenseComp struct {
    Value float64
}

type AttributeAllocRatioDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeAttributeAllocRatioIdComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioIdComp {
    return AttributeAllocRatioIdComp{Value: row.Id}
}

func MakeAttributeAllocRatioDimension_idComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioDimension_idComp {
    return AttributeAllocRatioDimension_idComp{Value: row.DimensionId}
}

func MakeAttributeAllocRatioClass_idComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioClass_idComp {
    return AttributeAllocRatioClass_idComp{Value: row.ClassId}
}

func MakeAttributeAllocRatioMax_healthComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioMax_healthComp {
    return AttributeAllocRatioMax_healthComp{Value: row.MaxHealth}
}

func MakeAttributeAllocRatioMax_manaComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioMax_manaComp {
    return AttributeAllocRatioMax_manaComp{Value: row.MaxMana}
}

func MakeAttributeAllocRatioPhysical_attackComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioPhysical_attackComp {
    return AttributeAllocRatioPhysical_attackComp{Value: row.PhysicalAttack}
}

func MakeAttributeAllocRatioMagic_attackComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioMagic_attackComp {
    return AttributeAllocRatioMagic_attackComp{Value: row.MagicAttack}
}

func MakeAttributeAllocRatioSpeedComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioSpeedComp {
    return AttributeAllocRatioSpeedComp{Value: row.Speed}
}

func MakeAttributeAllocRatioDefenseComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioDefenseComp {
    return AttributeAllocRatioDefenseComp{Value: row.Defense}
}

func MakeAttributeAllocRatioDescComp(row *pb.AttributeAllocRatioTable) AttributeAllocRatioDescComp {
    return AttributeAllocRatioDescComp{Value: row.Desc}
}

