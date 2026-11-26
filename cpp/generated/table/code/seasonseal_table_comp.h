
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/seasonseal_table.pb.h"

// ============================================================
// Per-column ECS components for SeasonSealTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct SeasonSealIdComp {
    uint32_t value;
};

struct SeasonSealSeason_idComp {
    uint32_t value;
};

struct SeasonSealNameComp {
    std::string_view value;
};

struct SeasonSealDescriptionComp {
    std::string_view value;
};

struct SeasonSealIcon_keyComp {
    std::string_view value;
};

struct SeasonSealEffect_idComp {
    uint32_t value;
};

struct SeasonSealBonus_bpsComp {
    uint32_t value;
};

struct SeasonSealCap_bpsComp {
    uint32_t value;
};

struct SeasonSealPer_round_limitComp {
    uint32_t value;
};

struct SeasonSealBattle_limitComp {
    uint32_t value;
};

struct SeasonSealActive_roundsComp {
    uint32_t value;
};

struct SeasonSealHealth_threshold_bpsComp {
    uint32_t value;
};

struct SeasonSealResonance_skill_idComp {
    uint32_t value;
};

struct SeasonSealResonance_kindComp {
    uint32_t value;
};

struct SeasonSealResonance_paramComp {
    uint32_t value;
};

struct SeasonSealResonance_descComp {
    std::string_view value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline SeasonSealIdComp MakeSeasonSealIdComp(const SeasonSealTable& row) {
    return { row.id() };
}
inline SeasonSealSeason_idComp MakeSeasonSealSeason_idComp(const SeasonSealTable& row) {
    return { row.season_id() };
}
inline SeasonSealNameComp MakeSeasonSealNameComp(const SeasonSealTable& row) {
    return { std::string_view(row.name()) };
}
inline SeasonSealDescriptionComp MakeSeasonSealDescriptionComp(const SeasonSealTable& row) {
    return { std::string_view(row.description()) };
}
inline SeasonSealIcon_keyComp MakeSeasonSealIcon_keyComp(const SeasonSealTable& row) {
    return { std::string_view(row.icon_key()) };
}
inline SeasonSealEffect_idComp MakeSeasonSealEffect_idComp(const SeasonSealTable& row) {
    return { row.effect_id() };
}
inline SeasonSealBonus_bpsComp MakeSeasonSealBonus_bpsComp(const SeasonSealTable& row) {
    return { row.bonus_bps() };
}
inline SeasonSealCap_bpsComp MakeSeasonSealCap_bpsComp(const SeasonSealTable& row) {
    return { row.cap_bps() };
}
inline SeasonSealPer_round_limitComp MakeSeasonSealPer_round_limitComp(const SeasonSealTable& row) {
    return { row.per_round_limit() };
}
inline SeasonSealBattle_limitComp MakeSeasonSealBattle_limitComp(const SeasonSealTable& row) {
    return { row.battle_limit() };
}
inline SeasonSealActive_roundsComp MakeSeasonSealActive_roundsComp(const SeasonSealTable& row) {
    return { row.active_rounds() };
}
inline SeasonSealHealth_threshold_bpsComp MakeSeasonSealHealth_threshold_bpsComp(const SeasonSealTable& row) {
    return { row.health_threshold_bps() };
}
inline SeasonSealResonance_skill_idComp MakeSeasonSealResonance_skill_idComp(const SeasonSealTable& row) {
    return { row.resonance_skill_id() };
}
inline SeasonSealResonance_kindComp MakeSeasonSealResonance_kindComp(const SeasonSealTable& row) {
    return { row.resonance_kind() };
}
inline SeasonSealResonance_paramComp MakeSeasonSealResonance_paramComp(const SeasonSealTable& row) {
    return { row.resonance_param() };
}
inline SeasonSealResonance_descComp MakeSeasonSealResonance_descComp(const SeasonSealTable& row) {
    return { std::string_view(row.resonance_desc()) };
}
