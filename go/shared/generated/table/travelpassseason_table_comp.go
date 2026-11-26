
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for TravelPassSeasonTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type TravelPassSeasonIdComp struct {
    Value uint32
}

type TravelPassSeasonEnabledComp struct {
    Value uint32
}

type TravelPassSeasonNameComp struct {
    Value string
}

type TravelPassSeasonDescriptionComp struct {
    Value string
}

type TravelPassSeasonIcon_keyComp struct {
    Value string
}

type TravelPassSeasonBanner_asset_keyComp struct {
    Value string
}

type TravelPassSeasonStarts_at_msComp struct {
    Value uint64
}

type TravelPassSeasonEnds_at_msComp struct {
    Value uint64
}

type TravelPassSeasonMax_pointsComp struct {
    Value uint32
}

type TravelPassSeasonPve_win_pointsComp struct {
    Value uint32
}

type TravelPassSeasonHard_or_pvp_win_pointsComp struct {
    Value uint32
}

type TravelPassSeasonPvp_qualified_other_pointsComp struct {
    Value uint32
}

type TravelPassSeasonPremium_currency_typeComp struct {
    Value uint32
}

type TravelPassSeasonPremium_priceComp struct {
    Value uint64
}

type TravelPassSeasonConfig_revisionComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeTravelPassSeasonIdComp(row *pb.TravelPassSeasonTable) TravelPassSeasonIdComp {
    return TravelPassSeasonIdComp{Value: row.Id}
}

func MakeTravelPassSeasonEnabledComp(row *pb.TravelPassSeasonTable) TravelPassSeasonEnabledComp {
    return TravelPassSeasonEnabledComp{Value: row.Enabled}
}

func MakeTravelPassSeasonNameComp(row *pb.TravelPassSeasonTable) TravelPassSeasonNameComp {
    return TravelPassSeasonNameComp{Value: row.Name}
}

func MakeTravelPassSeasonDescriptionComp(row *pb.TravelPassSeasonTable) TravelPassSeasonDescriptionComp {
    return TravelPassSeasonDescriptionComp{Value: row.Description}
}

func MakeTravelPassSeasonIcon_keyComp(row *pb.TravelPassSeasonTable) TravelPassSeasonIcon_keyComp {
    return TravelPassSeasonIcon_keyComp{Value: row.IconKey}
}

func MakeTravelPassSeasonBanner_asset_keyComp(row *pb.TravelPassSeasonTable) TravelPassSeasonBanner_asset_keyComp {
    return TravelPassSeasonBanner_asset_keyComp{Value: row.BannerAssetKey}
}

func MakeTravelPassSeasonStarts_at_msComp(row *pb.TravelPassSeasonTable) TravelPassSeasonStarts_at_msComp {
    return TravelPassSeasonStarts_at_msComp{Value: row.StartsAtMs}
}

func MakeTravelPassSeasonEnds_at_msComp(row *pb.TravelPassSeasonTable) TravelPassSeasonEnds_at_msComp {
    return TravelPassSeasonEnds_at_msComp{Value: row.EndsAtMs}
}

func MakeTravelPassSeasonMax_pointsComp(row *pb.TravelPassSeasonTable) TravelPassSeasonMax_pointsComp {
    return TravelPassSeasonMax_pointsComp{Value: row.MaxPoints}
}

func MakeTravelPassSeasonPve_win_pointsComp(row *pb.TravelPassSeasonTable) TravelPassSeasonPve_win_pointsComp {
    return TravelPassSeasonPve_win_pointsComp{Value: row.PveWinPoints}
}

func MakeTravelPassSeasonHard_or_pvp_win_pointsComp(row *pb.TravelPassSeasonTable) TravelPassSeasonHard_or_pvp_win_pointsComp {
    return TravelPassSeasonHard_or_pvp_win_pointsComp{Value: row.HardOrPvpWinPoints}
}

func MakeTravelPassSeasonPvp_qualified_other_pointsComp(row *pb.TravelPassSeasonTable) TravelPassSeasonPvp_qualified_other_pointsComp {
    return TravelPassSeasonPvp_qualified_other_pointsComp{Value: row.PvpQualifiedOtherPoints}
}

func MakeTravelPassSeasonPremium_currency_typeComp(row *pb.TravelPassSeasonTable) TravelPassSeasonPremium_currency_typeComp {
    return TravelPassSeasonPremium_currency_typeComp{Value: row.PremiumCurrencyType}
}

func MakeTravelPassSeasonPremium_priceComp(row *pb.TravelPassSeasonTable) TravelPassSeasonPremium_priceComp {
    return TravelPassSeasonPremium_priceComp{Value: row.PremiumPrice}
}

func MakeTravelPassSeasonConfig_revisionComp(row *pb.TravelPassSeasonTable) TravelPassSeasonConfig_revisionComp {
    return TravelPassSeasonConfig_revisionComp{Value: row.ConfigRevision}
}

