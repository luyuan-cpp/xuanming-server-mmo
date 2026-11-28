
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for CosmeticShowcaseTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type CosmeticShowcaseIdComp struct {
    Value uint32
}

type CosmeticShowcaseCosmetic_keyComp struct {
    Value string
}

type CosmeticShowcaseSlot_kindComp struct {
    Value uint32
}

type CosmeticShowcaseNameComp struct {
    Value string
}

type CosmeticShowcaseAsset_keyComp struct {
    Value string
}

type CosmeticShowcaseSecondary_asset_keyComp struct {
    Value string
}

type CosmeticShowcaseCard_item_idComp struct {
    Value uint32
}

type CosmeticShowcaseCard_item_countComp struct {
    Value uint32
}

type CosmeticShowcaseSpirit_shop_goods_idComp struct {
    Value uint32
}

type CosmeticShowcaseEnabledComp struct {
    Value uint32
}

type CosmeticShowcaseSort_orderComp struct {
    Value uint32
}

type CosmeticShowcaseAcquire_hintComp struct {
    Value string
}

type CosmeticShowcaseDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeCosmeticShowcaseIdComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseIdComp {
    return CosmeticShowcaseIdComp{Value: row.Id}
}

func MakeCosmeticShowcaseCosmetic_keyComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseCosmetic_keyComp {
    return CosmeticShowcaseCosmetic_keyComp{Value: row.CosmeticKey}
}

func MakeCosmeticShowcaseSlot_kindComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseSlot_kindComp {
    return CosmeticShowcaseSlot_kindComp{Value: row.SlotKind}
}

func MakeCosmeticShowcaseNameComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseNameComp {
    return CosmeticShowcaseNameComp{Value: row.Name}
}

func MakeCosmeticShowcaseAsset_keyComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseAsset_keyComp {
    return CosmeticShowcaseAsset_keyComp{Value: row.AssetKey}
}

func MakeCosmeticShowcaseSecondary_asset_keyComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseSecondary_asset_keyComp {
    return CosmeticShowcaseSecondary_asset_keyComp{Value: row.SecondaryAssetKey}
}

func MakeCosmeticShowcaseCard_item_idComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseCard_item_idComp {
    return CosmeticShowcaseCard_item_idComp{Value: row.CardItemId}
}

func MakeCosmeticShowcaseCard_item_countComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseCard_item_countComp {
    return CosmeticShowcaseCard_item_countComp{Value: row.CardItemCount}
}

func MakeCosmeticShowcaseSpirit_shop_goods_idComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseSpirit_shop_goods_idComp {
    return CosmeticShowcaseSpirit_shop_goods_idComp{Value: row.SpiritShopGoodsId}
}

func MakeCosmeticShowcaseEnabledComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseEnabledComp {
    return CosmeticShowcaseEnabledComp{Value: row.Enabled}
}

func MakeCosmeticShowcaseSort_orderComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseSort_orderComp {
    return CosmeticShowcaseSort_orderComp{Value: row.SortOrder}
}

func MakeCosmeticShowcaseAcquire_hintComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseAcquire_hintComp {
    return CosmeticShowcaseAcquire_hintComp{Value: row.AcquireHint}
}

func MakeCosmeticShowcaseDescComp(row *pb.CosmeticShowcaseTable) CosmeticShowcaseDescComp {
    return CosmeticShowcaseDescComp{Value: row.Desc}
}

