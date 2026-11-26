
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/playerlevelexp_table.pb.h"

// ============================================================
// Per-column ECS components for PlayerLevelExpTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct PlayerLevelExpIdComp {
    uint32_t value;
};

struct PlayerLevelExpRequired_total_expComp {
    uint64_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline PlayerLevelExpIdComp MakePlayerLevelExpIdComp(const PlayerLevelExpTable& row) {
    return { row.id() };
}
inline PlayerLevelExpRequired_total_expComp MakePlayerLevelExpRequired_total_expComp(const PlayerLevelExpTable& row) {
    return { row.required_total_exp() };
}
