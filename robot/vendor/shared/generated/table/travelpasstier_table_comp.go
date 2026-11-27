
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for TravelPassTierTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type TravelPassTierIdComp struct {
    Value uint32
}

type TravelPassTierPass_idComp struct {
    Value uint32
}

type TravelPassTierTier_indexComp struct {
    Value uint32
}

type TravelPassTierRequired_pointsComp struct {
    Value uint32
}

type TravelPassTierFree_item_idComp struct {
    Value uint32
}

type TravelPassTierFree_item_countComp struct {
    Value uint32
}

type TravelPassTierFree_goldComp struct {
    Value uint64
}

type TravelPassTierFree_cosmetic_keyComp struct {
    Value string
}

type TravelPassTierFree_cosmetic_nameComp struct {
    Value string
}

type TravelPassTierPremium_item_idComp struct {
    Value uint32
}

type TravelPassTierPremium_item_countComp struct {
    Value uint32
}

type TravelPassTierPremium_goldComp struct {
    Value uint64
}

type TravelPassTierPremium_cosmetic_keyComp struct {
    Value string
}

type TravelPassTierPremium_cosmetic_nameComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeTravelPassTierIdComp(row *pb.TravelPassTierTable) TravelPassTierIdComp {
    return TravelPassTierIdComp{Value: row.Id}
}

func MakeTravelPassTierPass_idComp(row *pb.TravelPassTierTable) TravelPassTierPass_idComp {
    return TravelPassTierPass_idComp{Value: row.PassId}
}

func MakeTravelPassTierTier_indexComp(row *pb.TravelPassTierTable) TravelPassTierTier_indexComp {
    return TravelPassTierTier_indexComp{Value: row.TierIndex}
}

func MakeTravelPassTierRequired_pointsComp(row *pb.TravelPassTierTable) TravelPassTierRequired_pointsComp {
    return TravelPassTierRequired_pointsComp{Value: row.RequiredPoints}
}

func MakeTravelPassTierFree_item_idComp(row *pb.TravelPassTierTable) TravelPassTierFree_item_idComp {
    return TravelPassTierFree_item_idComp{Value: row.FreeItemId}
}

func MakeTravelPassTierFree_item_countComp(row *pb.TravelPassTierTable) TravelPassTierFree_item_countComp {
    return TravelPassTierFree_item_countComp{Value: row.FreeItemCount}
}

func MakeTravelPassTierFree_goldComp(row *pb.TravelPassTierTable) TravelPassTierFree_goldComp {
    return TravelPassTierFree_goldComp{Value: row.FreeGold}
}

func MakeTravelPassTierFree_cosmetic_keyComp(row *pb.TravelPassTierTable) TravelPassTierFree_cosmetic_keyComp {
    return TravelPassTierFree_cosmetic_keyComp{Value: row.FreeCosmeticKey}
}

func MakeTravelPassTierFree_cosmetic_nameComp(row *pb.TravelPassTierTable) TravelPassTierFree_cosmetic_nameComp {
    return TravelPassTierFree_cosmetic_nameComp{Value: row.FreeCosmeticName}
}

func MakeTravelPassTierPremium_item_idComp(row *pb.TravelPassTierTable) TravelPassTierPremium_item_idComp {
    return TravelPassTierPremium_item_idComp{Value: row.PremiumItemId}
}

func MakeTravelPassTierPremium_item_countComp(row *pb.TravelPassTierTable) TravelPassTierPremium_item_countComp {
    return TravelPassTierPremium_item_countComp{Value: row.PremiumItemCount}
}

func MakeTravelPassTierPremium_goldComp(row *pb.TravelPassTierTable) TravelPassTierPremium_goldComp {
    return TravelPassTierPremium_goldComp{Value: row.PremiumGold}
}

func MakeTravelPassTierPremium_cosmetic_keyComp(row *pb.TravelPassTierTable) TravelPassTierPremium_cosmetic_keyComp {
    return TravelPassTierPremium_cosmetic_keyComp{Value: row.PremiumCosmeticKey}
}

func MakeTravelPassTierPremium_cosmetic_nameComp(row *pb.TravelPassTierTable) TravelPassTierPremium_cosmetic_nameComp {
    return TravelPassTierPremium_cosmetic_nameComp{Value: row.PremiumCosmeticName}
}

