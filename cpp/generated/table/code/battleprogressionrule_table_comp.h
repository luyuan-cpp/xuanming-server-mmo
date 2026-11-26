
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/battleprogressionrule_table.pb.h"

// ============================================================
// Per-column ECS components for BattleProgressionRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct BattleProgressionRuleIdComp {
    uint32_t value;
};

struct BattleProgressionRuleWeekly_silver_soft_capComp {
    uint64_t value;
};

struct BattleProgressionRuleDiminished_rate_permyriadComp {
    uint32_t value;
};

struct BattleProgressionRuleWeekly_silver_hard_capComp {
    uint64_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline BattleProgressionRuleIdComp MakeBattleProgressionRuleIdComp(const BattleProgressionRuleTable& row) {
    return { row.id() };
}
inline BattleProgressionRuleWeekly_silver_soft_capComp MakeBattleProgressionRuleWeekly_silver_soft_capComp(const BattleProgressionRuleTable& row) {
    return { row.weekly_silver_soft_cap() };
}
inline BattleProgressionRuleDiminished_rate_permyriadComp MakeBattleProgressionRuleDiminished_rate_permyriadComp(const BattleProgressionRuleTable& row) {
    return { row.diminished_rate_permyriad() };
}
inline BattleProgressionRuleWeekly_silver_hard_capComp MakeBattleProgressionRuleWeekly_silver_hard_capComp(const BattleProgressionRuleTable& row) {
    return { row.weekly_silver_hard_cap() };
}
