
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/petrule_table.pb.h"

// ============================================================
// Per-column ECS components for PetRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct PetRuleIdComp {
    uint32_t value;
};

struct PetRuleMax_petsComp {
    uint32_t value;
};

struct PetRuleName_max_lenComp {
    uint32_t value;
};

struct PetRuleRename_cost_goldComp {
    uint64_t value;
};

struct PetRuleSummon_cooldown_secondsComp {
    uint32_t value;
};

struct PetRuleDescComp {
    std::string_view value;
};

struct PetRuleEntry_config_versionComp {
    uint32_t value;
};

struct PetRuleEntry_total_pointsComp {
    uint32_t value;
};

struct PetRuleEntry_max_directionsComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline PetRuleIdComp MakePetRuleIdComp(const PetRuleTable& row) {
    return { row.id() };
}
inline PetRuleMax_petsComp MakePetRuleMax_petsComp(const PetRuleTable& row) {
    return { row.max_pets() };
}
inline PetRuleName_max_lenComp MakePetRuleName_max_lenComp(const PetRuleTable& row) {
    return { row.name_max_len() };
}
inline PetRuleRename_cost_goldComp MakePetRuleRename_cost_goldComp(const PetRuleTable& row) {
    return { row.rename_cost_gold() };
}
inline PetRuleSummon_cooldown_secondsComp MakePetRuleSummon_cooldown_secondsComp(const PetRuleTable& row) {
    return { row.summon_cooldown_seconds() };
}
inline PetRuleDescComp MakePetRuleDescComp(const PetRuleTable& row) {
    return { std::string_view(row.desc()) };
}
inline PetRuleEntry_config_versionComp MakePetRuleEntry_config_versionComp(const PetRuleTable& row) {
    return { row.entry_config_version() };
}
inline PetRuleEntry_total_pointsComp MakePetRuleEntry_total_pointsComp(const PetRuleTable& row) {
    return { row.entry_total_points() };
}
inline PetRuleEntry_max_directionsComp MakePetRuleEntry_max_directionsComp(const PetRuleTable& row) {
    return { row.entry_max_directions() };
}
