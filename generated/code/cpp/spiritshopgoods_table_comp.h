
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/spiritshopgoods_table.pb.h"

// ============================================================
// Per-column ECS components for SpiritShopGoodsTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct SpiritShopGoodsIdComp {
    uint32_t value;
};

struct SpiritShopGoodsTab_idComp {
    uint32_t value;
};

struct SpiritShopGoodsSort_orderComp {
    uint32_t value;
};

struct SpiritShopGoodsEnabledComp {
    uint32_t value;
};

struct SpiritShopGoodsNameComp {
    std::string_view value;
};

struct SpiritShopGoodsDescriptionComp {
    std::string_view value;
};

struct SpiritShopGoodsIcon_keyComp {
    std::string_view value;
};

struct SpiritShopGoodsCurrency_typeComp {
    uint32_t value;
};

struct SpiritShopGoodsPriceComp {
    uint64_t value;
};

struct SpiritShopGoodsMax_buy_per_requestComp {
    uint32_t value;
};

struct SpiritShopGoodsItem_id_1Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_count_1Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_bound_1Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_id_2Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_count_2Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_bound_2Comp {
    uint32_t value;
};

struct SpiritShopGoodsLimit_reset_kindComp {
    uint32_t value;
};

struct SpiritShopGoodsLimit_countComp {
    uint32_t value;
};

struct SpiritShopGoodsLimit_group_idComp {
    uint32_t value;
};

struct SpiritShopGoodsLimit_group_weightComp {
    uint32_t value;
};

struct SpiritShopGoodsBadge_textComp {
    std::string_view value;
};

struct SpiritShopGoodsStart_time_msComp {
    uint64_t value;
};

struct SpiritShopGoodsEnd_time_msComp {
    uint64_t value;
};

struct SpiritShopGoodsGrant_modeComp {
    uint32_t value;
};

struct SpiritShopGoodsItem_id_3Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_count_3Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_bound_3Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_id_4Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_count_4Comp {
    uint32_t value;
};

struct SpiritShopGoodsItem_bound_4Comp {
    uint32_t value;
};

struct SpiritShopGoodsOriginal_priceComp {
    uint64_t value;
};

struct SpiritShopGoodsMin_levelComp {
    uint32_t value;
};

struct SpiritShopGoodsMax_levelComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline SpiritShopGoodsIdComp MakeSpiritShopGoodsIdComp(const SpiritShopGoodsTable& row) {
    return { row.id() };
}
inline SpiritShopGoodsTab_idComp MakeSpiritShopGoodsTab_idComp(const SpiritShopGoodsTable& row) {
    return { row.tab_id() };
}
inline SpiritShopGoodsSort_orderComp MakeSpiritShopGoodsSort_orderComp(const SpiritShopGoodsTable& row) {
    return { row.sort_order() };
}
inline SpiritShopGoodsEnabledComp MakeSpiritShopGoodsEnabledComp(const SpiritShopGoodsTable& row) {
    return { row.enabled() };
}
inline SpiritShopGoodsNameComp MakeSpiritShopGoodsNameComp(const SpiritShopGoodsTable& row) {
    return { std::string_view(row.name()) };
}
inline SpiritShopGoodsDescriptionComp MakeSpiritShopGoodsDescriptionComp(const SpiritShopGoodsTable& row) {
    return { std::string_view(row.description()) };
}
inline SpiritShopGoodsIcon_keyComp MakeSpiritShopGoodsIcon_keyComp(const SpiritShopGoodsTable& row) {
    return { std::string_view(row.icon_key()) };
}
inline SpiritShopGoodsCurrency_typeComp MakeSpiritShopGoodsCurrency_typeComp(const SpiritShopGoodsTable& row) {
    return { row.currency_type() };
}
inline SpiritShopGoodsPriceComp MakeSpiritShopGoodsPriceComp(const SpiritShopGoodsTable& row) {
    return { row.price() };
}
inline SpiritShopGoodsMax_buy_per_requestComp MakeSpiritShopGoodsMax_buy_per_requestComp(const SpiritShopGoodsTable& row) {
    return { row.max_buy_per_request() };
}
inline SpiritShopGoodsItem_id_1Comp MakeSpiritShopGoodsItem_id_1Comp(const SpiritShopGoodsTable& row) {
    return { row.item_id_1() };
}
inline SpiritShopGoodsItem_count_1Comp MakeSpiritShopGoodsItem_count_1Comp(const SpiritShopGoodsTable& row) {
    return { row.item_count_1() };
}
inline SpiritShopGoodsItem_bound_1Comp MakeSpiritShopGoodsItem_bound_1Comp(const SpiritShopGoodsTable& row) {
    return { row.item_bound_1() };
}
inline SpiritShopGoodsItem_id_2Comp MakeSpiritShopGoodsItem_id_2Comp(const SpiritShopGoodsTable& row) {
    return { row.item_id_2() };
}
inline SpiritShopGoodsItem_count_2Comp MakeSpiritShopGoodsItem_count_2Comp(const SpiritShopGoodsTable& row) {
    return { row.item_count_2() };
}
inline SpiritShopGoodsItem_bound_2Comp MakeSpiritShopGoodsItem_bound_2Comp(const SpiritShopGoodsTable& row) {
    return { row.item_bound_2() };
}
inline SpiritShopGoodsLimit_reset_kindComp MakeSpiritShopGoodsLimit_reset_kindComp(const SpiritShopGoodsTable& row) {
    return { row.limit_reset_kind() };
}
inline SpiritShopGoodsLimit_countComp MakeSpiritShopGoodsLimit_countComp(const SpiritShopGoodsTable& row) {
    return { row.limit_count() };
}
inline SpiritShopGoodsLimit_group_idComp MakeSpiritShopGoodsLimit_group_idComp(const SpiritShopGoodsTable& row) {
    return { row.limit_group_id() };
}
inline SpiritShopGoodsLimit_group_weightComp MakeSpiritShopGoodsLimit_group_weightComp(const SpiritShopGoodsTable& row) {
    return { row.limit_group_weight() };
}
inline SpiritShopGoodsBadge_textComp MakeSpiritShopGoodsBadge_textComp(const SpiritShopGoodsTable& row) {
    return { std::string_view(row.badge_text()) };
}
inline SpiritShopGoodsStart_time_msComp MakeSpiritShopGoodsStart_time_msComp(const SpiritShopGoodsTable& row) {
    return { row.start_time_ms() };
}
inline SpiritShopGoodsEnd_time_msComp MakeSpiritShopGoodsEnd_time_msComp(const SpiritShopGoodsTable& row) {
    return { row.end_time_ms() };
}
inline SpiritShopGoodsGrant_modeComp MakeSpiritShopGoodsGrant_modeComp(const SpiritShopGoodsTable& row) {
    return { row.grant_mode() };
}
inline SpiritShopGoodsItem_id_3Comp MakeSpiritShopGoodsItem_id_3Comp(const SpiritShopGoodsTable& row) {
    return { row.item_id_3() };
}
inline SpiritShopGoodsItem_count_3Comp MakeSpiritShopGoodsItem_count_3Comp(const SpiritShopGoodsTable& row) {
    return { row.item_count_3() };
}
inline SpiritShopGoodsItem_bound_3Comp MakeSpiritShopGoodsItem_bound_3Comp(const SpiritShopGoodsTable& row) {
    return { row.item_bound_3() };
}
inline SpiritShopGoodsItem_id_4Comp MakeSpiritShopGoodsItem_id_4Comp(const SpiritShopGoodsTable& row) {
    return { row.item_id_4() };
}
inline SpiritShopGoodsItem_count_4Comp MakeSpiritShopGoodsItem_count_4Comp(const SpiritShopGoodsTable& row) {
    return { row.item_count_4() };
}
inline SpiritShopGoodsItem_bound_4Comp MakeSpiritShopGoodsItem_bound_4Comp(const SpiritShopGoodsTable& row) {
    return { row.item_bound_4() };
}
inline SpiritShopGoodsOriginal_priceComp MakeSpiritShopGoodsOriginal_priceComp(const SpiritShopGoodsTable& row) {
    return { row.original_price() };
}
inline SpiritShopGoodsMin_levelComp MakeSpiritShopGoodsMin_levelComp(const SpiritShopGoodsTable& row) {
    return { row.min_level() };
}
inline SpiritShopGoodsMax_levelComp MakeSpiritShopGoodsMax_levelComp(const SpiritShopGoodsTable& row) {
    return { row.max_level() };
}
