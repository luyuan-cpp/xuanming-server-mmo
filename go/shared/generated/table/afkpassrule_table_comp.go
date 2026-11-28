
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for AfkPassRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type AfkPassRuleIdComp struct {
    Value uint32
}

type AfkPassRulePeriod_daysComp struct {
    Value uint32
}

type AfkPassRulePrice_currency_typeComp struct {
    Value uint32
}

type AfkPassRulePriceComp struct {
    Value uint64
}

type AfkPassRuleMax_remaining_daysComp struct {
    Value uint32
}

type AfkPassRuleRemind_before_hoursComp struct {
    Value uint32
}

type AfkPassRuleDescComp struct {
    Value string
}

type AfkPassRuleBonus_item_idComp struct {
    Value uint32
}

type AfkPassRuleBonus_item_countComp struct {
    Value uint32
}

type AfkPassRuleBonus_frame_keyComp struct {
    Value string
}

type AfkPassRulePlan_nameComp struct {
    Value string
}

type AfkPassRuleIcon_keyComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeAfkPassRuleIdComp(row *pb.AfkPassRuleTable) AfkPassRuleIdComp {
    return AfkPassRuleIdComp{Value: row.Id}
}

func MakeAfkPassRulePeriod_daysComp(row *pb.AfkPassRuleTable) AfkPassRulePeriod_daysComp {
    return AfkPassRulePeriod_daysComp{Value: row.PeriodDays}
}

func MakeAfkPassRulePrice_currency_typeComp(row *pb.AfkPassRuleTable) AfkPassRulePrice_currency_typeComp {
    return AfkPassRulePrice_currency_typeComp{Value: row.PriceCurrencyType}
}

func MakeAfkPassRulePriceComp(row *pb.AfkPassRuleTable) AfkPassRulePriceComp {
    return AfkPassRulePriceComp{Value: row.Price}
}

func MakeAfkPassRuleMax_remaining_daysComp(row *pb.AfkPassRuleTable) AfkPassRuleMax_remaining_daysComp {
    return AfkPassRuleMax_remaining_daysComp{Value: row.MaxRemainingDays}
}

func MakeAfkPassRuleRemind_before_hoursComp(row *pb.AfkPassRuleTable) AfkPassRuleRemind_before_hoursComp {
    return AfkPassRuleRemind_before_hoursComp{Value: row.RemindBeforeHours}
}

func MakeAfkPassRuleDescComp(row *pb.AfkPassRuleTable) AfkPassRuleDescComp {
    return AfkPassRuleDescComp{Value: row.Desc}
}

func MakeAfkPassRuleBonus_item_idComp(row *pb.AfkPassRuleTable) AfkPassRuleBonus_item_idComp {
    return AfkPassRuleBonus_item_idComp{Value: row.BonusItemId}
}

func MakeAfkPassRuleBonus_item_countComp(row *pb.AfkPassRuleTable) AfkPassRuleBonus_item_countComp {
    return AfkPassRuleBonus_item_countComp{Value: row.BonusItemCount}
}

func MakeAfkPassRuleBonus_frame_keyComp(row *pb.AfkPassRuleTable) AfkPassRuleBonus_frame_keyComp {
    return AfkPassRuleBonus_frame_keyComp{Value: row.BonusFrameKey}
}

func MakeAfkPassRulePlan_nameComp(row *pb.AfkPassRuleTable) AfkPassRulePlan_nameComp {
    return AfkPassRulePlan_nameComp{Value: row.PlanName}
}

func MakeAfkPassRuleIcon_keyComp(row *pb.AfkPassRuleTable) AfkPassRuleIcon_keyComp {
    return AfkPassRuleIcon_keyComp{Value: row.IconKey}
}

