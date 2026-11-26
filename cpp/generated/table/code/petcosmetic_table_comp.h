
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/petcosmetic_table.pb.h"

// ============================================================
// Per-column ECS components for PetCosmeticTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct PetCosmeticIdComp {
    uint32_t value;
};

struct PetCosmeticCosmetic_keyComp {
    std::string_view value;
};

struct PetCosmeticNameComp {
    std::string_view value;
};

struct PetCosmeticTarget_pet_table_idComp {
    uint32_t value;
};

struct PetCosmeticVisual_pet_table_idComp {
    uint32_t value;
};

struct PetCosmeticVisual_art_keyComp {
    std::string_view value;
};

struct PetCosmeticEnabledComp {
    uint32_t value;
};

struct PetCosmeticSort_orderComp {
    uint32_t value;
};

struct PetCosmeticMall_sku_idComp {
    uint64_t value;
};

struct PetCosmeticAcquire_hintComp {
    std::string_view value;
};

struct PetCosmeticDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline PetCosmeticIdComp MakePetCosmeticIdComp(const PetCosmeticTable& row) {
    return { row.id() };
}
inline PetCosmeticCosmetic_keyComp MakePetCosmeticCosmetic_keyComp(const PetCosmeticTable& row) {
    return { std::string_view(row.cosmetic_key()) };
}
inline PetCosmeticNameComp MakePetCosmeticNameComp(const PetCosmeticTable& row) {
    return { std::string_view(row.name()) };
}
inline PetCosmeticTarget_pet_table_idComp MakePetCosmeticTarget_pet_table_idComp(const PetCosmeticTable& row) {
    return { row.target_pet_table_id() };
}
inline PetCosmeticVisual_pet_table_idComp MakePetCosmeticVisual_pet_table_idComp(const PetCosmeticTable& row) {
    return { row.visual_pet_table_id() };
}
inline PetCosmeticVisual_art_keyComp MakePetCosmeticVisual_art_keyComp(const PetCosmeticTable& row) {
    return { std::string_view(row.visual_art_key()) };
}
inline PetCosmeticEnabledComp MakePetCosmeticEnabledComp(const PetCosmeticTable& row) {
    return { row.enabled() };
}
inline PetCosmeticSort_orderComp MakePetCosmeticSort_orderComp(const PetCosmeticTable& row) {
    return { row.sort_order() };
}
inline PetCosmeticMall_sku_idComp MakePetCosmeticMall_sku_idComp(const PetCosmeticTable& row) {
    return { row.mall_sku_id() };
}
inline PetCosmeticAcquire_hintComp MakePetCosmeticAcquire_hintComp(const PetCosmeticTable& row) {
    return { std::string_view(row.acquire_hint()) };
}
inline PetCosmeticDescComp MakePetCosmeticDescComp(const PetCosmeticTable& row) {
    return { std::string_view(row.desc()) };
}
