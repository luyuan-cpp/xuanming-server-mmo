
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for SkillScrollCraftTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type SkillScrollCraftIdComp struct {
    Value uint32
}

type SkillScrollCraftNameComp struct {
    Value string
}

type SkillScrollCraftDescriptionComp struct {
    Value string
}

type SkillScrollCraftRarity_tierComp struct {
    Value uint32
}

type SkillScrollCraftFragment_item_idComp struct {
    Value uint32
}

type SkillScrollCraftRandom_fragment_countComp struct {
    Value uint32
}

type SkillScrollCraftRandom_gold_costComp struct {
    Value uint64
}

type SkillScrollCraftTargeted_fragment_countComp struct {
    Value uint32
}

type SkillScrollCraftTargeted_gold_costComp struct {
    Value uint64
}

type SkillScrollCraftDismantle_fragment_countComp struct {
    Value uint32
}

type SkillScrollCraftRare_scroll_item_idComp struct {
    Value uint32
}

type SkillScrollCraftRare_scroll_countComp struct {
    Value uint32
}

type SkillScrollCraftRare_ink_item_idComp struct {
    Value uint32
}

type SkillScrollCraftRare_ink_countComp struct {
    Value uint32
}

type SkillScrollCraftRare_gold_costComp struct {
    Value uint64
}

type SkillScrollCraftEnabledComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeSkillScrollCraftIdComp(row *pb.SkillScrollCraftTable) SkillScrollCraftIdComp {
    return SkillScrollCraftIdComp{Value: row.Id}
}

func MakeSkillScrollCraftNameComp(row *pb.SkillScrollCraftTable) SkillScrollCraftNameComp {
    return SkillScrollCraftNameComp{Value: row.Name}
}

func MakeSkillScrollCraftDescriptionComp(row *pb.SkillScrollCraftTable) SkillScrollCraftDescriptionComp {
    return SkillScrollCraftDescriptionComp{Value: row.Description}
}

func MakeSkillScrollCraftRarity_tierComp(row *pb.SkillScrollCraftTable) SkillScrollCraftRarity_tierComp {
    return SkillScrollCraftRarity_tierComp{Value: row.RarityTier}
}

func MakeSkillScrollCraftFragment_item_idComp(row *pb.SkillScrollCraftTable) SkillScrollCraftFragment_item_idComp {
    return SkillScrollCraftFragment_item_idComp{Value: row.FragmentItemId}
}

func MakeSkillScrollCraftRandom_fragment_countComp(row *pb.SkillScrollCraftTable) SkillScrollCraftRandom_fragment_countComp {
    return SkillScrollCraftRandom_fragment_countComp{Value: row.RandomFragmentCount}
}

func MakeSkillScrollCraftRandom_gold_costComp(row *pb.SkillScrollCraftTable) SkillScrollCraftRandom_gold_costComp {
    return SkillScrollCraftRandom_gold_costComp{Value: row.RandomGoldCost}
}

func MakeSkillScrollCraftTargeted_fragment_countComp(row *pb.SkillScrollCraftTable) SkillScrollCraftTargeted_fragment_countComp {
    return SkillScrollCraftTargeted_fragment_countComp{Value: row.TargetedFragmentCount}
}

func MakeSkillScrollCraftTargeted_gold_costComp(row *pb.SkillScrollCraftTable) SkillScrollCraftTargeted_gold_costComp {
    return SkillScrollCraftTargeted_gold_costComp{Value: row.TargetedGoldCost}
}

func MakeSkillScrollCraftDismantle_fragment_countComp(row *pb.SkillScrollCraftTable) SkillScrollCraftDismantle_fragment_countComp {
    return SkillScrollCraftDismantle_fragment_countComp{Value: row.DismantleFragmentCount}
}

func MakeSkillScrollCraftRare_scroll_item_idComp(row *pb.SkillScrollCraftTable) SkillScrollCraftRare_scroll_item_idComp {
    return SkillScrollCraftRare_scroll_item_idComp{Value: row.RareScrollItemId}
}

func MakeSkillScrollCraftRare_scroll_countComp(row *pb.SkillScrollCraftTable) SkillScrollCraftRare_scroll_countComp {
    return SkillScrollCraftRare_scroll_countComp{Value: row.RareScrollCount}
}

func MakeSkillScrollCraftRare_ink_item_idComp(row *pb.SkillScrollCraftTable) SkillScrollCraftRare_ink_item_idComp {
    return SkillScrollCraftRare_ink_item_idComp{Value: row.RareInkItemId}
}

func MakeSkillScrollCraftRare_ink_countComp(row *pb.SkillScrollCraftTable) SkillScrollCraftRare_ink_countComp {
    return SkillScrollCraftRare_ink_countComp{Value: row.RareInkCount}
}

func MakeSkillScrollCraftRare_gold_costComp(row *pb.SkillScrollCraftTable) SkillScrollCraftRare_gold_costComp {
    return SkillScrollCraftRare_gold_costComp{Value: row.RareGoldCost}
}

func MakeSkillScrollCraftEnabledComp(row *pb.SkillScrollCraftTable) SkillScrollCraftEnabledComp {
    return SkillScrollCraftEnabledComp{Value: row.Enabled}
}

