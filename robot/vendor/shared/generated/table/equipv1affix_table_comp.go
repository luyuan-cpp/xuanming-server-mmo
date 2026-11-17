
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for EquipV1AffixTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type EquipV1AffixIdComp struct {
    Value uint32
}

type EquipV1AffixPoolComp struct {
    Value uint32
}

type EquipV1AffixEffectComp struct {
    Value uint32
}

type EquipV1AffixEffect_paramComp struct {
    Value uint32
}

type EquipV1AffixValue_permyriadComp struct {
    Value uint32
}

type EquipV1AffixSource_capComp struct {
    Value uint32
}

type EquipV1AffixNameComp struct {
    Value string
}

type EquipV1AffixDescriptionComp struct {
    Value string
}

type EquipV1AffixMaterial_idComp struct {
    Value uint32
}

type EquipV1AffixMaterial_countComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeEquipV1AffixIdComp(row *pb.EquipV1AffixTable) EquipV1AffixIdComp {
    return EquipV1AffixIdComp{Value: row.Id}
}

func MakeEquipV1AffixPoolComp(row *pb.EquipV1AffixTable) EquipV1AffixPoolComp {
    return EquipV1AffixPoolComp{Value: row.Pool}
}

func MakeEquipV1AffixEffectComp(row *pb.EquipV1AffixTable) EquipV1AffixEffectComp {
    return EquipV1AffixEffectComp{Value: row.Effect}
}

func MakeEquipV1AffixEffect_paramComp(row *pb.EquipV1AffixTable) EquipV1AffixEffect_paramComp {
    return EquipV1AffixEffect_paramComp{Value: row.EffectParam}
}

func MakeEquipV1AffixValue_permyriadComp(row *pb.EquipV1AffixTable) EquipV1AffixValue_permyriadComp {
    return EquipV1AffixValue_permyriadComp{Value: row.ValuePermyriad}
}

func MakeEquipV1AffixSource_capComp(row *pb.EquipV1AffixTable) EquipV1AffixSource_capComp {
    return EquipV1AffixSource_capComp{Value: row.SourceCap}
}

func MakeEquipV1AffixNameComp(row *pb.EquipV1AffixTable) EquipV1AffixNameComp {
    return EquipV1AffixNameComp{Value: row.Name}
}

func MakeEquipV1AffixDescriptionComp(row *pb.EquipV1AffixTable) EquipV1AffixDescriptionComp {
    return EquipV1AffixDescriptionComp{Value: row.Description}
}

func MakeEquipV1AffixMaterial_idComp(row *pb.EquipV1AffixTable) EquipV1AffixMaterial_idComp {
    return EquipV1AffixMaterial_idComp{Value: row.MaterialId}
}

func MakeEquipV1AffixMaterial_countComp(row *pb.EquipV1AffixTable) EquipV1AffixMaterial_countComp {
    return EquipV1AffixMaterial_countComp{Value: row.MaterialCount}
}

