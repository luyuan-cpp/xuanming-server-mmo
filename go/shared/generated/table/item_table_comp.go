
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

type ItemNameComp struct {
    Value string
}

type ItemDescriptionComp struct {
    Value string
}

type ItemIcon_keyComp struct {
    Value string
}

type ItemEquip_levelComp struct {
    Value uint32
}

type ItemEquip_classComp struct {
    Value uint32
}

type ItemAffix_poolComp struct {
    Value uint32
}

type ItemAffix_ruleComp struct {
    Value uint32
}

type ItemEquip_profileComp struct {
    Value uint32
}

type ItemForge_material_idComp struct {
    Value uint32
}

type ItemForge_material_countComp struct {
    Value uint32
}

type ItemArtifact_idComp struct {
    Value uint32
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

func MakeItemNameComp(row *pb.ItemTable) ItemNameComp {
    return ItemNameComp{Value: row.Name}
}

func MakeItemDescriptionComp(row *pb.ItemTable) ItemDescriptionComp {
    return ItemDescriptionComp{Value: row.Description}
}

func MakeItemIcon_keyComp(row *pb.ItemTable) ItemIcon_keyComp {
    return ItemIcon_keyComp{Value: row.IconKey}
}

func MakeItemEquip_levelComp(row *pb.ItemTable) ItemEquip_levelComp {
    return ItemEquip_levelComp{Value: row.EquipLevel}
}

func MakeItemEquip_classComp(row *pb.ItemTable) ItemEquip_classComp {
    return ItemEquip_classComp{Value: row.EquipClass}
}

func MakeItemAffix_poolComp(row *pb.ItemTable) ItemAffix_poolComp {
    return ItemAffix_poolComp{Value: row.AffixPool}
}

func MakeItemAffix_ruleComp(row *pb.ItemTable) ItemAffix_ruleComp {
    return ItemAffix_ruleComp{Value: row.AffixRule}
}

func MakeItemEquip_profileComp(row *pb.ItemTable) ItemEquip_profileComp {
    return ItemEquip_profileComp{Value: row.EquipProfile}
}

func MakeItemForge_material_idComp(row *pb.ItemTable) ItemForge_material_idComp {
    return ItemForge_material_idComp{Value: row.ForgeMaterialId}
}

func MakeItemForge_material_countComp(row *pb.ItemTable) ItemForge_material_countComp {
    return ItemForge_material_countComp{Value: row.ForgeMaterialCount}
}

func MakeItemArtifact_idComp(row *pb.ItemTable) ItemArtifact_idComp {
    return ItemArtifact_idComp{Value: row.ArtifactId}
}

