
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/festivalbeasttier_table.pb.h"

// ============================================================
// Per-column ECS components for FestivalBeastTierTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct FestivalBeastTierIdComp {
    uint32_t value;
};

struct FestivalBeastTierMin_levelComp {
    uint32_t value;
};

struct FestivalBeastTierMax_levelComp {
    uint32_t value;
};

struct FestivalBeastTierBoss_healthComp {
    uint64_t value;
};

struct FestivalBeastTierBoss_physical_attackComp {
    uint64_t value;
};

struct FestivalBeastTierBoss_defenseComp {
    uint64_t value;
};

struct FestivalBeastTierBoss_resistanceComp {
    uint64_t value;
};

struct FestivalBeastTierBoss_speedComp {
    uint64_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline FestivalBeastTierIdComp MakeFestivalBeastTierIdComp(const FestivalBeastTierTable& row) {
    return { row.id() };
}
inline FestivalBeastTierMin_levelComp MakeFestivalBeastTierMin_levelComp(const FestivalBeastTierTable& row) {
    return { row.min_level() };
}
inline FestivalBeastTierMax_levelComp MakeFestivalBeastTierMax_levelComp(const FestivalBeastTierTable& row) {
    return { row.max_level() };
}
inline FestivalBeastTierBoss_healthComp MakeFestivalBeastTierBoss_healthComp(const FestivalBeastTierTable& row) {
    return { row.boss_health() };
}
inline FestivalBeastTierBoss_physical_attackComp MakeFestivalBeastTierBoss_physical_attackComp(const FestivalBeastTierTable& row) {
    return { row.boss_physical_attack() };
}
inline FestivalBeastTierBoss_defenseComp MakeFestivalBeastTierBoss_defenseComp(const FestivalBeastTierTable& row) {
    return { row.boss_defense() };
}
inline FestivalBeastTierBoss_resistanceComp MakeFestivalBeastTierBoss_resistanceComp(const FestivalBeastTierTable& row) {
    return { row.boss_resistance() };
}
inline FestivalBeastTierBoss_speedComp MakeFestivalBeastTierBoss_speedComp(const FestivalBeastTierTable& row) {
    return { row.boss_speed() };
}
