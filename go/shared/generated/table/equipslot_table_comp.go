
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for EquipSlotTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type EquipSlotIdComp struct {
    Value uint32
}

type EquipSlotEquip_kindComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeEquipSlotIdComp(row *pb.EquipSlotTable) EquipSlotIdComp {
    return EquipSlotIdComp{Value: row.Id}
}

func MakeEquipSlotEquip_kindComp(row *pb.EquipSlotTable) EquipSlotEquip_kindComp {
    return EquipSlotEquip_kindComp{Value: row.EquipKind}
}

