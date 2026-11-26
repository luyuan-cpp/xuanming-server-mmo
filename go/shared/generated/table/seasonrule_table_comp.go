
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for SeasonRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type SeasonRuleIdComp struct {
    Value uint32
}

type SeasonRuleNameComp struct {
    Value string
}

type SeasonRuleStarts_at_msComp struct {
    Value uint64
}

type SeasonRuleEnds_at_msComp struct {
    Value uint64
}

type SeasonRuleEnabledComp struct {
    Value bool
}

type SeasonRuleSeal_max_pointsComp struct {
    Value uint32
}

type SeasonRulePve_win_pointsComp struct {
    Value uint32
}

type SeasonRulePvp_win_pointsComp struct {
    Value uint32
}

type SeasonRulePvp_other_pointsComp struct {
    Value uint32
}

type SeasonRuleMax_health_bps_capComp struct {
    Value uint32
}

type SeasonRuleDefense_bps_capComp struct {
    Value uint32
}

type SeasonRuleCatchup_currency_typeComp struct {
    Value uint32
}

type SeasonRuleCatchup_price_per_pointComp struct {
    Value uint64
}

type SeasonRuleDefault_seal_idComp struct {
    Value uint32
}

type SeasonRuleDescComp struct {
    Value string
}

type SeasonRuleHonor_title_nameComp struct {
    Value string
}

type SeasonRuleHonor_badge_icon_keyComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeSeasonRuleIdComp(row *pb.SeasonRuleTable) SeasonRuleIdComp {
    return SeasonRuleIdComp{Value: row.Id}
}

func MakeSeasonRuleNameComp(row *pb.SeasonRuleTable) SeasonRuleNameComp {
    return SeasonRuleNameComp{Value: row.Name}
}

func MakeSeasonRuleStarts_at_msComp(row *pb.SeasonRuleTable) SeasonRuleStarts_at_msComp {
    return SeasonRuleStarts_at_msComp{Value: row.StartsAtMs}
}

func MakeSeasonRuleEnds_at_msComp(row *pb.SeasonRuleTable) SeasonRuleEnds_at_msComp {
    return SeasonRuleEnds_at_msComp{Value: row.EndsAtMs}
}

func MakeSeasonRuleEnabledComp(row *pb.SeasonRuleTable) SeasonRuleEnabledComp {
    return SeasonRuleEnabledComp{Value: row.Enabled}
}

func MakeSeasonRuleSeal_max_pointsComp(row *pb.SeasonRuleTable) SeasonRuleSeal_max_pointsComp {
    return SeasonRuleSeal_max_pointsComp{Value: row.SealMaxPoints}
}

func MakeSeasonRulePve_win_pointsComp(row *pb.SeasonRuleTable) SeasonRulePve_win_pointsComp {
    return SeasonRulePve_win_pointsComp{Value: row.PveWinPoints}
}

func MakeSeasonRulePvp_win_pointsComp(row *pb.SeasonRuleTable) SeasonRulePvp_win_pointsComp {
    return SeasonRulePvp_win_pointsComp{Value: row.PvpWinPoints}
}

func MakeSeasonRulePvp_other_pointsComp(row *pb.SeasonRuleTable) SeasonRulePvp_other_pointsComp {
    return SeasonRulePvp_other_pointsComp{Value: row.PvpOtherPoints}
}

func MakeSeasonRuleMax_health_bps_capComp(row *pb.SeasonRuleTable) SeasonRuleMax_health_bps_capComp {
    return SeasonRuleMax_health_bps_capComp{Value: row.MaxHealthBpsCap}
}

func MakeSeasonRuleDefense_bps_capComp(row *pb.SeasonRuleTable) SeasonRuleDefense_bps_capComp {
    return SeasonRuleDefense_bps_capComp{Value: row.DefenseBpsCap}
}

func MakeSeasonRuleCatchup_currency_typeComp(row *pb.SeasonRuleTable) SeasonRuleCatchup_currency_typeComp {
    return SeasonRuleCatchup_currency_typeComp{Value: row.CatchupCurrencyType}
}

func MakeSeasonRuleCatchup_price_per_pointComp(row *pb.SeasonRuleTable) SeasonRuleCatchup_price_per_pointComp {
    return SeasonRuleCatchup_price_per_pointComp{Value: row.CatchupPricePerPoint}
}

func MakeSeasonRuleDefault_seal_idComp(row *pb.SeasonRuleTable) SeasonRuleDefault_seal_idComp {
    return SeasonRuleDefault_seal_idComp{Value: row.DefaultSealId}
}

func MakeSeasonRuleDescComp(row *pb.SeasonRuleTable) SeasonRuleDescComp {
    return SeasonRuleDescComp{Value: row.Desc}
}

func MakeSeasonRuleHonor_title_nameComp(row *pb.SeasonRuleTable) SeasonRuleHonor_title_nameComp {
    return SeasonRuleHonor_title_nameComp{Value: row.HonorTitleName}
}

func MakeSeasonRuleHonor_badge_icon_keyComp(row *pb.SeasonRuleTable) SeasonRuleHonor_badge_icon_keyComp {
    return SeasonRuleHonor_badge_icon_keyComp{Value: row.HonorBadgeIconKey}
}

