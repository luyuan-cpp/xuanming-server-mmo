
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/rolenamerule_table.pb.h"

// ============================================================
// Per-column ECS components for RoleNameRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct RoleNameRuleIdComp {
    uint32_t value;
};

struct RoleNameRuleMin_charsComp {
    uint32_t value;
};

struct RoleNameRuleMax_charsComp {
    uint32_t value;
};

struct RoleNameRuleGenerated_prefixComp {
    std::string_view value;
};

struct RoleNameRuleGenerated_suffix_lenComp {
    uint32_t value;
};

struct RoleNameRuleMax_generate_attemptsComp {
    uint32_t value;
};

struct RoleNameRuleDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline RoleNameRuleIdComp MakeRoleNameRuleIdComp(const RoleNameRuleTable& row) {
    return { row.id() };
}
inline RoleNameRuleMin_charsComp MakeRoleNameRuleMin_charsComp(const RoleNameRuleTable& row) {
    return { row.min_chars() };
}
inline RoleNameRuleMax_charsComp MakeRoleNameRuleMax_charsComp(const RoleNameRuleTable& row) {
    return { row.max_chars() };
}
inline RoleNameRuleGenerated_prefixComp MakeRoleNameRuleGenerated_prefixComp(const RoleNameRuleTable& row) {
    return { std::string_view(row.generated_prefix()) };
}
inline RoleNameRuleGenerated_suffix_lenComp MakeRoleNameRuleGenerated_suffix_lenComp(const RoleNameRuleTable& row) {
    return { row.generated_suffix_len() };
}
inline RoleNameRuleMax_generate_attemptsComp MakeRoleNameRuleMax_generate_attemptsComp(const RoleNameRuleTable& row) {
    return { row.max_generate_attempts() };
}
inline RoleNameRuleDescComp MakeRoleNameRuleDescComp(const RoleNameRuleTable& row) {
    return { std::string_view(row.desc()) };
}
