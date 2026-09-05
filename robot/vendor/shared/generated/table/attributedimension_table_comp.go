
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for AttributeDimensionTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type AttributeDimensionIdComp struct {
    Value uint32
}

type AttributeDimensionPool_idComp struct {
    Value uint32
}

type AttributeDimensionNameComp struct {
    Value string
}

type AttributeDimensionDescComp struct {
    Value string
}

type AttributeDimensionSortComp struct {
    Value uint32
}

type AttributeDimensionBase_per_levelComp struct {
    Value uint32
}

type AttributeDimensionMax_healthComp struct {
    Value float64
}

type AttributeDimensionMax_manaComp struct {
    Value float64
}

type AttributeDimensionPhysical_attackComp struct {
    Value float64
}

type AttributeDimensionMagic_attackComp struct {
    Value float64
}

type AttributeDimensionSpeedComp struct {
    Value float64
}

type AttributeDimensionDefenseComp struct {
    Value float64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeAttributeDimensionIdComp(row *pb.AttributeDimensionTable) AttributeDimensionIdComp {
    return AttributeDimensionIdComp{Value: row.Id}
}

func MakeAttributeDimensionPool_idComp(row *pb.AttributeDimensionTable) AttributeDimensionPool_idComp {
    return AttributeDimensionPool_idComp{Value: row.PoolId}
}

func MakeAttributeDimensionNameComp(row *pb.AttributeDimensionTable) AttributeDimensionNameComp {
    return AttributeDimensionNameComp{Value: row.Name}
}

func MakeAttributeDimensionDescComp(row *pb.AttributeDimensionTable) AttributeDimensionDescComp {
    return AttributeDimensionDescComp{Value: row.Desc}
}

func MakeAttributeDimensionSortComp(row *pb.AttributeDimensionTable) AttributeDimensionSortComp {
    return AttributeDimensionSortComp{Value: row.Sort}
}

func MakeAttributeDimensionBase_per_levelComp(row *pb.AttributeDimensionTable) AttributeDimensionBase_per_levelComp {
    return AttributeDimensionBase_per_levelComp{Value: row.BasePerLevel}
}

func MakeAttributeDimensionMax_healthComp(row *pb.AttributeDimensionTable) AttributeDimensionMax_healthComp {
    return AttributeDimensionMax_healthComp{Value: row.MaxHealth}
}

func MakeAttributeDimensionMax_manaComp(row *pb.AttributeDimensionTable) AttributeDimensionMax_manaComp {
    return AttributeDimensionMax_manaComp{Value: row.MaxMana}
}

func MakeAttributeDimensionPhysical_attackComp(row *pb.AttributeDimensionTable) AttributeDimensionPhysical_attackComp {
    return AttributeDimensionPhysical_attackComp{Value: row.PhysicalAttack}
}

func MakeAttributeDimensionMagic_attackComp(row *pb.AttributeDimensionTable) AttributeDimensionMagic_attackComp {
    return AttributeDimensionMagic_attackComp{Value: row.MagicAttack}
}

func MakeAttributeDimensionSpeedComp(row *pb.AttributeDimensionTable) AttributeDimensionSpeedComp {
    return AttributeDimensionSpeedComp{Value: row.Speed}
}

func MakeAttributeDimensionDefenseComp(row *pb.AttributeDimensionTable) AttributeDimensionDefenseComp {
    return AttributeDimensionDefenseComp{Value: row.Defense}
}

