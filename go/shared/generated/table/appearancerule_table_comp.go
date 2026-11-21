
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for AppearanceRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type AppearanceRuleIdComp struct {
    Value uint32
}

type AppearanceRuleCard_item_idComp struct {
    Value uint32
}

type AppearanceRuleCard_item_countComp struct {
    Value uint32
}

type AppearanceRuleTrial_duration_secondsComp struct {
    Value uint32
}

type AppearanceRuleMall_sku_idComp struct {
    Value uint64
}

type AppearanceRuleMall_price_fenComp struct {
    Value uint64
}

type AppearanceRuleDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeAppearanceRuleIdComp(row *pb.AppearanceRuleTable) AppearanceRuleIdComp {
    return AppearanceRuleIdComp{Value: row.Id}
}

func MakeAppearanceRuleCard_item_idComp(row *pb.AppearanceRuleTable) AppearanceRuleCard_item_idComp {
    return AppearanceRuleCard_item_idComp{Value: row.CardItemId}
}

func MakeAppearanceRuleCard_item_countComp(row *pb.AppearanceRuleTable) AppearanceRuleCard_item_countComp {
    return AppearanceRuleCard_item_countComp{Value: row.CardItemCount}
}

func MakeAppearanceRuleTrial_duration_secondsComp(row *pb.AppearanceRuleTable) AppearanceRuleTrial_duration_secondsComp {
    return AppearanceRuleTrial_duration_secondsComp{Value: row.TrialDurationSeconds}
}

func MakeAppearanceRuleMall_sku_idComp(row *pb.AppearanceRuleTable) AppearanceRuleMall_sku_idComp {
    return AppearanceRuleMall_sku_idComp{Value: row.MallSkuId}
}

func MakeAppearanceRuleMall_price_fenComp(row *pb.AppearanceRuleTable) AppearanceRuleMall_price_fenComp {
    return AppearanceRuleMall_price_fenComp{Value: row.MallPriceFen}
}

func MakeAppearanceRuleDescComp(row *pb.AppearanceRuleTable) AppearanceRuleDescComp {
    return AppearanceRuleDescComp{Value: row.Desc}
}

