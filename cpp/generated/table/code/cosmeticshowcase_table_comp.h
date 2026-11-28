
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/cosmeticshowcase_table.pb.h"

// ============================================================
// Per-column ECS components for CosmeticShowcaseTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct CosmeticShowcaseIdComp {
    uint32_t value;
};

struct CosmeticShowcaseCosmetic_keyComp {
    std::string_view value;
};

struct CosmeticShowcaseSlot_kindComp {
    uint32_t value;
};

struct CosmeticShowcaseNameComp {
    std::string_view value;
};

struct CosmeticShowcaseAsset_keyComp {
    std::string_view value;
};

struct CosmeticShowcaseSecondary_asset_keyComp {
    std::string_view value;
};

struct CosmeticShowcaseCard_item_idComp {
    uint32_t value;
};

struct CosmeticShowcaseCard_item_countComp {
    uint32_t value;
};

struct CosmeticShowcaseSpirit_shop_goods_idComp {
    uint32_t value;
};

struct CosmeticShowcaseEnabledComp {
    uint32_t value;
};

struct CosmeticShowcaseSort_orderComp {
    uint32_t value;
};

struct CosmeticShowcaseAcquire_hintComp {
    std::string_view value;
};

struct CosmeticShowcaseDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline CosmeticShowcaseIdComp MakeCosmeticShowcaseIdComp(const CosmeticShowcaseTable& row) {
    return { row.id() };
}
inline CosmeticShowcaseCosmetic_keyComp MakeCosmeticShowcaseCosmetic_keyComp(const CosmeticShowcaseTable& row) {
    return { std::string_view(row.cosmetic_key()) };
}
inline CosmeticShowcaseSlot_kindComp MakeCosmeticShowcaseSlot_kindComp(const CosmeticShowcaseTable& row) {
    return { row.slot_kind() };
}
inline CosmeticShowcaseNameComp MakeCosmeticShowcaseNameComp(const CosmeticShowcaseTable& row) {
    return { std::string_view(row.name()) };
}
inline CosmeticShowcaseAsset_keyComp MakeCosmeticShowcaseAsset_keyComp(const CosmeticShowcaseTable& row) {
    return { std::string_view(row.asset_key()) };
}
inline CosmeticShowcaseSecondary_asset_keyComp MakeCosmeticShowcaseSecondary_asset_keyComp(const CosmeticShowcaseTable& row) {
    return { std::string_view(row.secondary_asset_key()) };
}
inline CosmeticShowcaseCard_item_idComp MakeCosmeticShowcaseCard_item_idComp(const CosmeticShowcaseTable& row) {
    return { row.card_item_id() };
}
inline CosmeticShowcaseCard_item_countComp MakeCosmeticShowcaseCard_item_countComp(const CosmeticShowcaseTable& row) {
    return { row.card_item_count() };
}
inline CosmeticShowcaseSpirit_shop_goods_idComp MakeCosmeticShowcaseSpirit_shop_goods_idComp(const CosmeticShowcaseTable& row) {
    return { row.spirit_shop_goods_id() };
}
inline CosmeticShowcaseEnabledComp MakeCosmeticShowcaseEnabledComp(const CosmeticShowcaseTable& row) {
    return { row.enabled() };
}
inline CosmeticShowcaseSort_orderComp MakeCosmeticShowcaseSort_orderComp(const CosmeticShowcaseTable& row) {
    return { row.sort_order() };
}
inline CosmeticShowcaseAcquire_hintComp MakeCosmeticShowcaseAcquire_hintComp(const CosmeticShowcaseTable& row) {
    return { std::string_view(row.acquire_hint()) };
}
inline CosmeticShowcaseDescComp MakeCosmeticShowcaseDescComp(const CosmeticShowcaseTable& row) {
    return { std::string_view(row.desc()) };
}
