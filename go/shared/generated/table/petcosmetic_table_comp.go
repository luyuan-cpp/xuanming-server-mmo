
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for PetCosmeticTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type PetCosmeticIdComp struct {
    Value uint32
}

type PetCosmeticCosmetic_keyComp struct {
    Value string
}

type PetCosmeticNameComp struct {
    Value string
}

type PetCosmeticTarget_pet_table_idComp struct {
    Value uint32
}

type PetCosmeticVisual_pet_table_idComp struct {
    Value uint32
}

type PetCosmeticVisual_art_keyComp struct {
    Value string
}

type PetCosmeticEnabledComp struct {
    Value uint32
}

type PetCosmeticSort_orderComp struct {
    Value uint32
}

type PetCosmeticMall_sku_idComp struct {
    Value uint64
}

type PetCosmeticAcquire_hintComp struct {
    Value string
}

type PetCosmeticDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakePetCosmeticIdComp(row *pb.PetCosmeticTable) PetCosmeticIdComp {
    return PetCosmeticIdComp{Value: row.Id}
}

func MakePetCosmeticCosmetic_keyComp(row *pb.PetCosmeticTable) PetCosmeticCosmetic_keyComp {
    return PetCosmeticCosmetic_keyComp{Value: row.CosmeticKey}
}

func MakePetCosmeticNameComp(row *pb.PetCosmeticTable) PetCosmeticNameComp {
    return PetCosmeticNameComp{Value: row.Name}
}

func MakePetCosmeticTarget_pet_table_idComp(row *pb.PetCosmeticTable) PetCosmeticTarget_pet_table_idComp {
    return PetCosmeticTarget_pet_table_idComp{Value: row.TargetPetTableId}
}

func MakePetCosmeticVisual_pet_table_idComp(row *pb.PetCosmeticTable) PetCosmeticVisual_pet_table_idComp {
    return PetCosmeticVisual_pet_table_idComp{Value: row.VisualPetTableId}
}

func MakePetCosmeticVisual_art_keyComp(row *pb.PetCosmeticTable) PetCosmeticVisual_art_keyComp {
    return PetCosmeticVisual_art_keyComp{Value: row.VisualArtKey}
}

func MakePetCosmeticEnabledComp(row *pb.PetCosmeticTable) PetCosmeticEnabledComp {
    return PetCosmeticEnabledComp{Value: row.Enabled}
}

func MakePetCosmeticSort_orderComp(row *pb.PetCosmeticTable) PetCosmeticSort_orderComp {
    return PetCosmeticSort_orderComp{Value: row.SortOrder}
}

func MakePetCosmeticMall_sku_idComp(row *pb.PetCosmeticTable) PetCosmeticMall_sku_idComp {
    return PetCosmeticMall_sku_idComp{Value: row.MallSkuId}
}

func MakePetCosmeticAcquire_hintComp(row *pb.PetCosmeticTable) PetCosmeticAcquire_hintComp {
    return PetCosmeticAcquire_hintComp{Value: row.AcquireHint}
}

func MakePetCosmeticDescComp(row *pb.PetCosmeticTable) PetCosmeticDescComp {
    return PetCosmeticDescComp{Value: row.Desc}
}

