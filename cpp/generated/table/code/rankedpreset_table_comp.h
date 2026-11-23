
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/rankedpreset_table.pb.h"

// ============================================================
// Per-column ECS components for RankedPresetTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct RankedPresetIdComp {
    uint32_t value;
};

struct RankedPresetRole_kindComp {
    uint32_t value;
};

struct RankedPresetCodeComp {
    std::string_view value;
};

struct RankedPresetNameComp {
    std::string_view value;
};

struct RankedPresetRole_tagComp {
    std::string_view value;
};

struct RankedPresetPet_table_idComp {
    uint32_t value;
};

struct RankedPresetWeight_conComp {
    uint32_t value;
};

struct RankedPresetWeight_spiComp {
    uint32_t value;
};

struct RankedPresetWeight_strComp {
    uint32_t value;
};

struct RankedPresetWeight_agiComp {
    uint32_t value;
};

struct RankedPresetSortComp {
    uint32_t value;
};

struct RankedPresetDescComp {
    std::string_view value;
};

struct RankedPresetSkill_idsComp {
    std::span<const uint32_t> values;
};

struct RankedPresetPet_entry_idsComp {
    std::span<const uint32_t> values;
};

struct RankedPresetPet_entry_pointsComp {
    std::span<const uint32_t> values;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline RankedPresetIdComp MakeRankedPresetIdComp(const RankedPresetTable& row) {
    return { row.id() };
}
inline RankedPresetRole_kindComp MakeRankedPresetRole_kindComp(const RankedPresetTable& row) {
    return { row.role_kind() };
}
inline RankedPresetCodeComp MakeRankedPresetCodeComp(const RankedPresetTable& row) {
    return { std::string_view(row.code()) };
}
inline RankedPresetNameComp MakeRankedPresetNameComp(const RankedPresetTable& row) {
    return { std::string_view(row.name()) };
}
inline RankedPresetRole_tagComp MakeRankedPresetRole_tagComp(const RankedPresetTable& row) {
    return { std::string_view(row.role_tag()) };
}
inline RankedPresetPet_table_idComp MakeRankedPresetPet_table_idComp(const RankedPresetTable& row) {
    return { row.pet_table_id() };
}
inline RankedPresetWeight_conComp MakeRankedPresetWeight_conComp(const RankedPresetTable& row) {
    return { row.weight_con() };
}
inline RankedPresetWeight_spiComp MakeRankedPresetWeight_spiComp(const RankedPresetTable& row) {
    return { row.weight_spi() };
}
inline RankedPresetWeight_strComp MakeRankedPresetWeight_strComp(const RankedPresetTable& row) {
    return { row.weight_str() };
}
inline RankedPresetWeight_agiComp MakeRankedPresetWeight_agiComp(const RankedPresetTable& row) {
    return { row.weight_agi() };
}
inline RankedPresetSortComp MakeRankedPresetSortComp(const RankedPresetTable& row) {
    return { row.sort() };
}
inline RankedPresetDescComp MakeRankedPresetDescComp(const RankedPresetTable& row) {
    return { std::string_view(row.desc()) };
}
inline RankedPresetSkill_idsComp MakeRankedPresetSkill_idsComp(const RankedPresetTable& row) {
    const auto& rf = row.skill_ids();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline RankedPresetPet_entry_idsComp MakeRankedPresetPet_entry_idsComp(const RankedPresetTable& row) {
    const auto& rf = row.pet_entry_ids();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline RankedPresetPet_entry_pointsComp MakeRankedPresetPet_entry_pointsComp(const RankedPresetTable& row) {
    const auto& rf = row.pet_entry_points();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
