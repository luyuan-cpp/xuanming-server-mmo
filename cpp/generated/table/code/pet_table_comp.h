
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/pet_table.pb.h"

// ============================================================
// Per-column ECS components for PetTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct PetIdComp {
    uint32_t value;
};

struct PetNameComp {
    std::string_view value;
};

struct PetDescComp {
    std::string_view value;
};

struct PetModel_idComp {
    uint32_t value;
};

struct PetQualityComp {
    uint32_t value;
};

struct PetUnlock_levelComp {
    uint32_t value;
};

struct PetLevel_capComp {
    uint32_t value;
};

struct PetInit_healthComp {
    uint64_t value;
};

struct PetInit_manaComp {
    uint64_t value;
};

struct PetInit_speedComp {
    uint64_t value;
};

struct PetShop_price_goldComp {
    uint64_t value;
};

struct PetAcquisition_tierComp {
    uint32_t value;
};

struct PetCultivation_tierComp {
    uint32_t value;
};

struct PetTransfer_policyComp {
    uint32_t value;
};

struct PetExclusive_gift_slotsComp {
    uint32_t value;
};

struct PetFixed_initial_aptitudeComp {
    bool value;
};

struct PetAptitude_minComp {
    std::span<const uint32_t> values;
};

struct PetAptitude_maxComp {
    std::span<const uint32_t> values;
};

struct PetSkillComp {
    std::span<const uint32_t> values;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline PetIdComp MakePetIdComp(const PetTable& row) {
    return { row.id() };
}
inline PetNameComp MakePetNameComp(const PetTable& row) {
    return { std::string_view(row.name()) };
}
inline PetDescComp MakePetDescComp(const PetTable& row) {
    return { std::string_view(row.desc()) };
}
inline PetModel_idComp MakePetModel_idComp(const PetTable& row) {
    return { row.model_id() };
}
inline PetQualityComp MakePetQualityComp(const PetTable& row) {
    return { row.quality() };
}
inline PetUnlock_levelComp MakePetUnlock_levelComp(const PetTable& row) {
    return { row.unlock_level() };
}
inline PetLevel_capComp MakePetLevel_capComp(const PetTable& row) {
    return { row.level_cap() };
}
inline PetInit_healthComp MakePetInit_healthComp(const PetTable& row) {
    return { row.init_health() };
}
inline PetInit_manaComp MakePetInit_manaComp(const PetTable& row) {
    return { row.init_mana() };
}
inline PetInit_speedComp MakePetInit_speedComp(const PetTable& row) {
    return { row.init_speed() };
}
inline PetShop_price_goldComp MakePetShop_price_goldComp(const PetTable& row) {
    return { row.shop_price_gold() };
}
inline PetAcquisition_tierComp MakePetAcquisition_tierComp(const PetTable& row) {
    return { row.acquisition_tier() };
}
inline PetCultivation_tierComp MakePetCultivation_tierComp(const PetTable& row) {
    return { row.cultivation_tier() };
}
inline PetTransfer_policyComp MakePetTransfer_policyComp(const PetTable& row) {
    return { row.transfer_policy() };
}
inline PetExclusive_gift_slotsComp MakePetExclusive_gift_slotsComp(const PetTable& row) {
    return { row.exclusive_gift_slots() };
}
inline PetFixed_initial_aptitudeComp MakePetFixed_initial_aptitudeComp(const PetTable& row) {
    return { row.fixed_initial_aptitude() };
}
inline PetAptitude_minComp MakePetAptitude_minComp(const PetTable& row) {
    const auto& rf = row.aptitude_min();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline PetAptitude_maxComp MakePetAptitude_maxComp(const PetTable& row) {
    const auto& rf = row.aptitude_max();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
inline PetSkillComp MakePetSkillComp(const PetTable& row) {
    const auto& rf = row.skill();
    return { std::span<const uint32_t>(rf.data(), rf.size()) };
}
