
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
