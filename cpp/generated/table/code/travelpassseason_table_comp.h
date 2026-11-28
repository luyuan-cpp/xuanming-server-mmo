
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/travelpassseason_table.pb.h"

// ============================================================
// Per-column ECS components for TravelPassSeasonTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct TravelPassSeasonIdComp {
    uint32_t value;
};

struct TravelPassSeasonEnabledComp {
    uint32_t value;
};

struct TravelPassSeasonNameComp {
    std::string_view value;
};

struct TravelPassSeasonDescriptionComp {
    std::string_view value;
};

struct TravelPassSeasonIcon_keyComp {
    std::string_view value;
};

struct TravelPassSeasonBanner_asset_keyComp {
    std::string_view value;
};

struct TravelPassSeasonStarts_at_msComp {
    uint64_t value;
};

struct TravelPassSeasonEnds_at_msComp {
    uint64_t value;
};

struct TravelPassSeasonMax_pointsComp {
    uint32_t value;
};

struct TravelPassSeasonPve_win_pointsComp {
    uint32_t value;
};

struct TravelPassSeasonHard_or_pvp_win_pointsComp {
    uint32_t value;
};

struct TravelPassSeasonPvp_qualified_other_pointsComp {
    uint32_t value;
};

struct TravelPassSeasonPremium_currency_typeComp {
    uint32_t value;
};

struct TravelPassSeasonPremium_priceComp {
    uint64_t value;
};

struct TravelPassSeasonConfig_revisionComp {
    uint32_t value;
};

struct TravelPassSeasonSort_orderComp {
    uint32_t value;
};

struct TravelPassSeasonIs_archivedComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline TravelPassSeasonIdComp MakeTravelPassSeasonIdComp(const TravelPassSeasonTable& row) {
    return { row.id() };
}
inline TravelPassSeasonEnabledComp MakeTravelPassSeasonEnabledComp(const TravelPassSeasonTable& row) {
    return { row.enabled() };
}
inline TravelPassSeasonNameComp MakeTravelPassSeasonNameComp(const TravelPassSeasonTable& row) {
    return { std::string_view(row.name()) };
}
inline TravelPassSeasonDescriptionComp MakeTravelPassSeasonDescriptionComp(const TravelPassSeasonTable& row) {
    return { std::string_view(row.description()) };
}
inline TravelPassSeasonIcon_keyComp MakeTravelPassSeasonIcon_keyComp(const TravelPassSeasonTable& row) {
    return { std::string_view(row.icon_key()) };
}
inline TravelPassSeasonBanner_asset_keyComp MakeTravelPassSeasonBanner_asset_keyComp(const TravelPassSeasonTable& row) {
    return { std::string_view(row.banner_asset_key()) };
}
inline TravelPassSeasonStarts_at_msComp MakeTravelPassSeasonStarts_at_msComp(const TravelPassSeasonTable& row) {
    return { row.starts_at_ms() };
}
inline TravelPassSeasonEnds_at_msComp MakeTravelPassSeasonEnds_at_msComp(const TravelPassSeasonTable& row) {
    return { row.ends_at_ms() };
}
inline TravelPassSeasonMax_pointsComp MakeTravelPassSeasonMax_pointsComp(const TravelPassSeasonTable& row) {
    return { row.max_points() };
}
inline TravelPassSeasonPve_win_pointsComp MakeTravelPassSeasonPve_win_pointsComp(const TravelPassSeasonTable& row) {
    return { row.pve_win_points() };
}
inline TravelPassSeasonHard_or_pvp_win_pointsComp MakeTravelPassSeasonHard_or_pvp_win_pointsComp(const TravelPassSeasonTable& row) {
    return { row.hard_or_pvp_win_points() };
}
inline TravelPassSeasonPvp_qualified_other_pointsComp MakeTravelPassSeasonPvp_qualified_other_pointsComp(const TravelPassSeasonTable& row) {
    return { row.pvp_qualified_other_points() };
}
inline TravelPassSeasonPremium_currency_typeComp MakeTravelPassSeasonPremium_currency_typeComp(const TravelPassSeasonTable& row) {
    return { row.premium_currency_type() };
}
inline TravelPassSeasonPremium_priceComp MakeTravelPassSeasonPremium_priceComp(const TravelPassSeasonTable& row) {
    return { row.premium_price() };
}
inline TravelPassSeasonConfig_revisionComp MakeTravelPassSeasonConfig_revisionComp(const TravelPassSeasonTable& row) {
    return { row.config_revision() };
}
inline TravelPassSeasonSort_orderComp MakeTravelPassSeasonSort_orderComp(const TravelPassSeasonTable& row) {
    return { row.sort_order() };
}
inline TravelPassSeasonIs_archivedComp MakeTravelPassSeasonIs_archivedComp(const TravelPassSeasonTable& row) {
    return { row.is_archived() };
}
