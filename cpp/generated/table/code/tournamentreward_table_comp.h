
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/tournamentreward_table.pb.h"

// ============================================================
// Per-column ECS components for TournamentRewardTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct TournamentRewardIdComp {
    uint32_t value;
};

struct TournamentRewardTier_kindComp {
    uint32_t value;
};

struct TournamentRewardMin_rankComp {
    uint32_t value;
};

struct TournamentRewardMax_rankComp {
    uint32_t value;
};

struct TournamentRewardGold_amountComp {
    uint64_t value;
};

struct TournamentRewardTitle_codeComp {
    std::string_view value;
};

struct TournamentRewardBadge_codeComp {
    std::string_view value;
};

struct TournamentRewardMail_titleComp {
    std::string_view value;
};

struct TournamentRewardMail_bodyComp {
    std::string_view value;
};

struct TournamentRewardDescComp {
    std::string_view value;
};

struct TournamentRewardItem_idsComp {
    std::span<const uint32_t> values;
};

struct TournamentRewardItem_countsComp {
    std::span<const uint32_t> values;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline TournamentRewardIdComp MakeTournamentRewardIdComp(const TournamentRewardTable& row) {
    return { row.id() };
}
inline TournamentRewardTier_kindComp MakeTournamentRewardTier_kindComp(const TournamentRewardTable& row) {
    return { row.tier_kind() };
}
inline TournamentRewardMin_rankComp MakeTournamentRewardMin_rankComp(const TournamentRewardTable& row) {
    return { row.min_rank() };
}
inline TournamentRewardMax_rankComp MakeTournamentRewardMax_rankComp(const TournamentRewardTable& row) {
    return { row.max_rank() };
}
inline TournamentRewardGold_amountComp MakeTournamentRewardGold_amountComp(const TournamentRewardTable& row) {
    return { row.gold_amount() };
}
inline TournamentRewardTitle_codeComp MakeTournamentRewardTitle_codeComp(const TournamentRewardTable& row) {
    return { std::string_view(row.title_code()) };
}
inline TournamentRewardBadge_codeComp MakeTournamentRewardBadge_codeComp(const TournamentRewardTable& row) {
    return { std::string_view(row.badge_code()) };
}
inline TournamentRewardMail_titleComp MakeTournamentRewardMail_titleComp(const TournamentRewardTable& row) {
    return { std::string_view(row.mail_title()) };
}
inline TournamentRewardMail_bodyComp MakeTournamentRewardMail_bodyComp(const TournamentRewardTable& row) {
    return { std::string_view(row.mail_body()) };
}
inline TournamentRewardDescComp MakeTournamentRewardDescComp(const TournamentRewardTable& row) {
    return { std::string_view(row.desc()) };
}
inline TournamentRewardItem_idsComp MakeTournamentRewardItem_idsComp(const TournamentRewardTable& row) {
    const auto& rf = row.item_ids();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline TournamentRewardItem_countsComp MakeTournamentRewardItem_countsComp(const TournamentRewardTable& row) {
    const auto& rf = row.item_counts();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
