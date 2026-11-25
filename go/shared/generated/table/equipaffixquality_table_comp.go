
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for EquipAffixQualityTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type EquipAffixQualityIdComp struct {
    Value uint32
}

type EquipAffixQualityNameComp struct {
    Value string
}

type EquipAffixQualityRatio_pctComp struct {
    Value uint32
}

type EquipAffixQualityApprentice_weightComp struct {
    Value uint32
}

type EquipAffixQualityArtisan_weightComp struct {
    Value uint32
}

type EquipAffixQualityMaster_weightComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeEquipAffixQualityIdComp(row *pb.EquipAffixQualityTable) EquipAffixQualityIdComp {
    return EquipAffixQualityIdComp{Value: row.Id}
}

func MakeEquipAffixQualityNameComp(row *pb.EquipAffixQualityTable) EquipAffixQualityNameComp {
    return EquipAffixQualityNameComp{Value: row.Name}
}

func MakeEquipAffixQualityRatio_pctComp(row *pb.EquipAffixQualityTable) EquipAffixQualityRatio_pctComp {
    return EquipAffixQualityRatio_pctComp{Value: row.RatioPct}
}

func MakeEquipAffixQualityApprentice_weightComp(row *pb.EquipAffixQualityTable) EquipAffixQualityApprentice_weightComp {
    return EquipAffixQualityApprentice_weightComp{Value: row.ApprenticeWeight}
}

func MakeEquipAffixQualityArtisan_weightComp(row *pb.EquipAffixQualityTable) EquipAffixQualityArtisan_weightComp {
    return EquipAffixQualityArtisan_weightComp{Value: row.ArtisanWeight}
}

func MakeEquipAffixQualityMaster_weightComp(row *pb.EquipAffixQualityTable) EquipAffixQualityMaster_weightComp {
    return EquipAffixQualityMaster_weightComp{Value: row.MasterWeight}
}

