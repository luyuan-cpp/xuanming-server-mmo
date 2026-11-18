// 通过公开打造、预览、属性收集和存盘接缝验证，不调用生产私有实现。
#include <gtest/gtest.h>

#include <array>
#include <cstdint>
#include <functional>
#include <limits>
#include <memory>
#include <set>
#include <string>
#include <unordered_set>
#include <vector>

#include "core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/bag_marshal.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_equip.h"
#include "player/system/player_equipment_v1.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_gain_block_comp.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixrule_table.h"
#include "table/code/equipartifact_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipattributecap_table.h"
#include "table/code/equipenhance_table.h"
#include "table/code/equipgem_table.h"
#include "table/code/equipgemsocket_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/equipv1affix_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
constexpr Guid kCraftPlayer = 848484;
constexpr Guid kMaterialGuid = (uint64_t{1} << 60) + 48001;
constexpr uint32_t kMaterial = 3001;
int persistRequests = 0;
bool persistAccepted = false;

bool CapturePersistRequest(entt::entity) {
    ++persistRequests;
    return persistAccepted;
}

bool ArmCraftItemSegment(uint64_t first = 4800000, uint64_t last = 4900000) {
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false;
    options.kindName = "item";
    options.initialStep = 100000;
    if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
        [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
    client.Warm();
    client.OnResponse(0, first, last);
    return client.IsReady();
}

EquipmentCraftChoice CreateChoice(uint32_t config) {
    EquipmentCraftChoice choice;
    choice.set_operation(EquipmentCraftChoice::CREATE_ITEM);
    choice.set_config_id(config);
    return choice;
}

EquipmentCraftChoice InstallChoice(Guid item, uint32_t slot, uint32_t affix) {
    EquipmentCraftChoice choice;
    choice.set_operation(EquipmentCraftChoice::INSTALL_AFFIX);
    choice.set_item_id(item);
    choice.set_affix_slot(slot);
    choice.set_affix_id(affix);
    return choice;
}

// 读取公开战斗快照字段，检查词条池的配置效果确实进入对应数值。
uint32_t EffectValue(const EquipmentEffects& effects, uint32_t effect, uint32_t parameter) {
    switch (effect) {
    case 1: return effects.physical_attack_bps();
    case 2: return effects.magic_attack_bps();
    case 3: return effects.max_health_bps();
    case 4: return effects.speed_bps();
    case 5: return effects.outgoing_heal_bps();
    case 6: return effects.shield_capacity_bps();
    case 7: return effects.physical_crit_bps();
    case 8: return effects.magic_crit_bps();
    case 9: return effects.physical_resist_bps();
    case 10: return effects.magic_resist_bps();
    case 11: {
        const auto found = effects.ailment_hit_bps().find(parameter);
        return found == effects.ailment_hit_bps().end() ? 0u : found->second;
    }
    case 12: {
        const auto found = effects.ailment_resist_bps().find(parameter);
        return found == effects.ailment_resist_bps().end() ? 0u : found->second;
    }
    case 13: {
        const auto found = effects.mana_reduction_bps().find(parameter);
        return found == effects.mana_reduction_bps().end() ? 0u : found->second;
    }
    default: return 0;
    }
}

class PlayerEquipmentCraftTest : public testing::Test {
protected:
    static void LoadTables() {
        ItemTableManager::Instance().Load();
        EquipV1AffixTableManager::Instance().Load();
        EquipArtifactTableManager::Instance().Load();
        EquipSlotTableManager::Instance().Load();
        EquipAttributeTableManager::Instance().Load();
        EquipAttributeCapTableManager::Instance().Load();
        EquipAffixPoolTableManager::Instance().Load();
        EquipAffixRuleTableManager::Instance().Load();
        EquipEnhanceTableManager::Instance().Load();
        EquipGemTableManager::Instance().Load();
        EquipGemSocketTableManager::Instance().Load();
        ClassTableManager::Instance().Load();
        AttributeDimensionTableManager::Instance().Load();
        AttributePoolTableManager::Instance().Load();
        AttributeRuleTableManager::Instance().Load();
        AttributeAllocRatioTableManager::Instance().Load();
    }

    void CreatePlayer() {
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kCraftPlayer);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 9;
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(kCraftPlayer);
        tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerItemGainBlockComp>(player);
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(80);
        tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player);
        auto& base = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player);
        base.set_health(100);
        base.set_mana(50);
        tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(player);
        tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player);
        auto& currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        for (uint32_t kind = 0; kind < kCurrencyMax; ++kind) currency.add_values(0);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        ASSERT_TRUE(tlsEcs.playerList.emplace(kCraftPlayer, player).second);
    }

    void SetUp() override {
        LoadTables();
        ASSERT_TRUE(ArmCraftItemSegment());
        originalItemBlocks = GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kItem);
        for (const auto id : originalItemBlocks) GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, id);
        BagService::SetItemInstanceInitializer({});
        PlayerEquipSystem::SetRandomSourceForTest({});
        persistRequests = 0;
        persistAccepted = false;
        PlayerEquipmentV1System::SetPersistForTest(&CapturePersistRequest);
        ASSERT_NO_FATAL_FAILURE(CreatePlayer());
    }

    void TearDown() override {
        PlayerEquipmentV1System::SetPersistForTest(nullptr);
        BagService::SetItemInstanceInitializer({});
        PlayerEquipSystem::SetRandomSourceForTest({});
        tlsEcs.playerList.erase(kCraftPlayer);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        const auto current = GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kItem);
        for (const auto id : current) GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, id);
        for (const auto id : originalItemBlocks) GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, id);
        // 热更用例只改内存行，结束时恢复正式快照，不影响其他测试。
        LoadTables();
        EXPECT_TRUE(ArmCraftItemSegment(1, uint64_t{1} << 54));
    }

    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    Bag& Equipment() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kEquipment]; }
    ClientRequestLedgerComp& Ledger() { return tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player); }

    void Stock(uint32_t count) {
        Inventory().InsertItemForRestore(kMaterialGuid, kMaterial, count, 0);
        ASSERT_EQ(count, Inventory().GetTotalItemCount(kMaterial));
    }

    std::array<std::string, 3> Assets() {
        BagAllData saved;
        bag_marshal::Marshal(player, saved);
        return {saved.SerializeAsString(), Ledger().SerializeAsString(),
            tlsEcs.actorRegistry.get<CurrencyComp>(player).SerializeAsString()};
    }

    void MakeRequest(const EquipmentCraftChoice& choice, CommitEquipmentCraftRequest& request) {
        GetEquipmentCraftRulesResponse rules;
        ASSERT_EQ(kSuccess, PlayerEquipmentV1System::GetRules(player, rules));
        request.Clear();
        *request.mutable_choice() = choice;
        request.set_rules_fingerprint(rules.rules_fingerprint());
        request.set_expected_forge_revision(rules.forge_revision());
        request.set_client_request_id(++nextKey);
    }

    uint32_t Preview(const EquipmentCraftChoice& choice, PreviewEquipmentCraftResponse& response) {
        PreviewEquipmentCraftRequest request;
        *request.mutable_choice() = choice;
        return PlayerEquipmentV1System::Preview(player, request, response);
    }

    CommitEquipmentCraftResponse Commit(const CommitEquipmentCraftRequest& request) {
        CommitEquipmentCraftResponse response;
        PlayerEquipmentV1System::Commit(player, request, response);
        return response;
    }

    void RejectUnchanged(const CommitEquipmentCraftRequest& request) {
        const auto before = Assets();
        const int beforePersist = persistRequests;
        const auto response = Commit(request);
        EXPECT_EQ(CommitEquipmentCraftResponse::REJECTED, response.outcome());
        EXPECT_TRUE(response.has_error_message());
        EXPECT_NE(kSuccess, response.error_message().id());
        EXPECT_FALSE(response.durable());
        EXPECT_EQ(before, Assets());
        EXPECT_EQ(beforePersist, persistRequests);
    }

    void Create(uint32_t config, Guid& id) {
        CommitEquipmentCraftRequest request;
        ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(config), request));
        const auto response = Commit(request);
        ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, response.outcome()) << response.DebugString();
        ASSERT_FALSE(response.has_error_message()) << response.DebugString();
        ASSERT_NE(0u, response.item_id());
        id = response.item_id();
    }

    void Install(Guid id, uint32_t slot, uint32_t affix) {
        CommitEquipmentCraftRequest request;
        ASSERT_NO_FATAL_FAILURE(MakeRequest(InstallChoice(id, slot, affix), request));
        const auto response = Commit(request);
        ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, response.outcome()) << response.DebugString();
        ASSERT_FALSE(response.has_error_message()) << response.DebugString();
        EXPECT_EQ(id, response.item_id());
    }

    PlayerAllData CaptureSnapshot() {
        PlayerAllData snapshot;
        auto* db = snapshot.mutable_player_database_data();
        db->set_player_id(kCraftPlayer);
        *db->mutable_client_request_ledger() = Ledger();
        bag_marshal::Marshal(player, *db->mutable_bag_component());
        return snapshot;
    }

    void RecordPersisted(const PlayerAllData& saved, Guid playerId = kCraftPlayer,
                         uint64_t ownerEpoch = 9, uint64_t saveSeq = 17) {
        auto snapshot = std::make_shared<PlayerAllData>(saved);
        snapshot->set_last_save_seq(saveSeq);
        snapshot->mutable_player_database_data()->set_player_id(playerId);
        auto& persisted = tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player);
        persisted.RememberCommit(snapshot, ownerEpoch, saveSeq);
        persisted.Replace(*snapshot);
    }

    void RestartFromSerializedAssets() {
        BagAllData bag;
        bag_marshal::Marshal(player, bag);
        const auto bagBytes = bag.SerializeAsString();
        const auto ledgerBytes = Ledger().SerializeAsString();
        tlsEcs.playerList.erase(kCraftPlayer);
        tlsEcs.actorRegistry.destroy(player);
        ASSERT_NO_FATAL_FAILURE(CreatePlayer());
        BagAllData restored;
        ASSERT_TRUE(restored.ParseFromString(bagBytes));
        bag_marshal::Unmarshal(player, restored);
        ASSERT_TRUE(Ledger().ParseFromString(ledgerBytes));
        ASSERT_TRUE(client_request_ledger::Validate(Ledger()).empty());
    }

    entt::entity player{entt::null};
    uint64_t nextKey{700000};
    std::unordered_set<uint32_t> originalItemBlocks;
};

TEST_F(PlayerEquipmentCraftTest, RulesExposeFiftyThreeRecipesAndThirtySixChoicesWithoutMutation) {
    const auto before = Assets();
    GetEquipmentCraftRulesResponse rules;
    ASSERT_EQ(kSuccess, PlayerEquipmentV1System::GetRules(player, rules));
    EXPECT_EQ(53, rules.recipes_size());
    EXPECT_EQ(36, rules.affixes_size());
    EXPECT_EQ(kClientRequestFingerprintBytes, rules.rules_fingerprint().size());
    std::array<int, 4> profiles{}, pools{};
    std::set<uint32_t> configs;
    for (const auto& recipe : rules.recipes()) {
        ASSERT_LT(recipe.product().equip_profile(), profiles.size());
        ++profiles[recipe.product().equip_profile()];
        EXPECT_TRUE(configs.insert(recipe.product().config_id()).second);
        EXPECT_EQ(kMaterial, recipe.material_config_id());
        EXPECT_EQ(0u, recipe.product().item_id());
    }
    for (const auto& affix : rules.affixes()) {
        ASSERT_LT(affix.pool(), pools.size());
        ++pools[affix.pool()];
        EXPECT_EQ(kMaterial, affix.material_config_id());
        EXPECT_EQ(10u, affix.material_count());
    }
    EXPECT_EQ((std::array<int, 4>{0, 36, 9, 8}), profiles);
    EXPECT_EQ((std::array<int, 4>{0, 11, 14, 11}), pools);
    EXPECT_EQ(before, Assets());
    EXPECT_EQ(0, persistRequests);
}

TEST_F(PlayerEquipmentCraftTest, CreateConsumesExactlyTwentyAndPublishesOneInitializedInstance) {
    ASSERT_NO_FATAL_FAILURE(Stock(25));
    const auto before = Assets();
    PreviewEquipmentCraftResponse preview;
    ASSERT_EQ(kSuccess, Preview(CreateChoice(3101), preview));
    ASSERT_TRUE(preview.preview().available());
    EXPECT_EQ(20u, preview.preview().material().count());
    EXPECT_EQ(25u, preview.preview().material().available());
    EXPECT_EQ(before, Assets());
    Guid id{};
    ASSERT_NO_FATAL_FAILURE(Create(3101, id));
    const auto* item = Inventory().GetItemCompByGuid(id);
    ASSERT_NE(nullptr, item);
    EXPECT_TRUE(item->has_equip());
    EXPECT_TRUE(item->equip().affixes().empty());
    EXPECT_TRUE(item->equip().fixed_affixes().empty());
    EXPECT_EQ(5u, Inventory().GetTotalItemCount(kMaterial));
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(3101));
    EXPECT_EQ(1u, Ledger().forge_revision());
    EXPECT_EQ(1, Ledger().records_size());
    EXPECT_EQ(id, Ledger().records(0).equipment_item_id());
    EXPECT_EQ(1, persistRequests);
}

TEST_F(PlayerEquipmentCraftTest, InsufficientMaterialsRejectWithoutClampingOrReceipt) {
    ASSERT_NO_FATAL_FAILURE(Stock(19));
    PreviewEquipmentCraftResponse preview;
    ASSERT_EQ(kSuccess, Preview(CreateChoice(3101), preview));
    EXPECT_FALSE(preview.preview().available());
    CommitEquipmentCraftRequest request;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), request));
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
    EXPECT_EQ(19u, Inventory().GetTotalItemCount(kMaterial));
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(3101));
    EXPECT_TRUE(Ledger().records().empty());
}

TEST_F(PlayerEquipmentCraftTest, FullInventoryDoesNotUseTemporaryStorageOrConsumeMaterials) {
    Inventory().SetCapacityForRestore(1);
    ASSERT_NO_FATAL_FAILURE(Stock(21));
    PreviewEquipmentCraftResponse preview;
    ASSERT_EQ(kSuccess, Preview(CreateChoice(3101), preview));
    EXPECT_FALSE(preview.preview().available());
    CommitEquipmentCraftRequest request;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), request));
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
    EXPECT_EQ(0u, tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kTemporary].OccupiedGridCount());
}

TEST_F(PlayerEquipmentCraftTest, ConsumingLastMaterialStackMakesRoomForProduct) {
    Inventory().SetCapacityForRestore(1);
    ASSERT_NO_FATAL_FAILURE(Stock(20));
    PreviewEquipmentCraftResponse preview;
    ASSERT_EQ(kSuccess, Preview(CreateChoice(3101), preview));
    ASSERT_TRUE(preview.preview().available());
    Guid id{};
    ASSERT_NO_FATAL_FAILURE(Create(3101, id));
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kMaterial));
    EXPECT_EQ(1u, Inventory().OccupiedGridCount());
    EXPECT_TRUE(Inventory().ContainsGuid(id));
}

TEST_F(PlayerEquipmentCraftTest, GlobalAndPlayerGainBlocksRejectBeforeAnyAssetMutation) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    for (bool personal : {false, true}) {
        SCOPED_TRACE(personal);
        if (personal) {
            ASSERT_EQ(kSuccess, GainBlockService::BlockPlayerItemGain(player, 3101));
        } else {
            GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, 3101);
        }
        PreviewEquipmentCraftResponse preview;
        ASSERT_EQ(kSuccess, Preview(CreateChoice(3101), preview));
        EXPECT_FALSE(preview.preview().available());
        CommitEquipmentCraftRequest request;
        ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), request));
        ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
        if (personal) {
            ASSERT_EQ(kSuccess, GainBlockService::UnblockPlayerItemGain(player, 3101));
        } else {
            GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, 3101);
        }
    }
}

TEST_F(PlayerEquipmentCraftTest, MissingIdSegmentLeavesMaterialsLayoutAndLedgerUnchanged) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    CommitEquipmentCraftRequest request;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), request));
    tlsGuidSegmentRegistry.Get(GuidKind::kItem).Reset();
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
}

TEST_F(PlayerEquipmentCraftTest, ExactExchangeCallbackRejectionDoesNotPublishCandidateOrReceipts) {
    ASSERT_NO_FATAL_FAILURE(Stock(20));
    InitItemParam item;
    item.itemPBComp.set_item_id(kInvalidGuid);
    item.itemPBComp.set_config_id(3101);
    item.itemPBComp.set_size(1);
    item.itemPBComp.mutable_equip();
    std::vector<GuidVector> written{{Guid{999}}};
    const auto previousWritten = written;
    const auto before = Assets();
    int callbacks = 0;
    EXPECT_EQ(kFeatureUnavailable, BagService::ExchangeInventoryExact(player, {{kMaterial, 20}},
        {item}, {}, 777, "{}", written, [&](const std::vector<GuidVector>& candidates) {
            ++callbacks;
            EXPECT_EQ(1u, candidates.size());
            return uint32_t{kFeatureUnavailable};
        }));
    EXPECT_EQ(1, callbacks);
    EXPECT_EQ(before, Assets());
    EXPECT_EQ(previousWritten, written);
    EXPECT_EQ(0, persistRequests);
}

TEST_F(PlayerEquipmentCraftTest, FullSetEightInstancesAndTwelveAffixesCostsExactlyThreeHundred) {
    ASSERT_NO_FATAL_FAILURE(Stock(300));
    std::array<Guid, 8> ids{};
    for (uint32_t i = 0; i < 4; ++i) {
        ASSERT_NO_FATAL_FAILURE(Create(3101 + i, ids[i]));
        ASSERT_NO_FATAL_FAILURE(Install(ids[i], 1, 101));
        ASSERT_NO_FATAL_FAILURE(Install(ids[i], 2, 201));
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, ids[i], 3 + i));
    }
    for (uint32_t i = 4; i < 6; ++i) {
        ASSERT_NO_FATAL_FAILURE(Create(3201, ids[i]));
        ASSERT_NO_FATAL_FAILURE(Install(ids[i], 1, 301));
        ASSERT_NO_FATAL_FAILURE(Install(ids[i], 2, 302));
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, ids[i], 3 + i));
    }
    for (uint32_t i = 6; i < 8; ++i) {
        ASSERT_NO_FATAL_FAILURE(Create(3301, ids[i]));
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, ids[i], 3 + i));
    }
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kMaterial));
    EXPECT_EQ(8u, Equipment().OccupiedGridCount());
    EXPECT_EQ(8u, std::set<Guid>(ids.begin(), ids.end()).size());
    EXPECT_EQ(20u, Ledger().forge_revision());
    uint32_t installed = 0;
    Equipment().ForEachItem([&](Guid, const ItemComp& item) { installed += item.equip().fixed_affixes_size(); });
    EXPECT_EQ(12u, installed);
    EquipmentEffects effects;
    PlayerEquipmentV1System::CollectEffects(player, effects);
    EXPECT_EQ(3200u, effects.physical_attack_bps());
    EXPECT_EQ(800u, effects.physical_crit_bps());
    EXPECT_EQ(800u, effects.max_health_bps());
    EXPECT_EQ(200u, effects.speed_bps());
    ASSERT_EQ(2, effects.artifacts_size());
    EXPECT_NE(effects.artifacts(0).item_id(), effects.artifacts(1).item_id());
    for (const auto& artifact : effects.artifacts()) {
        EXPECT_EQ(1u, artifact.effect_id());
        EXPECT_EQ(2000u, artifact.bonus_bps());
        EXPECT_EQ(1u, artifact.per_round_limit());
    }
}

TEST_F(PlayerEquipmentCraftTest, EveryFormalAffixMatchesPreviewInstalledDisplayAndCollectedEffect) {
    ASSERT_NO_FATAL_FAILURE(Stock(2000));
    GetEquipmentCraftRulesResponse rules;
    ASSERT_EQ(kSuccess, PlayerEquipmentV1System::GetRules(player, rules));
    ASSERT_EQ(36, rules.affixes_size());
    for (const auto& affix : rules.affixes()) {
        SCOPED_TRACE(affix.affix_id());
        const bool accessory = affix.pool() == 3;
        Guid id{};
        ASSERT_NO_FATAL_FAILURE(Create(accessory ? 3201 : 3101, id));
        const uint32_t slot = affix.pool() == 2 ? 2 : 1;
        const auto choice = InstallChoice(id, slot, affix.affix_id());
        PreviewEquipmentCraftResponse preview;
        const auto before = Assets();
        ASSERT_EQ(kSuccess, Preview(choice, preview));
        ASSERT_TRUE(preview.preview().available());
        EXPECT_EQ(before, Assets());
        const EquipFixedAffixInfo* selected = nullptr;
        for (const auto& entry : preview.preview().result().fixed_affix_slots())
            if (entry.slot() == slot && entry.has_selected()) selected = &entry.selected();
        ASSERT_NE(nullptr, selected);
        EXPECT_EQ(affix.SerializeAsString(), selected->SerializeAsString());
        ASSERT_NO_FATAL_FAILURE(Install(id, slot, affix.affix_id()));
        BagItemInfo actual;
        ASSERT_NE(nullptr, Inventory().GetItemCompByGuid(id));
        PlayerEquipSystem::FillItemDisplay(*Inventory().GetItemCompByGuid(id), actual);
        const EquipFixedAffixInfo* actualSelected = nullptr;
        for (const auto& entry : actual.fixed_affix_slots())
            if (entry.slot() == slot && entry.has_selected()) actualSelected = &entry.selected();
        ASSERT_NE(nullptr, actualSelected);
        EXPECT_EQ(selected->SerializeAsString(), actualSelected->SerializeAsString());
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, id, accessory ? 7 : 3));
        EquipmentEffects effects;
        PlayerEquipmentV1System::CollectEffects(player, effects);
        EXPECT_EQ(affix.value_bps(), EffectValue(effects, affix.effect(), affix.effect_param()));
    }
}

TEST_F(PlayerEquipmentCraftTest, WrongPoolsInvalidSlotsAndDuplicateAccessoryChoicesCannotConsume) {
    ASSERT_NO_FATAL_FAILURE(Stock(200));
    Guid weapon{}, accessory{}, artifact{};
    ASSERT_NO_FATAL_FAILURE(Create(3101, weapon));
    ASSERT_NO_FATAL_FAILURE(Create(3201, accessory));
    ASSERT_NO_FATAL_FAILURE(Create(3301, artifact));
    const std::array<EquipmentCraftChoice, 7> invalid{
        InstallChoice(weapon, 1, 201), InstallChoice(weapon, 2, 101),
        InstallChoice(accessory, 1, 101), InstallChoice(accessory, 2, 201),
        InstallChoice(weapon, 0, 101), InstallChoice(weapon, 3, 101), InstallChoice(artifact, 1, 301)};
    for (const auto& choice : invalid) {
        CommitEquipmentCraftRequest request;
        ASSERT_NO_FATAL_FAILURE(MakeRequest(choice, request));
        ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
    }
    ASSERT_NO_FATAL_FAILURE(Install(accessory, 1, 301));
    CommitEquipmentCraftRequest duplicate;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(InstallChoice(accessory, 2, 301), duplicate));
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(duplicate));
}

TEST_F(PlayerEquipmentCraftTest, FirstInstallPreservesEnhancementAndGemsAndNeverOverwritesFilledSlot) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    Guid id{};
    ASSERT_NO_FATAL_FAILURE(Create(3101, id));
    auto* item = Inventory().GetItemCompByGuid(id);
    ASSERT_NE(nullptr, item);
    item->mutable_equip()->set_enhance_level(5);
    auto* gem = item->mutable_equip()->add_gems();
    gem->set_socket(0); gem->set_gem_config_id(2101);
    ASSERT_NO_FATAL_FAILURE(Install(id, 1, 101));
    CommitEquipmentCraftRequest replacement;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(InstallChoice(id, 1, 102), replacement));
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(replacement));
    item = Inventory().GetItemCompByGuid(id);
    ASSERT_NE(nullptr, item);
    EXPECT_EQ(5u, item->equip().enhance_level());
    ASSERT_EQ(1, item->equip().gems_size());
    EXPECT_EQ(2101u, item->equip().gems(0).gem_config_id());
    ASSERT_EQ(1, item->equip().fixed_affixes_size());
    EXPECT_EQ(101u, item->equip().fixed_affixes(0).affix_id());
}

TEST_F(PlayerEquipmentCraftTest, InstallingOnEquippedInstanceChangesEffectWithoutMovingItsSlot) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    Guid id{};
    ASSERT_NO_FATAL_FAILURE(Create(3101, id));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, id, 3));
    ASSERT_NO_FATAL_FAILURE(Install(id, 1, 103));
    EXPECT_EQ(id, Equipment().Layout().At(3));
    EXPECT_FALSE(Inventory().ContainsGuid(id));
    EXPECT_EQ(70u, Inventory().GetTotalItemCount(kMaterial));
    EquipmentEffects effects;
    PlayerEquipmentV1System::CollectEffects(player, effects);
    EXPECT_EQ(800u, effects.outgoing_heal_bps());
    ASSERT_TRUE(effects.support_sources_complete());
    ASSERT_EQ(1, effects.support_sources_size());
    const auto& source = effects.support_sources(0);
    EXPECT_EQ(id, source.item_id()); EXPECT_EQ(103, source.affix_id());
    EXPECT_EQ(1, source.effect_kind()); EXPECT_EQ(800, source.raw_bonus_bps());
    EXPECT_EQ(effects.outgoing_heal_bps(), source.effective_bonus_bps());
    EXPECT_EQ(3200, source.source_cap_bps());
}

TEST_F(PlayerEquipmentCraftTest, SameKeyReplaysFirstIdentityAndDifferentParametersAreRejected) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    CommitEquipmentCraftRequest original;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), original));
    const auto first = Commit(original);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, first.outcome());
    Guid other{};
    ASSERT_NO_FATAL_FAILURE(Create(3102, other));
    const auto before = Assets();
    const auto replay = Commit(original);
    EXPECT_EQ(CommitEquipmentCraftResponse::APPLIED, replay.outcome());
    EXPECT_TRUE(replay.replayed());
    EXPECT_EQ(first.item_id(), replay.item_id());
    EXPECT_EQ(1u, replay.result_forge_revision());
    EXPECT_EQ(2u, replay.current_forge_revision());
    EXPECT_EQ(before, Assets());
    auto mismatch = original;
    mismatch.mutable_choice()->set_config_id(3103);
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(mismatch));
}

TEST_F(PlayerEquipmentCraftTest, AttributeSemanticHotReloadInvalidatesConfirmationEvenWhenDisplayIsUnchanged) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    CommitEquipmentCraftRequest request;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), request));
    GetEquipmentCraftRulesResponse before;
    ASSERT_EQ(kSuccess, PlayerEquipmentV1System::GetRules(player, before));
    auto* attribute = const_cast<EquipAttributeTable*>(
        EquipAttributeTableManager::Instance().FindByIdSilent(1).first);
    ASSERT_NE(nullptr, attribute);
    ASSERT_EQ(4u, attribute->effect());
    // 显示名和数值不变，只把双伤害语义改成物伤；确认必须受原始配置摘要保护。
    attribute->set_effect(3);
    attribute->set_effect_param(3);
    GetEquipmentCraftRulesResponse after;
    ASSERT_EQ(kSuccess, PlayerEquipmentV1System::GetRules(player, after));
    EXPECT_NE(before.rules_fingerprint(), after.rules_fingerprint());
    before.clear_rules_fingerprint();
    after.clear_rules_fingerprint();
    EXPECT_EQ(before.SerializeAsString(), after.SerializeAsString());
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
}

TEST_F(PlayerEquipmentCraftTest, InvalidArtifactLimitsRejectRulesPreviewAndCommitWithoutSpending) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    GetEquipmentCraftRulesResponse originalRules;
    ASSERT_EQ(kSuccess, PlayerEquipmentV1System::GetRules(player, originalRules));
    struct InvalidLimit {
        uint32_t artifact;
        void (EquipArtifactTable::*setValue)(uint32_t);
    };
    const std::array<InvalidLimit, 10> invalidLimits{{
        {6, &EquipArtifactTable::set_cap_bps},
        {6, &EquipArtifactTable::set_active_rounds},
        {4, &EquipArtifactTable::set_health_threshold_bps},
        {1, &EquipArtifactTable::set_per_round_limit},
        {2, &EquipArtifactTable::set_per_round_limit},
        {3, &EquipArtifactTable::set_per_round_limit},
        {4, &EquipArtifactTable::set_per_round_limit},
        {5, &EquipArtifactTable::set_per_round_limit},
        {7, &EquipArtifactTable::set_per_round_limit},
        {8, &EquipArtifactTable::set_per_round_limit},
    }};
    for (const auto& invalid : invalidLimits) {
        SCOPED_TRACE(invalid.artifact);
        auto* artifact = const_cast<EquipArtifactTable*>(
            EquipArtifactTableManager::Instance().FindByIdSilent(invalid.artifact).first);
        ASSERT_NE(nullptr, artifact);
        const auto original = *artifact;
        const auto choice = CreateChoice(3300 + invalid.artifact);
        CommitEquipmentCraftRequest request;
        ASSERT_NO_FATAL_FAILURE(MakeRequest(choice, request));
        const auto before = Assets();
        (artifact->*invalid.setValue)(0);
        GetEquipmentCraftRulesResponse rejectedRules;
        EXPECT_EQ(kInvalidTableData, PlayerEquipmentV1System::GetRules(player, rejectedRules));
        EXPECT_TRUE(rejectedRules.recipes().empty());
        PreviewEquipmentCraftResponse rejectedPreview;
        EXPECT_EQ(kInvalidTableData, Preview(choice, rejectedPreview));
        EXPECT_FALSE(rejectedPreview.has_preview());
        EXPECT_EQ(before, Assets());
        ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
        // 配置恢复后，同一公开查询与预览重新可用，失败没有留下业务水位。
        *artifact = original;
        GetEquipmentCraftRulesResponse restoredRules;
        ASSERT_EQ(kSuccess, PlayerEquipmentV1System::GetRules(player, restoredRules));
        EXPECT_EQ(originalRules.rules_fingerprint(), restoredRules.rules_fingerprint());
        PreviewEquipmentCraftResponse restoredPreview;
        ASSERT_EQ(kSuccess, Preview(choice, restoredPreview));
        EXPECT_TRUE(restoredPreview.preview().available());
        EXPECT_EQ(before, Assets());
    }
    EXPECT_EQ(0, persistRequests);
    Guid id{};
    ASSERT_NO_FATAL_FAILURE(Create(3306, id));
    EXPECT_EQ(60u, Inventory().GetTotalItemCount(kMaterial));
    EXPECT_EQ(1u, Ledger().forge_revision());
    EXPECT_TRUE(Inventory().ContainsGuid(id));
}

TEST_F(PlayerEquipmentCraftTest, EvictedReceiptWithOldRevisionIsUnknownAndCannotCreateAgain) {
    ASSERT_NO_FATAL_FAILURE(Stock(700));
    CommitEquipmentCraftRequest original;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), original));
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, Commit(original).outcome());
    for (int i = 0; i < client_request_ledger::kCapacity; ++i) {
        Guid id{};
        ASSERT_NO_FATAL_FAILURE(Create(3101, id));
    }
    ASSERT_EQ(client_request_ledger::kCapacity, Ledger().records_size());
    for (const auto& entry : Ledger().records()) ASSERT_NE(original.client_request_id(), entry.key());
    const auto before = Assets();
    const auto response = Commit(original);
    EXPECT_EQ(CommitEquipmentCraftResponse::UNKNOWN, response.outcome());
    EXPECT_FALSE(response.durable());
    EXPECT_FALSE(response.replayed());
    EXPECT_EQ(before, Assets());
    EXPECT_EQ(33u, Inventory().GetTotalItemCount(3101));
    EXPECT_EQ(33u, Ledger().forge_revision());
}

TEST_F(PlayerEquipmentCraftTest, MarshalRestartPreservesCreationAndInstallReceiptsAndFixedAffix) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    CommitEquipmentCraftRequest create;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), create));
    const auto created = Commit(create);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, created.outcome());
    CommitEquipmentCraftRequest install;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(InstallChoice(created.item_id(), 1, 101), install));
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, Commit(install).outcome());
    const auto before = Assets();
    ASSERT_NO_FATAL_FAILURE(RestartFromSerializedAssets());
    EXPECT_EQ(before, Assets());
    auto replay = Commit(create);
    EXPECT_TRUE(replay.replayed());
    EXPECT_FALSE(replay.durable());
    EXPECT_EQ(created.item_id(), replay.item_id());
    EXPECT_EQ(1u, replay.result_forge_revision());
    replay = Commit(install);
    EXPECT_TRUE(replay.replayed());
    EXPECT_EQ(2u, replay.result_forge_revision());
    ASSERT_NE(nullptr, Inventory().GetItemCompByGuid(created.item_id()));
    ASSERT_EQ(1, Inventory().GetItemCompByGuid(created.item_id())->equip().fixed_affixes_size());
    EXPECT_EQ(101u, Inventory().GetItemCompByGuid(created.item_id())->equip().fixed_affixes(0).affix_id());
    RecordPersisted(CaptureSnapshot());
    EXPECT_TRUE(Commit(install).durable());
    EXPECT_EQ(before, Assets());
}

TEST_F(PlayerEquipmentCraftTest, SaturatedBusinessRevisionOrReceiptSequenceRejectsWithoutWrapping) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    for (bool business : {true, false}) {
        SCOPED_TRACE(business);
        Ledger().Clear();
        if (business) Ledger().set_forge_revision(std::numeric_limits<uint64_t>::max());
        else Ledger().set_next_seq(std::numeric_limits<uint64_t>::max());
        CommitEquipmentCraftRequest request;
        ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), request));
        PreviewEquipmentCraftResponse preview;
        ASSERT_EQ(kSuccess, Preview(CreateChoice(3101), preview));
        EXPECT_FALSE(preview.preview().available());
        ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
    }
}

TEST_F(PlayerEquipmentCraftTest, AppliedIsDurableOnlyAfterMatchingConfirmedSnapshot) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    CommitEquipmentCraftRequest first;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3101), first));
    const auto applied = Commit(first);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, applied.outcome());
    EXPECT_FALSE(applied.durable());
    EXPECT_EQ(1, persistRequests);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerLastPersistedSnapshotComp>(player));
    const auto firstSnapshot = CaptureSnapshot();
    // 调度存盘成功也不是提交证据，必须等待确认快照。
    persistAccepted = true;
    CommitEquipmentCraftRequest second;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3102), second));
    const auto later = Commit(second);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, later.outcome());
    EXPECT_FALSE(later.durable());
    EXPECT_EQ(2, persistRequests);
    const auto before = Assets();
    const auto secondSnapshot = CaptureSnapshot();
    RecordPersisted(secondSnapshot, kCraftPlayer + 1);
    EXPECT_FALSE(Commit(second).durable());
    RecordPersisted(firstSnapshot);
    EXPECT_FALSE(Commit(second).durable());
    RecordPersisted(secondSnapshot, kCraftPlayer, 8, 17);
    auto unconfirmed = Commit(second);
    EXPECT_EQ(CommitEquipmentCraftResponse::APPLIED, unconfirmed.outcome());
    EXPECT_FALSE(unconfirmed.durable());
    RecordPersisted(secondSnapshot, kCraftPlayer, 9, 0);
    unconfirmed = Commit(second);
    EXPECT_EQ(CommitEquipmentCraftResponse::APPLIED, unconfirmed.outcome());
    EXPECT_FALSE(unconfirmed.durable());
    // 只有快照内容、没有提交钩子的证明，不得把内存成功误报为落盘成功。
    tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player).Replace(secondSnapshot);
    unconfirmed = Commit(second);
    EXPECT_EQ(CommitEquipmentCraftResponse::APPLIED, unconfirmed.outcome());
    EXPECT_FALSE(unconfirmed.durable());
    RecordPersisted(secondSnapshot);
    EXPECT_TRUE(Commit(second).durable());
    EXPECT_TRUE(Commit(first).durable());
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 0;
    EXPECT_FALSE(Commit(second).durable());
    EXPECT_EQ(before, Assets());
}

TEST_F(PlayerEquipmentCraftTest, ZeroKeyStaleRulesLowLevelAndFrozenOrBattleStateCannotSpend) {
    ASSERT_NO_FATAL_FAILURE(Stock(100));
    CommitEquipmentCraftRequest request;
    ASSERT_NO_FATAL_FAILURE(MakeRequest(CreateChoice(3181), request));
    auto invalid = request;
    invalid.set_client_request_id(0);
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(invalid));
    invalid = request;
    invalid.set_rules_fingerprint(std::string(kClientRequestFingerprintBytes, 'x'));
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(invalid));
    tlsEcs.actorRegistry.get<LevelComp>(player).set_level(1);
    PreviewEquipmentCraftResponse preview;
    ASSERT_EQ(kSuccess, Preview(CreateChoice(3181), preview));
    EXPECT_FALSE(preview.preview().available());
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
    tlsEcs.actorRegistry.get<LevelComp>(player).set_level(80);
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
    tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
    tlsEcs.actorRegistry.remove<InBattleComp>(player);
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 0;
    ASSERT_NO_FATAL_FAILURE(RejectUnchanged(request));
}
} // namespace
