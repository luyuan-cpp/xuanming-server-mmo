#include "player_equipment_v1.h"

#include <algorithm>
#include <array>
#include <chrono>
#include <cstdint>
#include <limits>
#include <map>
#include <random>
#include <set>
#include <string>
#include <vector>
#include <google/protobuf/message.h>
#include "core/type_define/player_guid_comp.h"
#include "engine/core/time/system/time.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/client_request_ledger.h"
#include "player/system/equip_craft_quality_rules.h"
#include "player/system/player_attribute.h"
#include "player/system/player_equip.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_level_rules.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/player_season.h"
#include "battle/system/player_battle.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_craft_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "services/battle/system/equipment_battle_rules.h"
#include "table/code/equipaffixquality_table.h"
#include "table/code/equipartifact_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipenhance_table.h"
#include "table/code/equipv1affix_table.h"
#include "table/code/item_table.h"
#include "table/code/table_loader.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
constexpr uint64_t kMaxPayableGold = static_cast<uint64_t>(std::numeric_limits<int64_t>::max());

PlayerEquipmentV1System::PersistFn persistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;
PlayerEquipmentV1System::QualityRollFn qualityRollFn = nullptr;
struct CraftPersistRequestComp { std::chrono::steady_clock::time_point requested{}; };

uint32_t RollBelow(uint32_t bound) {
    if (bound <= 1) return 0;
    if (qualityRollFn != nullptr) return qualityRollFn(bound) % bound;
    thread_local std::mt19937_64 rng{std::random_device{}()};
    return static_cast<uint32_t>(std::uniform_int_distribution<uint32_t>(0, bound - 1)(rng));
}

const ItemTable* ItemRow(uint32_t id) { return ItemTableManager::Instance().FindByIdSilent(id).first; }
const EquipV1AffixTable* AffixRow(uint32_t id) { return EquipV1AffixTableManager::Instance().FindByIdSilent(id).first; }
const EquipAffixQualityTable* QualityRow(uint32_t tier) {
    return EquipAffixQualityTableManager::Instance().FindByIdSilent(tier).first;
}

uint32_t QualityRatioPct(uint32_t tier) {
    if (const auto* row = QualityRow(tier); row != nullptr && row->ratio_pct() >= 1 && row->ratio_pct() <= 100) {
        return row->ratio_pct();
    }
    return equipcraft::DefaultQualityRatioPct(tier);
}

std::string QualityName(uint32_t tier) {
    if (const auto* row = QualityRow(tier); row != nullptr && !row->name().empty()) {
        return row->name();
    }
    return equipcraft::DefaultQualityName(tier);
}

std::array<uint32_t, equipcraft::kMaxQualityTier> QualityWeightsForMastery(uint32_t masteryTier) {
    std::array<uint32_t, equipcraft::kMaxQualityTier> weights{2500, 3500, 2200, 1300, 500};
    uint32_t sum = 0;
    for (uint32_t tier = equipcraft::kMinQualityTier; tier <= equipcraft::kMaxQualityTier; ++tier) {
        const auto* row = QualityRow(tier);
        if (row == nullptr) return weights;
        const uint32_t w = masteryTier == equipcraft::kMasteryMaster ? row->master_weight()
            : (masteryTier == equipcraft::kMasteryArtisan ? row->artisan_weight() : row->apprentice_weight());
        weights[tier - 1] = w;
        sum += w;
    }
    return sum == equipcraft::kQualityWeightTotalBps ? weights : std::array<uint32_t, 5>{2500, 3500, 2200, 1300, 500};
}

uint32_t RollQualityForMastery(uint32_t masteryTier) {
    const auto weights = QualityWeightsForMastery(masteryTier);
    return equipcraft::RollQualityTier(weights, RollBelow(equipcraft::kQualityWeightTotalBps));
}

std::string PlayerDisplayName(entt::entity player, uint64_t playerId) {
    if (const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(player);
        profile != nullptr && !profile->name().empty()) {
        return profile->name();
    }
    return "宗师#" + std::to_string(playerId);
}

uint32_t PoolFor(uint32_t profile, uint32_t slot) {
    if (slot < 1 || slot > 2) return 0;
    if (profile == 1) return slot;
    return profile == 2 ? 3u : 0u;
}
bool ValidMaterial(uint32_t id, uint32_t count) {
    const auto* row = ItemRow(id);
    return id != 0 && count != 0 && row != nullptr && row->max_stack_size() != 0 &&
        row->equip_kind() == 0 && row->battle_usable() == 0;
}
bool ValidAffix(const EquipV1AffixTable& row) {
    if (row.id() == 0 || row.value_permyriad() == 0 || row.source_cap() < row.value_permyriad() ||
        row.source_cap() > 10000 || !ValidMaterial(row.material_id(), row.material_count())) return false;
    const auto effect = row.effect(), param = row.effect_param();
    if (effect < 1 || effect > 13 || row.source_cap() > equipbattle::MaximumSourceBps(effect)) return false;
    if (effect <= 10 && param != 0) return false;
    if ((effect == 11 || effect == 12) && (param < 1 || param > 7)) return false;
    if (effect == 13 && (param < 1 || param > 12)) return false;
    if (row.pool() == 1) return effect == 1 || effect == 2 || effect == 5 || effect == 6 || effect == 11;
    if (row.pool() == 2) return effect == 7 || effect == 8 || effect == 13;
    return row.pool() == 3 && (effect == 3 || effect == 4 || effect == 9 || effect == 10 || effect == 12);
}
bool ValidArtifact(const EquipArtifactTable& row) {
    return equipbattle::ValidArtifactDefinition(row.id(), row.bonus_bps(), row.cap_bps(),
        row.per_round_limit(), row.battle_limit(), row.active_rounds(), row.health_threshold_bps());
}
bool ValidTemplate(const ItemTable& row) {
    if (row.max_stack_size() != 1 || row.affix_pool() != 0 || row.affix_rule() != 0 ||
        !ValidMaterial(row.forge_material_id(), row.forge_material_count())) return false;
    if (row.equip_profile() == 1) return row.equip_kind() >= 11 && row.equip_kind() <= 14 && row.artifact_id() == 0;
    if (row.equip_profile() == 2) return row.equip_kind() == 15 && row.artifact_id() == 0;
    const auto* artifact = EquipArtifactTableManager::Instance().FindByIdSilent(row.artifact_id()).first;
    return row.equip_profile() == 3 && row.equip_kind() == 16 && artifact != nullptr && ValidArtifact(*artifact);
}
bool ValidSelections(const ItemComp& item, uint32_t profile) {
    if (!item.has_equip() || item.equip().fixed_affixes_size() > 2) return false;
    std::set<uint32_t> slots, ids;
    for (const auto& selected : item.equip().fixed_affixes()) {
        const auto* row = AffixRow(selected.affix_id());
        if (row == nullptr || !ValidAffix(*row) || PoolFor(profile, selected.slot()) != row->pool() ||
            !slots.insert(selected.slot()).second || !ids.insert(selected.affix_id()).second) return false;
    }
    return true;
}

void FillAffixCatalog(const EquipV1AffixTable& row, EquipFixedAffixInfo& info) {
    const bool exempt = equipcraft::IsPolishExempt(row.effect());
    info.set_affix_id(row.id()); info.set_pool(row.pool());
    info.set_name(row.name()); info.set_description(row.description());
    info.set_value_bps(row.value_permyriad()); info.set_source_cap_bps(row.source_cap());
    info.set_effect(row.effect()); info.set_effect_param(row.effect_param());
    info.set_unit(row.effect() >= 7 && row.effect() <= 12 ? 2 : 1);
    info.set_material_config_id(row.material_id()); info.set_material_count(row.material_count());
    info.set_quality_tier(0);
    info.clear_quality_name();
    info.set_quality_ratio_pct(100);
    info.set_max_value_bps(row.value_permyriad());
    info.set_polish_pity_count(0);
    info.set_polish_pity_limit(exempt ? 0u : equipcraft::kPolishPityLimit);
    info.set_polish_exempt(exempt);
    info.set_polish_material_config_id(equipcraft::kPolishSandConfigId);
    info.set_polish_material_count(equipcraft::kPolishSandCount);
    info.set_polish_gold_cost(equipcraft::kPolishGoldCost);
    info.set_reforge_material_config_id(equipcraft::kReforgeSandConfigId);
    info.set_reforge_material_count(equipcraft::kReforgeSandCount);
    info.set_reforge_gold_cost(equipcraft::kReforgeGoldCost);
}

void FillAffixInstance(const EquipV1AffixTable& row, const EquipFixedAffix& selected, EquipFixedAffixInfo& info) {
    FillAffixCatalog(row, info);
    const bool exempt = equipcraft::IsPolishExempt(row.effect());
    const uint32_t tier = equipcraft::EffectiveQualityTier(selected.quality_tier(), exempt);
    const uint32_t ratioPct = QualityRatioPct(tier);
    info.set_quality_tier(tier);
    info.set_quality_name(QualityName(tier));
    info.set_quality_ratio_pct(ratioPct);
    info.set_value_bps(equipcraft::ScaleValueByQuality(row.value_permyriad(), ratioPct));
    info.set_polish_pity_count((exempt || tier >= equipcraft::kMaxQualityTier) ? 0u : selected.polish_pity_count());
}

void FillArtifact(const EquipArtifactTable& row, EquipArtifactInfo& info) {
    info.set_effect_id(row.id()); info.set_name(row.name()); info.set_description(row.description());
    info.set_bonus_bps(row.bonus_bps()); info.set_cap_bps(row.cap_bps());
    info.set_per_round_limit(row.per_round_limit()); info.set_battle_limit(row.battle_limit());
    info.set_active_rounds(row.active_rounds()); info.set_health_threshold_bps(row.health_threshold_bps());
}
void FillProduct(const ItemComp& item, BagItemInfo& info) {
    info.Clear();
    const auto* row = ItemRow(item.config_id());
    if (row == nullptr) return;
    info.set_item_id(item.item_id()); info.set_config_id(item.config_id()); info.set_count(item.size());
    info.set_max_stack(row->max_stack_size()); info.set_equip_kind(row->equip_kind());
    info.set_name(row->name()); info.set_description(row->description()); info.set_icon_key(row->icon_key());
    PlayerEquipSystem::FillItemDisplay(item, info);
}
uint32_t BuildDefinition(GetEquipmentCraftRulesResponse& rules) {
    rules.Clear();
    if (const auto qualityErr = PlayerEquipmentV1System::ValidateQualityTable(); qualityErr != kSuccess) {
        return qualityErr;
    }
    if (!ValidMaterial(equipcraft::kPolishSandConfigId, equipcraft::kPolishSandCount) ||
        !ValidMaterial(equipcraft::kGuaranteeCharmConfigId, 1)) {
        return kInvalidTableData;
    }
    std::vector<const ItemTable*> recipes;
    for (const auto& row : ItemTableManager::Instance().FindAll().data()) {
        if (row.equip_profile() == 0) continue;
        if (!ValidTemplate(row)) return kInvalidTableData;
        recipes.push_back(&row);
    }
    std::sort(recipes.begin(), recipes.end(), [](auto* a, auto* b) { return a->id() < b->id(); });
    for (const auto* row : recipes) {
        ItemComp item; item.set_config_id(row->id()); item.set_size(1); item.mutable_equip();
        auto* recipe = rules.add_recipes();
        FillProduct(item, *recipe->mutable_product());
        recipe->set_material_config_id(row->forge_material_id()); recipe->set_material_count(row->forge_material_count());
    }
    std::vector<const EquipV1AffixTable*> affixes;
    std::map<std::pair<uint32_t, uint32_t>, uint32_t> caps;
    for (const auto& row : EquipV1AffixTableManager::Instance().FindAll().data()) {
        if (!ValidAffix(row)) return kInvalidTableData;
        const auto [it, added] = caps.emplace(std::pair{row.effect(), row.effect_param()}, row.source_cap());
        if (!added && it->second != row.source_cap()) return kInvalidTableData;
        affixes.push_back(&row);
    }
    if (recipes.empty() || affixes.empty()) return kFeatureUnavailable;
    std::sort(affixes.begin(), affixes.end(), [](auto* a, auto* b) { return a->id() < b->id(); });
    for (const auto* row : affixes) FillAffixCatalog(*row, *rules.add_affixes());
    for (uint32_t tier = equipcraft::kMinQualityTier; tier <= equipcraft::kMaxQualityTier; ++tier) {
        const auto* q = QualityRow(tier);
        auto* out = rules.add_quality_tiers();
        out->set_quality_tier(q->id());
        out->set_name(q->name());
        out->set_ratio_pct(q->ratio_pct());
        out->set_apprentice_rate_bps(q->apprentice_weight());
        out->set_artisan_rate_bps(q->artisan_weight());
        out->set_master_rate_bps(q->master_weight());
    }
    // 预览显示值之外的属性语义、品相权重和材料规则同样属于版本；热更不能让旧确认继续扣料。
    rules.set_rules_fingerprint(
        ClientRequestFingerprintDigest(ItemTableManager::Instance().FindAll()) +
        ClientRequestFingerprintDigest(EquipAttributeTableManager::Instance().FindAll()) +
        ClientRequestFingerprintDigest(EquipEnhanceTableManager::Instance().FindAll()) +
        ClientRequestFingerprintDigest(EquipV1AffixTableManager::Instance().FindAll()) +
        ClientRequestFingerprintDigest(EquipArtifactTableManager::Instance().FindAll()) +
        ClientRequestFingerprintDigest(EquipAffixQualityTableManager::Instance().FindAll()));
    rules.set_rules_fingerprint(ClientRequestFingerprintDigest(rules));
    return kSuccess;
}
struct CraftDefinitionCacheKey {
    uint64_t tableLoadVersion{0};
    std::array<std::uintptr_t, 6> snapshots{};
    std::array<std::size_t, 6> rowCounts{};
    bool operator==(const CraftDefinitionCacheKey&) const = default;
};
struct CraftDefinitionCache {
    bool initialized{false};
    CraftDefinitionCacheKey key{};
    uint32_t error{kFeatureUnavailable};
    GetEquipmentCraftRulesResponse rules{};
};
CraftDefinitionCache& DefinitionCache() {
    static CraftDefinitionCache cache;
    return cache;
}
CraftDefinitionCacheKey CurrentDefinitionCacheKey() {
    const auto& items = ItemTableManager::Instance();
    const auto& attrs = EquipAttributeTableManager::Instance();
    const auto& enhances = EquipEnhanceTableManager::Instance();
    const auto& affixes = EquipV1AffixTableManager::Instance();
    const auto& artifacts = EquipArtifactTableManager::Instance();
    const auto& qualities = EquipAffixQualityTableManager::Instance();
    return CraftDefinitionCacheKey{
        GetTableLoadVersion(),
        {
            reinterpret_cast<std::uintptr_t>(&items.GetSnapshot()),
            reinterpret_cast<std::uintptr_t>(&attrs.GetSnapshot()),
            reinterpret_cast<std::uintptr_t>(&enhances.GetSnapshot()),
            reinterpret_cast<std::uintptr_t>(&affixes.GetSnapshot()),
            reinterpret_cast<std::uintptr_t>(&artifacts.GetSnapshot()),
            reinterpret_cast<std::uintptr_t>(&qualities.GetSnapshot()),
        },
        {items.Count(), attrs.Count(), enhances.Count(), affixes.Count(), artifacts.Count(), qualities.Count()},
    };
}
const CraftDefinitionCache& CachedDefinition() {
    auto& cache = DefinitionCache();
    const auto key = CurrentDefinitionCacheKey();
    if (!cache.initialized || !(cache.key == key)) {
        GetEquipmentCraftRulesResponse rebuilt;
        cache.error = BuildDefinition(rebuilt);
        cache.rules = (cache.error == kSuccess) ? std::move(rebuilt) : GetEquipmentCraftRulesResponse{};
        cache.key = key;
        cache.initialized = true;
    }
    return cache;
}
uint32_t CheckPlayer(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    const auto* identity = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    if (identity == nullptr || identity->value == 0 || ledger == nullptr ||
        tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player) == nullptr ||
        tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
        !client_request_ledger::Validate(*ledger).empty()) return kServiceUnavailable;
    return kSuccess;
}
uint32_t CheckWritable(entt::entity player) {
    if (!PlayerLifecycleSystem::IsPlayerMutable(player)) return kEquipFrozen;
    if (PlayerBattleSystem::IsInBattle(player)) return kEquipInBattle;
    return kSuccess;
}
bool Durable(entt::entity player, uint64_t revision) {
    const auto* persisted = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
    const auto* identity = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const auto* owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
    if (revision == 0 || identity == nullptr || !player_mutability::HasFencedOwnership(player) ||
        owner == nullptr || persisted == nullptr || !persisted->IsCommittedForOwner(owner->epoch)) return false;
    const auto& saved = persisted->snapshot->player_database_data();
    return saved.player_id() == identity->value && saved.client_request_ledger().forge_revision() >= revision;
}
void RequestPersist(entt::entity player, bool force) {
    if (!PlayerLifecycleSystem::IsPlayerMutable(player)) return;
    auto& state = tlsEcs.actorRegistry.get_or_emplace<CraftPersistRequestComp>(player);
    const auto now = std::chrono::steady_clock::now();
    if (!force && state.requested != std::chrono::steady_clock::time_point{} &&
        now - state.requested < std::chrono::seconds(1)) return;
    state.requested = now;
    persistFn(player);
}
} // namespace

uint32_t PlayerEquipmentV1System::ValidateQualityTable() {
    const auto& all = EquipAffixQualityTableManager::Instance().FindAll().data();
    if (all.size() != static_cast<int>(equipcraft::kMaxQualityTier)) return kInvalidTableData;
    uint32_t prevRatio = 0;
    uint32_t sumApprentice = 0, sumArtisan = 0, sumMaster = 0;
    for (uint32_t tier = equipcraft::kMinQualityTier; tier <= equipcraft::kMaxQualityTier; ++tier) {
        const auto* row = QualityRow(tier);
        if (row == nullptr || row->name().empty() || row->ratio_pct() < 1 || row->ratio_pct() > 100 ||
            row->ratio_pct() <= prevRatio) {
            return kInvalidTableData;
        }
        prevRatio = row->ratio_pct();
        sumApprentice += row->apprentice_weight();
        sumArtisan += row->artisan_weight();
        sumMaster += row->master_weight();
    }
    if (prevRatio != 100 || sumApprentice != equipcraft::kQualityWeightTotalBps ||
        sumArtisan != equipcraft::kQualityWeightTotalBps || sumMaster != equipcraft::kQualityWeightTotalBps) {
        return kInvalidTableData;
    }
    return kSuccess;
}

void PlayerEquipmentV1System::FillForgeMastery(entt::entity player, CraftMasteryInfo& out) {
    out.Clear();
    uint32_t exp = 0;
    if (tlsEcs.actorRegistry.valid(player)) {
        if (const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player); craft != nullptr) {
            exp = craft->forge_mastery_exp();
        }
    }
    const uint32_t tier = equipcraft::MasteryTierForExp(exp);
    out.set_tier(tier);
    out.set_tier_name(equipcraft::MasteryTierName(tier));
    out.set_exp(exp);
    out.set_next_tier_exp(equipcraft::MasteryNextExp(exp));
    out.set_top_quality_rate_bps(equipcraft::ForgeTopQualityRateBps(tier));
}

void PlayerEquipmentV1System::FillDisplay(const ItemComp& item, BagItemInfo& info) {
    const auto* row = ItemRow(item.config_id());
    if (row == nullptr) return;
    info.set_equip_profile(row->equip_profile());
    info.clear_fixed_affix_slots(); info.clear_artifact();
    info.clear_crafter_name(); info.set_crafter_player_id(0);
    if (item.has_equip()) {
        if (!item.equip().crafter_name().empty()) info.set_crafter_name(item.equip().crafter_name());
        if (item.equip().crafter_player_id() != 0) info.set_crafter_player_id(item.equip().crafter_player_id());
    }
    if (row->equip_profile() == 1 || row->equip_profile() == 2) {
        const bool valid = ValidSelections(item, row->equip_profile());
        for (uint32_t slot = 1; slot <= 2; ++slot) {
            auto* shown = info.add_fixed_affix_slots();
            shown->set_slot(slot); shown->set_pool(PoolFor(row->equip_profile(), slot));
            if (!valid) continue;
            for (const auto& selected : item.equip().fixed_affixes())
                if (selected.slot() == slot) FillAffixInstance(*AffixRow(selected.affix_id()), selected, *shown->mutable_selected());
        }
    } else if (row->equip_profile() == 3) {
        const auto* artifact = EquipArtifactTableManager::Instance().FindByIdSilent(row->artifact_id()).first;
        if (artifact != nullptr && ValidArtifact(*artifact)) FillArtifact(*artifact, *info.mutable_artifact());
    }
}

void PlayerEquipmentV1System::CollectEffects(entt::entity player, EquipmentEffects& effects) {
    effects.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return;
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return;
    effects.set_support_sources_complete(true);
    std::map<std::pair<uint32_t, uint32_t>, std::pair<uint64_t, uint32_t>> values;
    std::vector<std::pair<Guid, const ItemTable*>> artifacts;
    bags->bags[kEquipment].ForEachItem([&](Guid id, const ItemComp& item) {
        if (item.size() != 1) return;
        const auto* row = ItemRow(item.config_id());
        if (row == nullptr) { effects.set_support_sources_complete(false); return; }
        if (row->equip_profile() < 1 || row->equip_profile() > 3) return;
        if (!item.has_equip() || !ValidSelections(item, row->equip_profile())) {
            effects.set_support_sources_complete(false);
            return;
        }
        for (const auto& selected : item.equip().fixed_affixes()) {
            const auto& affix = *AffixRow(selected.affix_id());
            const bool exempt = equipcraft::IsPolishExempt(affix.effect());
            const uint32_t tier = equipcraft::EffectiveQualityTier(selected.quality_tier(), exempt);
            const uint32_t scaledValue = equipcraft::ScaleValueByQuality(affix.value_permyriad(), QualityRatioPct(tier));
            auto& total = values[{affix.effect(), affix.effect_param()}];
            total.first += scaledValue;
            total.second = total.second == 0 ? affix.source_cap() : std::min(total.second, affix.source_cap());
            if (affix.effect() == 5 || affix.effect() == 6) {
                auto* source = effects.add_support_sources();
                source->set_kind(1); source->set_item_id(id); source->set_affix_id(affix.id());
                source->set_name(row->name() + " · " + affix.name());
                source->set_effect_kind(affix.effect() == 6 ? 2 : 1);
                source->set_raw_bonus_bps(scaledValue);
                source->set_source_cap_bps(affix.source_cap());
            }
        }
        if (row->equip_profile() == 3) artifacts.emplace_back(id, row);
    });
    for (const auto& [key, total] : values) {
        const uint32_t value = static_cast<uint32_t>(std::min<uint64_t>(total.first, total.second));
        switch (key.first) {
        case 1: effects.set_physical_attack_bps(value); break;
        case 2: effects.set_magic_attack_bps(value); break;
        case 3: effects.set_max_health_bps(value); break;
        case 4: effects.set_speed_bps(value); break;
        case 5: effects.set_outgoing_heal_bps(value); break;
        case 6: effects.set_shield_capacity_bps(value); break;
        case 7: effects.set_physical_crit_bps(value); break;
        case 8: effects.set_magic_crit_bps(value); break;
        case 9: effects.set_physical_resist_bps(value); break;
        case 10: effects.set_magic_resist_bps(value); break;
        case 11: (*effects.mutable_ailment_hit_bps())[key.second] = value; break;
        case 12: (*effects.mutable_ailment_resist_bps())[key.second] = value; break;
        case 13: (*effects.mutable_mana_reduction_bps())[key.second] = value; break;
        default: break;
        }
    }
    auto* supportSources = effects.mutable_support_sources();
    std::sort(supportSources->begin(), supportSources->end(), [](const auto& a, const auto& b) {
        return a.item_id() != b.item_id() ? a.item_id() < b.item_id() : a.affix_id() < b.affix_id();
    });
    uint32_t healingLeft = effects.outgoing_heal_bps(), shieldLeft = effects.shield_capacity_bps();
    for (auto& source : *supportSources) {
        auto& left = source.effect_kind() == 2 ? shieldLeft : healingLeft;
        const auto contribution = std::min(left, static_cast<uint32_t>(source.raw_bonus_bps()));
        source.set_effective_bonus_bps(contribution); left -= contribution;
        if (contribution < source.raw_bonus_bps()) source.set_reason("装备同类来源上限已截取");
    }
    std::sort(artifacts.begin(), artifacts.end(), [](const auto& a, const auto& b) { return a.first < b.first; });
    for (const auto& [id, item] : artifacts) {
        const auto* row = EquipArtifactTableManager::Instance().FindByIdSilent(item->artifact_id()).first;
        if (row == nullptr || !ValidArtifact(*row)) { effects.set_support_sources_complete(false); continue; }
        auto* source = effects.add_artifacts(); source->set_item_id(id); source->set_effect_id(row->id());
        source->set_bonus_bps(row->bonus_bps()); source->set_cap_bps(row->cap_bps());
        source->set_per_round_limit(row->per_round_limit()); source->set_battle_limit(row->battle_limit());
        source->set_active_rounds(row->active_rounds()); source->set_health_threshold_bps(row->health_threshold_bps());
        source->set_name(row->name());
    }
    // 汇总单槽赛季印轻属性（仅气血与防御）及本季轻机制，与永久八部位和两件法宝独立存放。
    PlayerSeasonSystem::CollectEffects(player, TimeSystem::NowMillisecondsUTC(), effects);
}

uint32_t PlayerEquipmentV1System::GetRules(entt::entity player, GetEquipmentCraftRulesResponse& response) {
    response.Clear();
    if (const auto error = CheckPlayer(player); error != kSuccess) return error;
    const auto& cached = CachedDefinition();
    if (cached.error != kSuccess) return cached.error;
    response = cached.rules;
    response.set_forge_revision(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).forge_revision());
    FillForgeMastery(player, *response.mutable_forge_mastery());
    return kSuccess;
}

namespace {
struct PreparedCraft {
    ItemComp result;
    ItemComp original;
    BagType holder{kInventory};
    ItemCountMap consumed{};
    uint32_t materialId{0};
    uint32_t materialCount{0};
    uint32_t extraMaterialId{0};
    uint32_t extraMaterialCount{0};
    uint64_t baseGoldCost{0};
    uint64_t proxyFeeGold{0};
    uint64_t proxyMasterPlayerId{0};
    std::string proxyMasterName{};
    uint32_t effectiveMasteryTier{equipcraft::kMasteryApprentice};
    int targetAffixIndex{-1};
    bool polishExempt{false};
    bool rerollQualityOnReforge{false}; // 从豁免速度词条重铸成非速度词条时防白嫖 5 档，重新掷品相。
    bool tradableOnly{false};
    bool willBindItem{false};
    bool bindConfirmBlocked{false};
};

bool UnknownFields(const google::protobuf::Message& message) {
    return !message.GetReflection()->GetUnknownFields(message).empty();
}

bool LocateEquipment(const PlayerBagsComp& bags, Guid itemId, ItemComp& out, BagType& holderOut) {
    for (const auto holder : {kInventory, kEquipment}) {
        bool found = false;
        bags.bags[holder].ForEachItem([&](Guid id, const ItemComp& item) {
            if (id == itemId) { out = item; holderOut = holder; found = true; }
        });
        if (found) return true;
    }
    return false;
}

bool CanCreditProxyMaster(entt::entity master, uint64_t netFee) {
    if (netFee == 0) return true;
    if (tlsEcs.actorRegistry.try_get<CurrencyComp>(master) == nullptr ||
        tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(master) == nullptr) return false;
    if (GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency,
                                            static_cast<uint32_t>(kCurrencyGold)) ||
        CurrencySystem::IsCurrencyBlocked(master, kCurrencyGold)) return false;
    return !CurrencySystem::CreditOverflows(master, kCurrencyGold, netFee);
}

uint32_t ResolveProxyMaster(entt::entity buyer, uint64_t proxyMasterId, PreparedCraft& prepared) {
    if (proxyMasterId == 0) {
        const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(buyer);
        const uint32_t exp = craft != nullptr ? craft->forge_mastery_exp() : 0u;
        prepared.effectiveMasteryTier = equipcraft::MasteryTierForExp(exp);
        return kSuccess;
    }
    const uint64_t buyerId = tlsEcs.GetPlayerId(buyer);
    if (proxyMasterId == buyerId) return kEquipProxyMasterUnavailable;
    const entt::entity master = tlsEcs.GetPlayer(proxyMasterId);
    if (!tlsEcs.actorRegistry.valid(master) || !PlayerLifecycleSystem::IsPlayerMutable(master)) {
        return kEquipProxyMasterUnavailable;
    }
    const auto* masterCraft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(master);
    if (masterCraft == nullptr || !masterCraft->proxy_stall_open() ||
        equipcraft::MasteryTierForExp(masterCraft->forge_mastery_exp()) < equipcraft::kMasteryMaster ||
        !equipcraft::ValidProxyFee(masterCraft->proxy_forge_fee_gold())) {
        return kEquipProxyMasterUnavailable;
    }
    const uint64_t netFee = equipcraft::ProxyNetGold(masterCraft->proxy_forge_fee_gold());
    if (!CanCreditProxyMaster(master, netFee)) {
        return kEquipProxyMasterUnavailable;
    }
    prepared.proxyMasterPlayerId = proxyMasterId;
    prepared.proxyFeeGold = masterCraft->proxy_forge_fee_gold();
    prepared.proxyMasterName = PlayerDisplayName(master, proxyMasterId);
    prepared.effectiveMasteryTier = equipcraft::kMasteryMaster;
    return kSuccess;
}

void PlanBoundPolicy(entt::entity player, const Bag& inventory, const EquipmentCraftChoice& choice,
                     bool isCreateNew, PreparedCraft& prepared) {
    bool allTradableEnough = true;
    bool anyCountableBound = false;
    for (const auto& [configId, count] : prepared.consumed) {
        if (count == 0) continue;
        if (BagService::RemovableTradableCount(player, inventory, configId) < count) {
            allTradableEnough = false;
        }
        if (inventory.GetTotalItemCount(configId) > inventory.GetTotalTradableItemCount(configId)) {
            anyCountableBound = true;
        }
    }
    if (isCreateNew) {
        if (!choice.allow_bound_materials() && allTradableEnough) {
            prepared.tradableOnly = true;
            prepared.willBindItem = false;
            return;
        }
        prepared.tradableOnly = false;
        prepared.willBindItem = anyCountableBound;
        return;
    }
    if (prepared.original.bound()) {
        prepared.tradableOnly = false;
        prepared.willBindItem = false;
        return;
    }
    // 既有可交易装备：可交易材料充足时一律强制 TRADEABLE_ONLY（哪怕包里另有绑定堆或透传 allow_bound_materials=true，
    // 也绝不误耗绑定堆染毒纯 T 装备）；仅当可交易料不足时，未确认拦 kEquipBindConfirmRequired，确认后由提交段按实际
    // consumedBound 决定是否转绑。
    if (allTradableEnough) {
        prepared.tradableOnly = true;
        prepared.willBindItem = false;
        return;
    }
    if (!choice.allow_bound_materials()) {
        prepared.tradableOnly = true;
        prepared.willBindItem = true;
        prepared.bindConfirmBlocked = true;
        return;
    }
    prepared.tradableOnly = false;
    prepared.willBindItem = anyCountableBound;
}

uint32_t PrepareChoice(entt::entity player, const EquipmentCraftChoice& choice, PreparedCraft& prepared) {
    if (UnknownFields(choice)) return kFeatureUnavailable;
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return kServiceUnavailable;
    const auto& inventory = bags->bags[kInventory];

    if (choice.operation() == EquipmentCraftChoice::CREATE_ITEM) {
        if (choice.item_id() != 0 || choice.affix_slot() != 0 || choice.affix_id() != 0 ||
            choice.use_guarantee_charm() || choice.has_expected_polish_pity_count()) {
            return kInvalidParameter;
        }
        const auto* row = ItemRow(choice.config_id());
        if (row == nullptr || row->equip_profile() == 0 || !ValidTemplate(*row)) return kInvalidParameter;
        if (const auto proxyErr = ResolveProxyMaster(player, choice.proxy_master_player_id(), prepared);
            proxyErr != kSuccess) return proxyErr;
        prepared.result.set_config_id(row->id()); prepared.result.set_size(1); prepared.result.mutable_equip();
        prepared.materialId = row->forge_material_id(); prepared.materialCount = row->forge_material_count();
        prepared.consumed[prepared.materialId] += prepared.materialCount;
        PlanBoundPolicy(player, inventory, choice, true, prepared);
        return kSuccess;
    }

    if (choice.operation() == EquipmentCraftChoice::INSTALL_AFFIX) {
        if (choice.config_id() != 0 || choice.item_id() == 0 || choice.affix_id() == 0 ||
            choice.use_guarantee_charm() || choice.has_expected_polish_pity_count()) {
            return kInvalidParameter;
        }
        if (!LocateEquipment(*bags, choice.item_id(), prepared.original, prepared.holder)) return kEquipItemNotFound;
        const auto* itemRow = ItemRow(prepared.original.config_id());
        const auto* affix = AffixRow(choice.affix_id());
        if (itemRow == nullptr || affix == nullptr || !ValidAffix(*affix) ||
            PoolFor(itemRow->equip_profile(), choice.affix_slot()) != affix->pool() ||
            prepared.original.size() != 1 || !ValidSelections(prepared.original, itemRow->equip_profile())) {
            return kInvalidParameter;
        }
        if (UnknownFields(prepared.original) || UnknownFields(prepared.original.equip())) return kFeatureUnavailable;
        for (const auto& selected : prepared.original.equip().fixed_affixes()) {
            if (UnknownFields(selected)) return kFeatureUnavailable;
            if (selected.slot() == choice.affix_slot() || selected.affix_id() == choice.affix_id()) return kInvalidParameter;
        }
        if (const auto proxyErr = ResolveProxyMaster(player, choice.proxy_master_player_id(), prepared);
            proxyErr != kSuccess) return proxyErr;
        prepared.result = prepared.original;
        auto* selected = prepared.result.mutable_equip()->add_fixed_affixes();
        selected->set_slot(choice.affix_slot()); selected->set_affix_id(choice.affix_id());
        prepared.targetAffixIndex = prepared.result.equip().fixed_affixes_size() - 1;
        prepared.polishExempt = equipcraft::IsPolishExempt(affix->effect());
        prepared.materialId = affix->material_id(); prepared.materialCount = affix->material_count();
        prepared.consumed[prepared.materialId] += prepared.materialCount;
        PlanBoundPolicy(player, inventory, choice, false, prepared);
        return kSuccess;
    }

    if (choice.operation() == EquipmentCraftChoice::REFORGE_AFFIX) {
        if (choice.config_id() != 0 || choice.item_id() == 0 || choice.affix_id() == 0 ||
            choice.use_guarantee_charm() || choice.proxy_master_player_id() != 0 ||
            choice.has_expected_polish_pity_count()) {
            return kInvalidParameter;
        }
        if (!LocateEquipment(*bags, choice.item_id(), prepared.original, prepared.holder)) return kEquipItemNotFound;
        const auto* itemRow = ItemRow(prepared.original.config_id());
        const auto* newAffix = AffixRow(choice.affix_id());
        if (itemRow == nullptr || newAffix == nullptr || !ValidAffix(*newAffix) ||
            PoolFor(itemRow->equip_profile(), choice.affix_slot()) != newAffix->pool() ||
            prepared.original.size() != 1 || !ValidSelections(prepared.original, itemRow->equip_profile())) {
            return kInvalidParameter;
        }
        if (UnknownFields(prepared.original) || UnknownFields(prepared.original.equip())) return kFeatureUnavailable;
        int foundIndex = -1;
        for (int i = 0; i < prepared.original.equip().fixed_affixes_size(); ++i) {
            const auto& selected = prepared.original.equip().fixed_affixes(i);
            if (UnknownFields(selected)) return kFeatureUnavailable;
            if (selected.slot() == choice.affix_slot()) {
                if (selected.affix_id() == choice.affix_id()) return kEquipAffixReforgeSame;
                foundIndex = i;
            } else if (selected.affix_id() == choice.affix_id()) {
                return kInvalidParameter;
            }
        }
        if (foundIndex < 0) return kEquipAffixSlotEmpty;
        prepared.result = prepared.original;
        auto* target = prepared.result.mutable_equip()->mutable_fixed_affixes(foundIndex);
        const auto* oldAffix = AffixRow(target->affix_id());
        const bool oldExempt = oldAffix != nullptr && equipcraft::IsPolishExempt(oldAffix->effect());
        const bool newExempt = equipcraft::IsPolishExempt(newAffix->effect());
        target->set_affix_id(choice.affix_id());
        if (newExempt) {
            target->set_quality_tier(equipcraft::kMaxQualityTier);
            target->set_polish_pity_count(0);
        } else if (oldExempt) {
            // 防止先装免随机 G02 速度再重铸成普通词条白嫖极品；提交时按普通首装规则掷品相。
            prepared.rerollQualityOnReforge = true;
            const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player);
            prepared.effectiveMasteryTier = equipcraft::MasteryTierForExp(craft != nullptr ? craft->forge_mastery_exp() : 0u);
        }
        // 非豁免 -> 非豁免重铸：100% 原样保留 quality_tier 与 polish_pity_count。
        prepared.targetAffixIndex = foundIndex;
        prepared.polishExempt = newExempt;
        prepared.materialId = equipcraft::kReforgeSandConfigId;
        prepared.materialCount = equipcraft::kReforgeSandCount;
        prepared.baseGoldCost = equipcraft::kReforgeGoldCost;
        prepared.consumed[prepared.materialId] += prepared.materialCount;
        PlanBoundPolicy(player, inventory, choice, false, prepared);
        return kSuccess;
    }

    if (choice.operation() == EquipmentCraftChoice::POLISH_AFFIX) {
        if (choice.config_id() != 0 || choice.item_id() == 0 || choice.affix_id() != 0) {
            return kInvalidParameter;
        }
        if (!LocateEquipment(*bags, choice.item_id(), prepared.original, prepared.holder)) return kEquipItemNotFound;
        const auto* itemRow = ItemRow(prepared.original.config_id());
        if (itemRow == nullptr || PoolFor(itemRow->equip_profile(), choice.affix_slot()) == 0 ||
            prepared.original.size() != 1 || !ValidSelections(prepared.original, itemRow->equip_profile())) {
            return kInvalidParameter;
        }
        if (UnknownFields(prepared.original) || UnknownFields(prepared.original.equip())) return kFeatureUnavailable;
        int foundIndex = -1;
        for (int i = 0; i < prepared.original.equip().fixed_affixes_size(); ++i) {
            const auto& selected = prepared.original.equip().fixed_affixes(i);
            if (UnknownFields(selected)) return kFeatureUnavailable;
            if (selected.slot() == choice.affix_slot()) {
                foundIndex = i;
                break;
            }
        }
        if (foundIndex < 0) return kEquipAffixSlotEmpty;
        const auto& current = prepared.original.equip().fixed_affixes(foundIndex);
        const auto* affix = AffixRow(current.affix_id());
        if (affix == nullptr) return kInvalidParameter;
        if (equipcraft::IsPolishExempt(affix->effect())) return kEquipAffixPolishExempt;
        const uint32_t currentTier = equipcraft::EffectiveQualityTier(current.quality_tier(), false);
        if (currentTier >= equipcraft::kMaxQualityTier) return kEquipAffixAlreadyMaxQuality;
        if (choice.has_expected_polish_pity_count() &&
            choice.expected_polish_pity_count() != current.polish_pity_count()) {
            return kEquipPolishPityChanged;
        }
        if (const auto proxyErr = ResolveProxyMaster(player, choice.proxy_master_player_id(), prepared);
            proxyErr != kSuccess) return proxyErr;
        prepared.result = prepared.original;
        prepared.targetAffixIndex = foundIndex;
        prepared.materialId = equipcraft::kPolishSandConfigId;
        prepared.materialCount = equipcraft::kPolishSandCount;
        prepared.baseGoldCost = equipcraft::kPolishGoldCost;
        prepared.consumed[prepared.materialId] += prepared.materialCount;
        if (choice.use_guarantee_charm()) {
            prepared.extraMaterialId = equipcraft::kGuaranteeCharmConfigId;
            prepared.extraMaterialCount = 1;
            prepared.consumed[prepared.extraMaterialId] += prepared.extraMaterialCount;
        }
        PlanBoundPolicy(player, inventory, choice, false, prepared);
        return kSuccess;
    }

    if (choice.operation() == EquipmentCraftChoice::UNSEAL_TO_TRADABLE) {
        if (choice.config_id() != 0 || choice.item_id() == 0 || choice.affix_slot() != 0 || choice.affix_id() != 0 ||
            choice.source_item_id() != 0 || choice.use_guarantee_charm() || choice.proxy_master_player_id() != 0 ||
            choice.has_expected_polish_pity_count()) {
            return kInvalidParameter;
        }
        if (ItemRow(equipcraft::kUnsealTalismanConfigId) == nullptr) return kInvalidTableData;
        if (!LocateEquipment(*bags, choice.item_id(), prepared.original, prepared.holder)) return kEquipItemNotFound;
        const auto* itemRow = ItemRow(prepared.original.config_id());
        if (itemRow == nullptr || itemRow->equip_kind() == 0 || prepared.original.size() != 1 ||
            !prepared.original.has_equip()) {
            return kInvalidParameter;
        }
        if (UnknownFields(prepared.original) || UnknownFields(prepared.original.equip())) return kFeatureUnavailable;
        if (!prepared.original.bound()) return kInvalidParameter; // 已是可交易装备无需剥离
        // 要求先取下所有宝石，防止剥离过程误销毁玩家宝石。
        for (const auto& gem : prepared.original.equip().gems()) {
            if (gem.gem_config_id() != 0) return kInvalidParameter;
        }
        prepared.result = prepared.original;
        prepared.result.set_bound(false);
        auto* eq = prepared.result.mutable_equip();
        eq->set_enhance_level(0);
        eq->set_enhance_pity_count(0);
        eq->clear_fixed_affixes();
        eq->clear_crafter_name();
        eq->set_crafter_player_id(0);
        prepared.materialId = equipcraft::kUnsealTalismanConfigId;
        prepared.materialCount = 1;
        prepared.consumed[prepared.materialId] += prepared.materialCount;
        prepared.tradableOnly = false;
        prepared.willBindItem = false;
        return kSuccess;
    }

    if (choice.operation() == EquipmentCraftChoice::INHERIT_ENHANCE) {
        if (choice.config_id() != 0 || choice.item_id() == 0 || choice.source_item_id() == 0 ||
            choice.source_item_id() == choice.item_id() || choice.affix_slot() != 0 || choice.affix_id() != 0 ||
            choice.use_guarantee_charm() || choice.proxy_master_player_id() != 0 ||
            choice.has_expected_polish_pity_count()) {
            return kInvalidParameter;
        }
        if (ItemRow(equipcraft::kInheritTalismanConfigId) == nullptr) return kInvalidTableData;
        if (!LocateEquipment(*bags, choice.item_id(), prepared.original, prepared.holder)) return kEquipItemNotFound;
        ItemComp sourceItem;
        BagType sourceHolder = kInventory;
        if (!LocateEquipment(*bags, choice.source_item_id(), sourceItem, sourceHolder)) return kEquipItemNotFound;
        const auto* targetRow = ItemRow(prepared.original.config_id());
        const auto* sourceRow = ItemRow(sourceItem.config_id());
        if (targetRow == nullptr || sourceRow == nullptr || targetRow->equip_kind() == 0 ||
            targetRow->equip_kind() != sourceRow->equip_kind() ||
            targetRow->equip_profile() != sourceRow->equip_profile() ||
            targetRow->equip_level() != sourceRow->equip_level() ||
            prepared.original.size() != 1 || sourceItem.size() != 1 ||
            !prepared.original.has_equip() || !sourceItem.has_equip()) {
            return kInvalidParameter;
        }
        if (UnknownFields(prepared.original) || UnknownFields(prepared.original.equip()) ||
            UnknownFields(sourceItem) || UnknownFields(sourceItem.equip())) {
            return kFeatureUnavailable;
        }
        const uint32_t sourceLevel = sourceItem.equip().enhance_level();
        const uint32_t targetLevel = prepared.original.equip().enhance_level();
        if (sourceLevel == 0 || sourceLevel <= targetLevel) return kInvalidParameter;
        prepared.result = prepared.original;
        prepared.result.mutable_equip()->set_enhance_level(sourceLevel);
        prepared.result.mutable_equip()->set_enhance_pity_count(sourceItem.equip().enhance_pity_count());
        // 目标副套装备强制绑定仅自用，避免通过流派副套继承复制/洗白多件可交易高强成品。
        prepared.result.set_bound(true);
        prepared.willBindItem = !prepared.original.bound();
        prepared.materialId = equipcraft::kInheritTalismanConfigId;
        prepared.materialCount = 1;
        prepared.consumed[prepared.materialId] += prepared.materialCount;
        prepared.tradableOnly = false;
        return kSuccess;
    }

    return kInvalidParameter;
}

uint32_t Availability(entt::entity player, const PreparedCraft& prepared) {
    if (const auto error = CheckWritable(player); error != kSuccess) return error;
    const auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (ledger.forge_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) return kServiceUnavailable;
    const auto* level = tlsEcs.actorRegistry.try_get<LevelComp>(player);
    const auto* row = ItemRow(prepared.result.config_id());
    if (level == nullptr || row == nullptr) return kServiceUnavailable;
    if (playerlevel::CappedLevel(level->level()) < row->equip_level()) return kEquipLevelNotEnough;
    const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    const auto verdict = BagService::CheckRemovable(player, inventory, prepared.consumed);
    if (verdict == BagService::RemovalVerdict::kInsufficient) return kEquipEnhanceMaterialNotEnough;
    if (verdict != BagService::RemovalVerdict::kEnough) return kFeatureUnavailable;
    if (prepared.bindConfirmBlocked) return kEquipBindConfirmRequired;
    const uint64_t totalGold = prepared.baseGoldCost + prepared.proxyFeeGold;
    if (totalGold > 0 && (totalGold > kMaxPayableGold ||
                          !CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(totalGold)))) {
        return kEquipCraftGoldNotEnough;
    }
    if (prepared.original.item_id() == 0) {
        return BagService::CheckInventoryExchangeSpace(player, prepared.consumed,
                                                       {{prepared.result.config_id(), 1}}, prepared.tradableOnly,
                                                       prepared.result.bound() || prepared.willBindItem);
    }
    return kSuccess;
}

void FillMaterialEntry(entt::entity player, uint32_t configId, uint32_t count, bool tradableOnly,
                       EquipmentCraftMaterial& cost) {
    const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    const auto* material = ItemRow(configId);
    cost.set_config_id(configId); cost.set_count(count);
    if (material != nullptr) { cost.set_name(material->name()); cost.set_icon_key(material->icon_key()); }
    cost.set_owned(inventory.GetTotalItemCount(configId));
    cost.set_available(tradableOnly ? BagService::RemovableTradableCount(player, inventory, configId)
                                    : BagService::RemovableCount(player, inventory, configId));
}

void FillCraftResult(entt::entity player, const ClientRequestResult& result, bool replayed,
                     CommitEquipmentCraftResponse& response) {
    response.set_outcome(CommitEquipmentCraftResponse::APPLIED); response.set_replayed(replayed);
    response.set_item_id(result.equipmentItemId); response.set_result_forge_revision(result.forgeRevision);
    response.set_current_forge_revision(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).forge_revision());
    response.set_durable(Durable(player, result.forgeRevision));
    response.set_result_quality_tier(result.equipmentQualityTier);
    response.set_result_polish_pity_count(result.equipmentPolishPityCount);
    response.set_guaranteed_by_pity(result.equipmentGuaranteedByPity);
    PlayerEquipmentV1System::FillForgeMastery(player, *response.mutable_forge_mastery());
    auto error = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
    if (error == kSuccess) error = PlayerBagSystem::BuildSnapshot(player, kEquipment, *response.mutable_equipment());
    if (error != kSuccess) {
        response.clear_bag(); response.clear_equipment(); response.mutable_error_message()->set_id(error);
    }
    if (!response.durable()) RequestPersist(player, !replayed);
}
} // namespace

uint32_t PlayerEquipmentV1System::Preview(entt::entity player, const PreviewEquipmentCraftRequest& request,
                                          PreviewEquipmentCraftResponse& response) {
    response.Clear();
    if (const auto error = CheckPlayer(player); error != kSuccess) return error;
    if (!request.has_choice() || UnknownFields(request)) return kInvalidParameter;
    const auto& cached = CachedDefinition();
    if (cached.error != kSuccess) return cached.error;
    PreparedCraft prepared;
    if (const auto error = PrepareChoice(player, request.choice(), prepared); error != kSuccess) return error;
    auto& preview = *response.mutable_preview();
    *preview.mutable_choice() = request.choice();
    preview.set_rules_fingerprint(cached.rules.rules_fingerprint());
    preview.set_expected_forge_revision(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).forge_revision());
    ItemComp previewItem = prepared.result;
    if (prepared.willBindItem) previewItem.set_bound(true);
    if (request.choice().operation() == EquipmentCraftChoice::POLISH_AFFIX && prepared.targetAffixIndex >= 0) {
        auto* target = previewItem.mutable_equip()->mutable_fixed_affixes(prepared.targetAffixIndex);
        if (request.choice().use_guarantee_charm() || target->polish_pity_count() + 1 >= equipcraft::kPolishPityLimit) {
            target->set_quality_tier(equipcraft::kMaxQualityTier);
            target->set_polish_pity_count(0);
        }
    }
    FillProduct(previewItem, *preview.mutable_result());
    FillMaterialEntry(player, prepared.materialId, prepared.materialCount, prepared.tradableOnly,
                      *preview.mutable_material());
    if (prepared.extraMaterialId != 0 && prepared.extraMaterialCount != 0) {
        FillMaterialEntry(player, prepared.extraMaterialId, prepared.extraMaterialCount, prepared.tradableOnly,
                          *preview.mutable_extra_material());
    }
    preview.set_gold_cost(prepared.baseGoldCost);
    preview.set_proxy_fee_gold(prepared.proxyFeeGold);
    if (!prepared.proxyMasterName.empty()) preview.set_proxy_master_name(prepared.proxyMasterName);
    preview.set_will_bind_item(prepared.willBindItem);
    const auto unavailable = Availability(player, prepared);
    preview.set_available(unavailable == kSuccess);
    if (unavailable != kSuccess) preview.set_unavailable_tip(unavailable);
    return kSuccess;
}

void PlayerEquipmentV1System::Commit(entt::entity player, const CommitEquipmentCraftRequest& request,
                                     CommitEquipmentCraftResponse& response) {
    response.Clear(); response.set_client_request_id(request.client_request_id());
    const auto reject = [&](uint32_t error, bool known = true) {
        response.set_outcome(known ? CommitEquipmentCraftResponse::REJECTED : CommitEquipmentCraftResponse::UNKNOWN);
        response.mutable_error_message()->set_id(error);
    };
    if (const auto error = CheckPlayer(player); error != kSuccess) { reject(error, false); return; }
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    response.set_current_forge_revision(ledger.forge_revision());
    if (request.client_request_id() == 0 || !request.has_choice() || UnknownFields(request)) {
        reject(kInvalidParameter); return;
    }
    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kEquipmentCraft, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) { FillCraftResult(player, first, true, response); return; }
    if (verdict == ClientRequestVerdict::kMismatch) { reject(kInvalidParameter); return; }
    if (verdict != ClientRequestVerdict::kNew) { reject(kServiceUnavailable, false); return; }
    // 回执未命中而水位已变化，原意图可能成功后被淘汰；只能拒绝重做，不能伪称首次失败。
    if (request.expected_forge_revision() != ledger.forge_revision()) {
        reject(kEquipEnhanceLevelChanged, false); return;
    }
    const auto& cached = CachedDefinition();
    if (cached.error != kSuccess) { reject(cached.error); return; }
    if (request.rules_fingerprint() != cached.rules.rules_fingerprint()) { reject(kInvalidParameter); return; }
    PreparedCraft prepared;
    if (const auto error = PrepareChoice(player, request.choice(), prepared); error != kSuccess) { reject(error); return; }
    if (const auto error = Availability(player, prepared); error != kSuccess) { reject(error); return; }

    ClientRequestResult applied;
    applied.forgeRevision = ledger.forge_revision() + 1;
    applied.equipmentItemId = prepared.result.item_id();

    const auto op = request.choice().operation();
    if ((op == EquipmentCraftChoice::INSTALL_AFFIX || prepared.rerollQualityOnReforge) &&
        prepared.targetAffixIndex >= 0) {
        auto* target = prepared.result.mutable_equip()->mutable_fixed_affixes(prepared.targetAffixIndex);
        const uint32_t tier = prepared.polishExempt ? equipcraft::kMaxQualityTier
                                                    : RollQualityForMastery(prepared.effectiveMasteryTier);
        target->set_quality_tier(tier);
        target->set_polish_pity_count(0);
        applied.equipmentQualityTier = tier;
        applied.equipmentPolishPityCount = 0;
        applied.equipmentGuaranteedByPity = false;
    } else if (op == EquipmentCraftChoice::REFORGE_AFFIX && prepared.targetAffixIndex >= 0) {
        const auto& target = prepared.result.equip().fixed_affixes(prepared.targetAffixIndex);
        applied.equipmentQualityTier = equipcraft::EffectiveQualityTier(target.quality_tier(), prepared.polishExempt);
        applied.equipmentPolishPityCount = target.polish_pity_count();
    } else if (op == EquipmentCraftChoice::POLISH_AFFIX && prepared.targetAffixIndex >= 0) {
        auto* target = prepared.result.mutable_equip()->mutable_fixed_affixes(prepared.targetAffixIndex);
        const uint32_t currentTier = equipcraft::EffectiveQualityTier(target->quality_tier(), false);
        const uint32_t rolledTier = RollQualityForMastery(prepared.effectiveMasteryTier);
        const auto polished = equipcraft::ResolvePolish(currentTier, target->polish_pity_count(),
                                                        rolledTier, request.choice().use_guarantee_charm());
        target->set_quality_tier(polished.newTier);
        target->set_polish_pity_count(polished.newPityCount);
        applied.equipmentQualityTier = polished.newTier;
        applied.equipmentPolishPityCount = polished.newPityCount;
        applied.equipmentGuaranteedByPity = polished.guaranteedByPity;
    }

    // 宗师（本人或主城原子代打宗师）产出极品非豁免词条时永久刻上宗师署名（0 战力加成）。
    if ((op == EquipmentCraftChoice::INSTALL_AFFIX || op == EquipmentCraftChoice::POLISH_AFFIX) &&
        !prepared.polishExempt && applied.equipmentQualityTier == equipcraft::kMaxQualityTier &&
        prepared.effectiveMasteryTier == equipcraft::kMasteryMaster) {
        const uint64_t signerId = prepared.proxyMasterPlayerId != 0
            ? prepared.proxyMasterPlayerId : tlsEcs.GetPlayerId(player);
        const std::string signerName = prepared.proxyMasterPlayerId != 0
            ? prepared.proxyMasterName : PlayerDisplayName(player, signerId);
        prepared.result.mutable_equip()->set_crafter_player_id(signerId);
        prepared.result.mutable_equip()->set_crafter_name(signerName);
    }

    const auto ledgerBefore = ledger.SerializeAsString();
    std::vector<InitItemParam> gained;
    std::vector<BagService::InstanceReplacement> replacements;
    if (op == EquipmentCraftChoice::CREATE_ITEM) {
        InitItemParam param; param.itemPBComp = prepared.result; param.itemPBComp.set_item_id(kInvalidGuid);
        gained.push_back(std::move(param));
    } else {
        replacements.push_back({prepared.holder, prepared.original, prepared.result});
    }

    const uint64_t totalGold = prepared.baseGoldCost + prepared.proxyFeeGold;
    const TransactionType goldTxType = (op == EquipmentCraftChoice::POLISH_AFFIX || op == EquipmentCraftChoice::REFORGE_AFFIX)
        ? TX_EQUIP_POLISH_REFORGE : TX_CRAFT_PROXY_FEE;
    const bool bindOnConsumedBound =
        op == EquipmentCraftChoice::CREATE_ITEM || op == EquipmentCraftChoice::INSTALL_AFFIX ||
        op == EquipmentCraftChoice::REFORGE_AFFIX || op == EquipmentCraftChoice::POLISH_AFFIX;
    bool consumedBound = false;
    std::vector<GuidVector> written;
    const auto error = BagService::ExchangeInventoryExact(player, prepared.consumed,
        gained, replacements, request.client_request_id(), "{\"operation\":\"equipment_craft\"}", written,
        [&](const std::vector<GuidVector>& created) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || CheckWritable(player) != kSuccess) return kFeatureUnavailable;
            if (!gained.empty()) {
                if (created.size() != 1 || created.front().size() != 1 || created.front().front() == 0 ||
                    created.front().front() == kInvalidGuid) return kEquipInternalError;
                applied.equipmentItemId = created.front().front();
            }
            if (totalGold > 0 && (totalGold > kMaxPayableGold ||
                                  !CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(totalGold)))) {
                return kEquipCraftGoldNotEnough;
            }
            entt::entity proxyMaster = entt::null;
            uint64_t proxyNetGold = 0;
            if (prepared.proxyMasterPlayerId != 0) {
                proxyMaster = tlsEcs.GetPlayer(prepared.proxyMasterPlayerId);
                const auto* masterCraft = tlsEcs.actorRegistry.valid(proxyMaster)
                    ? tlsEcs.actorRegistry.try_get<PlayerCraftComp>(proxyMaster) : nullptr;
                if (masterCraft == nullptr || !PlayerLifecycleSystem::IsPlayerMutable(proxyMaster) ||
                    !masterCraft->proxy_stall_open() ||
                    equipcraft::MasteryTierForExp(masterCraft->forge_mastery_exp()) < equipcraft::kMasteryMaster ||
                    masterCraft->proxy_forge_fee_gold() != prepared.proxyFeeGold) {
                    return kEquipProxyMasterUnavailable;
                }
                proxyNetGold = equipcraft::ProxyNetGold(prepared.proxyFeeGold);
                if (!CanCreditProxyMaster(proxyMaster, proxyNetGold)) {
                    return kEquipProxyMasterUnavailable;
                }
            }
            ClientRequestLedgerComp next = ledger;
            next.set_forge_revision(applied.forgeRevision);
            if (!client_request_ledger::Remember(next, request.client_request_id(),
                                                 ClientRequestKind::kEquipmentCraft, fingerprint, applied) ||
                !client_request_ledger::Validate(next).empty()) {
                return kEquipInternalError;
            }
            if (totalGold > 0) {
                const auto deductErr = CurrencySystem::DeductCurrency(
                    player, kCurrencyGold, static_cast<int64_t>(totalGold), goldTxType, request.client_request_id());
                if (deductErr != kSuccess) return kEquipCraftGoldNotEnough;
            }
            if (proxyMaster != entt::null && proxyNetGold > 0) {
                (void)CurrencySystem::AddCurrency(proxyMaster, kCurrencyGold, static_cast<int64_t>(proxyNetGold),
                                                  TX_CRAFT_PROXY_FEE, request.client_request_id());
            }
            // 本人打造/装条/打磨自然涨 1 点铸器熟练度（无体力、无日清、只增不减；重铸/解封/继承不涨熟练度）。
            if (prepared.proxyMasterPlayerId == 0 && op != EquipmentCraftChoice::REFORGE_AFFIX &&
                op != EquipmentCraftChoice::UNSEAL_TO_TRADABLE && op != EquipmentCraftChoice::INHERIT_ENHANCE) {
                if (auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player);
                    craft != nullptr && craft->forge_mastery_exp() < std::numeric_limits<uint32_t>::max()) {
                    craft->set_forge_mastery_exp(craft->forge_mastery_exp() + 1);
                }
            }
            ledger.Swap(&next);
            return kSuccess;
        }, prepared.tradableOnly, &consumedBound, bindOnConsumedBound);
    if (error != kSuccess) { reject(error); return; }
    if (!replacements.empty() && prepared.holder == kEquipment) {
        PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kEquipmentChanged);
        PlayerAttributeSystem::PushPanel(player);
    }
    FillCraftResult(player, applied, false, response);
}

void PlayerEquipmentV1System::SetPersistForTest(PersistFn fn) {
    persistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}

void PlayerEquipmentV1System::SetQualityRollForTest(QualityRollFn fn) {
    qualityRollFn = fn;
}

void PlayerEquipmentV1System::InvalidateRulesCacheForTest() {
    DefinitionCache().initialized = false;
}
