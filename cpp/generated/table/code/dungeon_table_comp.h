
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/dungeon_table.pb.h"

// ============================================================
// Per-column ECS components for DungeonTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct DungeonIdComp {
    uint32_t value;
};

struct DungeonScene_idComp {
    uint32_t value;
};

struct DungeonMax_team_sizeComp {
    uint32_t value;
};

struct DungeonTime_limitComp {
    uint32_t value;
};

struct DungeonMonsterComp {
    std::span<const uint32_t> values;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline DungeonIdComp MakeDungeonIdComp(const DungeonTable& row) {
    return { row.id() };
}
inline DungeonScene_idComp MakeDungeonScene_idComp(const DungeonTable& row) {
    return { row.scene_id() };
}
inline DungeonMax_team_sizeComp MakeDungeonMax_team_sizeComp(const DungeonTable& row) {
    return { row.max_team_size() };
}
inline DungeonTime_limitComp MakeDungeonTime_limitComp(const DungeonTable& row) {
    return { row.time_limit() };
}
inline DungeonMonsterComp MakeDungeonMonsterComp(const DungeonTable& row) {
    const auto& rf = row.monster();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
