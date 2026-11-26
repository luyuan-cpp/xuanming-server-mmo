
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/trialjournalmilestone_table.pb.h"

// ============================================================
// Per-column ECS components for TrialJournalMilestoneTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct TrialJournalMilestoneIdComp {
    uint32_t value;
};

struct TrialJournalMilestoneScope_chapter_idComp {
    uint32_t value;
};

struct TrialJournalMilestoneSeason_idComp {
    uint32_t value;
};

struct TrialJournalMilestoneRequired_sealsComp {
    uint32_t value;
};

struct TrialJournalMilestoneBound_sand_countComp {
    uint32_t value;
};

struct TrialJournalMilestoneBound_artifact_voucher_countComp {
    uint32_t value;
};

struct TrialJournalMilestoneBadge_keyComp {
    std::string_view value;
};

struct TrialJournalMilestoneBadge_nameComp {
    std::string_view value;
};

struct TrialJournalMilestoneCommentary_keyComp {
    std::string_view value;
};

struct TrialJournalMilestoneCommentary_titleComp {
    std::string_view value;
};

struct TrialJournalMilestoneCommentary_bodyComp {
    std::string_view value;
};

struct TrialJournalMilestoneGrant_appearance_idComp {
    std::string_view value;
};

struct TrialJournalMilestoneEnabledComp {
    bool value;
};

struct TrialJournalMilestoneTitleComp {
    std::string_view value;
};

struct TrialJournalMilestoneDescComp {
    std::string_view value;
};

struct TrialJournalMilestoneGrant_pet_cosmetic_idComp {
    uint32_t value;
};

struct TrialJournalMilestoneGrant_skill_vfx_idComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline TrialJournalMilestoneIdComp MakeTrialJournalMilestoneIdComp(const TrialJournalMilestoneTable& row) {
    return { row.id() };
}
inline TrialJournalMilestoneScope_chapter_idComp MakeTrialJournalMilestoneScope_chapter_idComp(const TrialJournalMilestoneTable& row) {
    return { row.scope_chapter_id() };
}
inline TrialJournalMilestoneSeason_idComp MakeTrialJournalMilestoneSeason_idComp(const TrialJournalMilestoneTable& row) {
    return { row.season_id() };
}
inline TrialJournalMilestoneRequired_sealsComp MakeTrialJournalMilestoneRequired_sealsComp(const TrialJournalMilestoneTable& row) {
    return { row.required_seals() };
}
inline TrialJournalMilestoneBound_sand_countComp MakeTrialJournalMilestoneBound_sand_countComp(const TrialJournalMilestoneTable& row) {
    return { row.bound_sand_count() };
}
inline TrialJournalMilestoneBound_artifact_voucher_countComp MakeTrialJournalMilestoneBound_artifact_voucher_countComp(const TrialJournalMilestoneTable& row) {
    return { row.bound_artifact_voucher_count() };
}
inline TrialJournalMilestoneBadge_keyComp MakeTrialJournalMilestoneBadge_keyComp(const TrialJournalMilestoneTable& row) {
    return { std::string_view(row.badge_key()) };
}
inline TrialJournalMilestoneBadge_nameComp MakeTrialJournalMilestoneBadge_nameComp(const TrialJournalMilestoneTable& row) {
    return { std::string_view(row.badge_name()) };
}
inline TrialJournalMilestoneCommentary_keyComp MakeTrialJournalMilestoneCommentary_keyComp(const TrialJournalMilestoneTable& row) {
    return { std::string_view(row.commentary_key()) };
}
inline TrialJournalMilestoneCommentary_titleComp MakeTrialJournalMilestoneCommentary_titleComp(const TrialJournalMilestoneTable& row) {
    return { std::string_view(row.commentary_title()) };
}
inline TrialJournalMilestoneCommentary_bodyComp MakeTrialJournalMilestoneCommentary_bodyComp(const TrialJournalMilestoneTable& row) {
    return { std::string_view(row.commentary_body()) };
}
inline TrialJournalMilestoneGrant_appearance_idComp MakeTrialJournalMilestoneGrant_appearance_idComp(const TrialJournalMilestoneTable& row) {
    return { std::string_view(row.grant_appearance_id()) };
}
inline TrialJournalMilestoneEnabledComp MakeTrialJournalMilestoneEnabledComp(const TrialJournalMilestoneTable& row) {
    return { row.enabled() };
}
inline TrialJournalMilestoneTitleComp MakeTrialJournalMilestoneTitleComp(const TrialJournalMilestoneTable& row) {
    return { std::string_view(row.title()) };
}
inline TrialJournalMilestoneDescComp MakeTrialJournalMilestoneDescComp(const TrialJournalMilestoneTable& row) {
    return { std::string_view(row.desc()) };
}
inline TrialJournalMilestoneGrant_pet_cosmetic_idComp MakeTrialJournalMilestoneGrant_pet_cosmetic_idComp(const TrialJournalMilestoneTable& row) {
    return { row.grant_pet_cosmetic_id() };
}
inline TrialJournalMilestoneGrant_skill_vfx_idComp MakeTrialJournalMilestoneGrant_skill_vfx_idComp(const TrialJournalMilestoneTable& row) {
    return { row.grant_skill_vfx_id() };
}
