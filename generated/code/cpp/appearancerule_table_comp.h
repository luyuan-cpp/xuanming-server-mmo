
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/appearancerule_table.pb.h"

// ============================================================
// Per-column ECS components for AppearanceRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct AppearanceRuleIdComp {
    uint32_t value;
};

struct AppearanceRuleCard_item_idComp {
    uint32_t value;
};

struct AppearanceRuleCard_item_countComp {
    uint32_t value;
};

struct AppearanceRuleTrial_duration_secondsComp {
    uint32_t value;
};

struct AppearanceRuleMall_sku_idComp {
    uint64_t value;
};

struct AppearanceRuleMall_price_fenComp {
    uint64_t value;
};

struct AppearanceRuleDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline AppearanceRuleIdComp MakeAppearanceRuleIdComp(const AppearanceRuleTable& row) {
    return { row.id() };
}
inline AppearanceRuleCard_item_idComp MakeAppearanceRuleCard_item_idComp(const AppearanceRuleTable& row) {
    return { row.card_item_id() };
}
inline AppearanceRuleCard_item_countComp MakeAppearanceRuleCard_item_countComp(const AppearanceRuleTable& row) {
    return { row.card_item_count() };
}
inline AppearanceRuleTrial_duration_secondsComp MakeAppearanceRuleTrial_duration_secondsComp(const AppearanceRuleTable& row) {
    return { row.trial_duration_seconds() };
}
inline AppearanceRuleMall_sku_idComp MakeAppearanceRuleMall_sku_idComp(const AppearanceRuleTable& row) {
    return { row.mall_sku_id() };
}
inline AppearanceRuleMall_price_fenComp MakeAppearanceRuleMall_price_fenComp(const AppearanceRuleTable& row) {
    return { row.mall_price_fen() };
}
inline AppearanceRuleDescComp MakeAppearanceRuleDescComp(const AppearanceRuleTable& row) {
    return { std::string_view(row.desc()) };
}
