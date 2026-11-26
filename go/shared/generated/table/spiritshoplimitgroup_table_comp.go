
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for SpiritShopLimitGroupTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type SpiritShopLimitGroupIdComp struct {
    Value uint32
}

type SpiritShopLimitGroupNameComp struct {
    Value string
}

type SpiritShopLimitGroupReset_kindComp struct {
    Value uint32
}

type SpiritShopLimitGroupMax_weightComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeSpiritShopLimitGroupIdComp(row *pb.SpiritShopLimitGroupTable) SpiritShopLimitGroupIdComp {
    return SpiritShopLimitGroupIdComp{Value: row.Id}
}

func MakeSpiritShopLimitGroupNameComp(row *pb.SpiritShopLimitGroupTable) SpiritShopLimitGroupNameComp {
    return SpiritShopLimitGroupNameComp{Value: row.Name}
}

func MakeSpiritShopLimitGroupReset_kindComp(row *pb.SpiritShopLimitGroupTable) SpiritShopLimitGroupReset_kindComp {
    return SpiritShopLimitGroupReset_kindComp{Value: row.ResetKind}
}

func MakeSpiritShopLimitGroupMax_weightComp(row *pb.SpiritShopLimitGroupTable) SpiritShopLimitGroupMax_weightComp {
    return SpiritShopLimitGroupMax_weightComp{Value: row.MaxWeight}
}

