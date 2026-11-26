
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
