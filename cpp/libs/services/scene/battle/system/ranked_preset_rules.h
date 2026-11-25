#pragma once

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <limits>
#include <map>
#include <numeric>
#include <sstream>
#include <string>
#include <utility>
#include <vector>

#include <muduo/base/Logging.h>

#include "core/utils/encode/sha256.h"
#include "player/system/attribute_allocation_rules.h"
#include "player/system/pet_rules.h"
#include "player/system/player_level_rules.h"
#include "proto/battle/battle_data.pb.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/pet_table.h"
#include "table/code/petentry_table.h"
#include "table/code/petrule_table.h"
#include "table/code/rankedpreset_table.h"
#include "table/code/rankedrule_table.h"
#include "table/code/skill_table.h"

// 论剑首发 V1 竞技预设、配表指纹与零资产侵入快照纯规则（零 ECS 依赖，不读写任何玩家真实装备/技能/宠物属性）。
namespace rankedpreset {

inline constexpr uint32_t kRuleRowId = 1;
inline constexpr uint32_t kRoleKindCharacter = 1;
inline constexpr uint32_t kRoleKindPet = 2;
inline constexpr uint32_t kExpectedWeightSum = 5;
inline constexpr uint32_t kPlayerPoolOwnerType = 0;
inline constexpr uint32_t kPetPoolOwnerType = 1;
inline constexpr uint64_t kFallbackSpeed = 120;

struct DerivedStats {
    uint64_t maxHealth = 1;
    uint64_t maxMana = 0;
    uint64_t physicalAttack = 0;
    uint64_t magicAttack = 0;
    uint64_t speed = kFallbackSpeed;
    uint64_t defense = 0;
};

// 最大余数法按 4 维权重分配总点数：先 floor(total * w / sumW)，余数降序补 1，同余数按 dimensionId 升序。
inline std::array<uint32_t, 4> AllocateWeightedPoints(
    uint32_t totalPoints,
    const std::array<std::pair<uint32_t, uint32_t>, 4>& dimWeights) {
    uint32_t sumW = 0;
    for (const auto& [dimId, w] : dimWeights) {
        (void)dimId;
        sumW += w;
    }
    std::array<uint32_t, 4> result{0, 0, 0, 0};
    if (sumW == 0 || totalPoints == 0) {
        return result;
    }
    std::array<uint32_t, 4> rem{0, 0, 0, 0};
    uint32_t assigned = 0;
    for (std::size_t i = 0; i < 4; ++i) {
        const uint64_t num = static_cast<uint64_t>(totalPoints) * dimWeights[i].second;
        result[i] = static_cast<uint32_t>(num / sumW);
        rem[i] = static_cast<uint32_t>(num % sumW);
        assigned += result[i];
    }
    uint32_t leftover = totalPoints > assigned ? (totalPoints - assigned) : 0;
    std::array<std::size_t, 4> order{0, 1, 2, 3};
    std::sort(order.begin(), order.end(), [&](std::size_t a, std::size_t b) {
        if (rem[a] != rem[b]) {
            return rem[a] > rem[b];
        }
        return dimWeights[a].first < dimWeights[b].first;
    });
    for (std::size_t i = 0; i < 4 && leftover > 0; ++i, --leftover) {
        result[order[i]] += 1;
    }
    return result;
}

inline bool IsRankedCastableSkill(const SkillTable& skill) {
    return skill.battle_model() == 1 && skill.ability_kind() == 1;
}

inline uint64_t FloorToU64(double value) {
    constexpr double kUint64Ceiling = 18446744073709551616.0;
    if (value <= 0.0) {
        return 0;
    }
    if (value >= kUint64Ceiling) {
        return std::numeric_limits<uint64_t>::max();
    }
    return static_cast<uint64_t>(std::floor(value));
}

inline attributerules::PoolRule ToPoolRule(const AttributePoolTable& row) {
    attributerules::PoolRule rule;
    rule.poolId = row.id();
    rule.unlockLevel = row.unlock_level();
    rule.pointsPerLevel = row.points_per_level();
    rule.basePoints = row.base_points();
    rule.dimensionCap = row.dimension_cap();
    return rule;
}

inline const AttributePoolTable* FindPoolByOwner(uint32_t ownerType) {
    const AttributePoolTable* best = nullptr;
    for (const auto& row : AttributePoolTableManager::Instance().FindAll().data()) {
        if (row.owner_type() != ownerType) {
            continue;
        }
        if (best == nullptr || row.id() < best->id()) {
            best = &row;
        }
    }
    return best;
}

inline std::vector<const AttributeDimensionTable*> SortedDimensionsForPool(uint32_t poolId) {
    std::vector<const AttributeDimensionTable*> dims;
    for (const auto* dim : AttributeDimensionTableManager::Instance().GetByPoolId(poolId)) {
        if (dim != nullptr) {
            dims.push_back(dim);
        }
    }
    std::sort(dims.begin(), dims.end(),
              [](const AttributeDimensionTable* lhs, const AttributeDimensionTable* rhs) {
                  return lhs->id() < rhs->id();
              });
    return dims;
}

inline const AttributeAllocRatioTable* FindAllocRatio(uint32_t dimensionId, uint32_t classId) {
    const AttributeAllocRatioTable* fallback = nullptr;
    for (const auto& row : AttributeAllocRatioTableManager::Instance().FindAll().data()) {
        if (row.dimension_id() != dimensionId) {
            continue;
        }
        if (classId != 0 && row.class_id() == classId) {
            return &row;
        }
        if (row.class_id() == 0) {
            fallback = &row;
        }
    }
    return fallback;
}

inline std::string ComputeConfigFingerprint() {
    const auto* rule = RankedRuleTableManager::Instance().FindByIdSilent(kRuleRowId).first;
    if (rule == nullptr) {
        return {};
    }
    std::vector<const RankedPresetTable*> presets;
    for (const auto& row : RankedPresetTableManager::Instance().FindAll().data()) {
        presets.push_back(&row);
    }
    std::sort(presets.begin(), presets.end(),
              [](const RankedPresetTable* a, const RankedPresetTable* b) { return a->id() < b->id(); });

    std::ostringstream oss;
    oss << "ranked-v1\n"
        << "rule:id=1,ver=" << rule->rule_version()
        << ",cfg=" << rule->battle_config_id()
        << ",cls=" << rule->reference_class_id()
        << ",lvl=" << rule->competitive_level()
        << ",defc=" << rule->default_character_preset_id()
        << ",defp=" << rule->default_pet_preset_id() << "\n";
    for (const auto* p : presets) {
        oss << "preset:" << p->id()
            << ",kind=" << p->role_kind()
            << ",pet=" << p->pet_table_id()
            << ",w=" << p->weight_con() << "/" << p->weight_spi() << "/"
            << p->weight_str() << "/" << p->weight_agi()
            << ",sk=";
        for (int i = 0; i < p->skill_ids_size(); ++i) {
            if (i > 0) oss << '|';
            oss << p->skill_ids(i);
        }
        oss << ",pe=";
        const int pairCount = std::min(p->pet_entry_ids_size(), p->pet_entry_points_size());
        for (int i = 0; i < pairCount; ++i) {
            if (i > 0) oss << '|';
            oss << p->pet_entry_ids(i) << ':' << p->pet_entry_points(i);
        }
        oss << "\n";
    }
    return Sha256::HashToHex(oss.str());
}

inline bool ValidateTables() {
    const auto* rule = RankedRuleTableManager::Instance().FindByIdSilent(kRuleRowId).first;
    if (rule == nullptr || rule->rule_version() == 0 || rule->battle_config_id() == 0 ||
        rule->reference_class_id() == 0 || rule->competitive_level() == 0) {
        LOG_ERROR << "[RankedPreset] table validation failed: RankedRule id=1 missing or invalid";
        return false;
    }
    const auto* dungeon = DungeonTableManager::Instance().FindByIdSilent(rule->battle_config_id()).first;
    if (dungeon == nullptr || !dungeon->monster().empty() ||
        dungeon->max_team_size() < 5 || dungeon->allow_pet_capture() != 0) {
        LOG_ERROR << "[RankedPreset] table validation failed: Dungeon invalid for battle_config_id="
                  << rule->battle_config_id();
        return false;
    }
    if (ClassTableManager::Instance().FindByIdSilent(rule->reference_class_id()).first == nullptr) {
        LOG_ERROR << "[RankedPreset] table validation failed: Class missing for reference_class_id="
                  << rule->reference_class_id();
        return false;
    }
    const auto* playerPool = FindPoolByOwner(kPlayerPoolOwnerType);
    const auto* petPool = FindPoolByOwner(kPetPoolOwnerType);
    if (playerPool == nullptr || SortedDimensionsForPool(playerPool->id()).size() != 4 ||
        petPool == nullptr || SortedDimensionsForPool(petPool->id()).size() != 4) {
        LOG_ERROR << "[RankedPreset] table validation failed: player or pet attribute pool dimensions invalid";
        return false;
    }
    const auto& petRules = PetRuleTableManager::Instance().FindAll().data();
    const uint32_t expectedEntryBudget = petRules.empty() ? 0u : petRules.Get(0).entry_total_points();
    if (expectedEntryBudget == 0) {
        LOG_ERROR << "[RankedPreset] table validation failed: PetRule.entry_total_points is 0";
        return false;
    }

    uint32_t charCount = 0;
    uint32_t petCount = 0;
    bool hasDefaultChar = false;
    bool hasDefaultPet = false;
    for (const auto& preset : RankedPresetTableManager::Instance().FindAll().data()) {
        if (preset.role_kind() != kRoleKindCharacter && preset.role_kind() != kRoleKindPet) {
            LOG_ERROR << "[RankedPreset] table validation failed: invalid role_kind preset_id=" << preset.id();
            return false;
        }
        const uint32_t wSum = preset.weight_con() + preset.weight_spi() + preset.weight_str() + preset.weight_agi();
        if (wSum != kExpectedWeightSum) {
            LOG_ERROR << "[RankedPreset] table validation failed: weight sum mismatch preset_id=" << preset.id()
                      << " sum=" << wSum;
            return false;
        }
        if (preset.skill_ids_size() == 0) {
            LOG_ERROR << "[RankedPreset] table validation failed: empty skill_ids preset_id=" << preset.id();
            return false;
        }
        const uint32_t requiredMask = preset.role_kind() == kRoleKindCharacter ? 1u : 2u;
        for (const uint32_t skillId : preset.skill_ids()) {
            const auto* skill = SkillTableManager::Instance().FindByIdSilent(skillId).first;
            if (skill == nullptr || skill->battle_model() != 1 || (skill->caster_mask() & requiredMask) == 0) {
                LOG_ERROR << "[RankedPreset] table validation failed: invalid skill preset_id=" << preset.id()
                          << " skill_id=" << skillId;
                return false;
            }
        }
        if (preset.role_kind() == kRoleKindCharacter) {
            ++charCount;
            if (preset.id() == rule->default_character_preset_id()) {
                hasDefaultChar = true;
            }
        } else {
            ++petCount;
            if (preset.id() == rule->default_pet_preset_id()) {
                hasDefaultPet = true;
            }
            if (PetTableManager::Instance().FindByIdSilent(preset.pet_table_id()).first == nullptr) {
                LOG_ERROR << "[RankedPreset] table validation failed: pet species missing preset_id=" << preset.id()
                          << " pet_table_id=" << preset.pet_table_id();
                return false;
            }
            if (preset.pet_entry_ids_size() != preset.pet_entry_points_size()) {
                LOG_ERROR << "[RankedPreset] table validation failed: pet_entry length mismatch preset_id="
                          << preset.id();
                return false;
            }
            uint32_t entrySum = 0;
            for (int i = 0; i < preset.pet_entry_ids_size(); ++i) {
                const uint32_t entryId = preset.pet_entry_ids(i);
                const uint32_t pts = preset.pet_entry_points(i);
                const auto* entry = PetEntryTableManager::Instance().FindByIdSilent(entryId).first;
                if (entry == nullptr || pts == 0 || pts > entry->cap()) {
                    LOG_ERROR << "[RankedPreset] table validation failed: invalid pet_entry preset_id=" << preset.id()
                              << " entry_id=" << entryId << " points=" << pts;
                    return false;
                }
                entrySum += pts;
            }
            if (entrySum != expectedEntryBudget) {
                LOG_ERROR << "[RankedPreset] table validation failed: pet_entry sum mismatch preset_id=" << preset.id()
                          << " sum=" << entrySum << " expected=" << expectedEntryBudget;
                return false;
            }
        }
    }
    if (charCount == 0 || petCount == 0 || !hasDefaultChar || !hasDefaultPet) {
        LOG_ERROR << "[RankedPreset] table validation failed: missing character/pet presets or defaults";
        return false;
    }
    return true;
}

inline bool BuildCompetitiveCharacterStats(const RankedRuleTable& rule,
                                           const RankedPresetTable& charPreset,
                                           DerivedStats& outStats,
                                           BaseAttributesComp& outBaseAttrs) {
    const auto* classRow = ClassTableManager::Instance().FindByIdSilent(rule.reference_class_id()).first;
    const auto* pool = FindPoolByOwner(kPlayerPoolOwnerType);
    if (classRow == nullptr || pool == nullptr) {
        return false;
    }
    const auto dims = SortedDimensionsForPool(pool->id());
    if (dims.size() != 4) {
        return false;
    }

    const uint32_t level = playerlevel::CappedLevel(rule.competitive_level());
    const auto poolRule = ToPoolRule(*pool);
    const uint32_t totalPoints = attributerules::TotalPoints(poolRule, level, 0);
    const std::array<std::pair<uint32_t, uint32_t>, 4> dimWeights{{
        {dims[0]->id(), charPreset.weight_con()},
        {dims[1]->id(), charPreset.weight_spi()},
        {dims[2]->id(), charPreset.weight_str()},
        {dims[3]->id(), charPreset.weight_agi()},
    }};
    const auto allocated = AllocateWeightedPoints(totalPoints, dimWeights);

    double stdHp = static_cast<double>(classRow->init_health());
    double stdMp = static_cast<double>(classRow->init_mana());
    double stdPatk = 0.0;
    double stdMatk = 0.0;
    double stdSpd = static_cast<double>(classRow->init_speed());
    double stdDef = 0.0;
    for (const auto* dim : dims) {
        if (dim->base_per_level() == 0) {
            continue;
        }
        const double pts = static_cast<double>(dim->base_per_level()) * playerlevel::kMaxLevel;
        stdHp += dim->max_health() * pts;
        stdMp += dim->max_mana() * pts;
        stdPatk += dim->physical_attack() * pts;
        stdMatk += dim->magic_attack() * pts;
        stdSpd += dim->speed() * pts;
        stdDef += dim->defense() * pts;
    }

    const auto& attrRules = AttributeRuleTableManager::Instance().FindAll().data();
    const double effBonus = attrRules.empty() ? attributerules::kDefaultEfficiencyBonus
                                              : attrRules.Get(0).alloc_efficiency_bonus();
    const auto formulaRule = attributerules::MakeFormulaRule(
        effBonus, attributerules::FullInvestmentPoints(poolRule, playerlevel::kMaxLevel));

    double hp = static_cast<double>(classRow->init_health());
    double mp = static_cast<double>(classRow->init_mana());
    double patk = 0.0;
    double matk = 0.0;
    double spd = static_cast<double>(classRow->init_speed());
    double def = 0.0;

    for (std::size_t i = 0; i < 4; ++i) {
        const auto* dim = dims[i];
        const double natural = static_cast<double>(dim->base_per_level()) * level;
        hp += dim->max_health() * natural;
        mp += dim->max_mana() * natural;
        patk += dim->physical_attack() * natural;
        matk += dim->magic_attack() * natural;
        spd += dim->speed() * natural;
        def += dim->defense() * natural;

        if (allocated[i] == 0) {
            continue;
        }
        if (const auto* ratio = FindAllocRatio(dim->id(), rule.reference_class_id()); ratio != nullptr) {
            hp += attributerules::AllocatedIncrement(stdHp, ratio->max_health(), allocated[i], formulaRule);
            mp += attributerules::AllocatedIncrement(stdMp, ratio->max_mana(), allocated[i], formulaRule);
            patk += attributerules::AllocatedIncrement(stdPatk, ratio->physical_attack(), allocated[i], formulaRule);
            matk += attributerules::AllocatedIncrement(stdMatk, ratio->magic_attack(), allocated[i], formulaRule);
            spd += attributerules::AllocatedIncrement(stdSpd, ratio->speed(), allocated[i], formulaRule);
            def += attributerules::AllocatedIncrement(stdDef, ratio->defense(), allocated[i], formulaRule);
        } else {
            const double allocD = static_cast<double>(allocated[i]);
            hp += dim->max_health() * allocD;
            mp += dim->max_mana() * allocD;
            patk += dim->physical_attack() * allocD;
            matk += dim->magic_attack() * allocD;
            spd += dim->speed() * allocD;
            def += dim->defense() * allocD;
        }
    }

    outStats.maxHealth = std::max<uint64_t>(FloorToU64(hp), 1);
    outStats.maxMana = FloorToU64(mp);
    outStats.physicalAttack = FloorToU64(patk);
    outStats.magicAttack = FloorToU64(matk);
    outStats.speed = FloorToU64(spd);
    if (outStats.speed == 0) {
        outStats.speed = kFallbackSpeed;
    }
    outStats.defense = FloorToU64(def);

    outBaseAttrs.Clear();
    outBaseAttrs.set_health(outStats.maxHealth);
    outBaseAttrs.set_mana(outStats.maxMana);
    outBaseAttrs.set_strength(classRow->init_strength());
    outBaseAttrs.set_armor(classRow->init_armor());
    outBaseAttrs.set_resistance(classRow->init_resistance());
    outBaseAttrs.set_critchance(classRow->init_critchance());
    outBaseAttrs.set_speed(outStats.speed);
    return true;
}

inline void AccumulatePetCultivationEntry(PetCultivationEffects& effects, const PetEntryTable& row, uint32_t bps) {
    switch (row.channel()) {
    case 1: effects.set_physical_damage_bps(effects.physical_damage_bps() + bps); break;
    case 2: effects.set_magic_damage_bps(effects.magic_damage_bps() + bps); break;
    case 3: effects.set_outgoing_heal_bps(effects.outgoing_heal_bps() + bps); break;
    case 4: effects.set_shield_capacity_bps(effects.shield_capacity_bps() + bps); break;
    case 5: (*effects.mutable_ailment_hit_bps())[row.state_kind()] += bps; break;
    case 6: (*effects.mutable_ailment_resist_bps())[row.state_kind()] += bps; break;
    case 7: effects.set_poison_damage_bps(effects.poison_damage_bps() + bps); break;
    case 8: effects.set_retreat_hit_bps(effects.retreat_hit_bps() + bps); break;
    case 9: effects.set_retreat_resist_bps(effects.retreat_resist_bps() + bps); break;
    default: break;
    }
}

inline bool BuildCompetitivePetSnapshot(const RankedRuleTable& rule,
                                        const RankedPresetTable& petPreset,
                                        uint64_t ownerPlayerId,
                                        uint32_t cosmeticPetTableId,
                                        const std::string& cosmeticPetName,
                                        ::BattlePetSnapshot& outPet) {
    const auto* species = PetTableManager::Instance().FindByIdSilent(petPreset.pet_table_id()).first;
    const auto* pool = FindPoolByOwner(kPetPoolOwnerType);
    if (species == nullptr || pool == nullptr) {
        return false;
    }
    const auto dims = SortedDimensionsForPool(pool->id());
    if (dims.size() != 4) {
        return false;
    }

    const uint32_t petLevel = petrules::EffectiveLevel(rule.competitive_level(), species->level_cap());
    const uint32_t totalPoints = attributerules::TotalPoints(ToPoolRule(*pool), petLevel, 0);
    const std::array<std::pair<uint32_t, uint32_t>, 4> dimWeights{{
        {dims[0]->id(), petPreset.weight_con()},
        {dims[1]->id(), petPreset.weight_spi()},
        {dims[2]->id(), petPreset.weight_str()},
        {dims[3]->id(), petPreset.weight_agi()},
    }};
    const auto allocated = AllocateWeightedPoints(totalPoints, dimWeights);

    std::vector<petrules::DimensionInput> dimInputs;
    dimInputs.reserve(4);
    for (std::size_t i = 0; i < 4; ++i) {
        const auto* dim = dims[i];
        petrules::DimensionInput input;
        input.dimensionId = dim->id();
        input.coefficients.basePerLevel = dim->base_per_level();
        input.coefficients.maxHealth = dim->max_health();
        input.coefficients.maxMana = dim->max_mana();
        input.coefficients.physicalAttack = dim->physical_attack();
        input.coefficients.magicAttack = dim->magic_attack();
        input.coefficients.speed = dim->speed();
        input.coefficients.defense = dim->defense();
        input.allocated = allocated[i];
        uint32_t apt = petrules::kAptitudeBase;
        if (static_cast<int>(i) < species->aptitude_min_size() &&
            static_cast<int>(i) < species->aptitude_max_size()) {
            const uint32_t lo = species->aptitude_min(static_cast<int>(i));
            const uint32_t hi = species->aptitude_max(static_cast<int>(i));
            if (hi > 0) {
                apt = lo + (hi >= lo ? (hi - lo) / 2 : 0);
            }
        }
        input.aptitude = apt;
        dimInputs.push_back(input);
    }

    petrules::BaseValues base;
    base.maxHealth = species->init_health();
    base.maxMana = species->init_mana();
    base.speed = species->init_speed();
    const auto derived = petrules::ComputeDerived(base, dimInputs, petLevel);

    outPet.Clear();
    outPet.set_pet_id(0);
    outPet.set_competitive_temporary(true);
    outPet.set_owner_player_id(ownerPlayerId);
    outPet.set_combat_pet_table_id(petPreset.pet_table_id());
    if (cosmeticPetTableId != 0 &&
        PetTableManager::Instance().FindByIdSilent(cosmeticPetTableId).first != nullptr) {
        const auto* cosmeticRow = PetTableManager::Instance().FindByIdSilent(cosmeticPetTableId).first;
        outPet.set_pet_table_id(cosmeticPetTableId);
        outPet.set_pet_name(!cosmeticPetName.empty() ? cosmeticPetName : cosmeticRow->name());
    } else {
        outPet.set_pet_table_id(petPreset.pet_table_id());
        outPet.set_pet_name(species->name());
    }
    outPet.set_level(petLevel);
    outPet.set_max_health(derived.maxHealth);
    outPet.set_max_mana(derived.maxMana);
    outPet.set_physical_attack(derived.physicalAttack);
    outPet.set_magic_attack(derived.magicAttack);
    outPet.set_defense(derived.defense);

    auto* attrs = outPet.mutable_base_attributes();
    attrs->set_health(derived.maxHealth);
    attrs->set_mana(derived.maxMana);
    attrs->set_speed(derived.speed > 0 ? derived.speed : kFallbackSpeed);

    const int pairCount = std::min(petPreset.pet_entry_ids_size(), petPreset.pet_entry_points_size());
    for (int i = 0; i < pairCount; ++i) {
        const auto* entry = PetEntryTableManager::Instance().FindByIdSilent(petPreset.pet_entry_ids(i)).first;
        if (entry == nullptr) {
            return false;
        }
        AccumulatePetCultivationEntry(*outPet.mutable_pet_cultivation(), *entry,
                                      petPreset.pet_entry_points(i) * entry->per_point_bps());
    }

    for (const uint32_t skillId : petPreset.skill_ids()) {
        const auto* skill = SkillTableManager::Instance().FindByIdSilent(skillId).first;
        if (skill == nullptr) {
            return false;
        }
        (*outPet.mutable_skill_ranks())[skillId] = 1;
        if (IsRankedCastableSkill(*skill)) {
            outPet.add_skill_table_ids(skillId);
        }
    }
    return true;
}

}  // namespace rankedpreset
