
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for PetRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type PetRuleIdComp struct {
    Value uint32
}

type PetRuleMax_petsComp struct {
    Value uint32
}

type PetRuleName_max_lenComp struct {
    Value uint32
}

type PetRuleRename_cost_goldComp struct {
    Value uint64
}

type PetRuleSummon_cooldown_secondsComp struct {
    Value uint32
}

type PetRuleDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakePetRuleIdComp(row *pb.PetRuleTable) PetRuleIdComp {
    return PetRuleIdComp{Value: row.Id}
}

func MakePetRuleMax_petsComp(row *pb.PetRuleTable) PetRuleMax_petsComp {
    return PetRuleMax_petsComp{Value: row.MaxPets}
}

func MakePetRuleName_max_lenComp(row *pb.PetRuleTable) PetRuleName_max_lenComp {
    return PetRuleName_max_lenComp{Value: row.NameMaxLen}
}

func MakePetRuleRename_cost_goldComp(row *pb.PetRuleTable) PetRuleRename_cost_goldComp {
    return PetRuleRename_cost_goldComp{Value: row.RenameCostGold}
}

func MakePetRuleSummon_cooldown_secondsComp(row *pb.PetRuleTable) PetRuleSummon_cooldown_secondsComp {
    return PetRuleSummon_cooldown_secondsComp{Value: row.SummonCooldownSeconds}
}

func MakePetRuleDescComp(row *pb.PetRuleTable) PetRuleDescComp {
    return PetRuleDescComp{Value: row.Desc}
}

