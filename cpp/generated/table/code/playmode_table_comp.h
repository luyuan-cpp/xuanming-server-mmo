
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/playmode_table.pb.h"

// ============================================================
// Per-column ECS components for PlayModeTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct PlayModeIdComp {
    uint32_t value;
};

struct PlayModeMatch_modeComp {
    uint32_t value;
};

struct PlayModeBattle_config_idComp {
    uint32_t value;
};

struct PlayModeEnabledComp {
    bool value;
};

struct PlayModeTeam_size_maxComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline PlayModeIdComp MakePlayModeIdComp(const PlayModeTable& row) {
    return { row.id() };
}
inline PlayModeMatch_modeComp MakePlayModeMatch_modeComp(const PlayModeTable& row) {
    return { row.match_mode() };
}
inline PlayModeBattle_config_idComp MakePlayModeBattle_config_idComp(const PlayModeTable& row) {
    return { row.battle_config_id() };
}
inline PlayModeEnabledComp MakePlayModeEnabledComp(const PlayModeTable& row) {
    return { row.enabled() };
}
inline PlayModeTeam_size_maxComp MakePlayModeTeam_size_maxComp(const PlayModeTable& row) {
    return { row.team_size_max() };
}
