
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/spiritshoplimitgroup_table.pb.h"

// ============================================================
// Per-column ECS components for SpiritShopLimitGroupTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct SpiritShopLimitGroupIdComp {
    uint32_t value;
};

struct SpiritShopLimitGroupNameComp {
    std::string_view value;
};

struct SpiritShopLimitGroupReset_kindComp {
    uint32_t value;
};

struct SpiritShopLimitGroupMax_weightComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline SpiritShopLimitGroupIdComp MakeSpiritShopLimitGroupIdComp(const SpiritShopLimitGroupTable& row) {
    return { row.id() };
}
inline SpiritShopLimitGroupNameComp MakeSpiritShopLimitGroupNameComp(const SpiritShopLimitGroupTable& row) {
    return { std::string_view(row.name()) };
}
inline SpiritShopLimitGroupReset_kindComp MakeSpiritShopLimitGroupReset_kindComp(const SpiritShopLimitGroupTable& row) {
    return { row.reset_kind() };
}
inline SpiritShopLimitGroupMax_weightComp MakeSpiritShopLimitGroupMax_weightComp(const SpiritShopLimitGroupTable& row) {
    return { row.max_weight() };
}
