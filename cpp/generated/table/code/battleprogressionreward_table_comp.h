
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/battleprogressionreward_table.pb.h"

// ============================================================
// Per-column ECS components for BattleProgressionRewardTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct BattleProgressionRewardIdComp {
    uint32_t value;
};

struct BattleProgressionRewardMatch_modeComp {
    uint32_t value;
};

struct BattleProgressionRewardDifficultyComp {
    uint32_t value;
};

struct BattleProgressionRewardOutcome_tierComp {
    uint32_t value;
};

struct BattleProgressionRewardExp_gainComp {
    uint64_t value;
};

struct BattleProgressionRewardSilver_gainComp {
    uint64_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline BattleProgressionRewardIdComp MakeBattleProgressionRewardIdComp(const BattleProgressionRewardTable& row) {
    return { row.id() };
}
inline BattleProgressionRewardMatch_modeComp MakeBattleProgressionRewardMatch_modeComp(const BattleProgressionRewardTable& row) {
    return { row.match_mode() };
}
inline BattleProgressionRewardDifficultyComp MakeBattleProgressionRewardDifficultyComp(const BattleProgressionRewardTable& row) {
    return { row.difficulty() };
}
inline BattleProgressionRewardOutcome_tierComp MakeBattleProgressionRewardOutcome_tierComp(const BattleProgressionRewardTable& row) {
    return { row.outcome_tier() };
}
inline BattleProgressionRewardExp_gainComp MakeBattleProgressionRewardExp_gainComp(const BattleProgressionRewardTable& row) {
    return { row.exp_gain() };
}
inline BattleProgressionRewardSilver_gainComp MakeBattleProgressionRewardSilver_gainComp(const BattleProgressionRewardTable& row) {
    return { row.silver_gain() };
}
