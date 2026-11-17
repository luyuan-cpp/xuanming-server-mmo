
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for EquipEnhanceTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type EquipEnhanceIdComp struct {
    Value uint32
}

type EquipEnhanceBonus_permyriadComp struct {
    Value uint32
}

type EquipEnhanceSuccess_rateComp struct {
    Value uint32
}

type EquipEnhanceFail_level_lossComp struct {
    Value uint32
}

type EquipEnhanceCost_goldComp struct {
    Value uint64
}

type EquipEnhanceMaterial_idComp struct {
    Value uint32
}

type EquipEnhanceMaterial_countComp struct {
    Value uint32
}

type EquipEnhanceProfile_idComp struct {
    Value uint32
}

type EquipEnhanceLevelComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeEquipEnhanceIdComp(row *pb.EquipEnhanceTable) EquipEnhanceIdComp {
    return EquipEnhanceIdComp{Value: row.Id}
}

func MakeEquipEnhanceBonus_permyriadComp(row *pb.EquipEnhanceTable) EquipEnhanceBonus_permyriadComp {
    return EquipEnhanceBonus_permyriadComp{Value: row.BonusPermyriad}
}

func MakeEquipEnhanceSuccess_rateComp(row *pb.EquipEnhanceTable) EquipEnhanceSuccess_rateComp {
    return EquipEnhanceSuccess_rateComp{Value: row.SuccessRate}
}

func MakeEquipEnhanceFail_level_lossComp(row *pb.EquipEnhanceTable) EquipEnhanceFail_level_lossComp {
    return EquipEnhanceFail_level_lossComp{Value: row.FailLevelLoss}
}

func MakeEquipEnhanceCost_goldComp(row *pb.EquipEnhanceTable) EquipEnhanceCost_goldComp {
    return EquipEnhanceCost_goldComp{Value: row.CostGold}
}

func MakeEquipEnhanceMaterial_idComp(row *pb.EquipEnhanceTable) EquipEnhanceMaterial_idComp {
    return EquipEnhanceMaterial_idComp{Value: row.MaterialId}
}

func MakeEquipEnhanceMaterial_countComp(row *pb.EquipEnhanceTable) EquipEnhanceMaterial_countComp {
    return EquipEnhanceMaterial_countComp{Value: row.MaterialCount}
}

func MakeEquipEnhanceProfile_idComp(row *pb.EquipEnhanceTable) EquipEnhanceProfile_idComp {
    return EquipEnhanceProfile_idComp{Value: row.ProfileId}
}

func MakeEquipEnhanceLevelComp(row *pb.EquipEnhanceTable) EquipEnhanceLevelComp {
    return EquipEnhanceLevelComp{Value: row.Level}
}

