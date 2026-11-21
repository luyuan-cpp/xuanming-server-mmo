#include "player_pet.h"
#include "pet_entry_rules.h"
#include "player_lifecycle.h"
#include "player_mutability.h"

#include <algorithm>
#include <limits>
#include <utility>
#include <vector>

#include <muduo/base/Logging.h>

#include "battle/system/player_battle.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/scene/player_pet.pb.h"
#include "table/code/pet_table.h"
#include "table/code/petentry_table.h"
#include "table/code/petrule_table.h"
#include "table/code/pettier_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {

using petentries::Points;

struct EntryConfig {
    uint32_t version = 0;
    uint32_t budget = 0;
    uint32_t maxDirections = 0;
    petentries::Rules rules;
};

EntryConfig ReadConfig() {
    EntryConfig config;
    const auto& globals = PetRuleTableManager::Instance().FindAll().data();
    if (globals.empty()) {
        return config;
    }
    const auto& globalRow = globals.Get(0);
    config.version = globalRow.entry_config_version();
    config.budget = globalRow.entry_total_points();
    config.maxDirections = globalRow.entry_max_directions();

    for (const auto& row : PetEntryTableManager::Instance().FindAll().data()) {
        const bool invalidRow =
            row.id() == 0 || row.cap() == 0 || row.channel() < 1 || row.channel() > 9 ||
            row.per_point_bps() == 0 ||
            static_cast<uint64_t>(row.cap()) * row.per_point_bps() > 10000 ||
            ((row.channel() == 5 || row.channel() == 6) && (row.state_kind() < 1 || row.state_kind() > 7));
        if (invalidRow) {
            config.version = 0;
            break;
        }
        config.rules[row.id()] = {row.cap(), row.per_point_bps()};
    }
    if (config.rules.empty() || config.budget == 0) {
        config.version = 0;
    }
    return config;
}

Points CopyTargetMap(const google::protobuf::Map<uint32_t, uint32_t>& source) {
    return {source.begin(), source.end()};
}

PetInstance* FindOwnedPet(entt::entity player, uint64_t petId, uint32_t& errorOut) {
    if (!tlsEcs.actorRegistry.valid(player)) {
        errorOut = kEntityIsNull;
        return nullptr;
    }
    auto* comp = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
    if (comp == nullptr) {
        errorOut = kServiceUnavailable;
        return nullptr;
    }
    for (auto& pet : *comp->mutable_pets()) {
        if (pet.pet_id() == petId) {
            errorOut = kSuccess;
            return &pet;
        }
    }
    errorOut = kPetNotFound;
    return nullptr;
}

uint32_t MapValidationError(petentries::Error err) {
    switch (err) {
    case petentries::Error::None: return kSuccess;
    case petentries::Error::Unknown: return kPetDimensionNotFound;
    case petentries::Error::Cap: return kPetDimensionCapExceeded;
    case petentries::Error::Budget: return kPetPointsNotEnough;
    default: return kInvalidParameter;
    }
}

void AccumulateEffect(PetCultivationEffects& effects, const PetEntryTable& row, uint32_t bps) {
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

uint32_t FillEntriesSnapshot(const PetInstance& pet, const EntryConfig& config,
                             const Points& values, PetEntriesInfo& info) {
    info.Clear();
    if (config.version == 0) {
        return kInvalidTableData;
    }

    info.set_pet_id(pet.pet_id());
    info.set_state_version(petentries::Version(pet.entry_state_version()));
    info.set_config_version(config.version);
    info.set_total_points(config.budget);
    info.set_max_directions(config.maxDirections);
    info.set_last_request_id(pet.entry_last_request_id());

    Points knownValues;
    uint64_t totalUsed = 0;
    uint64_t unknownUsed = 0;
    for (const auto& [entryId, points] : values) {
        totalUsed += points;
        if (config.rules.contains(entryId)) {
            knownValues[entryId] = points;
        } else {
            unknownUsed += points;
        }
    }

    const bool effective =
        petentries::Validate(values, knownValues, config.rules, config.budget, config.maxDirections) ==
        petentries::Error::None;
    info.set_effective(effective);
    info.set_unknown_used_points(unknownUsed);
    info.set_remaining_points(
        effective && totalUsed <= config.budget ? static_cast<uint32_t>(config.budget - totalUsed) : 0u);

    std::vector<const PetEntryTable*> sortedRows;
    for (const auto& row : PetEntryTableManager::Instance().FindAll().data()) {
        sortedRows.push_back(&row);
    }
    std::sort(sortedRows.begin(), sortedRows.end(), [](const PetEntryTable* lhs, const PetEntryTable* rhs) {
        return lhs->sort() == rhs->sort() ? lhs->id() < rhs->id() : lhs->sort() < rhs->sort();
    });

    for (const auto* row : sortedRows) {
        auto* out = info.add_entries();
        out->set_id(row->id());
        out->set_name(row->name());
        out->set_desc(row->desc());
        out->set_category(row->category());
        out->set_channel(row->channel());
        out->set_state_kind(row->state_kind());
        out->set_cap(row->cap());
        out->set_per_point_bps(row->per_point_bps());
        out->set_sort(row->sort());
        out->set_icon(row->icon());

        const auto found = values.find(row->id());
        const uint32_t points = found == values.end() ? 0u : found->second;
        out->set_allocated(points);
        const uint32_t effectiveBps = effective ? points * row->per_point_bps() : 0u;
        out->set_effective_bps(effectiveBps);
        AccumulateEffect(*info.mutable_effects(), *row, effectiveBps);
    }
    return kSuccess;
}

}  // namespace

uint32_t PetSystem::BuildEntriesInfo(const PetInstance& pet, PetEntriesInfo& info) {
    return FillEntriesSnapshot(pet, ReadConfig(), CopyTargetMap(pet.entry_allocated()), info);
}

uint32_t PetSystem::BuildEntries(entt::entity player, uint64_t petId, PetEntriesInfo& info) {
    info.Clear();
    uint32_t error = kSuccess;
    const auto* pet = FindOwnedPet(player, petId, error);
    return pet != nullptr ? BuildEntriesInfo(*pet, info) : error;
}

void PetSystem::FillCultivationTier(const PetInstance& pet, PetInfo& info) {
    uint32_t tier = pet.cultivation_tier();
    if (tier == 0) {
        if (const auto* species = PetTableManager::Instance().FindByIdSilent(pet.pet_table_id()).first) {
            tier = species->cultivation_tier();
        }
    }
    info.set_cultivation_tier(tier);
    if (const auto* tierRow = PetTierTableManager::Instance().FindByIdSilent(tier).first) {
        info.set_cultivation_tier_name(tierRow->name());
        info.set_cultivation_art_key(tierRow->art_key());
    }
}

void PetSystem::PreviewEntries(entt::entity player, const PreviewPetEntriesRequest& request,
                               PreviewPetEntriesResponse& response) {
    response.Clear();
    uint32_t error = kSuccess;
    const auto* pet = FindOwnedPet(player, request.pet_id(), error);
    if (pet == nullptr) {
        response.mutable_error_message()->set_id(error);
        return;
    }

    const auto config = ReadConfig();
    const auto stored = CopyTargetMap(pet->entry_allocated());
    const auto target = CopyTargetMap(request.target());

    if (config.version == 0) {
        error = kInvalidTableData;
    } else if (request.config_version() != config.version ||
               request.state_version() != petentries::Version(pet->entry_state_version())) {
        error = kInvalidParameter;
    } else {
        error = MapValidationError(
            petentries::Validate(stored, target, config.rules, config.budget, config.maxDirections));
    }

    if (error != kSuccess) {
        response.mutable_error_message()->set_id(error);
        if (BuildEntriesInfo(*pet, *response.mutable_entries()) != kSuccess) {
            response.clear_entries();
        }
        return;
    }

    const auto previewPoints = petentries::ReplaceKnown(stored, target, config.rules);
    if (const auto fillErr = FillEntriesSnapshot(*pet, config, previewPoints, *response.mutable_entries());
        fillErr != kSuccess) {
        response.clear_entries();
        response.mutable_error_message()->set_id(fillErr);
    }
}

void PetSystem::SaveEntries(entt::entity player, const SavePetEntriesRequest& request,
                            SavePetEntriesResponse& response) {
    response.Clear();
    response.set_request_id(request.request_id());

    uint32_t error = kSuccess;
    auto* pet = FindOwnedPet(player, request.pet_id(), error);
    if (pet == nullptr) {
        response.mutable_error_message()->set_id(error);
        return;
    }

    const auto fail = [&](uint32_t err) {
        response.mutable_error_message()->set_id(err);
        if (BuildEntriesInfo(*pet, *response.mutable_entries()) != kSuccess) {
            response.clear_entries();
        }
    };

    if (!player_mutability::IsPlayerMutable(player)) {
        fail(kInvalidParameter);
        return;
    }
    if (PlayerBattleSystem::IsInBattle(player)) {
        fail(kPetInBattle);
        return;
    }
    if (!petentries::ValidRequestId(request.request_id())) {
        fail(kInvalidParameter);
        return;
    }

    const auto target = CopyTargetMap(request.target());
    if (pet->entry_last_request_id() == request.request_id()) {
        const bool sameRequest =
            pet->entry_last_base_version() == request.state_version() &&
            pet->entry_last_config_version() == request.config_version() &&
            CopyTargetMap(pet->entry_last_target()) == target;
        if (!sameRequest) {
            fail(kInvalidParameter);
            return;
        }
        if (const auto buildErr = BuildEntriesInfo(*pet, *response.mutable_entries()); buildErr != kSuccess) {
            response.clear_entries();
            response.mutable_error_message()->set_id(buildErr);
            return;
        }
        response.set_replayed(true);
        return;
    }

    const auto config = ReadConfig();
    if (config.version == 0) {
        fail(kInvalidTableData);
        return;
    }
    if (request.config_version() != config.version ||
        request.state_version() != petentries::Version(pet->entry_state_version()) ||
        pet->entry_state_version() == std::numeric_limits<uint64_t>::max()) {
        fail(kInvalidParameter);
        return;
    }

    const auto stored = CopyTargetMap(pet->entry_allocated());
    error = MapValidationError(
        petentries::Validate(stored, target, config.rules, config.budget, config.maxDirections));
    if (error != kSuccess) {
        fail(error);
        return;
    }

    // scene 单线程原子替换整只宝宝实例，确保存档、迁移和交易快照与幂等回执保持同一版本。
    PetInstance candidate = *pet;
    candidate.mutable_entry_allocated()->clear();
    for (const auto& [entryId, points] : petentries::ReplaceKnown(stored, target, config.rules)) {
        (*candidate.mutable_entry_allocated())[entryId] = points;
    }
    candidate.set_entry_state_version(petentries::Version(pet->entry_state_version()) + 1);
    candidate.set_entry_last_request_id(request.request_id());
    candidate.set_entry_last_base_version(request.state_version());
    candidate.set_entry_last_config_version(request.config_version());
    *candidate.mutable_entry_last_target() = request.target();

    PetEntriesInfo validated;
    if (FillEntriesSnapshot(candidate, config, CopyTargetMap(candidate.entry_allocated()), validated) != kSuccess) {
        fail(kInvalidTableData);
        return;
    }

    pet->Swap(&candidate);
    *response.mutable_entries() = std::move(validated);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "pet_entries_saved");
    LOG_INFO << "[PlayerPet] Entries saved: player_id=" << tlsEcs.GetPlayerId(player)
             << " pet_id=" << pet->pet_id()
             << " state_version=" << pet->entry_state_version()
             << " remaining=" << response.entries().remaining_points();
}
