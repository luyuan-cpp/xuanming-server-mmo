
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/monster_table.pb.h"

// ============================================================
// Per-column ECS components for MonsterTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct MonsterIdComp {
    uint32_t value;
};

struct MonsterHealthComp {
    uint64_t value;
};

struct MonsterStrengthComp {
    uint64_t value;
};

struct MonsterArmorComp {
    uint64_t value;
};

struct MonsterResistanceComp {
    uint64_t value;
};

struct MonsterCritchanceComp {
    uint64_t value;
};

struct MonsterSpeedComp {
    uint64_t value;
};

struct MonsterExp_rewardComp {
    uint64_t value;
};

struct MonsterGold_rewardComp {
    uint64_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline MonsterIdComp MakeMonsterIdComp(const MonsterTable& row) {
    return { row.id() };
}
inline MonsterHealthComp MakeMonsterHealthComp(const MonsterTable& row) {
    return { row.health() };
}
inline MonsterStrengthComp MakeMonsterStrengthComp(const MonsterTable& row) {
    return { row.strength() };
}
inline MonsterArmorComp MakeMonsterArmorComp(const MonsterTable& row) {
    return { row.armor() };
}
inline MonsterResistanceComp MakeMonsterResistanceComp(const MonsterTable& row) {
    return { row.resistance() };
}
inline MonsterCritchanceComp MakeMonsterCritchanceComp(const MonsterTable& row) {
    return { row.critchance() };
}
inline MonsterSpeedComp MakeMonsterSpeedComp(const MonsterTable& row) {
    return { row.speed() };
}
inline MonsterExp_rewardComp MakeMonsterExp_rewardComp(const MonsterTable& row) {
    return { row.exp_reward() };
}
inline MonsterGold_rewardComp MakeMonsterGold_rewardComp(const MonsterTable& row) {
    return { row.gold_reward() };
}
