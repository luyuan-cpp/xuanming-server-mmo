
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for RankedPresetTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type RankedPresetIdComp struct {
    Value uint32
}

type RankedPresetRole_kindComp struct {
    Value uint32
}

type RankedPresetCodeComp struct {
    Value string
}

type RankedPresetNameComp struct {
    Value string
}

type RankedPresetRole_tagComp struct {
    Value string
}

type RankedPresetPet_table_idComp struct {
    Value uint32
}

type RankedPresetWeight_conComp struct {
    Value uint32
}

type RankedPresetWeight_spiComp struct {
    Value uint32
}

type RankedPresetWeight_strComp struct {
    Value uint32
}

type RankedPresetWeight_agiComp struct {
    Value uint32
}

type RankedPresetSortComp struct {
    Value uint32
}

type RankedPresetDescComp struct {
    Value string
}

type RankedPresetSkill_idsComp struct {
    Values []uint32
}

type RankedPresetPet_entry_idsComp struct {
    Values []uint32
}

type RankedPresetPet_entry_pointsComp struct {
    Values []uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeRankedPresetIdComp(row *pb.RankedPresetTable) RankedPresetIdComp {
    return RankedPresetIdComp{Value: row.Id}
}

func MakeRankedPresetRole_kindComp(row *pb.RankedPresetTable) RankedPresetRole_kindComp {
    return RankedPresetRole_kindComp{Value: row.RoleKind}
}

func MakeRankedPresetCodeComp(row *pb.RankedPresetTable) RankedPresetCodeComp {
    return RankedPresetCodeComp{Value: row.Code}
}

func MakeRankedPresetNameComp(row *pb.RankedPresetTable) RankedPresetNameComp {
    return RankedPresetNameComp{Value: row.Name}
}

func MakeRankedPresetRole_tagComp(row *pb.RankedPresetTable) RankedPresetRole_tagComp {
    return RankedPresetRole_tagComp{Value: row.RoleTag}
}

func MakeRankedPresetPet_table_idComp(row *pb.RankedPresetTable) RankedPresetPet_table_idComp {
    return RankedPresetPet_table_idComp{Value: row.PetTableId}
}

func MakeRankedPresetWeight_conComp(row *pb.RankedPresetTable) RankedPresetWeight_conComp {
    return RankedPresetWeight_conComp{Value: row.WeightCon}
}

func MakeRankedPresetWeight_spiComp(row *pb.RankedPresetTable) RankedPresetWeight_spiComp {
    return RankedPresetWeight_spiComp{Value: row.WeightSpi}
}

func MakeRankedPresetWeight_strComp(row *pb.RankedPresetTable) RankedPresetWeight_strComp {
    return RankedPresetWeight_strComp{Value: row.WeightStr}
}

func MakeRankedPresetWeight_agiComp(row *pb.RankedPresetTable) RankedPresetWeight_agiComp {
    return RankedPresetWeight_agiComp{Value: row.WeightAgi}
}

func MakeRankedPresetSortComp(row *pb.RankedPresetTable) RankedPresetSortComp {
    return RankedPresetSortComp{Value: row.Sort}
}

func MakeRankedPresetDescComp(row *pb.RankedPresetTable) RankedPresetDescComp {
    return RankedPresetDescComp{Value: row.Desc}
}

func MakeRankedPresetSkill_idsComp(row *pb.RankedPresetTable) RankedPresetSkill_idsComp {
    return RankedPresetSkill_idsComp{Values: row.SkillIds}
}

func MakeRankedPresetPet_entry_idsComp(row *pb.RankedPresetTable) RankedPresetPet_entry_idsComp {
    return RankedPresetPet_entry_idsComp{Values: row.PetEntryIds}
}

func MakeRankedPresetPet_entry_pointsComp(row *pb.RankedPresetTable) RankedPresetPet_entry_pointsComp {
    return RankedPresetPet_entry_pointsComp{Values: row.PetEntryPoints}
}

