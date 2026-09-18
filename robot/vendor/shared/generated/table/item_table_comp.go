
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for ItemTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type ItemIdComp struct {
    Value uint32
}

type ItemMax_stack_sizeComp struct {
    Value uint32
}

type ItemEquip_kindComp struct {
    Value uint32
}

type ItemBattle_usableComp struct {
    Value uint32
}

type ItemBattle_heal_hpComp struct {
    Value uint64
}

type ItemBattle_heal_mpComp struct {
    Value uint64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeItemIdComp(row *pb.ItemTable) ItemIdComp {
    return ItemIdComp{Value: row.Id}
}

func MakeItemMax_stack_sizeComp(row *pb.ItemTable) ItemMax_stack_sizeComp {
    return ItemMax_stack_sizeComp{Value: row.MaxStackSize}
}

func MakeItemEquip_kindComp(row *pb.ItemTable) ItemEquip_kindComp {
    return ItemEquip_kindComp{Value: row.EquipKind}
}

func MakeItemBattle_usableComp(row *pb.ItemTable) ItemBattle_usableComp {
    return ItemBattle_usableComp{Value: row.BattleUsable}
}

func MakeItemBattle_heal_hpComp(row *pb.ItemTable) ItemBattle_heal_hpComp {
    return ItemBattle_heal_hpComp{Value: row.BattleHealHp}
}

func MakeItemBattle_heal_mpComp(row *pb.ItemTable) ItemBattle_heal_mpComp {
    return ItemBattle_heal_mpComp{Value: row.BattleHealMp}
}

