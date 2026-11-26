
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/travelpasstier_table.pb.h"

// ============================================================
// Per-column ECS components for TravelPassTierTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct TravelPassTierIdComp {
    uint32_t value;
};

struct TravelPassTierPass_idComp {
    uint32_t value;
};

struct TravelPassTierTier_indexComp {
    uint32_t value;
};

struct TravelPassTierRequired_pointsComp {
    uint32_t value;
};

struct TravelPassTierFree_item_idComp {
    uint32_t value;
};

struct TravelPassTierFree_item_countComp {
    uint32_t value;
};

struct TravelPassTierFree_goldComp {
    uint64_t value;
};

struct TravelPassTierFree_cosmetic_keyComp {
    std::string_view value;
};

struct TravelPassTierFree_cosmetic_nameComp {
    std::string_view value;
};

struct TravelPassTierPremium_item_idComp {
    uint32_t value;
};

struct TravelPassTierPremium_item_countComp {
    uint32_t value;
};

struct TravelPassTierPremium_goldComp {
    uint64_t value;
};

struct TravelPassTierPremium_cosmetic_keyComp {
    std::string_view value;
};

struct TravelPassTierPremium_cosmetic_nameComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline TravelPassTierIdComp MakeTravelPassTierIdComp(const TravelPassTierTable& row) {
    return { row.id() };
}
inline TravelPassTierPass_idComp MakeTravelPassTierPass_idComp(const TravelPassTierTable& row) {
    return { row.pass_id() };
}
inline TravelPassTierTier_indexComp MakeTravelPassTierTier_indexComp(const TravelPassTierTable& row) {
    return { row.tier_index() };
}
inline TravelPassTierRequired_pointsComp MakeTravelPassTierRequired_pointsComp(const TravelPassTierTable& row) {
    return { row.required_points() };
}
inline TravelPassTierFree_item_idComp MakeTravelPassTierFree_item_idComp(const TravelPassTierTable& row) {
    return { row.free_item_id() };
}
inline TravelPassTierFree_item_countComp MakeTravelPassTierFree_item_countComp(const TravelPassTierTable& row) {
    return { row.free_item_count() };
}
inline TravelPassTierFree_goldComp MakeTravelPassTierFree_goldComp(const TravelPassTierTable& row) {
    return { row.free_gold() };
}
inline TravelPassTierFree_cosmetic_keyComp MakeTravelPassTierFree_cosmetic_keyComp(const TravelPassTierTable& row) {
    return { std::string_view(row.free_cosmetic_key()) };
}
inline TravelPassTierFree_cosmetic_nameComp MakeTravelPassTierFree_cosmetic_nameComp(const TravelPassTierTable& row) {
    return { std::string_view(row.free_cosmetic_name()) };
}
inline TravelPassTierPremium_item_idComp MakeTravelPassTierPremium_item_idComp(const TravelPassTierTable& row) {
    return { row.premium_item_id() };
}
inline TravelPassTierPremium_item_countComp MakeTravelPassTierPremium_item_countComp(const TravelPassTierTable& row) {
    return { row.premium_item_count() };
}
inline TravelPassTierPremium_goldComp MakeTravelPassTierPremium_goldComp(const TravelPassTierTable& row) {
    return { row.premium_gold() };
}
inline TravelPassTierPremium_cosmetic_keyComp MakeTravelPassTierPremium_cosmetic_keyComp(const TravelPassTierTable& row) {
    return { std::string_view(row.premium_cosmetic_key()) };
}
inline TravelPassTierPremium_cosmetic_nameComp MakeTravelPassTierPremium_cosmetic_nameComp(const TravelPassTierTable& row) {
    return { std::string_view(row.premium_cosmetic_name()) };
}
