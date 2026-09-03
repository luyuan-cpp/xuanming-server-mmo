
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/class_table.pb.h"

// ============================================================
// Per-column ECS components for ClassTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct ClassIdComp {
    uint32_t value;
};

struct ClassInit_healthComp {
    uint64_t value;
};

struct ClassInit_manaComp {
    uint64_t value;
};

struct ClassInit_strengthComp {
    uint64_t value;
};

struct ClassInit_armorComp {
    uint64_t value;
};

struct ClassInit_resistanceComp {
    uint64_t value;
};

struct ClassInit_critchanceComp {
    uint64_t value;
};

struct ClassInit_speedComp {
    uint64_t value;
};

struct ClassSkillComp {
    std::span<const uint32_t> values;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline ClassIdComp MakeClassIdComp(const ClassTable& row) {
    return { row.id() };
}
inline ClassInit_healthComp MakeClassInit_healthComp(const ClassTable& row) {
    return { row.init_health() };
}
inline ClassInit_manaComp MakeClassInit_manaComp(const ClassTable& row) {
    return { row.init_mana() };
}
inline ClassInit_strengthComp MakeClassInit_strengthComp(const ClassTable& row) {
    return { row.init_strength() };
}
inline ClassInit_armorComp MakeClassInit_armorComp(const ClassTable& row) {
    return { row.init_armor() };
}
inline ClassInit_resistanceComp MakeClassInit_resistanceComp(const ClassTable& row) {
    return { row.init_resistance() };
}
inline ClassInit_critchanceComp MakeClassInit_critchanceComp(const ClassTable& row) {
    return { row.init_critchance() };
}
inline ClassInit_speedComp MakeClassInit_speedComp(const ClassTable& row) {
    return { row.init_speed() };
}
inline ClassSkillComp MakeClassSkillComp(const ClassTable& row) {
    const auto& rf = row.skill();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
