
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/trialjournalseal_table.pb.h"

// ============================================================
// Per-column ECS components for TrialJournalSealTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct TrialJournalSealIdComp {
    uint32_t value;
};

struct TrialJournalSealEntry_idComp {
    uint32_t value;
};

struct TrialJournalSealSeal_slot_kindComp {
    uint32_t value;
};

struct TrialJournalSealCondition_typeComp {
    uint32_t value;
};

struct TrialJournalSealCondition_paramComp {
    uint32_t value;
};

struct TrialJournalSealEnabledComp {
    bool value;
};

struct TrialJournalSealTitleComp {
    std::string_view value;
};

struct TrialJournalSealDescComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline TrialJournalSealIdComp MakeTrialJournalSealIdComp(const TrialJournalSealTable& row) {
    return { row.id() };
}
inline TrialJournalSealEntry_idComp MakeTrialJournalSealEntry_idComp(const TrialJournalSealTable& row) {
    return { row.entry_id() };
}
inline TrialJournalSealSeal_slot_kindComp MakeTrialJournalSealSeal_slot_kindComp(const TrialJournalSealTable& row) {
    return { row.seal_slot_kind() };
}
inline TrialJournalSealCondition_typeComp MakeTrialJournalSealCondition_typeComp(const TrialJournalSealTable& row) {
    return { row.condition_type() };
}
inline TrialJournalSealCondition_paramComp MakeTrialJournalSealCondition_paramComp(const TrialJournalSealTable& row) {
    return { row.condition_param() };
}
inline TrialJournalSealEnabledComp MakeTrialJournalSealEnabledComp(const TrialJournalSealTable& row) {
    return { row.enabled() };
}
inline TrialJournalSealTitleComp MakeTrialJournalSealTitleComp(const TrialJournalSealTable& row) {
    return { std::string_view(row.title()) };
}
inline TrialJournalSealDescComp MakeTrialJournalSealDescComp(const TrialJournalSealTable& row) {
    return { std::string_view(row.desc()) };
}
