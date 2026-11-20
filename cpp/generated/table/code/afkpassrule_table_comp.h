
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/afkpassrule_table.pb.h"

// ============================================================
// Per-column ECS components for AfkPassRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct AfkPassRuleIdComp {
    uint32_t value;
};

struct AfkPassRulePeriod_daysComp {
    uint32_t value;
};

struct AfkPassRulePrice_currency_typeComp {
    uint32_t value;
};

struct AfkPassRulePriceComp {
    uint64_t value;
};

struct AfkPassRuleMax_remaining_daysComp {
    uint32_t value;
};

struct AfkPassRuleRemind_before_hoursComp {
    uint32_t value;
};

struct AfkPassRuleDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline AfkPassRuleIdComp MakeAfkPassRuleIdComp(const AfkPassRuleTable& row) {
    return { row.id() };
}
inline AfkPassRulePeriod_daysComp MakeAfkPassRulePeriod_daysComp(const AfkPassRuleTable& row) {
    return { row.period_days() };
}
inline AfkPassRulePrice_currency_typeComp MakeAfkPassRulePrice_currency_typeComp(const AfkPassRuleTable& row) {
    return { row.price_currency_type() };
}
inline AfkPassRulePriceComp MakeAfkPassRulePriceComp(const AfkPassRuleTable& row) {
    return { row.price() };
}
inline AfkPassRuleMax_remaining_daysComp MakeAfkPassRuleMax_remaining_daysComp(const AfkPassRuleTable& row) {
    return { row.max_remaining_days() };
}
inline AfkPassRuleRemind_before_hoursComp MakeAfkPassRuleRemind_before_hoursComp(const AfkPassRuleTable& row) {
    return { row.remind_before_hours() };
}
inline AfkPassRuleDescComp MakeAfkPassRuleDescComp(const AfkPassRuleTable& row) {
    return { std::string_view(row.desc()) };
}
