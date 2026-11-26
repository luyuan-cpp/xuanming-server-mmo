
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/skillvfxcosmetic_table.pb.h"

// ============================================================
// Per-column ECS components for SkillVfxCosmeticTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct SkillVfxCosmeticIdComp {
    uint32_t value;
};

struct SkillVfxCosmeticVfx_keyComp {
    std::string_view value;
};

struct SkillVfxCosmeticNameComp {
    std::string_view value;
};

struct SkillVfxCosmeticSkill_table_idComp {
    uint32_t value;
};

struct SkillVfxCosmeticFx_idComp {
    std::string_view value;
};

struct SkillVfxCosmeticProjectile_fx_idComp {
    std::string_view value;
};

struct SkillVfxCosmeticTelegraph_fx_idComp {
    std::string_view value;
};

struct SkillVfxCosmeticTint_hexComp {
    std::string_view value;
};

struct SkillVfxCosmeticEnabledComp {
    uint32_t value;
};

struct SkillVfxCosmeticSort_orderComp {
    uint32_t value;
};

struct SkillVfxCosmeticMall_sku_idComp {
    uint64_t value;
};

struct SkillVfxCosmeticAcquire_hintComp {
    std::string_view value;
};

struct SkillVfxCosmeticDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline SkillVfxCosmeticIdComp MakeSkillVfxCosmeticIdComp(const SkillVfxCosmeticTable& row) {
    return { row.id() };
}
inline SkillVfxCosmeticVfx_keyComp MakeSkillVfxCosmeticVfx_keyComp(const SkillVfxCosmeticTable& row) {
    return { std::string_view(row.vfx_key()) };
}
inline SkillVfxCosmeticNameComp MakeSkillVfxCosmeticNameComp(const SkillVfxCosmeticTable& row) {
    return { std::string_view(row.name()) };
}
inline SkillVfxCosmeticSkill_table_idComp MakeSkillVfxCosmeticSkill_table_idComp(const SkillVfxCosmeticTable& row) {
    return { row.skill_table_id() };
}
inline SkillVfxCosmeticFx_idComp MakeSkillVfxCosmeticFx_idComp(const SkillVfxCosmeticTable& row) {
    return { std::string_view(row.fx_id()) };
}
inline SkillVfxCosmeticProjectile_fx_idComp MakeSkillVfxCosmeticProjectile_fx_idComp(const SkillVfxCosmeticTable& row) {
    return { std::string_view(row.projectile_fx_id()) };
}
inline SkillVfxCosmeticTelegraph_fx_idComp MakeSkillVfxCosmeticTelegraph_fx_idComp(const SkillVfxCosmeticTable& row) {
    return { std::string_view(row.telegraph_fx_id()) };
}
inline SkillVfxCosmeticTint_hexComp MakeSkillVfxCosmeticTint_hexComp(const SkillVfxCosmeticTable& row) {
    return { std::string_view(row.tint_hex()) };
}
inline SkillVfxCosmeticEnabledComp MakeSkillVfxCosmeticEnabledComp(const SkillVfxCosmeticTable& row) {
    return { row.enabled() };
}
inline SkillVfxCosmeticSort_orderComp MakeSkillVfxCosmeticSort_orderComp(const SkillVfxCosmeticTable& row) {
    return { row.sort_order() };
}
inline SkillVfxCosmeticMall_sku_idComp MakeSkillVfxCosmeticMall_sku_idComp(const SkillVfxCosmeticTable& row) {
    return { row.mall_sku_id() };
}
inline SkillVfxCosmeticAcquire_hintComp MakeSkillVfxCosmeticAcquire_hintComp(const SkillVfxCosmeticTable& row) {
    return { std::string_view(row.acquire_hint()) };
}
inline SkillVfxCosmeticDescComp MakeSkillVfxCosmeticDescComp(const SkillVfxCosmeticTable& row) {
    return { std::string_view(row.desc()) };
}
