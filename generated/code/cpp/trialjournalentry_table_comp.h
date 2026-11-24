
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/trialjournalentry_table.pb.h"

// ============================================================
// Per-column ECS components for TrialJournalEntryTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct TrialJournalEntryIdComp {
    uint32_t value;
};

struct TrialJournalEntryChapter_idComp {
    uint32_t value;
};

struct TrialJournalEntrySeason_idComp {
    uint32_t value;
};

struct TrialJournalEntrySource_kindComp {
    uint32_t value;
};

struct TrialJournalEntryDifficultyComp {
    uint32_t value;
};

struct TrialJournalEntryVariant_keyComp {
    std::string_view value;
};

struct TrialJournalEntryRule_versionComp {
    uint32_t value;
};

struct TrialJournalEntryRule_version_nameComp {
    std::string_view value;
};

struct TrialJournalEntryEnabledComp {
    bool value;
};

struct TrialJournalEntrySort_orderComp {
    uint32_t value;
};

struct TrialJournalEntryDisplay_nameComp {
    std::string_view value;
};

struct TrialJournalEntryChapter_nameComp {
    std::string_view value;
};

struct TrialJournalEntryIcon_keyComp {
    std::string_view value;
};

struct TrialJournalEntryUnlock_modeComp {
    uint32_t value;
};

struct TrialJournalEntryJump_targetComp {
    std::string_view value;
};

struct TrialJournalEntryDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline TrialJournalEntryIdComp MakeTrialJournalEntryIdComp(const TrialJournalEntryTable& row) {
    return { row.id() };
}
inline TrialJournalEntryChapter_idComp MakeTrialJournalEntryChapter_idComp(const TrialJournalEntryTable& row) {
    return { row.chapter_id() };
}
inline TrialJournalEntrySeason_idComp MakeTrialJournalEntrySeason_idComp(const TrialJournalEntryTable& row) {
    return { row.season_id() };
}
inline TrialJournalEntrySource_kindComp MakeTrialJournalEntrySource_kindComp(const TrialJournalEntryTable& row) {
    return { row.source_kind() };
}
inline TrialJournalEntryDifficultyComp MakeTrialJournalEntryDifficultyComp(const TrialJournalEntryTable& row) {
    return { row.difficulty() };
}
inline TrialJournalEntryVariant_keyComp MakeTrialJournalEntryVariant_keyComp(const TrialJournalEntryTable& row) {
    return { std::string_view(row.variant_key()) };
}
inline TrialJournalEntryRule_versionComp MakeTrialJournalEntryRule_versionComp(const TrialJournalEntryTable& row) {
    return { row.rule_version() };
}
inline TrialJournalEntryRule_version_nameComp MakeTrialJournalEntryRule_version_nameComp(const TrialJournalEntryTable& row) {
    return { std::string_view(row.rule_version_name()) };
}
inline TrialJournalEntryEnabledComp MakeTrialJournalEntryEnabledComp(const TrialJournalEntryTable& row) {
    return { row.enabled() };
}
inline TrialJournalEntrySort_orderComp MakeTrialJournalEntrySort_orderComp(const TrialJournalEntryTable& row) {
    return { row.sort_order() };
}
inline TrialJournalEntryDisplay_nameComp MakeTrialJournalEntryDisplay_nameComp(const TrialJournalEntryTable& row) {
    return { std::string_view(row.display_name()) };
}
inline TrialJournalEntryChapter_nameComp MakeTrialJournalEntryChapter_nameComp(const TrialJournalEntryTable& row) {
    return { std::string_view(row.chapter_name()) };
}
inline TrialJournalEntryIcon_keyComp MakeTrialJournalEntryIcon_keyComp(const TrialJournalEntryTable& row) {
    return { std::string_view(row.icon_key()) };
}
inline TrialJournalEntryUnlock_modeComp MakeTrialJournalEntryUnlock_modeComp(const TrialJournalEntryTable& row) {
    return { row.unlock_mode() };
}
inline TrialJournalEntryJump_targetComp MakeTrialJournalEntryJump_targetComp(const TrialJournalEntryTable& row) {
    return { std::string_view(row.jump_target()) };
}
inline TrialJournalEntryDescComp MakeTrialJournalEntryDescComp(const TrialJournalEntryTable& row) {
    return { std::string_view(row.desc()) };
}
