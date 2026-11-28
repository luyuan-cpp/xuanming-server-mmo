
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/appearanceseries_table.pb.h"

// ============================================================
// Per-column ECS components for AppearanceSeriesTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct AppearanceSeriesIdComp {
    uint32_t value;
};

struct AppearanceSeriesSeries_keyComp {
    std::string_view value;
};

struct AppearanceSeriesNameComp {
    std::string_view value;
};

struct AppearanceSeriesSort_orderComp {
    uint32_t value;
};

struct AppearanceSeriesEnabledComp {
    uint32_t value;
};

struct AppearanceSeriesDescComp {
    std::string_view value;
};

struct AppearanceSeriesCompletion_badge_nameComp {
    std::string_view value;
};

struct AppearanceSeriesEpilogue_titleComp {
    std::string_view value;
};

struct AppearanceSeriesEpilogue_bodyComp {
    std::string_view value;
};

struct AppearanceSeriesBundle_discount_bpsComp {
    uint32_t value;
};

struct AppearanceSeriesUnit_price_diamondComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline AppearanceSeriesIdComp MakeAppearanceSeriesIdComp(const AppearanceSeriesTable& row) {
    return { row.id() };
}
inline AppearanceSeriesSeries_keyComp MakeAppearanceSeriesSeries_keyComp(const AppearanceSeriesTable& row) {
    return { std::string_view(row.series_key()) };
}
inline AppearanceSeriesNameComp MakeAppearanceSeriesNameComp(const AppearanceSeriesTable& row) {
    return { std::string_view(row.name()) };
}
inline AppearanceSeriesSort_orderComp MakeAppearanceSeriesSort_orderComp(const AppearanceSeriesTable& row) {
    return { row.sort_order() };
}
inline AppearanceSeriesEnabledComp MakeAppearanceSeriesEnabledComp(const AppearanceSeriesTable& row) {
    return { row.enabled() };
}
inline AppearanceSeriesDescComp MakeAppearanceSeriesDescComp(const AppearanceSeriesTable& row) {
    return { std::string_view(row.desc()) };
}
inline AppearanceSeriesCompletion_badge_nameComp MakeAppearanceSeriesCompletion_badge_nameComp(const AppearanceSeriesTable& row) {
    return { std::string_view(row.completion_badge_name()) };
}
inline AppearanceSeriesEpilogue_titleComp MakeAppearanceSeriesEpilogue_titleComp(const AppearanceSeriesTable& row) {
    return { std::string_view(row.epilogue_title()) };
}
inline AppearanceSeriesEpilogue_bodyComp MakeAppearanceSeriesEpilogue_bodyComp(const AppearanceSeriesTable& row) {
    return { std::string_view(row.epilogue_body()) };
}
inline AppearanceSeriesBundle_discount_bpsComp MakeAppearanceSeriesBundle_discount_bpsComp(const AppearanceSeriesTable& row) {
    return { row.bundle_discount_bps() };
}
inline AppearanceSeriesUnit_price_diamondComp MakeAppearanceSeriesUnit_price_diamondComp(const AppearanceSeriesTable& row) {
    return { row.unit_price_diamond() };
}
