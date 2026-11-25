
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/pettier_table.pb.h"

// ============================================================
// Per-column ECS components for PetTierTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct PetTierIdComp {
    uint32_t value;
};

struct PetTierNameComp {
    std::string_view value;
};

struct PetTierArt_keyComp {
    std::string_view value;
};

struct PetTierDescComp {
    std::string_view value;
};

struct PetTierAscend_cost_item_idComp {
    uint32_t value;
};

struct PetTierAscend_cost_item_countComp {
    uint32_t value;
};

struct PetTierAscend_cost_goldComp {
    uint64_t value;
};

struct PetTierAscend_min_pet_levelComp {
    uint32_t value;
};

struct PetTierBonus_entry_pointsComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline PetTierIdComp MakePetTierIdComp(const PetTierTable& row) {
    return { row.id() };
}
inline PetTierNameComp MakePetTierNameComp(const PetTierTable& row) {
    return { std::string_view(row.name()) };
}
inline PetTierArt_keyComp MakePetTierArt_keyComp(const PetTierTable& row) {
    return { std::string_view(row.art_key()) };
}
inline PetTierDescComp MakePetTierDescComp(const PetTierTable& row) {
    return { std::string_view(row.desc()) };
}
inline PetTierAscend_cost_item_idComp MakePetTierAscend_cost_item_idComp(const PetTierTable& row) {
    return { row.ascend_cost_item_id() };
}
inline PetTierAscend_cost_item_countComp MakePetTierAscend_cost_item_countComp(const PetTierTable& row) {
    return { row.ascend_cost_item_count() };
}
inline PetTierAscend_cost_goldComp MakePetTierAscend_cost_goldComp(const PetTierTable& row) {
    return { row.ascend_cost_gold() };
}
inline PetTierAscend_min_pet_levelComp MakePetTierAscend_min_pet_levelComp(const PetTierTable& row) {
    return { row.ascend_min_pet_level() };
}
inline PetTierBonus_entry_pointsComp MakePetTierBonus_entry_pointsComp(const PetTierTable& row) {
    return { row.bonus_entry_points() };
}
