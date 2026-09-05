
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/attributerule_table.pb.h"

// ============================================================
// Per-column ECS components for AttributeRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct AttributeRuleIdComp {
    uint32_t value;
};

struct AttributeRuleMax_schemesComp {
    uint32_t value;
};

struct AttributeRuleFree_scheme_countComp {
    uint32_t value;
};

struct AttributeRuleCreate_scheme_cost_goldComp {
    uint64_t value;
};

struct AttributeRuleSwitch_cooldown_secondsComp {
    uint32_t value;
};

struct AttributeRuleScheme_name_max_lenComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline AttributeRuleIdComp MakeAttributeRuleIdComp(const AttributeRuleTable& row) {
    return { row.id() };
}
inline AttributeRuleMax_schemesComp MakeAttributeRuleMax_schemesComp(const AttributeRuleTable& row) {
    return { row.max_schemes() };
}
inline AttributeRuleFree_scheme_countComp MakeAttributeRuleFree_scheme_countComp(const AttributeRuleTable& row) {
    return { row.free_scheme_count() };
}
inline AttributeRuleCreate_scheme_cost_goldComp MakeAttributeRuleCreate_scheme_cost_goldComp(const AttributeRuleTable& row) {
    return { row.create_scheme_cost_gold() };
}
inline AttributeRuleSwitch_cooldown_secondsComp MakeAttributeRuleSwitch_cooldown_secondsComp(const AttributeRuleTable& row) {
    return { row.switch_cooldown_seconds() };
}
inline AttributeRuleScheme_name_max_lenComp MakeAttributeRuleScheme_name_max_lenComp(const AttributeRuleTable& row) {
    return { row.scheme_name_max_len() };
}
