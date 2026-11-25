// V1 装备 5 档品相、只升不降打磨保底、重铸继承、防染毒绑定、率土式分卷宗技能制书与主城原子代打单测。
#include <gtest/gtest.h>

#include <array>
#include <cstdint>
#include <functional>
#include <string>
#include <vector>

#include "core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/client_request_ledger.h"
#include "player/system/equip_craft_quality_rules.h"
#include "player/system/player_craft.h"
#include "player/system/player_equip.h"
#include "player/system/player_equipment_v1.h"
#include "player/system/player_pet.h"
#include "player/system/player_skill.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_craft_comp.pb.h"
#include "proto/common/component/player_item_gain_block_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixquality_table.h"
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
#include "table/code/pet_table.h"
#include "table/code/petrule_table.h"
#include "table/code/skill_table.h"
#include "table/code/skillscrollcraft_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
constexpr Guid kBuyerId = 910001;
constexpr Guid kMasterId = 910002;
constexpr uint32_t kForgeMat = 3001;

bool ArmItemSegment(uint64_t first = 5800000, uint64_t last = 5900000) {
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

uint32_t gForcedQualityRoll = 0;
uint32_t FixedQualityRoll(uint32_t) { return gForcedQualityRoll; }

uint32_t gForcedCraftRoll = 0;
uint32_t FixedCraftRoll(uint32_t) { return gForcedCraftRoll; }

bool DummyPersist(entt::entity) { return true; }

class PlayerCraftQualityTest : public testing::Test {
protected:
    static void LoadAllTables() {
        ItemTableManager::Instance().Load();
        EquipV1AffixTableManager::Instance().Load();
        EquipAffixQualityTableManager::Instance().Load();
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
        SkillTableManager::Instance().Load();
        SkillScrollCraftTableManager::Instance().Load();
        PetTableManager::Instance().Load();
        PetRuleTableManager::Instance().Load();
        PlayerEquipmentV1System::InvalidateRulesCacheForTest();
    }

    entt::entity SpawnPlayer(Guid playerId, const std::string& name, uint64_t gold) {
        const auto entity = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, playerId);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(entity).epoch = 5;
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(playerId);
        tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(entity);
        tlsEcs.actorRegistry.emplace<PlayerCraftComp>(entity);
        tlsEcs.actorRegistry.emplace<PlayerItemGainBlockComp>(entity);
        tlsEcs.actorRegistry.emplace<LevelComp>(entity).set_level(80);
        auto& uint32s = tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(entity);
        uint32s.set_class_(1);
        tlsEcs.actorRegistry.emplace<PlayerProfileComp>(entity).set_name(name);
        tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(entity);
        tlsEcs.actorRegistry.emplace<PlayerPetComp>(entity);
        auto& base = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(entity);
        base.set_health(1000);
        base.set_mana(500);
        tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(entity);
        tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(entity);
        auto& currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
        for (uint32_t kind = 0; kind < kCurrencyMax; ++kind) currency.add_values(0);
        currency.set_values(kCurrencyGold, gold);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
        EXPECT_TRUE(tlsEcs.playerList.emplace(playerId, entity).second);
        return entity;
    }

    void SetUp() override {
        LoadAllTables();
        ASSERT_TRUE(ArmItemSegment());
        PlayerEquipmentV1System::SetPersistForTest(&DummyPersist);
        PlayerEquipmentV1System::SetQualityRollForTest(&FixedQualityRoll);
        PlayerCraftSystem::SetCraftRollForTest(&FixedCraftRoll);
        PlayerCraftSystem::SetCraftUnlockGateForTest(false);
        gForcedQualityRoll = 0;
        gForcedCraftRoll = 0;
        buyer = SpawnPlayer(kBuyerId, "侠客甲", 50000);
        master = SpawnPlayer(kMasterId, "欧冶子", 1000);
    }

    void TearDown() override {
        PlayerEquipmentV1System::SetPersistForTest(nullptr);
        PlayerEquipmentV1System::SetQualityRollForTest(nullptr);
        PlayerCraftSystem::SetCraftRollForTest(nullptr);
        PlayerCraftSystem::SetCraftUnlockGateForTest(false);
        for (const auto id : {kBuyerId, kMasterId}) tlsEcs.playerList.erase(id);
        for (const auto e : {buyer, master}) {
            if (tlsEcs.actorRegistry.valid(e)) tlsEcs.actorRegistry.destroy(e);
        }
        LoadAllTables();
    }

    Bag& InventoryOf(entt::entity e) { return tlsEcs.actorRegistry.get<PlayerBagsComp>(e).bags[kInventory]; }

    void GrantItem(entt::entity e, uint32_t configId, uint32_t count, bool bound) {
        InitItemParam param;
        param.itemPBComp.set_item_id(kInvalidGuid);
        param.itemPBComp.set_config_id(configId);
        param.itemPBComp.set_size(count);
        param.itemPBComp.set_bound(bound);
        ASSERT_EQ(kSuccess, BagService::AddItem(e, InventoryOf(e), param));
    }

    CommitEquipmentCraftResponse CommitEquip(entt::entity e, const EquipmentCraftChoice& choice, uint64_t keyOverride = 0) {
        GetEquipmentCraftRulesResponse rules;
        EXPECT_EQ(kSuccess, PlayerEquipmentV1System::GetRules(e, rules));
        CommitEquipmentCraftRequest req;
        *req.mutable_choice() = choice;
        req.set_rules_fingerprint(rules.rules_fingerprint());
        req.set_expected_forge_revision(rules.forge_revision());
        req.set_client_request_id(keyOverride != 0 ? keyOverride : ++nextKey);
        CommitEquipmentCraftResponse resp;
        PlayerEquipmentV1System::Commit(e, req, resp);
        return resp;
    }

    Guid ForgeAndInstall(entt::entity e, uint32_t equipConfigId, uint32_t slot, uint32_t affixId,
                         uint32_t forcedRollBps) {
        EquipmentCraftChoice create;
        create.set_operation(EquipmentCraftChoice::CREATE_ITEM);
        create.set_config_id(equipConfigId);
        const auto created = CommitEquip(e, create);
        EXPECT_EQ(CommitEquipmentCraftResponse::APPLIED, created.outcome()) << created.DebugString();

        gForcedQualityRoll = forcedRollBps;
        EquipmentCraftChoice install;
        install.set_operation(EquipmentCraftChoice::INSTALL_AFFIX);
        install.set_item_id(created.item_id());
        install.set_affix_slot(slot);
        install.set_affix_id(affixId);
        const auto installed = CommitEquip(e, install);
        EXPECT_EQ(CommitEquipmentCraftResponse::APPLIED, installed.outcome()) << installed.DebugString();
        return created.item_id();
    }

    entt::entity buyer{entt::null};
    entt::entity master{entt::null};
    uint64_t nextKey{900000};
};

TEST(EquipCraftQualityRulesTest, PureScalingPityMasteryAndProxyTaxAreExact) {
    // 800/400/300/200 乘 80/85/90/95/100 全部精确整除。
    EXPECT_EQ(640u, equipcraft::ScaleValueByQuality(800, 80));
    EXPECT_EQ(680u, equipcraft::ScaleValueByQuality(800, 85));
    EXPECT_EQ(720u, equipcraft::ScaleValueByQuality(800, 90));
    EXPECT_EQ(760u, equipcraft::ScaleValueByQuality(800, 95));
    EXPECT_EQ(800u, equipcraft::ScaleValueByQuality(800, 100));
    EXPECT_EQ(255u, equipcraft::ScaleValueByQuality(300, 85));
    EXPECT_EQ(190u, equipcraft::ScaleValueByQuality(200, 95));

    // 旧存档 quality_tier==0 兼容满值 5 档；速度豁免恒满 5 档。
    EXPECT_EQ(5u, equipcraft::EffectiveQualityTier(0, false));
    EXPECT_EQ(5u, equipcraft::EffectiveQualityTier(1, true));
    EXPECT_EQ(2u, equipcraft::EffectiveQualityTier(2, false));

    // 只升不降：3 档掷出 1 档保持 3 档，保底 +1。
    const auto stay = equipcraft::ResolvePolish(3, 2, 1, false);
    EXPECT_EQ(3u, stay.newTier);
    EXPECT_EQ(3u, stay.newPityCount);
    EXPECT_FALSE(stay.guaranteedByPity);

    // 第 8 次硬保底（当前已累计 7 次未极品）：即使掷出 1 档也必达 5 档并归零保底。
    const auto pity = equipcraft::ResolvePolish(3, 7, 1, false);
    EXPECT_EQ(5u, pity.newTier);
    EXPECT_EQ(0u, pity.newPityCount);
    EXPECT_TRUE(pity.guaranteedByPity);

    // 定星保底符直接满 5 档。
    const auto charm = equipcraft::ResolvePolish(1, 0, 1, true);
    EXPECT_EQ(5u, charm.newTier);
    EXPECT_EQ(0u, charm.newPityCount);
    EXPECT_TRUE(charm.guaranteedByPity);

    // 三档熟练度阈值与 5% 摆摊税。
    EXPECT_EQ(1u, equipcraft::MasteryTierForExp(0));
    EXPECT_EQ(1u, equipcraft::MasteryTierForExp(19));
    EXPECT_EQ(2u, equipcraft::MasteryTierForExp(20));
    EXPECT_EQ(3u, equipcraft::MasteryTierForExp(60));
    EXPECT_EQ(5u, equipcraft::ProxyTaxGold(100));
    EXPECT_EQ(95u, equipcraft::ProxyNetGold(100));
    EXPECT_EQ(3u, equipcraft::ProxyTaxGold(50));
    EXPECT_EQ(47u, equipcraft::ProxyNetGold(50));
}

TEST_F(PlayerCraftQualityTest, PolishIsOnlyUpReachesHardPityOnEighthAndPreservesAcrossReforge) {
    GrantItem(buyer, kForgeMat, 100, false);
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 20, false);

    // 首装掷出凡品 1 档（80%）：101 物理增伤满值 800 -> 640。
    const Guid weaponId = ForgeAndInstall(buyer, 3101, 1, 101, 0);
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(buyer, weaponId, 3));
    EquipmentEffects effects;
    PlayerEquipmentV1System::CollectEffects(buyer, effects);
    EXPECT_EQ(640u, effects.physical_attack_bps());

    // 打磨第 1 次掷出 3 档（roll=7000，学徒权重 2500+3500=6000..8200 -> 3 档 90% = 720）。
    gForcedQualityRoll = 7000;
    EquipmentCraftChoice polish;
    polish.set_operation(EquipmentCraftChoice::POLISH_AFFIX);
    polish.set_item_id(weaponId);
    polish.set_affix_slot(1);
    polish.set_expected_polish_pity_count(0);
    const auto p1 = CommitEquip(buyer, polish);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, p1.outcome()) << p1.DebugString();
    EXPECT_EQ(3u, p1.result_quality_tier());
    EXPECT_EQ(1u, p1.result_polish_pity_count());
    EXPECT_FALSE(p1.guaranteed_by_pity());
    PlayerEquipmentV1System::CollectEffects(buyer, effects);
    EXPECT_EQ(720u, effects.physical_attack_bps());

    // 过期 expected_polish_pity_count=0 必须拒绝、不扣砂不扣银两。
    const auto goldBeforeReject = CurrencySystem::GetBalance(buyer, kCurrencyGold);
    const auto stalePity = CommitEquip(buyer, polish);
    EXPECT_EQ(CommitEquipmentCraftResponse::REJECTED, stalePity.outcome());
    EXPECT_EQ(kEquipPolishPityChanged, stalePity.error_message().id());
    EXPECT_EQ(goldBeforeReject, CurrencySystem::GetBalance(buyer, kCurrencyGold));

    // 第 2 次打磨掷出 1 档（roll=0）：只升不降，保持 3 档，保底升到 2。
    gForcedQualityRoll = 0;
    polish.set_expected_polish_pity_count(1);
    const auto p2 = CommitEquip(buyer, polish);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, p2.outcome());
    EXPECT_EQ(3u, p2.result_quality_tier());
    EXPECT_EQ(2u, p2.result_polish_pity_count());

    // 重铸换词条 101 -> 102（法术增伤）：100% 无损保留 3 档品相与保底进度 2！
    EquipmentCraftChoice reforge;
    reforge.set_operation(EquipmentCraftChoice::REFORGE_AFFIX);
    reforge.set_item_id(weaponId);
    reforge.set_affix_slot(1);
    reforge.set_affix_id(102);
    const auto rf = CommitEquip(buyer, reforge);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, rf.outcome()) << rf.DebugString();
    EXPECT_EQ(3u, rf.result_quality_tier());
    EXPECT_EQ(2u, rf.result_polish_pity_count());
    PlayerEquipmentV1System::CollectEffects(buyer, effects);
    EXPECT_EQ(0u, effects.physical_attack_bps());
    EXPECT_EQ(720u, effects.magic_attack_bps());

    // 同词条重铸拒绝（kEquipAffixReforgeSame）。
    const auto sameRf = CommitEquip(buyer, reforge);
    EXPECT_EQ(CommitEquipmentCraftResponse::REJECTED, sameRf.outcome());
    EXPECT_EQ(kEquipAffixReforgeSame, sameRf.error_message().id());

    // 继续打磨第 3..8 次（全部掷 1 档），第 8 次必触发硬保底达 5 档极品（800）且保底归零！
    for (uint32_t expectedPity = 2; expectedPity <= 7; ++expectedPity) {
        gForcedQualityRoll = 0;
        polish.set_expected_polish_pity_count(expectedPity);
        const auto step = CommitEquip(buyer, polish);
        ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, step.outcome()) << "pity=" << expectedPity;
        if (expectedPity < 7) {
            EXPECT_EQ(3u, step.result_quality_tier());
            EXPECT_EQ(expectedPity + 1, step.result_polish_pity_count());
            EXPECT_FALSE(step.guaranteed_by_pity());
        } else {
            EXPECT_EQ(5u, step.result_quality_tier());
            EXPECT_EQ(0u, step.result_polish_pity_count());
            EXPECT_TRUE(step.guaranteed_by_pity());
        }
    }
    PlayerEquipmentV1System::CollectEffects(buyer, effects);
    EXPECT_EQ(800u, effects.magic_attack_bps());

    // 极品词条不可继续打磨。
    polish.clear_expected_polish_pity_count();
    const auto overPolish = CommitEquip(buyer, polish);
    EXPECT_EQ(CommitEquipmentCraftResponse::REJECTED, overPolish.outcome());
    EXPECT_EQ(kEquipAffixAlreadyMaxQuality, overPolish.error_message().id());
}

TEST_F(PlayerCraftQualityTest, SpeedAffixIsExemptAndCannotExploitReforgeToFreeTierFive) {
    GrantItem(buyer, kForgeMat, 100, false);
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 5, false);

    // 3201 首饰安装 G02 速度词条（302）：即使随机为 0 也豁免恒满 5 档（100）。
    const Guid ringId = ForgeAndInstall(buyer, 3201, 1, 302, 0);
    const auto* ring = InventoryOf(buyer).GetItemCompByGuid(ringId);
    ASSERT_NE(nullptr, ring);
    EXPECT_EQ(5u, ring->equip().fixed_affixes(0).quality_tier());

    // 尝试打磨速度词条被拒（kEquipAffixPolishExempt）。
    EquipmentCraftChoice polish;
    polish.set_operation(EquipmentCraftChoice::POLISH_AFFIX);
    polish.set_item_id(ringId);
    polish.set_affix_slot(1);
    const auto blocked = CommitEquip(buyer, polish);
    EXPECT_EQ(CommitEquipmentCraftResponse::REJECTED, blocked.outcome());
    EXPECT_EQ(kEquipAffixPolishExempt, blocked.error_message().id());

    // 防漏洞：速度（豁免 5 档）重铸成非速度词条 301（气血）时重新掷品相，不能白嫖 5 档！
    gForcedQualityRoll = 0; // 1 档 80%
    EquipmentCraftChoice reforge;
    reforge.set_operation(EquipmentCraftChoice::REFORGE_AFFIX);
    reforge.set_item_id(ringId);
    reforge.set_affix_slot(1);
    reforge.set_affix_id(301);
    const auto rf = CommitEquip(buyer, reforge);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, rf.outcome());
    EXPECT_EQ(1u, rf.result_quality_tier());
}

TEST_F(PlayerCraftQualityTest, TradableEquipmentBlocksBoundSandAndCharmUnlessExplicitlyConfirmed) {
    GrantItem(buyer, kForgeMat, 50, false);
    const Guid weaponId = ForgeAndInstall(buyer, 3101, 1, 101, 0);
    ASSERT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());

    // 仅发绑定重铸砂和绑定定星保底符。
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 5, true);
    GrantItem(buyer, equipcraft::kGuaranteeCharmConfigId, 1, true);

    // 默认 allow_bound_materials=false 拒绝染毒（kEquipBindConfirmRequired），装备仍可交易。
    EquipmentCraftChoice polish;
    polish.set_operation(EquipmentCraftChoice::POLISH_AFFIX);
    polish.set_item_id(weaponId);
    polish.set_affix_slot(1);
    polish.set_use_guarantee_charm(true);
    const auto blocked = CommitEquip(buyer, polish);
    EXPECT_EQ(CommitEquipmentCraftResponse::REJECTED, blocked.outcome());
    EXPECT_EQ(kEquipBindConfirmRequired, blocked.error_message().id());
    EXPECT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());

    // 玩家明确二次确认 allow_bound_materials=true：扣绑定砂+绑定定星符，必出极品，装备永久变绑。
    polish.set_allow_bound_materials(true);
    const auto ok = CommitEquip(buyer, polish);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, ok.outcome()) << ok.DebugString();
    EXPECT_EQ(5u, ok.result_quality_tier());
    EXPECT_TRUE(ok.guaranteed_by_pity());
    EXPECT_TRUE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());
}

TEST_F(PlayerCraftQualityTest, AtomicProxyForgeStampsMasterSignatureAndBurnsFivePercentTax) {
    // 把 master 设为铸器宗师（60 经验）并开启代打摊位（手工费 100 银两）。
    tlsEcs.actorRegistry.get<PlayerCraftComp>(master).set_forge_mastery_exp(60);
    SetCraftProxyStallRequest openReq;
    openReq.set_open(true);
    openReq.set_proxy_forge_fee_gold(100);
    openReq.set_proxy_scroll_fee_gold(0);
    SetCraftProxyStallResponse openResp;
    PlayerCraftSystem::SetProxyStall(master, openReq, openResp);
    ASSERT_FALSE(openResp.has_error_message()) << openResp.DebugString();

    ListCraftProxyStallsResponse listResp;
    PlayerCraftSystem::ListProxyStalls(buyer, listResp);
    ASSERT_EQ(1, listResp.stalls_size());
    EXPECT_EQ(kMasterId, listResp.stalls(0).master_player_id());
    EXPECT_EQ("欧冶子", listResp.stalls(0).master_name());
    EXPECT_EQ(100u, listResp.stalls(0).proxy_forge_fee_gold());

    GrantItem(buyer, kForgeMat, 50, false);
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 5, false);
    const Guid weaponId = ForgeAndInstall(buyer, 3101, 1, 101, 0);

    const uint64_t buyerGoldBefore = CurrencySystem::GetBalance(buyer, kCurrencyGold);
    const uint64_t masterGoldBefore = CurrencySystem::GetBalance(master, kCurrencyGold);

    // 宗师极品率 10%（权重 9000..9999 -> 5 档），roll=9200 在学徒仅 4 档、在宗师直接 5 档！
    gForcedQualityRoll = 9200;
    EquipmentCraftChoice proxyPolish;
    proxyPolish.set_operation(EquipmentCraftChoice::POLISH_AFFIX);
    proxyPolish.set_item_id(weaponId);
    proxyPolish.set_affix_slot(1);
    proxyPolish.set_proxy_master_player_id(kMasterId);
    const auto resp = CommitEquip(buyer, proxyPolish);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, resp.outcome()) << resp.DebugString();
    EXPECT_EQ(5u, resp.result_quality_tier());

    // 买家装备仍在买家包里（从未移交）、刻上宗师署名；买家扣 20+100=120 银两，宗师得 95 银两，5 银两税销毁。
    const auto* item = InventoryOf(buyer).GetItemCompByGuid(weaponId);
    ASSERT_NE(nullptr, item);
    EXPECT_EQ(kMasterId, item->equip().crafter_player_id());
    EXPECT_EQ("欧冶子", item->equip().crafter_name());
    EXPECT_EQ(buyerGoldBefore - 120u, CurrencySystem::GetBalance(buyer, kCurrencyGold));
    EXPECT_EQ(masterGoldBefore + 95u, CurrencySystem::GetBalance(master, kCurrencyGold));
}

TEST_F(PlayerCraftQualityTest, SkillScrollCraftingDismantleRareAntiLaunderingAndLearnGate) {
    EXPECT_TRUE(PlayerCraftSystem::ValidateTables());

    // 发可交易残页 100 + 绝学残卷 2 + 绑定天工秘墨 2。
    GrantItem(buyer, equipcraft::kScrollFragmentConfigId, 100, false);
    GrantItem(buyer, equipcraft::kRareScrollPageConfigId, 2, false);
    GrantItem(buyer, equipcraft::kRareScrollInkConfigId, 2, true);

    // 1. 常规卷宗 3（兵法成长卷：12020/12021/12022）定向拓印 12020 愈战愈勇（耗 40 残页 + 100 银两）。
    CraftSkillBookRequest craftReq;
    craftReq.set_mode(CraftSkillBookRequest::TARGETED_REGULAR);
    craftReq.set_scroll_id(3);
    craftReq.set_target_skill_table_id(12020);
    craftReq.set_expected_revision(0);
    craftReq.set_client_request_id(++nextKey);
    CraftSkillBookResponse craftResp;
    PlayerCraftSystem::CraftSkillBook(buyer, craftReq, craftResp);
    ASSERT_FALSE(craftResp.has_error_message()) << craftResp.DebugString();
    EXPECT_FALSE(craftResp.replayed());
    EXPECT_EQ(12020u, craftResp.crafted_book().skill_table_id());
    EXPECT_FALSE(craftResp.crafted_book().bound()); // 全可交易残页 -> 产物可交易。
    EXPECT_EQ(60u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));

    // 同键重放：不重复扣料，原样返回 replayed=true。
    CraftSkillBookResponse replayResp;
    PlayerCraftSystem::CraftSkillBook(buyer, craftReq, replayResp);
    ASSERT_FALSE(replayResp.has_error_message());
    EXPECT_TRUE(replayResp.replayed());
    EXPECT_EQ(craftResp.crafted_book().book_id(), replayResp.crafted_book().book_id());
    EXPECT_EQ(60u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));

    // 2. 开启制书解锁门槛：未研习 12020 时 UpgradeSkill 拒绝（kSkillCraftBookRequired）。
    PlayerCraftSystem::SetCraftUnlockGateForTest(true);
    UpgradeSkillRequest upReq;
    upReq.set_skill_table_id(12020);
    upReq.set_expected_rank(0);
    upReq.set_count(1);
    UpgradeSkillResponse upResp;
    PlayerSkillSystem::UpgradeSkill(buyer, upReq, upResp);
    EXPECT_EQ(kSkillCraftBookRequired, upResp.error_message().id());

    // 先给玩家放一个既有技能 10002（rank=3），验证研习新书绝不顶掉老技能！
    auto* existingSkill = tlsEcs.actorRegistry.get<PlayerSkillListComp>(buyer).add_skill_list();
    existingSkill->set_skill_table_id(10002);
    existingSkill->set_rank(3);

    // 3. 研习 12020 技能书：永久解锁且保留 10002 rank=3。
    LearnSkillBookRequest learnReq;
    learnReq.set_book_id(craftResp.crafted_book().book_id());
    learnReq.set_pet_id(0);
    learnReq.set_expected_revision(1);
    learnReq.set_client_request_id(++nextKey);
    LearnSkillBookResponse learnResp;
    PlayerCraftSystem::LearnSkillBook(buyer, learnReq, learnResp);
    ASSERT_FALSE(learnResp.has_error_message()) << learnResp.DebugString();
    EXPECT_EQ(12020u, learnResp.learned_skill_table_id());
    EXPECT_TRUE(PlayerCraftSystem::IsSkillBookUnlocked(buyer, 12020));
    EXPECT_EQ(3u, PlayerSkillSystem::EffectiveRank(buyer, 10002));
    EXPECT_EQ(1u, PlayerSkillSystem::EffectiveRank(buyer, 12020));

    // 解锁后可以正常修习升重（1 -> 2）。
    upReq.set_expected_rank(1);
    PlayerSkillSystem::UpgradeSkill(buyer, upReq, upResp);
    EXPECT_FALSE(upResp.has_error_message()) << upResp.DebugString();
    EXPECT_EQ(2u, PlayerSkillSystem::EffectiveRank(buyer, 12020));

    // 4. 常规随机拓印 + 拆页 80%（返还 16 残页）。
    CraftSkillBookRequest randReq;
    randReq.set_mode(CraftSkillBookRequest::RANDOM_REGULAR);
    randReq.set_scroll_id(3);
    randReq.set_expected_revision(2);
    randReq.set_client_request_id(++nextKey);
    CraftSkillBookResponse randResp;
    gForcedCraftRoll = 1; // 卷 3 第 2 本（12021 深谋远虑）
    PlayerCraftSystem::CraftSkillBook(buyer, randReq, randResp);
    ASSERT_FALSE(randResp.has_error_message()) << randResp.DebugString();
    EXPECT_EQ(12021u, randResp.crafted_book().skill_table_id());
    EXPECT_EQ(40u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));

    DismantleSkillBookRequest disReq;
    disReq.set_book_id(randResp.crafted_book().book_id());
    disReq.set_expected_revision(3);
    disReq.set_client_request_id(++nextKey);
    DismantleSkillBookResponse disResp;
    PlayerCraftSystem::DismantleSkillBook(buyer, disReq, disResp);
    ASSERT_FALSE(disResp.has_error_message()) << disResp.DebugString();
    EXPECT_EQ(16u, disResp.refunded_fragments());
    EXPECT_EQ(56u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));

    // 5. 稀世双轨制书（卷 2：10041 玄镜映法）：防洗白硬隔离，强制 bound=true 且不可拆页。
    CraftSkillBookRequest rareReq;
    rareReq.set_mode(CraftSkillBookRequest::TARGETED_RARE);
    rareReq.set_scroll_id(2);
    rareReq.set_target_skill_table_id(10041);
    rareReq.set_expected_revision(4);
    rareReq.set_client_request_id(++nextKey);
    CraftSkillBookResponse rareResp;
    PlayerCraftSystem::CraftSkillBook(buyer, rareReq, rareResp);
    ASSERT_FALSE(rareResp.has_error_message()) << rareResp.DebugString();
    EXPECT_EQ(10041u, rareResp.crafted_book().skill_table_id());
    EXPECT_EQ(1u, rareResp.crafted_book().rarity_tier());
    EXPECT_TRUE(rareResp.crafted_book().bound());

    DismantleSkillBookRequest badDisReq;
    badDisReq.set_book_id(rareResp.crafted_book().book_id());
    badDisReq.set_expected_revision(5);
    badDisReq.set_client_request_id(++nextKey);
    DismantleSkillBookResponse badDisResp;
    PlayerCraftSystem::DismantleSkillBook(buyer, badDisReq, badDisResp);
    EXPECT_EQ(kSkillBookNotDismantlable, badDisResp.error_message().id());
}

} // namespace
