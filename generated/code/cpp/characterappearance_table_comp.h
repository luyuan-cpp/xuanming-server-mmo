
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

struct CharacterAppearanceStarter_creatableComp {
    uint32_t value;
};

struct CharacterAppearanceCard_unlockableComp {
    uint32_t value;
};

struct CharacterAppearanceSeries_idComp {
    uint32_t value;
};

struct CharacterAppearanceArchive_nameComp {
    std::string_view value;
};

struct CharacterAppearanceNicknameComp {
    std::string_view value;
};

struct CharacterAppearanceBiography_summaryComp {
    std::string_view value;
};

struct CharacterAppearanceStory_titleComp {
    std::string_view value;
};

struct CharacterAppearanceStory_bodyComp {
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
inline CharacterAppearanceStarter_creatableComp MakeCharacterAppearanceStarter_creatableComp(const CharacterAppearanceTable& row) {
    return { row.starter_creatable() };
}
inline CharacterAppearanceCard_unlockableComp MakeCharacterAppearanceCard_unlockableComp(const CharacterAppearanceTable& row) {
    return { row.card_unlockable() };
}
inline CharacterAppearanceSeries_idComp MakeCharacterAppearanceSeries_idComp(const CharacterAppearanceTable& row) {
    return { row.series_id() };
}
inline CharacterAppearanceArchive_nameComp MakeCharacterAppearanceArchive_nameComp(const CharacterAppearanceTable& row) {
    return { std::string_view(row.archive_name()) };
}
inline CharacterAppearanceNicknameComp MakeCharacterAppearanceNicknameComp(const CharacterAppearanceTable& row) {
    return { std::string_view(row.nickname()) };
}
inline CharacterAppearanceBiography_summaryComp MakeCharacterAppearanceBiography_summaryComp(const CharacterAppearanceTable& row) {
    return { std::string_view(row.biography_summary()) };
}
inline CharacterAppearanceStory_titleComp MakeCharacterAppearanceStory_titleComp(const CharacterAppearanceTable& row) {
    return { std::string_view(row.story_title()) };
}
inline CharacterAppearanceStory_bodyComp MakeCharacterAppearanceStory_bodyComp(const CharacterAppearanceTable& row) {
    return { std::string_view(row.story_body()) };
}
