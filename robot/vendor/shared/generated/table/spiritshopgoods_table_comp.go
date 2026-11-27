
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for SpiritShopGoodsTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type SpiritShopGoodsIdComp struct {
    Value uint32
}

type SpiritShopGoodsTab_idComp struct {
    Value uint32
}

type SpiritShopGoodsSort_orderComp struct {
    Value uint32
}

type SpiritShopGoodsEnabledComp struct {
    Value uint32
}

type SpiritShopGoodsNameComp struct {
    Value string
}

type SpiritShopGoodsDescriptionComp struct {
    Value string
}

type SpiritShopGoodsIcon_keyComp struct {
    Value string
}

type SpiritShopGoodsCurrency_typeComp struct {
    Value uint32
}

type SpiritShopGoodsPriceComp struct {
    Value uint64
}

type SpiritShopGoodsMax_buy_per_requestComp struct {
    Value uint32
}

type SpiritShopGoodsItem_id_1Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_count_1Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_bound_1Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_id_2Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_count_2Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_bound_2Comp struct {
    Value uint32
}

type SpiritShopGoodsLimit_reset_kindComp struct {
    Value uint32
}

type SpiritShopGoodsLimit_countComp struct {
    Value uint32
}

type SpiritShopGoodsLimit_group_idComp struct {
    Value uint32
}

type SpiritShopGoodsLimit_group_weightComp struct {
    Value uint32
}

type SpiritShopGoodsBadge_textComp struct {
    Value string
}

type SpiritShopGoodsStart_time_msComp struct {
    Value uint64
}

type SpiritShopGoodsEnd_time_msComp struct {
    Value uint64
}

type SpiritShopGoodsGrant_modeComp struct {
    Value uint32
}

type SpiritShopGoodsItem_id_3Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_count_3Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_bound_3Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_id_4Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_count_4Comp struct {
    Value uint32
}

type SpiritShopGoodsItem_bound_4Comp struct {
    Value uint32
}

type SpiritShopGoodsOriginal_priceComp struct {
    Value uint64
}

type SpiritShopGoodsMin_levelComp struct {
    Value uint32
}

type SpiritShopGoodsMax_levelComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeSpiritShopGoodsIdComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsIdComp {
    return SpiritShopGoodsIdComp{Value: row.Id}
}

func MakeSpiritShopGoodsTab_idComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsTab_idComp {
    return SpiritShopGoodsTab_idComp{Value: row.TabId}
}

func MakeSpiritShopGoodsSort_orderComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsSort_orderComp {
    return SpiritShopGoodsSort_orderComp{Value: row.SortOrder}
}

func MakeSpiritShopGoodsEnabledComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsEnabledComp {
    return SpiritShopGoodsEnabledComp{Value: row.Enabled}
}

func MakeSpiritShopGoodsNameComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsNameComp {
    return SpiritShopGoodsNameComp{Value: row.Name}
}

func MakeSpiritShopGoodsDescriptionComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsDescriptionComp {
    return SpiritShopGoodsDescriptionComp{Value: row.Description}
}

func MakeSpiritShopGoodsIcon_keyComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsIcon_keyComp {
    return SpiritShopGoodsIcon_keyComp{Value: row.IconKey}
}

func MakeSpiritShopGoodsCurrency_typeComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsCurrency_typeComp {
    return SpiritShopGoodsCurrency_typeComp{Value: row.CurrencyType}
}

func MakeSpiritShopGoodsPriceComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsPriceComp {
    return SpiritShopGoodsPriceComp{Value: row.Price}
}

func MakeSpiritShopGoodsMax_buy_per_requestComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsMax_buy_per_requestComp {
    return SpiritShopGoodsMax_buy_per_requestComp{Value: row.MaxBuyPerRequest}
}

func MakeSpiritShopGoodsItem_id_1Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_id_1Comp {
    return SpiritShopGoodsItem_id_1Comp{Value: row.ItemId_1}
}

func MakeSpiritShopGoodsItem_count_1Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_count_1Comp {
    return SpiritShopGoodsItem_count_1Comp{Value: row.ItemCount_1}
}

func MakeSpiritShopGoodsItem_bound_1Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_bound_1Comp {
    return SpiritShopGoodsItem_bound_1Comp{Value: row.ItemBound_1}
}

func MakeSpiritShopGoodsItem_id_2Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_id_2Comp {
    return SpiritShopGoodsItem_id_2Comp{Value: row.ItemId_2}
}

func MakeSpiritShopGoodsItem_count_2Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_count_2Comp {
    return SpiritShopGoodsItem_count_2Comp{Value: row.ItemCount_2}
}

func MakeSpiritShopGoodsItem_bound_2Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_bound_2Comp {
    return SpiritShopGoodsItem_bound_2Comp{Value: row.ItemBound_2}
}

func MakeSpiritShopGoodsLimit_reset_kindComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsLimit_reset_kindComp {
    return SpiritShopGoodsLimit_reset_kindComp{Value: row.LimitResetKind}
}

func MakeSpiritShopGoodsLimit_countComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsLimit_countComp {
    return SpiritShopGoodsLimit_countComp{Value: row.LimitCount}
}

func MakeSpiritShopGoodsLimit_group_idComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsLimit_group_idComp {
    return SpiritShopGoodsLimit_group_idComp{Value: row.LimitGroupId}
}

func MakeSpiritShopGoodsLimit_group_weightComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsLimit_group_weightComp {
    return SpiritShopGoodsLimit_group_weightComp{Value: row.LimitGroupWeight}
}

func MakeSpiritShopGoodsBadge_textComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsBadge_textComp {
    return SpiritShopGoodsBadge_textComp{Value: row.BadgeText}
}

func MakeSpiritShopGoodsStart_time_msComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsStart_time_msComp {
    return SpiritShopGoodsStart_time_msComp{Value: row.StartTimeMs}
}

func MakeSpiritShopGoodsEnd_time_msComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsEnd_time_msComp {
    return SpiritShopGoodsEnd_time_msComp{Value: row.EndTimeMs}
}

func MakeSpiritShopGoodsGrant_modeComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsGrant_modeComp {
    return SpiritShopGoodsGrant_modeComp{Value: row.GrantMode}
}

func MakeSpiritShopGoodsItem_id_3Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_id_3Comp {
    return SpiritShopGoodsItem_id_3Comp{Value: row.ItemId_3}
}

func MakeSpiritShopGoodsItem_count_3Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_count_3Comp {
    return SpiritShopGoodsItem_count_3Comp{Value: row.ItemCount_3}
}

func MakeSpiritShopGoodsItem_bound_3Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_bound_3Comp {
    return SpiritShopGoodsItem_bound_3Comp{Value: row.ItemBound_3}
}

func MakeSpiritShopGoodsItem_id_4Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_id_4Comp {
    return SpiritShopGoodsItem_id_4Comp{Value: row.ItemId_4}
}

func MakeSpiritShopGoodsItem_count_4Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_count_4Comp {
    return SpiritShopGoodsItem_count_4Comp{Value: row.ItemCount_4}
}

func MakeSpiritShopGoodsItem_bound_4Comp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsItem_bound_4Comp {
    return SpiritShopGoodsItem_bound_4Comp{Value: row.ItemBound_4}
}

func MakeSpiritShopGoodsOriginal_priceComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsOriginal_priceComp {
    return SpiritShopGoodsOriginal_priceComp{Value: row.OriginalPrice}
}

func MakeSpiritShopGoodsMin_levelComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsMin_levelComp {
    return SpiritShopGoodsMin_levelComp{Value: row.MinLevel}
}

func MakeSpiritShopGoodsMax_levelComp(row *pb.SpiritShopGoodsTable) SpiritShopGoodsMax_levelComp {
    return SpiritShopGoodsMax_levelComp{Value: row.MaxLevel}
}

