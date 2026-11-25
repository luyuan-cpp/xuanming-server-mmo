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
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/pet_rules.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
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

uint32_t EffectiveCultivationTier(const PetInstance& pet) {
    uint32_t tier = pet.cultivation_tier();
    if (tier == 0) {
        if (const auto* species = PetTableManager::Instance().FindByIdSilent(pet.pet_table_id()).first) {
            tier = species->cultivation_tier();
        }
    }
    return tier == 0 ? 1u : tier;
}

uint32_t EffectiveBudgetForPet(const PetInstance& pet, const EntryConfig& config) {
    uint32_t bonus = 0;
    if (const auto* tierRow = PetTierTableManager::Instance().FindByIdSilent(EffectiveCultivationTier(pet)).first) {
        bonus = tierRow->bonus_entry_points();
    }
    return config.budget + bonus;
}

const PetEntryPreset* FindPreset(const PetInstance& pet, uint32_t slotId) {
    for (const auto& preset : pet.entry_presets()) {
        if (preset.slot_id() == slotId) {
            return &preset;
        }
    }
    return nullptr;
}

PetEntryPreset* FindOrCreatePreset(PetInstance& pet, uint32_t slotId) {
    for (auto& preset : *pet.mutable_entry_presets()) {
        if (preset.slot_id() == slotId) {
            return &preset;
        }
    }
    auto* created = pet.add_entry_presets();
    created->set_slot_id(slotId);
    created->set_tag(0);
    return created;
}

void SyncActivePresetFromAllocated(PetInstance& pet, uint32_t activeSlot) {
    auto* preset = FindOrCreatePreset(pet, activeSlot);
    preset->mutable_allocated()->clear();
    for (const auto& [entryId, points] : pet.entry_allocated()) {
        if (points > 0) {
            (*preset->mutable_allocated())[entryId] = points;
        }
    }
}

uint32_t FillEntriesSnapshot(const PetInstance& pet, const EntryConfig& config,
                             const Points& values, PetEntriesInfo& info) {
    info.Clear();
    if (config.version == 0) {
        return kInvalidTableData;
    }

    const uint32_t budget = EffectiveBudgetForPet(pet, config);
    const uint32_t activeSlot = petentries::EffectivePresetSlot(pet.active_entry_preset_slot());
    info.set_pet_id(pet.pet_id());
    info.set_state_version(petentries::Version(pet.entry_state_version()));
    info.set_config_version(config.version);
    info.set_total_points(budget);
    info.set_max_directions(config.maxDirections);
    info.set_last_request_id(pet.entry_last_request_id());
    info.set_active_preset_slot(activeSlot);

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
        petentries::Validate(values, knownValues, config.rules, budget, config.maxDirections) ==
        petentries::Error::None;
    info.set_effective(effective);
    info.set_unknown_used_points(unknownUsed);
    info.set_remaining_points(
        effective && totalUsed <= budget ? static_cast<uint32_t>(budget - totalUsed) : 0u);

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

    for (uint32_t slot = 1; slot <= petentries::kMaxPresetSlots; ++slot) {
        auto* summary = info.add_presets();
        summary->set_slot_id(slot);
        const auto* storedPreset = FindPreset(pet, slot);
        summary->set_tag(storedPreset != nullptr ? storedPreset->tag() : 0u);
        uint64_t presetUsed = 0;
        if (slot == activeSlot) {
            for (const auto& [entryId, points] : values) {
                if (points > 0) {
                    (*summary->mutable_allocated())[entryId] = points;
                    presetUsed += points;
                }
            }
        } else if (storedPreset != nullptr) {
            for (const auto& [entryId, points] : storedPreset->allocated()) {
                if (points > 0) {
                    (*summary->mutable_allocated())[entryId] = points;
                    presetUsed += points;
                }
            }
        }
        summary->set_used_points(
            presetUsed > std::numeric_limits<uint32_t>::max()
                ? std::numeric_limits<uint32_t>::max()
                : static_cast<uint32_t>(presetUsed));
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
    const uint32_t budget = EffectiveBudgetForPet(*pet, config);
    const auto stored = CopyTargetMap(pet->entry_allocated());
    const auto target = CopyTargetMap(request.target());

    if (config.version == 0) {
        error = kInvalidTableData;
    } else if (request.config_version() != config.version ||
               request.state_version() != petentries::Version(pet->entry_state_version())) {
        error = kInvalidParameter;
    } else {
        error = MapValidationError(
            petentries::Validate(stored, target, config.rules, budget, config.maxDirections));
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
            pet->entry_last_preset_slot() == 0 &&
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

    const uint32_t budget = EffectiveBudgetForPet(*pet, config);
    const auto stored = CopyTargetMap(pet->entry_allocated());
    error = MapValidationError(
        petentries::Validate(stored, target, config.rules, budget, config.maxDirections));
    if (error != kSuccess) {
        fail(error);
        return;
    }

    // scene 单线程原子替换整只宝宝实例，确保存档、迁移和交易快照与幂等回执保持同一版本。
    PetInstance candidate = *pet;
    const uint32_t activeSlot = petentries::EffectivePresetSlot(candidate.active_entry_preset_slot());
    candidate.set_active_entry_preset_slot(activeSlot);
    candidate.mutable_entry_allocated()->clear();
    for (const auto& [entryId, points] : petentries::ReplaceKnown(stored, target, config.rules)) {
        (*candidate.mutable_entry_allocated())[entryId] = points;
    }
    SyncActivePresetFromAllocated(candidate, activeSlot);
    candidate.set_entry_state_version(petentries::Version(pet->entry_state_version()) + 1);
    candidate.set_entry_last_request_id(request.request_id());
    candidate.set_entry_last_base_version(request.state_version());
    candidate.set_entry_last_config_version(request.config_version());
    candidate.set_entry_last_preset_slot(0);
    candidate.set_entry_last_preset_tag(0);
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
             << " preset_slot=" << activeSlot
             << " state_version=" << pet->entry_state_version()
             << " remaining=" << response.entries().remaining_points();
}

void PetSystem::SwitchEntryPreset(entt::entity player, const SwitchPetEntryPresetRequest& request,
                                  SwitchPetEntryPresetResponse& response) {
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
    if (!petentries::ValidPresetSlot(request.slot_id()) || !petentries::ValidPresetTag(request.tag())) {
        fail(kPetEntryPresetInvalid);
        return;
    }

    if (pet->entry_last_request_id() == request.request_id()) {
        const bool sameRequest =
            pet->entry_last_preset_slot() == request.slot_id() &&
            pet->entry_last_preset_tag() == request.tag() &&
            pet->entry_last_base_version() == request.state_version() &&
            pet->entry_last_config_version() == request.config_version();
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

    PetInstance candidate = *pet;
    const uint32_t currentSlot = petentries::EffectivePresetSlot(candidate.active_entry_preset_slot());
    // 先把当前槽位的权威分配落回预设槽，确保切走时不丢当前方案点位。
    SyncActivePresetFromAllocated(candidate, currentSlot);

    auto* targetPreset = FindOrCreatePreset(candidate, request.slot_id());
    if (request.tag() != petentries::kKeepPresetTag) {
        targetPreset->set_tag(request.tag());
    }
    candidate.set_active_entry_preset_slot(request.slot_id());
    candidate.mutable_entry_allocated()->clear();
    for (const auto& [entryId, points] : targetPreset->allocated()) {
        if (points > 0) {
            (*candidate.mutable_entry_allocated())[entryId] = points;
        }
    }
    candidate.set_entry_state_version(petentries::Version(pet->entry_state_version()) + 1);
    candidate.set_entry_last_request_id(request.request_id());
    candidate.set_entry_last_base_version(request.state_version());
    candidate.set_entry_last_config_version(request.config_version());
    candidate.set_entry_last_preset_slot(request.slot_id());
    candidate.set_entry_last_preset_tag(request.tag());
    candidate.mutable_entry_last_target()->clear();

    PetEntriesInfo validated;
    if (FillEntriesSnapshot(candidate, config, CopyTargetMap(candidate.entry_allocated()), validated) != kSuccess) {
        fail(kInvalidTableData);
        return;
    }

    pet->Swap(&candidate);
    *response.mutable_entries() = std::move(validated);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "pet_entry_preset_switched");
    LOG_INFO << "[PlayerPet] Entry preset switched: player_id=" << tlsEcs.GetPlayerId(player)
             << " pet_id=" << pet->pet_id()
             << " slot=" << currentSlot << " -> " << request.slot_id()
             << " state_version=" << pet->entry_state_version();
}

void PetSystem::FillTierAscension(entt::entity player, const PetInstance& pet,
                                  uint32_t effectiveLevel, PetInfo& info) {
    info.clear_next_tier();
    const uint32_t currentTier = EffectiveCultivationTier(pet);
    if (currentTier >= petentries::kMaxCultivationTier) {
        return;
    }
    const auto* currentRow = PetTierTableManager::Instance().FindByIdSilent(currentTier).first;
    const auto* nextRow = PetTierTableManager::Instance().FindByIdSilent(currentTier + 1).first;
    if (currentRow == nullptr || nextRow == nullptr) {
        return;
    }

    auto* out = info.mutable_next_tier();
    out->set_next_tier(nextRow->id());
    out->set_next_tier_name(nextRow->name());
    out->set_next_art_key(nextRow->art_key());
    out->set_min_pet_level(currentRow->ascend_min_pet_level());
    out->set_cost_item_id(currentRow->ascend_cost_item_id());
    out->set_cost_item_count(currentRow->ascend_cost_item_count());
    out->set_cost_gold(currentRow->ascend_cost_gold());
    out->set_bonus_entry_points(nextRow->bonus_entry_points());

    uint64_t ownedMaterial = 0;
    if (currentRow->ascend_cost_item_id() != 0) {
        if (const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player); bags != nullptr) {
            ownedMaterial = bags->bags[kInventory].GetTotalItemCount(currentRow->ascend_cost_item_id());
        }
    }
    out->set_owned_item_count(ownedMaterial);

    uint32_t blockTip = 0;
    if (PlayerBattleSystem::IsInBattle(player)) {
        blockTip = kPetInBattle;
    } else if (!player_mutability::IsPlayerMutable(player)) {
        blockTip = kInvalidParameter;
    } else if (effectiveLevel < currentRow->ascend_min_pet_level()) {
        blockTip = kPetTierLevelNotEnough;
    } else if (currentRow->ascend_cost_item_id() != 0 &&
               ownedMaterial < currentRow->ascend_cost_item_count()) {
        blockTip = kPetTierMaterialNotEnough;
    } else if (currentRow->ascend_cost_gold() > 0 &&
               CurrencySystem::GetBalance(player, kCurrencyGold) < currentRow->ascend_cost_gold()) {
        blockTip = kPetGoldNotEnough;
    }
    out->set_can_ascend(blockTip == 0);
    out->set_block_tip_id(blockTip);
}

void PetSystem::AscendTier(entt::entity player, const AscendPetTierRequest& request,
                           AscendPetTierResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());

    const auto fail = [&](uint32_t err) {
        response.mutable_error_message()->set_id(err);
        if (tlsEcs.actorRegistry.valid(player)) {
            response.set_gold(CurrencySystem::GetBalance(player, kCurrencyGold));
            if (BuildList(player, *response.mutable_pets()) != kSuccess) {
                response.clear_pets();
            }
        }
    };

    uint32_t error = kSuccess;
    auto* pet = FindOwnedPet(player, request.pet_id(), error);
    if (pet == nullptr) {
        fail(error);
        return;
    }
    if (request.client_request_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    if (pet->tier_last_request_id() == request.client_request_id()) {
        if (pet->tier_last_expected_tier() != request.expected_tier()) {
            fail(kInvalidParameter);
            return;
        }
        response.set_replayed(true);
        response.set_new_tier(pet->tier_last_result_tier());
        response.set_gold(CurrencySystem::GetBalance(player, kCurrencyGold));
        if (const auto listErr = BuildList(player, *response.mutable_pets()); listErr != kSuccess) {
            response.clear_pets();
            response.mutable_error_message()->set_id(listErr);
        }
        return;
    }

    if (PlayerBattleSystem::IsInBattle(player)) {
        fail(kPetInBattle);
        return;
    }
    if (!player_mutability::IsPlayerMutable(player)) {
        fail(kInvalidParameter);
        return;
    }

    const auto* species = PetTableManager::Instance().FindByIdSilent(pet->pet_table_id()).first;
    if (species == nullptr) {
        fail(kPetTableRowMissing);
        return;
    }
    const uint32_t currentTier = EffectiveCultivationTier(*pet);
    if (currentTier >= petentries::kMaxCultivationTier) {
        fail(kPetTierMaxReached);
        return;
    }
    if (request.expected_tier() != currentTier) {
        fail(kPetTierChanged);
        return;
    }

    const auto* currentRow = PetTierTableManager::Instance().FindByIdSilent(currentTier).first;
    const auto* nextRow = PetTierTableManager::Instance().FindByIdSilent(currentTier + 1).first;
    if (currentRow == nullptr || nextRow == nullptr) {
        fail(kInvalidTableData);
        return;
    }

    const uint32_t effectiveLevel = petrules::EffectiveLevel(pet->level(), species->level_cap());
    if (effectiveLevel < currentRow->ascend_min_pet_level()) {
        fail(kPetTierLevelNotEnough);
        return;
    }

    constexpr uint64_t kMaxPayableGold = static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
    const uint64_t costGold = currentRow->ascend_cost_gold();
    if (costGold > 0 && (costGold > kMaxPayableGold ||
                         !CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(costGold)))) {
        fail(kPetGoldNotEnough);
        return;
    }

    const uint32_t costItemId = currentRow->ascend_cost_item_id();
    const uint32_t costItemCount = currentRow->ascend_cost_item_count();
    const auto applyAscension = [&]() -> uint32_t {
        if (costGold > 0) {
            const auto goldErr = CurrencySystem::DeductCurrency(
                player, kCurrencyGold, static_cast<int64_t>(costGold), TX_CURRENCY_DEDUCT, pet->pet_table_id());
            if (goldErr != kSuccess) {
                return goldErr;
            }
        }
        pet->set_cultivation_tier(currentTier + 1);
        if (pet->entry_state_version() < std::numeric_limits<uint64_t>::max()) {
            pet->set_entry_state_version(petentries::Version(pet->entry_state_version()) + 1);
        }
        pet->set_tier_last_request_id(request.client_request_id());
        pet->set_tier_last_expected_tier(currentTier);
        pet->set_tier_last_result_tier(currentTier + 1);
        return kSuccess;
    };

    if (costItemId != 0 && costItemCount != 0) {
        const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
        if (bags == nullptr) {
            fail(kServiceUnavailable);
            return;
        }
        ItemCountMap consumed;
        consumed[costItemId] = costItemCount;
        const auto verdict = BagService::CheckRemovable(player, bags->bags[kInventory], consumed);
        if (verdict == BagService::RemovalVerdict::kInsufficient) {
            fail(kPetTierMaterialNotEnough);
            return;
        }
        if (verdict != BagService::RemovalVerdict::kEnough) {
            fail(kFeatureUnavailable);
            return;
        }
        std::vector<GuidVector> written;
        const auto exErr = BagService::ExchangeInventoryExact(
            player, consumed, {}, {}, request.client_request_id(), "{\"operation\":\"pet_tier_ascend\"}", written,
            [&](const std::vector<GuidVector>&) -> uint32_t {
                if (PlayerBattleSystem::IsInBattle(player)) return kPetInBattle;
                if (!player_mutability::IsPlayerMutable(player)) return kInvalidParameter;
                if (EffectiveCultivationTier(*pet) != currentTier) return kPetTierChanged;
                return applyAscension();
            });
        if (exErr != kSuccess) {
            fail(exErr);
            return;
        }
    } else {
        if (const auto commitErr = applyAscension(); commitErr != kSuccess) {
            fail(commitErr);
            return;
        }
    }

    response.set_new_tier(currentTier + 1);
    response.set_gold(CurrencySystem::GetBalance(player, kCurrencyGold));
    if (const auto listErr = BuildList(player, *response.mutable_pets()); listErr != kSuccess) {
        response.clear_pets();
        response.mutable_error_message()->set_id(listErr);
        return;
    }
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "pet_tier_ascended");
    LOG_INFO << "[PlayerPet] Tier ascended: player_id=" << tlsEcs.GetPlayerId(player)
             << " pet_id=" << pet->pet_id()
             << " tier=" << currentTier << " -> " << (currentTier + 1)
             << " cost_gold=" << costGold
             << " cost_item=" << costItemId << "x" << costItemCount;
}
