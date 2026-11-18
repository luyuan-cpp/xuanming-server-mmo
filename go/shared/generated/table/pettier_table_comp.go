
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

