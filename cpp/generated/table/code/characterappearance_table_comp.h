
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/characterappearance_table.pb.h"

// ============================================================
// Per-column ECS components for CharacterAppearanceTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct CharacterAppearanceIdComp {
    uint32_t value;
};

struct CharacterAppearanceAppearance_idComp {
    std::string_view value;
};

struct CharacterAppearanceNameComp {
    std::string_view value;
};

struct CharacterAppearanceEnabledComp {
    uint32_t value;
};

struct CharacterAppearanceSort_orderComp {
    uint32_t value;
};

struct CharacterAppearanceDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline CharacterAppearanceIdComp MakeCharacterAppearanceIdComp(const CharacterAppearanceTable& row) {
    return { row.id() };
}
inline CharacterAppearanceAppearance_idComp MakeCharacterAppearanceAppearance_idComp(const CharacterAppearanceTable& row) {
    return { std::string_view(row.appearance_id()) };
}
inline CharacterAppearanceNameComp MakeCharacterAppearanceNameComp(const CharacterAppearanceTable& row) {
    return { std::string_view(row.name()) };
}
inline CharacterAppearanceEnabledComp MakeCharacterAppearanceEnabledComp(const CharacterAppearanceTable& row) {
    return { row.enabled() };
}
inline CharacterAppearanceSort_orderComp MakeCharacterAppearanceSort_orderComp(const CharacterAppearanceTable& row) {
    return { row.sort_order() };
}
inline CharacterAppearanceDescComp MakeCharacterAppearanceDescComp(const CharacterAppearanceTable& row) {
    return { std::string_view(row.desc()) };
}
