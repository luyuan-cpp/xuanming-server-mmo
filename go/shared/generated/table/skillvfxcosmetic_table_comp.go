
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for SkillVfxCosmeticTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type SkillVfxCosmeticIdComp struct {
    Value uint32
}

type SkillVfxCosmeticVfx_keyComp struct {
    Value string
}

type SkillVfxCosmeticNameComp struct {
    Value string
}

type SkillVfxCosmeticSkill_table_idComp struct {
    Value uint32
}

type SkillVfxCosmeticFx_idComp struct {
    Value string
}

type SkillVfxCosmeticProjectile_fx_idComp struct {
    Value string
}

type SkillVfxCosmeticTelegraph_fx_idComp struct {
    Value string
}

type SkillVfxCosmeticTint_hexComp struct {
    Value string
}

type SkillVfxCosmeticEnabledComp struct {
    Value uint32
}

type SkillVfxCosmeticSort_orderComp struct {
    Value uint32
}

type SkillVfxCosmeticMall_sku_idComp struct {
    Value uint64
}

type SkillVfxCosmeticAcquire_hintComp struct {
    Value string
}

type SkillVfxCosmeticDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeSkillVfxCosmeticIdComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticIdComp {
    return SkillVfxCosmeticIdComp{Value: row.Id}
}

func MakeSkillVfxCosmeticVfx_keyComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticVfx_keyComp {
    return SkillVfxCosmeticVfx_keyComp{Value: row.VfxKey}
}

func MakeSkillVfxCosmeticNameComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticNameComp {
    return SkillVfxCosmeticNameComp{Value: row.Name}
}

func MakeSkillVfxCosmeticSkill_table_idComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticSkill_table_idComp {
    return SkillVfxCosmeticSkill_table_idComp{Value: row.SkillTableId}
}

func MakeSkillVfxCosmeticFx_idComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticFx_idComp {
    return SkillVfxCosmeticFx_idComp{Value: row.FxId}
}

func MakeSkillVfxCosmeticProjectile_fx_idComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticProjectile_fx_idComp {
    return SkillVfxCosmeticProjectile_fx_idComp{Value: row.ProjectileFxId}
}

func MakeSkillVfxCosmeticTelegraph_fx_idComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticTelegraph_fx_idComp {
    return SkillVfxCosmeticTelegraph_fx_idComp{Value: row.TelegraphFxId}
}

func MakeSkillVfxCosmeticTint_hexComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticTint_hexComp {
    return SkillVfxCosmeticTint_hexComp{Value: row.TintHex}
}

func MakeSkillVfxCosmeticEnabledComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticEnabledComp {
    return SkillVfxCosmeticEnabledComp{Value: row.Enabled}
}

func MakeSkillVfxCosmeticSort_orderComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticSort_orderComp {
    return SkillVfxCosmeticSort_orderComp{Value: row.SortOrder}
}

func MakeSkillVfxCosmeticMall_sku_idComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticMall_sku_idComp {
    return SkillVfxCosmeticMall_sku_idComp{Value: row.MallSkuId}
}

func MakeSkillVfxCosmeticAcquire_hintComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticAcquire_hintComp {
    return SkillVfxCosmeticAcquire_hintComp{Value: row.AcquireHint}
}

func MakeSkillVfxCosmeticDescComp(row *pb.SkillVfxCosmeticTable) SkillVfxCosmeticDescComp {
    return SkillVfxCosmeticDescComp{Value: row.Desc}
}

