#include "player_equipment_v1.h"

#include <algorithm>
#include <chrono>
#include <limits>
#include <map>
#include <set>
#include <string>
#include <vector>
#include "core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_attribute.h"
#include "player/system/player_equip.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "battle/system/player_battle.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "table/code/equipartifact_table.h"
#include "table/code/equipv1affix_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
PlayerEquipmentV1System::PersistFn persistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;
struct CraftPersistRequestComp { std::chrono::steady_clock::time_point requested{}; };

const ItemTable* ItemRow(uint32_t id) { return ItemTableManager::Instance().FindByIdSilent(id).first; }
const EquipV1AffixTable* AffixRow(uint32_t id) { return EquipV1AffixTableManager::Instance().FindByIdSilent(id).first; }
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
    if (effect < 1 || effect > 13) return false;
    if (effect <= 10 && param != 0) return false;
    if ((effect == 11 || effect == 12) && (param < 1 || param > 7)) return false;
    if (effect == 13 && (param < 1 || param > 12)) return false;
    if (row.pool() == 1) return effect == 1 || effect == 2 || effect == 5 || effect == 6 || effect == 11;
    if (row.pool() == 2) return effect == 7 || effect == 8 || effect == 13;
    return row.pool() == 3 && (effect == 3 || effect == 4 || effect == 9 || effect == 10 || effect == 12);
}
bool ValidArtifact(const EquipArtifactTable& row) {
    return row.id() >= 1 && row.id() <= 8 && row.bonus_bps() > 0 && row.bonus_bps() <= 10000 &&
        row.cap_bps() <= 10000 && row.health_threshold_bps() <= 10000 &&
        (row.per_round_limit() != 0 || row.battle_limit() != 0);
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
void FillAffix(const EquipV1AffixTable& row, EquipFixedAffixInfo& info) {
    info.set_affix_id(row.id()); info.set_pool(row.pool());
    info.set_name(row.name()); info.set_description(row.description());
    info.set_value_bps(row.value_permyriad()); info.set_source_cap_bps(row.source_cap());
    info.set_effect(row.effect()); info.set_effect_param(row.effect_param());
    info.set_unit(row.effect() >= 7 && row.effect() <= 12 ? 2 : 1);
    info.set_material_config_id(row.material_id()); info.set_material_count(row.material_count());
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
    std::map<std::pair<uint32_t,uint32_t>, uint32_t> caps;
    for (const auto& row : EquipV1AffixTableManager::Instance().FindAll().data()) {
        if (!ValidAffix(row)) return kInvalidTableData;
        const auto [it, added] = caps.emplace(std::pair{row.effect(),row.effect_param()},row.source_cap());
        if (!added && it->second != row.source_cap()) return kInvalidTableData;
        affixes.push_back(&row);
    }
    if (recipes.empty() || affixes.empty()) return kFeatureUnavailable;
    std::sort(affixes.begin(), affixes.end(), [](auto* a, auto* b) { return a->id() < b->id(); });
    for (const auto* row : affixes) FillAffix(*row, *rules.add_affixes());
    rules.set_rules_fingerprint(ClientRequestFingerprintDigest(rules));
    return kSuccess;
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
    if (revision == 0 || identity == nullptr || !player_mutability::HasFencedOwnership(player) ||
        persisted == nullptr || !persisted->HasSnapshot()) return false;
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

void PlayerEquipmentV1System::FillDisplay(const ItemComp& item, BagItemInfo& info) {
    const auto* row = ItemRow(item.config_id());
    if (row == nullptr) return;
    info.set_equip_profile(row->equip_profile());
    info.clear_fixed_affix_slots(); info.clear_artifact();
    if (row->equip_profile() == 1 || row->equip_profile() == 2) {
        const bool valid = ValidSelections(item, row->equip_profile());
        for (uint32_t slot = 1; slot <= 2; ++slot) {
            auto* shown = info.add_fixed_affix_slots();
            shown->set_slot(slot); shown->set_pool(PoolFor(row->equip_profile(), slot));
            if (!valid) continue;
            for (const auto& selected : item.equip().fixed_affixes())
                if (selected.slot() == slot) FillAffix(*AffixRow(selected.affix_id()), *shown->mutable_selected());
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
    std::map<std::pair<uint32_t,uint32_t>, std::pair<uint64_t,uint32_t>> values;
    std::vector<std::pair<Guid,const ItemTable*>> artifacts;
    bags->bags[kEquipment].ForEachItem([&](Guid id, const ItemComp& item) {
        const auto* row = ItemRow(item.config_id());
        if (row == nullptr || item.size() != 1 || !item.has_equip() ||
            row->equip_profile() < 1 || row->equip_profile() > 3 || !ValidSelections(item,row->equip_profile())) return;
        for (const auto& selected : item.equip().fixed_affixes()) {
            const auto& affix = *AffixRow(selected.affix_id());
            auto& total = values[{affix.effect(),affix.effect_param()}];
            total.first += affix.value_permyriad();
            total.second = total.second == 0 ? affix.source_cap() : std::min(total.second,affix.source_cap());
        }
        if (row->equip_profile() == 3) artifacts.emplace_back(id,row);
    });
    for (const auto& [key, total] : values) {
        const uint32_t value = static_cast<uint32_t>(std::min<uint64_t>(total.first,total.second));
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
    std::sort(artifacts.begin(), artifacts.end(), [](const auto& a,const auto& b) { return a.first < b.first; });
    for (const auto& [id, item] : artifacts) {
        const auto* row = EquipArtifactTableManager::Instance().FindByIdSilent(item->artifact_id()).first;
        if (row == nullptr || !ValidArtifact(*row)) continue;
        auto* source = effects.add_artifacts(); source->set_item_id(id); source->set_effect_id(row->id());
        source->set_bonus_bps(row->bonus_bps()); source->set_cap_bps(row->cap_bps());
        source->set_per_round_limit(row->per_round_limit()); source->set_battle_limit(row->battle_limit());
        source->set_active_rounds(row->active_rounds()); source->set_health_threshold_bps(row->health_threshold_bps());
    }
}

uint32_t PlayerEquipmentV1System::GetRules(entt::entity player, GetEquipmentCraftRulesResponse& response) {
    response.Clear();
    if (const auto error = CheckPlayer(player); error != kSuccess) return error;
    if (const auto error = BuildDefinition(response); error != kSuccess) { response.Clear(); return error; }
    response.set_forge_revision(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).forge_revision());
    return kSuccess;
}

namespace {
struct PreparedCraft {
    ItemComp result;
    ItemComp original;
    BagType holder{kInventory};
    uint32_t materialId{0};
    uint32_t materialCount{0};
};
bool UnknownFields(const google::protobuf::Message& message) {
    return !message.GetReflection()->GetUnknownFields(message).empty();
}
uint32_t PrepareChoice(entt::entity player, const EquipmentCraftChoice& choice, PreparedCraft& prepared) {
    if (UnknownFields(choice)) return kFeatureUnavailable;
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return kServiceUnavailable;
    if (choice.operation() == EquipmentCraftChoice::CREATE_ITEM) {
        if (choice.item_id() != 0 || choice.affix_slot() != 0 || choice.affix_id() != 0) return kInvalidParameter;
        const auto* row = ItemRow(choice.config_id());
        if (row == nullptr || row->equip_profile() == 0 || !ValidTemplate(*row)) return kInvalidParameter;
        prepared.result.set_config_id(row->id()); prepared.result.set_size(1); prepared.result.mutable_equip();
        prepared.materialId = row->forge_material_id(); prepared.materialCount = row->forge_material_count();
        return kSuccess;
    }
    if (choice.operation() != EquipmentCraftChoice::INSTALL_AFFIX || choice.config_id() != 0 ||
        choice.item_id() == 0 || choice.affix_id() == 0) return kInvalidParameter;
    bool found = false;
    for (const auto holder : {kInventory,kEquipment}) {
        bags->bags[holder].ForEachItem([&](Guid id,const ItemComp& item) {
            if (id == choice.item_id()) { prepared.original = item; prepared.holder = holder; found = true; }
        });
        if (found) break;
    }
    if (!found) return kEquipItemNotFound;
    const auto* itemRow = ItemRow(prepared.original.config_id());
    const auto* affix = AffixRow(choice.affix_id());
    if (itemRow == nullptr || affix == nullptr || !ValidAffix(*affix) ||
        PoolFor(itemRow->equip_profile(),choice.affix_slot()) != affix->pool() ||
        prepared.original.size() != 1 || !ValidSelections(prepared.original,itemRow->equip_profile())) return kInvalidParameter;
    if (UnknownFields(prepared.original) || UnknownFields(prepared.original.equip())) return kFeatureUnavailable;
    for (const auto& selected : prepared.original.equip().fixed_affixes()) {
        if (UnknownFields(selected)) return kFeatureUnavailable;
        // 本期只开放首次安装，已有条目不可覆盖；同件首饰不能两次选择相同词条。
        if (selected.slot() == choice.affix_slot() || selected.affix_id() == choice.affix_id()) return kInvalidParameter;
    }
    prepared.result = prepared.original;
    auto* selected = prepared.result.mutable_equip()->add_fixed_affixes();
    selected->set_slot(choice.affix_slot()); selected->set_affix_id(choice.affix_id());
    prepared.materialId = affix->material_id(); prepared.materialCount = affix->material_count();
    return kSuccess;
}
uint32_t Availability(entt::entity player,const PreparedCraft& prepared) {
    if (const auto error = CheckWritable(player); error != kSuccess) return error;
    const auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (ledger.forge_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) return kServiceUnavailable;
    const auto* level = tlsEcs.actorRegistry.try_get<LevelComp>(player);
    const auto* row = ItemRow(prepared.result.config_id());
    if (level == nullptr || row == nullptr) return kServiceUnavailable;
    if (level->level() < row->equip_level()) return kEquipLevelNotEnough;
    auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    const auto verdict = BagService::CheckRemovable(player,inventory,{{prepared.materialId,prepared.materialCount}});
    if (verdict == BagService::RemovalVerdict::kInsufficient) return kEquipEnhanceMaterialNotEnough;
    if (verdict != BagService::RemovalVerdict::kEnough) return kFeatureUnavailable;
    return kSuccess;
}
void FillMaterial(entt::entity player,const PreparedCraft& prepared,EquipmentCraftMaterial& cost) {
    const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    const auto* material = ItemRow(prepared.materialId);
    cost.set_config_id(prepared.materialId); cost.set_count(prepared.materialCount);
    if (material != nullptr) { cost.set_name(material->name()); cost.set_icon_key(material->icon_key()); }
    cost.set_owned(inventory.GetTotalItemCount(prepared.materialId));
    cost.set_available(BagService::RemovableCount(player,inventory,prepared.materialId));
}
void FillCraftResult(entt::entity player,const ClientRequestResult& result,bool replayed,
                     CommitEquipmentCraftResponse& response) {
    response.set_outcome(CommitEquipmentCraftResponse::APPLIED); response.set_replayed(replayed);
    response.set_item_id(result.equipmentItemId); response.set_result_forge_revision(result.forgeRevision);
    response.set_current_forge_revision(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).forge_revision());
    response.set_durable(Durable(player,result.forgeRevision));
    auto error = PlayerBagSystem::BuildSnapshot(player,kInventory,*response.mutable_bag());
    if (error == kSuccess) error = PlayerBagSystem::BuildSnapshot(player,kEquipment,*response.mutable_equipment());
    if (error != kSuccess) {
        response.clear_bag(); response.clear_equipment(); response.mutable_error_message()->set_id(error);
    }
    if (!response.durable()) RequestPersist(player,!replayed);
}
} // namespace

uint32_t PlayerEquipmentV1System::Preview(entt::entity player,const PreviewEquipmentCraftRequest& request,
                                        PreviewEquipmentCraftResponse& response) {
    response.Clear();
    if (const auto error = CheckPlayer(player); error != kSuccess) return error;
    if (!request.has_choice() || UnknownFields(request)) return kInvalidParameter;
    GetEquipmentCraftRulesResponse rules;
    if (const auto error = BuildDefinition(rules); error != kSuccess) return error;
    PreparedCraft prepared;
    if (const auto error = PrepareChoice(player,request.choice(),prepared); error != kSuccess) return error;
    auto& preview = *response.mutable_preview();
    *preview.mutable_choice() = request.choice();
    preview.set_rules_fingerprint(rules.rules_fingerprint());
    preview.set_expected_forge_revision(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).forge_revision());
    FillProduct(prepared.result,*preview.mutable_result());
    FillMaterial(player,prepared,*preview.mutable_material());
    const auto unavailable = Availability(player,prepared);
    preview.set_available(unavailable == kSuccess);
    if (unavailable != kSuccess) preview.set_unavailable_tip(unavailable);
    return kSuccess;
}

void PlayerEquipmentV1System::Commit(entt::entity player,const CommitEquipmentCraftRequest& request,
                                   CommitEquipmentCraftResponse& response) {
    response.Clear(); response.set_client_request_id(request.client_request_id());
    const auto reject = [&](uint32_t error,bool known = true) {
        response.set_outcome(known ? CommitEquipmentCraftResponse::REJECTED : CommitEquipmentCraftResponse::UNKNOWN);
        response.mutable_error_message()->set_id(error);
    };
    if (const auto error = CheckPlayer(player); error != kSuccess) { reject(error,false); return; }
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    response.set_current_forge_revision(ledger.forge_revision());
    if (request.client_request_id() == 0 || !request.has_choice() || UnknownFields(request)) {
        reject(kInvalidParameter); return;
    }
    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player,request.client_request_id(),ClientRequestKind::kEquipmentCraft,fingerprint,first);
    if (verdict == ClientRequestVerdict::kReplay) { FillCraftResult(player,first,true,response); return; }
    if (verdict == ClientRequestVerdict::kMismatch) { reject(kInvalidParameter); return; }
    if (verdict != ClientRequestVerdict::kNew) { reject(kServiceUnavailable,false); return; }
    // 回执未命中而水位已变化，原意图可能成功后被淘汰；只能拒绝重做，不能伪称首次失败。
    if (request.expected_forge_revision() != ledger.forge_revision()) {
        reject(kEquipEnhanceLevelChanged,false); return;
    }
    GetEquipmentCraftRulesResponse rules;
    if (const auto error = BuildDefinition(rules); error != kSuccess) { reject(error); return; }
    if (request.rules_fingerprint() != rules.rules_fingerprint()) { reject(kInvalidParameter); return; }
    PreparedCraft prepared;
    if (const auto error = PrepareChoice(player,request.choice(),prepared); error != kSuccess) { reject(error); return; }
    if (const auto error = Availability(player,prepared); error != kSuccess) { reject(error); return; }
    const auto ledgerBefore = ledger.SerializeAsString();
    std::vector<InitItemParam> gained;
    std::vector<BagService::InstanceReplacement> replacements;
    if (request.choice().operation() == EquipmentCraftChoice::CREATE_ITEM) {
        InitItemParam param; param.itemPBComp = prepared.result; param.itemPBComp.set_item_id(kInvalidGuid);
        gained.push_back(std::move(param));
    } else {
        replacements.push_back({prepared.holder,prepared.original,prepared.result});
    }
    ClientRequestResult applied;
    applied.forgeRevision = ledger.forge_revision() + 1;
    applied.equipmentItemId = prepared.result.item_id();
    std::vector<GuidVector> written;
    const auto error = BagService::ExchangeInventoryExact(player,{{prepared.materialId,prepared.materialCount}},
        gained,replacements,request.client_request_id(),"{\"operation\":\"equipment_craft\"}",written,
        [&](const std::vector<GuidVector>& created) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || CheckWritable(player) != kSuccess) return kFeatureUnavailable;
            if (!gained.empty()) {
                if (created.size() != 1 || created.front().size() != 1 || created.front().front() == 0 ||
                    created.front().front() == kInvalidGuid) return kEquipInternalError;
                applied.equipmentItemId = created.front().front();
            }
            ClientRequestLedgerComp next = ledger;
            next.set_forge_revision(applied.forgeRevision);
            if (!client_request_ledger::Remember(next,request.client_request_id(),ClientRequestKind::kEquipmentCraft,fingerprint,applied) ||
                !client_request_ledger::Validate(next).empty()) return kEquipInternalError;
            ledger.Swap(&next);
            return kSuccess;
        });
    if (error != kSuccess) { reject(error); return; }
    if (!replacements.empty() && prepared.holder == kEquipment) {
        PlayerAttributeSystem::Recalculate(player,PlayerAttributeSystem::RecalcReason::kEquipmentChanged);
        PlayerAttributeSystem::PushPanel(player);
    }
    FillCraftResult(player,applied,false,response);
}

void PlayerEquipmentV1System::SetPersistForTest(PersistFn fn) {
    persistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}
