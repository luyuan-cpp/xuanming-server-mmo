
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for PetTierTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type PetTierIdComp struct {
    Value uint32
}

type PetTierNameComp struct {
    Value string
}

type PetTierArt_keyComp struct {
    Value string
}

type PetTierDescComp struct {
    Value string
}

type PetTierAscend_cost_item_idComp struct {
    Value uint32
}

type PetTierAscend_cost_item_countComp struct {
    Value uint32
}

type PetTierAscend_cost_goldComp struct {
    Value uint64
}

type PetTierAscend_min_pet_levelComp struct {
    Value uint32
}

type PetTierBonus_entry_pointsComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakePetTierIdComp(row *pb.PetTierTable) PetTierIdComp {
    return PetTierIdComp{Value: row.Id}
}

func MakePetTierNameComp(row *pb.PetTierTable) PetTierNameComp {
    return PetTierNameComp{Value: row.Name}
}

func MakePetTierArt_keyComp(row *pb.PetTierTable) PetTierArt_keyComp {
    return PetTierArt_keyComp{Value: row.ArtKey}
}

func MakePetTierDescComp(row *pb.PetTierTable) PetTierDescComp {
    return PetTierDescComp{Value: row.Desc}
}

func MakePetTierAscend_cost_item_idComp(row *pb.PetTierTable) PetTierAscend_cost_item_idComp {
    return PetTierAscend_cost_item_idComp{Value: row.AscendCostItemId}
}

func MakePetTierAscend_cost_item_countComp(row *pb.PetTierTable) PetTierAscend_cost_item_countComp {
    return PetTierAscend_cost_item_countComp{Value: row.AscendCostItemCount}
}

func MakePetTierAscend_cost_goldComp(row *pb.PetTierTable) PetTierAscend_cost_goldComp {
    return PetTierAscend_cost_goldComp{Value: row.AscendCostGold}
}

func MakePetTierAscend_min_pet_levelComp(row *pb.PetTierTable) PetTierAscend_min_pet_levelComp {
    return PetTierAscend_min_pet_levelComp{Value: row.AscendMinPetLevel}
}

func MakePetTierBonus_entry_pointsComp(row *pb.PetTierTable) PetTierBonus_entry_pointsComp {
    return PetTierBonus_entry_pointsComp{Value: row.BonusEntryPoints}
}

