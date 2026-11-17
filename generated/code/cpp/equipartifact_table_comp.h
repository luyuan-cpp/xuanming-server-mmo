
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/equipartifact_table.pb.h"

// ============================================================
// Per-column ECS components for EquipArtifactTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct EquipArtifactIdComp {
    uint32_t value;
};

struct EquipArtifactBonus_bpsComp {
    uint32_t value;
};

struct EquipArtifactCap_bpsComp {
    uint32_t value;
};

struct EquipArtifactPer_round_limitComp {
    uint32_t value;
};

struct EquipArtifactBattle_limitComp {
    uint32_t value;
};

struct EquipArtifactActive_roundsComp {
    uint32_t value;
};

struct EquipArtifactHealth_threshold_bpsComp {
    uint32_t value;
};

struct EquipArtifactNameComp {
    std::string_view value;
};

struct EquipArtifactDescriptionComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline EquipArtifactIdComp MakeEquipArtifactIdComp(const EquipArtifactTable& row) {
    return { row.id() };
}
inline EquipArtifactBonus_bpsComp MakeEquipArtifactBonus_bpsComp(const EquipArtifactTable& row) {
    return { row.bonus_bps() };
}
inline EquipArtifactCap_bpsComp MakeEquipArtifactCap_bpsComp(const EquipArtifactTable& row) {
    return { row.cap_bps() };
}
inline EquipArtifactPer_round_limitComp MakeEquipArtifactPer_round_limitComp(const EquipArtifactTable& row) {
    return { row.per_round_limit() };
}
inline EquipArtifactBattle_limitComp MakeEquipArtifactBattle_limitComp(const EquipArtifactTable& row) {
    return { row.battle_limit() };
}
inline EquipArtifactActive_roundsComp MakeEquipArtifactActive_roundsComp(const EquipArtifactTable& row) {
    return { row.active_rounds() };
}
inline EquipArtifactHealth_threshold_bpsComp MakeEquipArtifactHealth_threshold_bpsComp(const EquipArtifactTable& row) {
    return { row.health_threshold_bps() };
}
inline EquipArtifactNameComp MakeEquipArtifactNameComp(const EquipArtifactTable& row) {
    return { std::string_view(row.name()) };
}
inline EquipArtifactDescriptionComp MakeEquipArtifactDescriptionComp(const EquipArtifactTable& row) {
    return { std::string_view(row.description()) };
}
