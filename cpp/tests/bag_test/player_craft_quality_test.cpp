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
#include "player/system/equip_attribute_rules.h"
#include "player/system/equip_craft_quality_rules.h"
#include "player/system/player_craft.h"
#include "player/system/player_equip.h"
#include "player/system/player_equipment_v1.h"
#include "player/system/player_mail_outbox.h"
#include "player/system/player_pet.h"
#include "player/system/player_skill.h"
#include "player/system/player_support_effects.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_craft_comp.pb.h"
#include "proto/common/component/player_item_gain_block_comp.pb.h"
#include "proto/common/component/player_mail_outbox_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/component/support_effects.pb.h"
#include "proto/common/component/team_comp.pb.h"
#include "proto/mail/mail_admin.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "services/battle/system/support_effect_rules.h"
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

bool ArmSegment(GuidKind kind, const std::string& name, uint64_t first, uint64_t last) {
    auto& client = tlsGuidSegmentRegistry.Get(kind);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false;
    options.kindName = name;
    options.initialStep = 100000;
    if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
        [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
    client.Warm();
    client.OnResponse(0, first, last);
    return client.IsReady();
}

bool ArmItemSegment(uint64_t first = 5800000, uint64_t last = 5900000) {
    return ArmSegment(GuidKind::kItem, "item", first, last);
}

uint32_t gForcedQualityRoll = 0;
uint32_t FixedQualityRoll(uint32_t) { return gForcedQualityRoll; }

uint32_t gForcedCraftRoll = 0;
uint32_t FixedCraftRoll(uint32_t) { return gForcedCraftRoll; }

bool DummyPersist(entt::entity) { return true; }
bool FailPersist(entt::entity) { return false; }

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
        tlsEcs.actorRegistry.emplace<PlayerMailOutboxComp>(entity);
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
        ASSERT_TRUE(ArmSegment(GuidKind::kTxLog, "txlog", 6800000, 6900000));
        PlayerCraftSystem::ClearProxyStallDirectoryForTest();
        mailSenderConfigured = true;
        mailNowMs = 10000;
        nextMailId = 500;
        sentMailRequests.clear();
        PlayerMailOutboxSystem::Dependencies deps;
        deps.senderConfigured = [this] { return mailSenderConfigured; };
        deps.isDurable = [](entt::entity, uint64_t) { return true; };
        deps.requestSave = [](entt::entity, const char*) {};
        deps.nowMs = [this] { return mailNowMs; };
        deps.send = [this](uint64_t ownerId, const MailOutboxEntry& entry, PlayerMailOutboxSystem::SendDone done) {
            mailpb::SendMailRequest req;
            if (!PlayerMailOutboxSystem::BuildSendRequest(ownerId, entry, req)) {
                return PlayerMailOutboxSystem::SendStart::kInvalidEntry;
            }
            sentMailRequests.push_back(req);
            pendingMailDone.push_back(std::move(done));
            return PlayerMailOutboxSystem::SendStart::kSent;
        };
        PlayerMailOutboxSystem::SetDependenciesForTest(std::move(deps));
        PlayerEquipmentV1System::SetPersistForTest(&DummyPersist);
        PlayerCraftSystem::SetPersistForTest(&DummyPersist);
        PlayerEquipmentV1System::SetQualityRollForTest(&FixedQualityRoll);
        PlayerCraftSystem::SetCraftRollForTest(&FixedCraftRoll);
        PlayerCraftSystem::SetCraftUnlockGateForTest(false);
        gForcedQualityRoll = 0;
        gForcedCraftRoll = 0;
        buyer = SpawnPlayer(kBuyerId, "侠客甲", 50000);
        master = SpawnPlayer(kMasterId, "欧冶子", 1000);
    }

    void TearDown() override {
        PlayerMailOutboxSystem::SetDependenciesForTest({});
        PlayerCraftSystem::ClearProxyStallDirectoryForTest();
        tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Reset();
        PlayerEquipmentV1System::SetPersistForTest(nullptr);
        PlayerCraftSystem::SetPersistForTest(nullptr);
        PlayerEquipmentV1System::SetQualityRollForTest(nullptr);
        PlayerCraftSystem::SetCraftRollForTest(nullptr);
        PlayerCraftSystem::SetCraftUnlockGateForTest(false);
        for (const auto id : {kBuyerId, kMasterId}) tlsEcs.playerList.erase(id);
        for (const auto e : {buyer, master}) {
            if (tlsEcs.actorRegistry.valid(e)) tlsEcs.actorRegistry.destroy(e);
        }
        LoadAllTables();
    }

    void FlushOutboxDeliveries() {
        for (int i = 0; i < 16; ++i) {
            const std::size_t before = pendingMailDone.size();
            mailNowMs += 1000;
            PlayerMailOutboxSystem::TickAt(mailNowMs);
            if (pendingMailDone.size() == before) break;
            auto callbacks = std::move(pendingMailDone);
            pendingMailDone.clear();
            for (auto& cb : callbacks) {
                cb(PlayerMailOutboxSystem::SendOutcome{true, 0, ++nextMailId});
            }
        }
    }

    bool mailSenderConfigured{true};
    uint64_t mailNowMs{10000};
    uint64_t nextMailId{500};
    std::vector<mailpb::SendMailRequest> sentMailRequests;
    std::vector<PlayerMailOutboxSystem::SendDone> pendingMailDone;

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

    // 买家装备仍在买家包里（从未移交）、刻上宗师署名；买家扣 20+100=120 银两，5 银两税销毁，
    // 95 净银两不再跨玩家直接改写余额，而是落买家 WALLET 持久出箱，经 MailAdmin -> mail_asset_op 异步入账。
    const auto* item = InventoryOf(buyer).GetItemCompByGuid(weaponId);
    ASSERT_NE(nullptr, item);
    EXPECT_EQ(kMasterId, item->equip().crafter_player_id());
    EXPECT_EQ("欧冶子", item->equip().crafter_name());
    EXPECT_EQ(buyerGoldBefore - 120u, CurrencySystem::GetBalance(buyer, kCurrencyGold));
    EXPECT_EQ(masterGoldBefore, CurrencySystem::GetBalance(master, kCurrencyGold));
    const auto& outbox = tlsEcs.actorRegistry.get<PlayerMailOutboxComp>(buyer);
    ASSERT_EQ(1, outbox.entries_size());
    EXPECT_EQ(static_cast<uint32_t>(mail_outbox_rules::MailOutboxSource::kCraftProxyFee), outbox.entries(0).source());
    EXPECT_EQ(kMasterId, outbox.entries(0).recipient_player_id());
    ASSERT_EQ(1, outbox.entries(0).currencies_size());
    EXPECT_EQ(0u, outbox.entries(0).currencies(0).currency_type());
    EXPECT_EQ(95u, outbox.entries(0).currencies(0).amount());

    FlushOutboxDeliveries();
    EXPECT_EQ(0, outbox.entries_size());
    ASSERT_EQ(1u, sentMailRequests.size());
    EXPECT_EQ(kMasterId, sentMailRequests[0].recipient_player_id());
    ASSERT_EQ(1, sentMailRequests[0].attachments().currencies_size());
    EXPECT_EQ(0u, sentMailRequests[0].attachments().currencies(0).currency_type());
    EXPECT_EQ(95u, sentMailRequests[0].attachments().currencies(0).amount());
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

TEST_F(PlayerCraftQualityTest, UnsealTalismanStripsBoundProgressionAndInheritCopiesEnhanceAsBound) {
    GrantItem(buyer, kForgeMat, 200, true);
    GrantItem(buyer, equipcraft::kUnsealTalismanConfigId, 2, true);
    GrantItem(buyer, equipcraft::kInheritTalismanConfigId, 2, true);

    // 1. 用绑定材料打造并装词条，装备必然绑定，再人为写入 +5 强化与宗师署名模拟已养成绑定装备。
    const Guid boundWeaponId = ForgeAndInstall(buyer, 3101, 1, 101, 0);
    auto* boundWeapon = InventoryOf(buyer).GetItemCompByGuid(boundWeaponId);
    ASSERT_NE(nullptr, boundWeapon);
    ASSERT_TRUE(boundWeapon->bound());
    boundWeapon->mutable_equip()->set_enhance_level(5);
    boundWeapon->mutable_equip()->set_enhance_pity_count(3);
    boundWeapon->mutable_equip()->set_crafter_name("青云宗师");
    boundWeapon->mutable_equip()->set_crafter_player_id(999);

    // 2. 再造一件同部位同等级副套武器（+0、可交易）用于测试流派副套继承符。
    GrantItem(buyer, kForgeMat, 100, false);
    EquipmentCraftChoice createAlt;
    createAlt.set_operation(EquipmentCraftChoice::CREATE_ITEM);
    createAlt.set_config_id(3101);
    const auto altCreated = CommitEquip(buyer, createAlt);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, altCreated.outcome()) << altCreated.DebugString();
    const Guid altWeaponId = altCreated.item_id();
    ASSERT_FALSE(InventoryOf(buyer).GetItemCompByGuid(altWeaponId)->bound());

    // 3. 流派副套继承符：把 boundWeapon (+5, pity=3) 继承到 altWeapon (+0)，altWeapon 升级为 +5 并强制转为绑定防洗白；源装备不动。
    EquipmentCraftChoice inherit;
    inherit.set_operation(EquipmentCraftChoice::INHERIT_ENHANCE);
    inherit.set_item_id(altWeaponId);
    inherit.set_source_item_id(boundWeaponId);
    const auto inheritResp = CommitEquip(buyer, inherit);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, inheritResp.outcome()) << inheritResp.DebugString();
    const auto* altAfter = InventoryOf(buyer).GetItemCompByGuid(altWeaponId);
    ASSERT_NE(nullptr, altAfter);
    EXPECT_EQ(5u, altAfter->equip().enhance_level());
    EXPECT_EQ(3u, altAfter->equip().enhance_pity_count());
    EXPECT_TRUE(altAfter->bound());
    EXPECT_EQ(5u, InventoryOf(buyer).GetItemCompByGuid(boundWeaponId)->equip().enhance_level());
    EXPECT_EQ(1u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kInheritTalismanConfigId));

    // 4. 解封剥离符：如果镶嵌了宝石必须先拒绝（防误销毁宝石）；取下宝石后剥离成功，清空所有绑定养成并恢复纯 T 原胚。
    boundWeapon = InventoryOf(buyer).GetItemCompByGuid(boundWeaponId);
    auto* gem = boundWeapon->mutable_equip()->add_gems();
    gem->set_socket(0);
    gem->set_gem_config_id(3201);
    EquipmentCraftChoice unseal;
    unseal.set_operation(EquipmentCraftChoice::UNSEAL_TO_TRADABLE);
    unseal.set_item_id(boundWeaponId);
    const auto blockedUnseal = CommitEquip(buyer, unseal);
    EXPECT_EQ(CommitEquipmentCraftResponse::REJECTED, blockedUnseal.outcome());

    boundWeapon->mutable_equip()->clear_gems();
    const auto unsealResp = CommitEquip(buyer, unseal);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, unsealResp.outcome()) << unsealResp.DebugString();
    const auto* unsealed = InventoryOf(buyer).GetItemCompByGuid(boundWeaponId);
    ASSERT_NE(nullptr, unsealed);
    EXPECT_FALSE(unsealed->bound());
    EXPECT_EQ(0u, unsealed->equip().enhance_level());
    EXPECT_EQ(0u, unsealed->equip().enhance_pity_count());
    EXPECT_EQ(0, unsealed->equip().fixed_affixes_size());
    EXPECT_TRUE(unsealed->equip().crafter_name().empty());
    EXPECT_EQ(0u, unsealed->equip().crafter_player_id());
    EXPECT_EQ(1u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kUnsealTalismanConfigId));
}

TEST_F(PlayerCraftQualityTest, EnhanceAndCraftBindStrictlyOnActualBoundConsumptionWithoutFalseBinding) {
    // 1. 造纯可交易武器并装词条。
    GrantItem(buyer, kForgeMat, 100, false);
    const Guid weaponId = ForgeAndInstall(buyer, 3101, 1, 101, 0);
    ASSERT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());

    // 2. PlanBoundPolicy 误绑回归：背包同时有充足可交易重铸砂(5)和绑定重铸砂(2)时，
    //    即使传 allow_bound_materials=true，也必须优先只扣可交易砂，预览 will_bind_item=false 且提交后保持纯 T。
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 5, false);
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 2, true);
    EquipmentCraftChoice polish;
    polish.set_operation(EquipmentCraftChoice::POLISH_AFFIX);
    polish.set_item_id(weaponId);
    polish.set_affix_slot(1);
    polish.set_allow_bound_materials(true);
    PreviewEquipmentCraftRequest previewReq;
    *previewReq.mutable_choice() = polish;
    PreviewEquipmentCraftResponse previewResp;
    ASSERT_EQ(kSuccess, PlayerEquipmentV1System::Preview(buyer, previewReq, previewResp));
    EXPECT_TRUE(previewResp.preview().available());
    EXPECT_FALSE(previewResp.preview().will_bind_item());
    EXPECT_FALSE(previewResp.preview().result().bound());

    gForcedQualityRoll = 7000;
    const auto polishResp = CommitEquip(buyer, polish);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, polishResp.outcome()) << polishResp.DebugString();
    EXPECT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());
    EXPECT_EQ(4u, InventoryOf(buyer).GetTotalTradableItemCount(equipcraft::kPolishSandConfigId));
    EXPECT_EQ(6u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kPolishSandConfigId));

    // 3. Enhance() T/B 隔离与无误绑回归：
    //    3a. 背包同时有充足可交易百炼石(20)与绑定百炼石(10)时，哪怕 allowBoundMaterials=true 也不误扣绑定石染毒纯 T 装。
    constexpr uint32_t kEnhanceStone = 3002;
    GrantItem(buyer, kEnhanceStone, 20, false);
    GrantItem(buyer, kEnhanceStone, 10, true);
    PlayerEquipSystem::EnhanceOutcome enhanceOutcome;
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(buyer, weaponId, 0, enhanceOutcome, std::nullopt, /*allowBoundMaterials=*/true));
    EXPECT_TRUE(enhanceOutcome.success);
    EXPECT_EQ(1u, enhanceOutcome.level);
    EXPECT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());
    EXPECT_EQ(18u, InventoryOf(buyer).GetTotalTradableItemCount(kEnhanceStone)); // +1 耗 2 颗纯可交易石
    EXPECT_EQ(28u, InventoryOf(buyer).GetTotalItemCount(kEnhanceStone));         // 10 颗绑定石原封未动

    //    3b. 清空可交易石，仅剩绑定石：默认 allowBoundMaterials=false 必须拦截 kEquipBindConfirmRequired，不扣石不变绑。
    ASSERT_EQ(kSuccess, BagService::RemoveItemsExact(buyer, InventoryOf(buyer), {{kEnhanceStone, 18}}, TX_EQUIP_ENHANCE, 1, "", true));
    EXPECT_EQ(0u, InventoryOf(buyer).GetTotalTradableItemCount(kEnhanceStone));
    EXPECT_EQ(10u, InventoryOf(buyer).GetTotalItemCount(kEnhanceStone));
    EXPECT_EQ(kEquipBindConfirmRequired, PlayerEquipSystem::Enhance(buyer, weaponId, 1, enhanceOutcome, std::nullopt, false));
    EXPECT_EQ(10u, InventoryOf(buyer).GetTotalItemCount(kEnhanceStone));
    EXPECT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());

    //    3c. 显式确认 allowBoundMaterials=true：扣绑定石且装备永久转绑（即便 +11 失败也同样转绑防洗白）。
    auto* weapon = InventoryOf(buyer).GetItemCompByGuid(weaponId);
    ASSERT_NE(nullptr, weapon);
    weapon->mutable_equip()->set_enhance_level(10);
    GrantItem(buyer, kEnhanceStone, 100, true);
    PlayerEquipSystem::SetRandomSourceForTest([](uint64_t n) { return n > 0 ? n - 1 : 0; }); // 必败
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(buyer, weaponId, 10, enhanceOutcome, 0, /*allowBoundMaterials=*/true));
    PlayerEquipSystem::SetRandomSourceForTest(nullptr);
    EXPECT_FALSE(enhanceOutcome.success);
    EXPECT_EQ(10u, enhanceOutcome.level);
    EXPECT_EQ(1u, enhanceOutcome.pityCount);
    EXPECT_TRUE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());
}

TEST_F(PlayerCraftQualityTest, AlwaysBoundRuleAndBatchInitCreditPreserveBoundIsolation) {
    EXPECT_TRUE(equipcraft::IsAlwaysBoundItem(equipcraft::kBoundArtifactVoucherConfigId));
    EXPECT_TRUE(equipcraft::IsAlwaysBoundItem(equipcraft::kGuaranteeCharmConfigId));
    EXPECT_TRUE(equipcraft::IsAlwaysBoundItem(equipcraft::kRareScrollInkConfigId));
    EXPECT_TRUE(equipcraft::IsAlwaysBoundItem(equipcraft::kUnsealTalismanConfigId));
    EXPECT_TRUE(equipcraft::IsAlwaysBoundItem(equipcraft::kInheritTalismanConfigId));
    EXPECT_FALSE(equipcraft::IsAlwaysBoundItem(equipcraft::kPolishSandConfigId));
    EXPECT_FALSE(equipcraft::IsAlwaysBoundItem(equipcraft::kScrollFragmentConfigId));
    EXPECT_FALSE(equipcraft::IsAlwaysBoundItem(equipcraft::kRareScrollPageConfigId));

    // 模拟 AssetOpSystem::Credit 的批次入包路径：3408 天工秘墨强制绑定，3407 绝学残卷保持可交易。
    std::vector<InitItemParam> creditItems;
    {
        InitItemParam page;
        page.itemPBComp.set_item_id(kInvalidGuid);
        page.itemPBComp.set_config_id(equipcraft::kRareScrollPageConfigId);
        page.itemPBComp.set_size(2);
        page.itemPBComp.set_bound(equipcraft::IsAlwaysBoundItem(equipcraft::kRareScrollPageConfigId));
        creditItems.push_back(std::move(page));
    }
    {
        InitItemParam ink;
        ink.itemPBComp.set_item_id(kInvalidGuid);
        ink.itemPBComp.set_config_id(equipcraft::kRareScrollInkConfigId);
        ink.itemPBComp.set_size(1);
        ink.itemPBComp.set_bound(equipcraft::IsAlwaysBoundItem(equipcraft::kRareScrollInkConfigId));
        creditItems.push_back(std::move(ink));
    }
    ASSERT_EQ(kSuccess, BagService::AddItems(buyer, InventoryOf(buyer), creditItems, TX_GUILD_SHOP, 99001u));
    EXPECT_EQ(2u, InventoryOf(buyer).GetTotalTradableItemCount(equipcraft::kRareScrollPageConfigId));
    EXPECT_EQ(1u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kRareScrollInkConfigId));
    EXPECT_EQ(0u, InventoryOf(buyer).GetTotalTradableItemCount(equipcraft::kRareScrollInkConfigId));
}

TEST_F(PlayerCraftQualityTest, SealAndUnsealSkillBookPreserveIdentitySignatureBoundAndIdempotency) {
    // 1. 让 master 成为制卷宗师，给 buyer 用可交易残页拓印 2 本可交易常规书 + 1 本绑定常规书 + 1 本稀世书。
    tlsEcs.actorRegistry.get<PlayerCraftComp>(master).set_scroll_mastery_exp(60);
    SetCraftProxyStallRequest openReq;
    openReq.set_open(true);
    openReq.set_proxy_scroll_fee_gold(60);
    SetCraftProxyStallResponse openResp;
    PlayerCraftSystem::SetProxyStall(master, openReq, openResp);
    ASSERT_FALSE(openResp.has_error_message()) << openResp.DebugString();

    GrantItem(buyer, equipcraft::kScrollFragmentConfigId, 100, false);
    GrantItem(buyer, equipcraft::kRareScrollPageConfigId, 1, false);
    GrantItem(buyer, equipcraft::kRareScrollInkConfigId, 1, true);

    auto craftBook = [&](CraftSkillBookRequest::CraftMode mode, uint32_t scrollId, uint32_t targetSkillId,
                         uint64_t expectedRev, uint64_t proxyId, uint32_t forcedRoll) {
        gForcedCraftRoll = forcedRoll;
        CraftSkillBookRequest req;
        req.set_mode(mode);
        req.set_scroll_id(scrollId);
        req.set_target_skill_table_id(targetSkillId);
        req.set_expected_revision(expectedRev);
        req.set_client_request_id(++nextKey);
        req.set_proxy_master_player_id(proxyId);
        CraftSkillBookResponse resp;
        PlayerCraftSystem::CraftSkillBook(buyer, req, resp);
        EXPECT_FALSE(resp.has_error_message()) << resp.DebugString();
        return resp.crafted_book();
    };

    // 宗师代制 2 本不同常规可交易书（12020 愈战愈勇、12021 深谋远虑）
    const auto tradableBook1 = craftBook(CraftSkillBookRequest::RANDOM_REGULAR, 3, 0, 0, kMasterId, 0);
    const auto tradableBook2 = craftBook(CraftSkillBookRequest::RANDOM_REGULAR, 3, 0, 1, kMasterId, 1);
    ASSERT_FALSE(tradableBook1.bound());
    ASSERT_FALSE(tradableBook2.bound());
    ASSERT_EQ("欧冶子", tradableBook1.crafter_name());

    // 稀世书（绑定）不可封卷
    const auto rareBook = craftBook(CraftSkillBookRequest::TARGETED_RARE, 2, 10041, 2, 0, 0);
    ASSERT_TRUE(rareBook.bound());
    {
        SealSkillBookRequest badRare;
        badRare.set_book_id(rareBook.book_id());
        badRare.set_expected_revision(3);
        badRare.set_client_request_id(++nextKey);
        SealSkillBookResponse badResp;
        PlayerCraftSystem::SealSkillBook(buyer, badRare, badResp);
        EXPECT_TRUE(badResp.has_error_message());
        EXPECT_EQ(kInvalidParameter, badResp.error_message().id());
    }

    // 2. 封卷两本可交易常规书入包：各自独立占 1 格（绝不并堆），携带完整技能与宗师署名，支持幂等重放。
    SealSkillBookRequest seal1;
    seal1.set_book_id(tradableBook1.book_id());
    seal1.set_expected_revision(3);
    seal1.set_client_request_id(++nextKey);
    SealSkillBookResponse seal1Resp;
    PlayerCraftSystem::SealSkillBook(buyer, seal1, seal1Resp);
    ASSERT_FALSE(seal1Resp.has_error_message()) << seal1Resp.DebugString();
    EXPECT_FALSE(seal1Resp.replayed());
    EXPECT_NE(0u, seal1Resp.item_id());
    EXPECT_EQ(tradableBook1.skill_table_id(), seal1Resp.skill_table_id());

    // 同键重放：不重复创建背包道具，原样返回首次 item_id。
    SealSkillBookResponse seal1Replay;
    PlayerCraftSystem::SealSkillBook(buyer, seal1, seal1Replay);
    ASSERT_FALSE(seal1Replay.has_error_message());
    EXPECT_TRUE(seal1Replay.replayed());
    EXPECT_EQ(seal1Resp.item_id(), seal1Replay.item_id());
    EXPECT_EQ(1u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kSealedSkillBookConfigId));

    // 封卷第 2 本，校验独立实例与 BagItemInfo 投影。
    SealSkillBookRequest seal2;
    seal2.set_book_id(tradableBook2.book_id());
    seal2.set_expected_revision(4);
    seal2.set_client_request_id(++nextKey);
    SealSkillBookResponse seal2Resp;
    PlayerCraftSystem::SealSkillBook(buyer, seal2, seal2Resp);
    ASSERT_FALSE(seal2Resp.has_error_message()) << seal2Resp.DebugString();
    EXPECT_NE(seal1Resp.item_id(), seal2Resp.item_id());
    EXPECT_EQ(2u, InventoryOf(buyer).GetTotalTradableItemCount(equipcraft::kSealedSkillBookConfigId));

    const auto* sealedItem1 = InventoryOf(buyer).GetItemCompByGuid(seal1Resp.item_id());
    ASSERT_NE(nullptr, sealedItem1);
    EXPECT_TRUE(ItemStore::IsIndependentInstance(*sealedItem1));
    EXPECT_FALSE(sealedItem1->bound());
    EXPECT_TRUE(sealedItem1->has_sealed_skill_book());
    EXPECT_EQ(tradableBook1.skill_table_id(), sealedItem1->sealed_skill_book().skill_table_id());
    EXPECT_EQ("欧冶子", sealedItem1->sealed_skill_book().crafter_name());

    BagItemInfo display;
    PlayerEquipSystem::FillItemDisplay(*sealedItem1, display);
    EXPECT_TRUE(display.has_sealed_skill_book());
    EXPECT_EQ(tradableBook1.skill_table_id(), display.sealed_skill_book().skill_table_id());
    EXPECT_EQ(3u, display.sealed_skill_book().scroll_id());
    EXPECT_EQ("欧冶子", display.sealed_skill_book().crafter_name());
    EXPECT_EQ("欧冶子", display.crafter_name());

    // 3. 模拟交易过户到 master（把第 1 本封卷道籍原样放进 master 背包，开卷回书架后保留技能/署名且支持重放）。
    InitItemParam transferred;
    transferred.itemPBComp = *sealedItem1;
    transferred.itemPBComp.set_item_id(kInvalidGuid);
    ASSERT_EQ(kSuccess, BagService::AddItem(master, InventoryOf(master), transferred));
    Guid masterSealedId = 0;
    InventoryOf(master).ForEachItem([&](Guid guid, const ItemComp& it) {
        if (it.config_id() == equipcraft::kSealedSkillBookConfigId) masterSealedId = guid;
    });
    ASSERT_NE(0u, masterSealedId);

    UnsealSkillBookRequest unsealReq;
    unsealReq.set_item_id(masterSealedId);
    unsealReq.set_expected_revision(0);
    unsealReq.set_client_request_id(++nextKey);
    UnsealSkillBookResponse unsealResp;
    PlayerCraftSystem::UnsealSkillBook(master, unsealReq, unsealResp);
    ASSERT_FALSE(unsealResp.has_error_message()) << unsealResp.DebugString();
    EXPECT_FALSE(unsealResp.replayed());
    EXPECT_EQ(tradableBook1.skill_table_id(), unsealResp.unsealed_book().skill_table_id());
    EXPECT_EQ("欧冶子", unsealResp.unsealed_book().crafter_name());
    EXPECT_FALSE(unsealResp.unsealed_book().bound());
    EXPECT_EQ(0u, InventoryOf(master).GetTotalItemCount(equipcraft::kSealedSkillBookConfigId));

    // 开卷同键重放：不重复加书。
    UnsealSkillBookResponse unsealReplay;
    PlayerCraftSystem::UnsealSkillBook(master, unsealReq, unsealReplay);
    ASSERT_FALSE(unsealReplay.has_error_message());
    EXPECT_TRUE(unsealReplay.replayed());
    EXPECT_EQ(unsealResp.unsealed_book().book_id(), unsealReplay.unsealed_book().book_id());
    EXPECT_EQ(1, unsealReplay.state().skill_books_size());

    // 4. 防洗白：若 3414 封卷道籍本身 bound=true，开卷回书架必须继承 bound=true，且禁止再封卷。
    auto* sealedItem2 = InventoryOf(buyer).GetItemCompByGuid(seal2Resp.item_id());
    ASSERT_NE(nullptr, sealedItem2);
    sealedItem2->set_bound(true);
    UnsealSkillBookRequest unsealBoundReq;
    unsealBoundReq.set_item_id(seal2Resp.item_id());
    unsealBoundReq.set_expected_revision(5);
    unsealBoundReq.set_client_request_id(++nextKey);
    UnsealSkillBookResponse unsealBoundResp;
    PlayerCraftSystem::UnsealSkillBook(buyer, unsealBoundReq, unsealBoundResp);
    ASSERT_FALSE(unsealBoundResp.has_error_message()) << unsealBoundResp.DebugString();
    EXPECT_TRUE(unsealBoundResp.unsealed_book().bound());

    SealSkillBookRequest resealBound;
    resealBound.set_book_id(unsealBoundResp.unsealed_book().book_id());
    resealBound.set_expected_revision(6);
    resealBound.set_client_request_id(++nextKey);
    SealSkillBookResponse resealResp;
    PlayerCraftSystem::SealSkillBook(buyer, resealBound, resealResp);
    EXPECT_TRUE(resealResp.has_error_message());
    EXPECT_EQ(kInvalidParameter, resealResp.error_message().id());
}

TEST(EquipCraftQualityRulesTest, V2ToV5PureRulesTitlesBitsAndWantedBounds) {
    // V2 零属性雅号解锁门槛与署名格式化
    EXPECT_FALSE(equipcraft::IsCraftTitleUnlocked(1, 59, 0, 0, 0, 0));
    EXPECT_TRUE(equipcraft::IsCraftTitleUnlocked(1, 60, 0, 0, 0, 0));
    EXPECT_FALSE(equipcraft::IsCraftTitleUnlocked(2, 60, 0, 4, 0, 0));
    EXPECT_TRUE(equipcraft::IsCraftTitleUnlocked(2, 60, 0, 5, 0, 0));
    EXPECT_TRUE(equipcraft::IsCraftTitleUnlocked(6, 60, 60, 0, 0, 0));
    EXPECT_EQ("双绝天工·欧冶子", equipcraft::FormatMasterSignature(6, "欧冶子"));
    EXPECT_EQ("欧冶子", equipcraft::FormatMasterSignature(0, "欧冶子"));

    // V4 卷宗里程碑位图（低位在前、去尾0、高位未知bit透传）
    EXPECT_EQ(2u, equipcraft::ScrollHalfThreshold(3));
    EXPECT_EQ(28u, equipcraft::ScrollHalfThreshold(56));
    std::string bits;
    bits.push_back(static_cast<char>(0x00));
    bits.push_back(static_cast<char>(0x80)); // 预置高位未知 bit 15
    equipcraft::SetBit(bits, equipcraft::ScrollMilestoneBitIndex(3, equipcraft::kScrollMilestoneHalfTier));
    EXPECT_TRUE(equipcraft::HasBit(bits, 4));
    EXPECT_TRUE(equipcraft::HasBit(bits, 15)); // 高位未知 bit 原样保留
    EXPECT_FALSE(equipcraft::HasBit(bits, 5));

    // V5 悬赏银两边界
    EXPECT_FALSE(equipcraft::ValidWantedOrderGold(99));
    EXPECT_TRUE(equipcraft::ValidWantedOrderGold(100));
    EXPECT_TRUE(equipcraft::ValidWantedOrderGold(50000));
    EXPECT_FALSE(equipcraft::ValidWantedOrderGold(50001));
}

TEST_F(PlayerCraftQualityTest, V2MasterReputationTitlesShowcaseTagAndFormattedSignature) {
    auto& masterCraft = tlsEcs.actorRegistry.get<PlayerCraftComp>(master);
    masterCraft.set_forge_mastery_exp(60);
    masterCraft.set_scroll_mastery_exp(60);

    // 未解锁百炼宗师(title=2，需5次极品)与非法招牌(tag=99)均必须拒绝
    {
        SetCraftProxyStallRequest badTitle;
        badTitle.set_open(true);
        badTitle.set_proxy_forge_fee_gold(70);
        badTitle.set_proxy_scroll_fee_gold(70);
        badTitle.set_equipped_craft_title_id(2);
        SetCraftProxyStallResponse resp;
        PlayerCraftSystem::SetProxyStall(master, badTitle, resp);
        EXPECT_TRUE(resp.has_error_message());
        EXPECT_EQ(kInvalidParameter, resp.error_message().id());
    }
    {
        SetCraftProxyStallRequest badTag;
        badTag.set_open(true);
        badTag.set_proxy_forge_fee_gold(70);
        badTag.set_proxy_scroll_fee_gold(70);
        badTag.set_master_showcase_tag(99);
        SetCraftProxyStallResponse resp;
        PlayerCraftSystem::SetProxyStall(master, badTag, resp);
        EXPECT_TRUE(resp.has_error_message());
        EXPECT_EQ(kInvalidParameter, resp.error_message().id());
    }

    // 佩戴已解锁的「双绝天工(6)」与招牌「双绝天工名匠(4)」并开摊
    SetCraftProxyStallRequest openReq;
    openReq.set_open(true);
    openReq.set_proxy_forge_fee_gold(75);
    openReq.set_proxy_scroll_fee_gold(65);
    openReq.set_master_showcase_tag(4);
    openReq.set_equipped_craft_title_id(6);
    SetCraftProxyStallResponse openResp;
    PlayerCraftSystem::SetProxyStall(master, openReq, openResp);
    ASSERT_FALSE(openResp.has_error_message()) << openResp.DebugString();
    EXPECT_EQ(6u, openResp.state().equipped_craft_title_id());
    EXPECT_EQ(4u, openResp.state().master_showcase_tag());

    // 买家请宗师代打坯 + 代装词条掷出极品(roll=9999) + 代制 1 本稀世绝学书 -> 验证格式化署名与口碑三项统计递增
    GrantItem(buyer, kForgeMat, 100, false);
    EquipmentCraftChoice createProxy;
    createProxy.set_operation(EquipmentCraftChoice::CREATE_ITEM);
    createProxy.set_config_id(3101);
    createProxy.set_proxy_master_player_id(kMasterId);
    const auto createdProxy = CommitEquip(buyer, createProxy);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, createdProxy.outcome()) << createdProxy.DebugString();

    gForcedQualityRoll = 9999;
    EquipmentCraftChoice installProxy;
    installProxy.set_operation(EquipmentCraftChoice::INSTALL_AFFIX);
    installProxy.set_item_id(createdProxy.item_id());
    installProxy.set_affix_slot(1);
    installProxy.set_affix_id(101);
    installProxy.set_proxy_master_player_id(kMasterId);
    const auto installedProxy = CommitEquip(buyer, installProxy);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, installedProxy.outcome()) << installedProxy.DebugString();

    const Guid weaponId = createdProxy.item_id();
    const auto* weapon = InventoryOf(buyer).GetItemCompByGuid(weaponId);
    ASSERT_NE(nullptr, weapon);
    EXPECT_EQ("双绝天工·欧冶子", weapon->equip().crafter_name());
    EXPECT_EQ(kMasterId, weapon->equip().crafter_player_id());

    GrantItem(buyer, equipcraft::kRareScrollPageConfigId, 1, false);
    GrantItem(buyer, equipcraft::kRareScrollInkConfigId, 1, true);
    CraftSkillBookRequest rareReq;
    rareReq.set_mode(CraftSkillBookRequest::TARGETED_RARE);
    rareReq.set_scroll_id(2);
    rareReq.set_target_skill_table_id(10041);
    rareReq.set_expected_revision(0);
    rareReq.set_client_request_id(++nextKey);
    rareReq.set_proxy_master_player_id(kMasterId);
    CraftSkillBookResponse rareResp;
    PlayerCraftSystem::CraftSkillBook(buyer, rareReq, rareResp);
    ASSERT_FALSE(rareResp.has_error_message()) << rareResp.DebugString();
    EXPECT_EQ("双绝天工·欧冶子", rareResp.crafted_book().crafter_name());

    ListCraftProxyStallsResponse listResp;
    PlayerCraftSystem::ListProxyStalls(buyer, listResp);
    ASSERT_EQ(1, listResp.stalls_size());
    const auto& stall = listResp.stalls(0);
    EXPECT_EQ(3u, stall.proxy_orders_completed()); // 造坯 + 装条 + 制稀世书
    EXPECT_EQ(1u, stall.supreme_forged_count());
    EXPECT_EQ(1u, stall.rare_books_crafted_count());
    EXPECT_EQ(4u, stall.master_showcase_tag());
    EXPECT_EQ(6u, stall.equipped_craft_title_id());
}

TEST_F(PlayerCraftQualityTest, V3BatchPolishStopsEarlyChargesActualAttemptsReplaysAndRespectsTradableGuard) {
    // 1. 造纯 T 武器并装 1 档凡品词条
    GrantItem(buyer, kForgeMat, 100, false);
    gForcedQualityRoll = 0; // 凡品(1)
    const Guid weaponId = ForgeAndInstall(buyer, 3101, 1, 101, 0);
    ASSERT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());
    ASSERT_EQ(1u, InventoryOf(buyer).GetItemCompByGuid(weaponId)->equip().fixed_affixes(0).quality_tier());

    // 2. 给 6 份可交易砂，设连磨目标 珍品(4)，最多 8 手；序列掷骰：第 1 手 上品(7500->3)，第 2 手 珍品(8800->4) 应在第 2 手即停！
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 6, false);
    static std::vector<uint32_t> seqRolls;
    static std::size_t seqIdx = 0;
    seqRolls = {7500, 8800, 9999};
    seqIdx = 0;
    PlayerEquipmentV1System::SetQualityRollForTest([](uint32_t) -> uint32_t {
        return seqIdx < seqRolls.size() ? seqRolls[seqIdx++] : 0u;
    });

    const uint64_t goldBefore = CurrencySystem::GetBalance(buyer, kCurrencyGold);
    EquipmentCraftChoice batchChoice;
    batchChoice.set_operation(EquipmentCraftChoice::POLISH_AFFIX);
    batchChoice.set_item_id(weaponId);
    batchChoice.set_affix_slot(1);
    batchChoice.set_target_quality_tier(4);
    batchChoice.set_max_polish_attempts(8);

    PreviewEquipmentCraftRequest previewReq;
    *previewReq.mutable_choice() = batchChoice;
    PreviewEquipmentCraftResponse previewResp;
    ASSERT_EQ(kSuccess, PlayerEquipmentV1System::Preview(buyer, previewReq, previewResp));
    EXPECT_TRUE(previewResp.preview().available());
    EXPECT_EQ(6u, previewResp.preview().max_polish_attempts());
    EXPECT_EQ(6u, previewResp.preview().max_polish_sand_cost());
    EXPECT_EQ(120u, previewResp.preview().max_polish_gold_cost());

    CommitEquipmentCraftRequest commitReq;
    *commitReq.mutable_choice() = batchChoice;
    commitReq.set_rules_fingerprint(previewResp.preview().rules_fingerprint());
    commitReq.set_expected_forge_revision(previewResp.preview().expected_forge_revision());
    commitReq.set_client_request_id(++nextKey);
    CommitEquipmentCraftResponse commitResp;
    PlayerEquipmentV1System::Commit(buyer, commitReq, commitResp);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, commitResp.outcome()) << commitResp.DebugString();
    EXPECT_EQ(4u, commitResp.result_quality_tier());
    EXPECT_EQ(2u, commitResp.result_polish_pity_count());
    EXPECT_EQ(2u, commitResp.polish_attempts_used());
    EXPECT_EQ(40u, commitResp.actual_gold_spent());
    ASSERT_EQ(2, commitResp.polish_steps_size());
    EXPECT_EQ(3u, commitResp.polish_steps(0).tier_after());
    EXPECT_EQ(4u, commitResp.polish_steps(1).tier_after());
    EXPECT_EQ(4u, InventoryOf(buyer).GetTotalTradableItemCount(equipcraft::kPolishSandConfigId)); // 只扣 2 砂！
    EXPECT_EQ(goldBefore - 40u, CurrencySystem::GetBalance(buyer, kCurrencyGold));

    // 弱网重放：原样返回 2 手与 40 银两，绝不重复扣砂
    CommitEquipmentCraftResponse replayResp;
    PlayerEquipmentV1System::Commit(buyer, commitReq, replayResp);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, replayResp.outcome());
    EXPECT_TRUE(replayResp.replayed());
    EXPECT_EQ(2u, replayResp.polish_attempts_used());
    EXPECT_EQ(40u, replayResp.actual_gold_spent());
    EXPECT_EQ(4u, InventoryOf(buyer).GetTotalTradableItemCount(equipcraft::kPolishSandConfigId));

    // 3. 防绑定染毒测试：当前剩余 4 份可交易砂，再加 5 份绑定砂，但保持 allow_bound_materials=false，
    //    连磨至极品(5) 全程掷 0 -> 最多只耗尽 4 份可交易砂(累计保底 2+4=6)，绝不碰绑定砂，装备保持纯 T！
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 5, true);
    PlayerEquipmentV1System::SetQualityRollForTest([](uint32_t) -> uint32_t { return 0u; });
    EquipmentCraftChoice toMax;
    toMax.set_operation(EquipmentCraftChoice::POLISH_AFFIX);
    toMax.set_item_id(weaponId);
    toMax.set_affix_slot(1);
    toMax.set_target_quality_tier(5);
    toMax.set_max_polish_attempts(8);
    toMax.set_allow_bound_materials(false);
    const auto toMaxResp = CommitEquip(buyer, toMax);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, toMaxResp.outcome()) << toMaxResp.DebugString();
    EXPECT_EQ(4u, toMaxResp.polish_attempts_used());
    EXPECT_EQ(6u, toMaxResp.result_polish_pity_count());
    EXPECT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());
    EXPECT_EQ(0u, InventoryOf(buyer).GetTotalTradableItemCount(equipcraft::kPolishSandConfigId));
    EXPECT_EQ(5u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kPolishSandConfigId)); // 5 份绑定砂完好无损

    // 4. 再补 2 份可交易砂，连磨至极品(5)：第 2 手(累计第 8 次)触发硬保底出极品！
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 2, false);
    const auto pityResp = CommitEquip(buyer, toMax);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, pityResp.outcome()) << pityResp.DebugString();
    EXPECT_EQ(2u, pityResp.polish_attempts_used());
    EXPECT_EQ(5u, pityResp.result_quality_tier());
    EXPECT_EQ(0u, pityResp.result_polish_pity_count());
    EXPECT_TRUE(pityResp.guaranteed_by_pity());
    EXPECT_FALSE(InventoryOf(buyer).GetItemCompByGuid(weaponId)->bound());
}

TEST_F(PlayerCraftQualityTest, V4ScrollMilestonesAndBatchDismantleGuardDuplicatesAndRareBooks) {
    // 1. 卷 3 共 3 项技能(12020/12021/12022)：先只解锁 1 项 -> 小成门槛 ceil(3/2)=2 必须拒绝
    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(buyer);
    craft.add_book_unlocked_skill_ids(12020);
    // 预置一个未知高位 bit 23 验证位图透传
    std::string initBits(3, '\0');
    initBits[2] = static_cast<char>(0x80);
    craft.set_claimed_scroll_milestone_bits(initBits);

    {
        ClaimScrollCollectionMilestoneRequest req;
        req.set_scroll_id(3);
        req.set_milestone_tier(1);
        req.set_expected_revision(0);
        req.set_client_request_id(++nextKey);
        ClaimScrollCollectionMilestoneResponse resp;
        PlayerCraftSystem::ClaimScrollCollectionMilestone(buyer, req, resp);
        EXPECT_TRUE(resp.has_error_message());
        EXPECT_EQ(kInvalidParameter, resp.error_message().id());
    }

    // 解锁第 2 项 -> 达小成，领 20 绑定残页 + 幂等重放 + 防二次领
    craft.add_book_unlocked_skill_ids(12021);
    ClaimScrollCollectionMilestoneRequest halfReq;
    halfReq.set_scroll_id(3);
    halfReq.set_milestone_tier(1);
    halfReq.set_expected_revision(0);
    halfReq.set_client_request_id(++nextKey);
    ClaimScrollCollectionMilestoneResponse halfResp;
    PlayerCraftSystem::ClaimScrollCollectionMilestone(buyer, halfReq, halfResp);
    ASSERT_FALSE(halfResp.has_error_message()) << halfResp.DebugString();
    EXPECT_EQ(20u, halfResp.rewarded_fragments());
    EXPECT_EQ(20u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));
    EXPECT_EQ(0u, InventoryOf(buyer).GetTotalTradableItemCount(equipcraft::kScrollFragmentConfigId)); // 强制绑定防洗白
    EXPECT_TRUE(equipcraft::HasBit(craft.claimed_scroll_milestone_bits(), 23)); // 高位未知 bit 透传保留

    ClaimScrollCollectionMilestoneResponse halfReplay;
    PlayerCraftSystem::ClaimScrollCollectionMilestone(buyer, halfReq, halfReplay);
    ASSERT_FALSE(halfReplay.has_error_message());
    EXPECT_TRUE(halfReplay.replayed());
    EXPECT_EQ(20u, halfReplay.rewarded_fragments());
    EXPECT_EQ(20u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));

    // 解锁第 3 项 -> 达圆满，领 40 绑定残页
    craft.add_book_unlocked_skill_ids(12022);
    ClaimScrollCollectionMilestoneRequest fullReq;
    fullReq.set_scroll_id(3);
    fullReq.set_milestone_tier(2);
    fullReq.set_expected_revision(1);
    fullReq.set_client_request_id(++nextKey);
    ClaimScrollCollectionMilestoneResponse fullResp;
    PlayerCraftSystem::ClaimScrollCollectionMilestone(buyer, fullReq, fullResp);
    ASSERT_FALSE(fullResp.has_error_message()) << fullResp.DebugString();
    EXPECT_EQ(40u, fullResp.rewarded_fragments());
    EXPECT_EQ(60u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));

    // 2. 批量拆页：先放入 3 本常规书 + 1 本稀世书
    auto addBook = [&](uint64_t id, uint32_t skillId, uint32_t scrollId, uint32_t rarity) {
        auto* b = craft.add_skill_books();
        b->set_book_id(id);
        b->set_skill_table_id(skillId);
        b->set_scroll_id(scrollId);
        b->set_rarity_tier(rarity);
        b->set_bound(false);
    };
    addBook(101, 12020, 3, 0);
    addBook(102, 12021, 3, 0);
    addBook(103, 12022, 3, 0);
    addBook(104, 10041, 2, 1);

    // 重复 book_id 整批拒
    {
        BatchDismantleSkillBooksRequest dupReq;
        dupReq.add_book_ids(101);
        dupReq.add_book_ids(101);
        dupReq.set_expected_revision(2);
        dupReq.set_client_request_id(++nextKey);
        BatchDismantleSkillBooksResponse resp;
        PlayerCraftSystem::BatchDismantleSkillBooks(buyer, dupReq, resp);
        EXPECT_TRUE(resp.has_error_message());
        EXPECT_EQ(kInvalidParameter, resp.error_message().id());
        EXPECT_EQ(4, craft.skill_books_size());
    }
    // 混入稀世书整批拒且零部分删除
    {
        BatchDismantleSkillBooksRequest rareReq;
        rareReq.add_book_ids(101);
        rareReq.add_book_ids(104);
        rareReq.set_expected_revision(2);
        rareReq.set_client_request_id(++nextKey);
        BatchDismantleSkillBooksResponse resp;
        PlayerCraftSystem::BatchDismantleSkillBooks(buyer, rareReq, resp);
        EXPECT_TRUE(resp.has_error_message());
        EXPECT_EQ(kSkillBookNotDismantlable, resp.error_message().id());
        EXPECT_EQ(4, craft.skill_books_size());
    }
    // 合法批量拆 3 本常规书 -> 返 48 绑定残页 + 留稀世书 + 幂等重放
    BatchDismantleSkillBooksRequest okReq;
    okReq.add_book_ids(101);
    okReq.add_book_ids(102);
    okReq.add_book_ids(103);
    okReq.set_expected_revision(2);
    okReq.set_client_request_id(++nextKey);
    BatchDismantleSkillBooksResponse okResp;
    PlayerCraftSystem::BatchDismantleSkillBooks(buyer, okReq, okResp);
    ASSERT_FALSE(okResp.has_error_message()) << okResp.DebugString();
    EXPECT_EQ(3u, okResp.dismantled_count());
    EXPECT_EQ(48u, okResp.refunded_fragments());
    EXPECT_EQ(1, craft.skill_books_size());
    EXPECT_EQ(108u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));

    BatchDismantleSkillBooksResponse okReplay;
    PlayerCraftSystem::BatchDismantleSkillBooks(buyer, okReq, okReplay);
    ASSERT_FALSE(okReplay.has_error_message());
    EXPECT_TRUE(okReplay.replayed());
    EXPECT_EQ(3u, okReplay.dismantled_count());
    EXPECT_EQ(48u, okReplay.refunded_fragments());
    EXPECT_EQ(108u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kScrollFragmentConfigId));
}

TEST_F(PlayerCraftQualityTest, V5TeamFreeProxyAndWantedOrderEscrowFulfillmentGuardBindAndSignature) {
    // 1. 同队情义 0 费原子代打/代制
    auto& masterCraft = tlsEcs.actorRegistry.get<PlayerCraftComp>(master);
    masterCraft.set_forge_mastery_exp(60);
    masterCraft.set_scroll_mastery_exp(60);
    SetCraftProxyStallRequest openReq;
    openReq.set_open(true);
    openReq.set_proxy_forge_fee_gold(80);
    openReq.set_proxy_scroll_fee_gold(80);
    openReq.set_team_free_proxy(true);
    SetCraftProxyStallResponse openResp;
    PlayerCraftSystem::SetProxyStall(master, openReq, openResp);
    ASSERT_FALSE(openResp.has_error_message());

    // 未组队时 viewer_gets_free_proxy=false
    {
        ListCraftProxyStallsResponse stalls;
        PlayerCraftSystem::ListProxyStalls(buyer, stalls);
        ASSERT_EQ(1, stalls.stalls_size());
        EXPECT_TRUE(stalls.stalls(0).team_free_proxy());
        EXPECT_FALSE(stalls.stalls(0).viewer_gets_free_proxy());
    }

    // 同队 TeamId=777 后：列表标 viewer_gets_free_proxy=true，装备代打与制书代制均 0 手工费
    tlsEcs.actorRegistry.emplace_or_replace<TeamId>(buyer).set_team_id(777);
    tlsEcs.actorRegistry.emplace_or_replace<TeamId>(master).set_team_id(777);
    {
        ListCraftProxyStallsResponse stalls;
        PlayerCraftSystem::ListProxyStalls(buyer, stalls);
        ASSERT_EQ(1, stalls.stalls_size());
        EXPECT_TRUE(stalls.stalls(0).viewer_gets_free_proxy());
    }
    const uint64_t masterGoldBefore = CurrencySystem::GetBalance(master, kCurrencyGold);
    const uint64_t buyerGoldBefore = CurrencySystem::GetBalance(buyer, kCurrencyGold);
    GrantItem(buyer, equipcraft::kScrollFragmentConfigId, 40, false);
    gForcedCraftRoll = 0;
    CraftSkillBookRequest proxyCraft;
    proxyCraft.set_mode(CraftSkillBookRequest::RANDOM_REGULAR);
    proxyCraft.set_scroll_id(3);
    proxyCraft.set_expected_revision(0);
    proxyCraft.set_client_request_id(++nextKey);
    proxyCraft.set_proxy_master_player_id(kMasterId);
    CraftSkillBookResponse proxyResp;
    PlayerCraftSystem::CraftSkillBook(buyer, proxyCraft, proxyResp);
    ASSERT_FALSE(proxyResp.has_error_message()) << proxyResp.DebugString();
    EXPECT_EQ(buyerGoldBefore - 50u, CurrencySystem::GetBalance(buyer, kCurrencyGold)); // 只扣基础 50 银两，代打费 0！
    EXPECT_EQ(masterGoldBefore, CurrencySystem::GetBalance(master, kCurrencyGold));
    EXPECT_EQ("欧冶子", proxyResp.crafted_book().crafter_name());
    EXPECT_EQ(1u, masterCraft.proxy_orders_completed());

    // 2. 西市悬赏求书板：发榜、防绑定银两洗白、撤单全退、禁自揭、验署名、揭榜交卷(书架+背包封卷双源)
    // 给 master 发榜 2 条求书单（第 1 条不限署名 1000 银两；第 2 条要求宗师署名 1000 银两）
    tlsEcs.actorRegistry.get<CurrencyComp>(master).set_values(kCurrencyGold, 3000);
    PostSkillBookWantedOrderRequest post1;
    post1.set_skill_table_id(12020);
    post1.set_require_master_signature(false);
    post1.set_reward_gold(1000);
    post1.set_expected_revision(0);
    post1.set_client_request_id(++nextKey);
    PostSkillBookWantedOrderResponse post1Resp;
    PlayerCraftSystem::PostWantedOrder(master, post1, post1Resp);
    ASSERT_FALSE(post1Resp.has_error_message()) << post1Resp.DebugString();
    EXPECT_EQ(2000u, CurrencySystem::GetBalance(master, kCurrencyGold));
    EXPECT_EQ(950u, post1Resp.order().seller_net_gold()); // 5% 税

    // 撤单全额退还 1000 银两 + 幂等重放
    CancelSkillBookWantedOrderRequest cancel1;
    cancel1.set_order_id(post1Resp.order().order_id());
    cancel1.set_expected_revision(1);
    cancel1.set_client_request_id(++nextKey);
    CancelSkillBookWantedOrderResponse cancel1Resp;
    PlayerCraftSystem::CancelWantedOrder(master, cancel1, cancel1Resp);
    ASSERT_FALSE(cancel1Resp.has_error_message()) << cancel1Resp.DebugString();
    EXPECT_EQ(1000u, cancel1Resp.refunded_gold());
    EXPECT_EQ(3000u, CurrencySystem::GetBalance(master, kCurrencyGold));
    CancelSkillBookWantedOrderResponse cancel1Replay;
    PlayerCraftSystem::CancelWantedOrder(master, cancel1, cancel1Replay);
    EXPECT_TRUE(cancel1Replay.replayed());
    EXPECT_EQ(3000u, CurrencySystem::GetBalance(master, kCurrencyGold));

    // 发榜要求宗师署名求 12020（1000 银两）
    PostSkillBookWantedOrderRequest postSig;
    postSig.set_skill_table_id(12020);
    postSig.set_require_master_signature(true);
    postSig.set_reward_gold(1000);
    postSig.set_expected_revision(2);
    postSig.set_client_request_id(++nextKey);
    PostSkillBookWantedOrderResponse postSigResp;
    PlayerCraftSystem::PostWantedOrder(master, postSig, postSigResp);
    ASSERT_FALSE(postSigResp.has_error_message()) << postSigResp.DebugString();

    // 列表查询能看到该单
    ListSkillBookWantedOrdersRequest listWantedReq;
    listWantedReq.set_filter_skill_table_id(12020);
    ListSkillBookWantedOrdersResponse listWantedResp;
    PlayerCraftSystem::ListWantedOrders(buyer, listWantedReq, listWantedResp);
    ASSERT_EQ(1, listWantedResp.orders_size());
    EXPECT_EQ(postSigResp.order().order_id(), listWantedResp.orders(0).order_id());

    // 禁止自己揭自己的榜
    {
        FulfillSkillBookWantedOrderRequest selfFulfill;
        selfFulfill.set_poster_player_id(kMasterId);
        selfFulfill.set_order_id(postSigResp.order().order_id());
        selfFulfill.set_bookshelf_book_id(1);
        selfFulfill.set_expected_revision(3);
        selfFulfill.set_client_request_id(++nextKey);
        FulfillSkillBookWantedOrderResponse resp;
        PlayerCraftSystem::FulfillWantedOrder(master, selfFulfill, resp);
        EXPECT_TRUE(resp.has_error_message());
        EXPECT_EQ(kInvalidParameter, resp.error_message().id());
    }

    // 禁止用无署名常规书揭要求署名的榜
    auto& buyerCraft = tlsEcs.actorRegistry.get<PlayerCraftComp>(buyer);
    auto* unsignedBook = buyerCraft.add_skill_books();
    unsignedBook->set_book_id(999);
    unsignedBook->set_skill_table_id(12020);
    unsignedBook->set_scroll_id(3);
    unsignedBook->set_rarity_tier(0);
    unsignedBook->set_bound(false);
    {
        FulfillSkillBookWantedOrderRequest badUnsigned;
        badUnsigned.set_poster_player_id(kMasterId);
        badUnsigned.set_order_id(postSigResp.order().order_id());
        badUnsigned.set_bookshelf_book_id(999);
        badUnsigned.set_expected_revision(1);
        badUnsigned.set_client_request_id(++nextKey);
        FulfillSkillBookWantedOrderResponse resp;
        PlayerCraftSystem::FulfillWantedOrder(buyer, badUnsigned, resp);
        EXPECT_TRUE(resp.has_error_message());
        EXPECT_EQ(kInvalidParameter, resp.error_message().id());
    }

    // 把刚才宗师代制的带署名 12020 书先封卷入包(3414)，再用背包封卷一键揭榜交卷 -> 卖家得 950 净银两，买家书架收到带署名未绑书！
    SealSkillBookRequest sealReq;
    sealReq.set_book_id(proxyResp.crafted_book().book_id());
    sealReq.set_expected_revision(1);
    sealReq.set_client_request_id(++nextKey);
    SealSkillBookResponse sealResp;
    PlayerCraftSystem::SealSkillBook(buyer, sealReq, sealResp);
    ASSERT_FALSE(sealResp.has_error_message()) << sealResp.DebugString();

    const uint64_t sellerGoldBeforeFulfill = CurrencySystem::GetBalance(buyer, kCurrencyGold);
    FulfillSkillBookWantedOrderRequest fulfillReq;
    fulfillReq.set_poster_player_id(kMasterId);
    fulfillReq.set_order_id(postSigResp.order().order_id());
    fulfillReq.set_sealed_item_id(sealResp.item_id());
    fulfillReq.set_expected_revision(2);
    fulfillReq.set_client_request_id(++nextKey);
    FulfillSkillBookWantedOrderResponse fulfillResp;
    PlayerCraftSystem::FulfillWantedOrder(buyer, fulfillReq, fulfillResp);
    ASSERT_FALSE(fulfillResp.has_error_message()) << fulfillResp.DebugString();
    EXPECT_EQ(950u, fulfillResp.seller_net_gold());
    EXPECT_EQ(sellerGoldBeforeFulfill + 950u, CurrencySystem::GetBalance(buyer, kCurrencyGold));
    EXPECT_EQ(0u, InventoryOf(buyer).GetTotalItemCount(equipcraft::kSealedSkillBookConfigId));
    EXPECT_EQ(0, masterCraft.wanted_orders_size());
    ASSERT_EQ(1, masterCraft.skill_books_size());
    EXPECT_EQ(12020u, masterCraft.skill_books(0).skill_table_id());
    EXPECT_EQ("欧冶子", masterCraft.skill_books(0).crafter_name());
    EXPECT_FALSE(masterCraft.skill_books(0).bound());

    // 揭榜幂等重放：不重复发银两
    FulfillSkillBookWantedOrderResponse fulfillReplay;
    PlayerCraftSystem::FulfillWantedOrder(buyer, fulfillReq, fulfillReplay);
    ASSERT_FALSE(fulfillReplay.has_error_message());
    EXPECT_TRUE(fulfillReplay.replayed());
    EXPECT_EQ(sellerGoldBeforeFulfill + 950u, CurrencySystem::GetBalance(buyer, kCurrencyGold));
}

TEST_F(PlayerCraftQualityTest, CrossNodeAndOfflineMasterProxyCraftRollsSignatureAndPaysOutAsyncViaAssetOp) {
    // 1. 宗师开启双绝代打摊位（铸器 100 银两、制卷 80 银两，佩戴「双绝天工(6)」），随后彻底下线（从本节点移除并销毁实体）。
    auto& masterCraft = tlsEcs.actorRegistry.get<PlayerCraftComp>(master);
    masterCraft.set_forge_mastery_exp(60);
    masterCraft.set_scroll_mastery_exp(60);
    SetCraftProxyStallRequest openReq;
    openReq.set_open(true);
    openReq.set_proxy_forge_fee_gold(100);
    openReq.set_proxy_scroll_fee_gold(80);
    openReq.set_equipped_craft_title_id(6);
    openReq.set_master_showcase_tag(4);
    SetCraftProxyStallResponse openResp;
    PlayerCraftSystem::SetProxyStall(master, openReq, openResp);
    ASSERT_FALSE(openResp.has_error_message()) << openResp.DebugString();

    tlsEcs.playerList.erase(kMasterId);
    tlsEcs.actorRegistry.destroy(master);
    master = entt::null;

    // 2. 宗师离线后，共享目录仍展示其摊位（24 小时营业）。
    ListCraftProxyStallsResponse listResp;
    PlayerCraftSystem::ListProxyStalls(buyer, listResp);
    ASSERT_EQ(1, listResp.stalls_size());
    EXPECT_EQ(kMasterId, listResp.stalls(0).master_player_id());
    EXPECT_EQ("欧冶子", listResp.stalls(0).master_name());
    EXPECT_EQ(100u, listResp.stalls(0).proxy_forge_fee_gold());
    EXPECT_EQ(80u, listResp.stalls(0).proxy_scroll_fee_gold());
    EXPECT_EQ(6u, listResp.stalls(0).equipped_craft_title_id());

    // 3. 买家对离线宗师下单：装备代打磨（roll=9200 在宗师档直接出 5 档极品并刻雅号署名，扣 20+100=120 银两，95 净银两进出箱）。
    GrantItem(buyer, kForgeMat, 50, false);
    GrantItem(buyer, equipcraft::kPolishSandConfigId, 5, false);
    const Guid weaponId = ForgeAndInstall(buyer, 3101, 1, 101, 0);
    const uint64_t buyerGoldBefore = CurrencySystem::GetBalance(buyer, kCurrencyGold);

    gForcedQualityRoll = 9200;
    EquipmentCraftChoice proxyPolish;
    proxyPolish.set_operation(EquipmentCraftChoice::POLISH_AFFIX);
    proxyPolish.set_item_id(weaponId);
    proxyPolish.set_affix_slot(1);
    proxyPolish.set_proxy_master_player_id(kMasterId);
    const auto equipResp = CommitEquip(buyer, proxyPolish);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, equipResp.outcome()) << equipResp.DebugString();
    EXPECT_EQ(5u, equipResp.result_quality_tier());
    const auto* weapon = InventoryOf(buyer).GetItemCompByGuid(weaponId);
    ASSERT_NE(nullptr, weapon);
    EXPECT_EQ(kMasterId, weapon->equip().crafter_player_id());
    EXPECT_EQ("双绝天工·欧冶子", weapon->equip().crafter_name());
    EXPECT_EQ(buyerGoldBefore - 120u, CurrencySystem::GetBalance(buyer, kCurrencyGold));

    // 4. 买家继续对离线宗师下单：技能制书代制（扣 50+80=130 银两，76 净银两进出箱，刻雅号署名）。
    GrantItem(buyer, equipcraft::kScrollFragmentConfigId, 40, false);
    gForcedCraftRoll = 0;
    CraftSkillBookRequest scrollReq;
    scrollReq.set_mode(CraftSkillBookRequest::RANDOM_REGULAR);
    scrollReq.set_scroll_id(3);
    scrollReq.set_expected_revision(0);
    scrollReq.set_client_request_id(++nextKey);
    scrollReq.set_proxy_master_player_id(kMasterId);
    CraftSkillBookResponse scrollResp;
    PlayerCraftSystem::CraftSkillBook(buyer, scrollReq, scrollResp);
    ASSERT_FALSE(scrollResp.has_error_message()) << scrollResp.DebugString();
    EXPECT_EQ(kMasterId, scrollResp.crafted_book().crafter_player_id());
    EXPECT_EQ("双绝天工·欧冶子", scrollResp.crafted_book().crafter_name());
    EXPECT_EQ(buyerGoldBefore - 250u, CurrencySystem::GetBalance(buyer, kCurrencyGold));

    // 5. 两笔 95% 净银两分成均落在买家出箱，Tick 投递发出确定性 MailAdmin.SendPersonalMail 后清空。
    const auto& outbox = tlsEcs.actorRegistry.get<PlayerMailOutboxComp>(buyer);
    ASSERT_EQ(2, outbox.entries_size());
    FlushOutboxDeliveries();
    EXPECT_EQ(0, outbox.entries_size());
    ASSERT_EQ(2u, sentMailRequests.size());
    EXPECT_EQ(kMasterId, sentMailRequests[0].recipient_player_id());
    EXPECT_EQ(95u, sentMailRequests[0].attachments().currencies(0).amount());
    EXPECT_EQ(kMasterId, sentMailRequests[1].recipient_player_id());
    EXPECT_EQ(76u, sentMailRequests[1].attachments().currencies(0).amount());

    // 6. 邮件发件通道未配置时 fail-closed 拒单且零扣费。
    mailSenderConfigured = false;
    const uint64_t goldBeforeFail = CurrencySystem::GetBalance(buyer, kCurrencyGold);
    EquipmentCraftChoice createProxy;
    createProxy.set_operation(EquipmentCraftChoice::CREATE_ITEM);
    createProxy.set_config_id(3101);
    createProxy.set_proxy_master_player_id(kMasterId);
    const auto blocked = CommitEquip(buyer, createProxy);
    EXPECT_NE(CommitEquipmentCraftResponse::APPLIED, blocked.outcome());
    EXPECT_EQ(goldBeforeFail, CurrencySystem::GetBalance(buyer, kCurrencyGold));
    EXPECT_EQ(0, outbox.entries_size());

    // 7. 从共享目录摘除后无法继续下单。
    mailSenderConfigured = true;
    PlayerCraftSystem::RemoveProxyStallForTest(kMasterId);
    PreviewEquipmentCraftRequest previewReq;
    *previewReq.mutable_choice() = createProxy;
    PreviewEquipmentCraftResponse previewResp;
    EXPECT_EQ(kEquipProxyMasterUnavailable, PlayerEquipmentV1System::Preview(buyer, previewReq, previewResp));
}

TEST_F(PlayerCraftQualityTest, SupportAndControlEnhancementFairBenefitWhileKeepingSpeedUnchanged) {
    GrantItem(buyer, kForgeMat, 400, false);
    gForcedQualityRoll = 9999; // 满 5 档品相

    // 1. 武器(3181)装 D03 施放治疗(103，满值 800 bps)：+0 -> 800，+10 -> 880(50% 份额)，+11 -> 896。
    const Guid weaponId = ForgeAndInstall(buyer, 3181, 1, 103, 9999);
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(buyer, weaponId, 3));
    auto& equipBag = tlsEcs.actorRegistry.get<PlayerBagsComp>(buyer).bags[kEquipment];

    auto expectHealBps = [&](uint32_t expectedBps, uint32_t expectedCap) {
        EquipmentEffects effects;
        PlayerEquipmentV1System::CollectEffects(buyer, effects);
        EXPECT_EQ(expectedBps, effects.outgoing_heal_bps());
        EXPECT_TRUE(supportrules::ValidEquipmentSources(effects));
        ASSERT_GE(effects.support_sources_size(), 1);
        EXPECT_EQ(expectedCap, effects.support_sources(0).source_cap_bps());
        SupportEffectSummary summary;
        PlayerSupportEffectSystem::Build(buyer, summary);
        EXPECT_DOUBLE_EQ(static_cast<double>(expectedBps), summary.healing().total_bonus_bps());
        EXPECT_EQ(expectedCap, summary.healing().equipment_cap_bps());
    };

    expectHealBps(800u, 3200u);

    auto* wornWeapon = equipBag.GetItemCompByGuid(weaponId);
    ASSERT_NE(nullptr, wornWeapon);
    wornWeapon->mutable_equip()->set_enhance_level(10);
    expectHealBps(880u, 3520u);
    {
        BagItemInfo display;
        PlayerEquipSystem::FillItemDisplay(*wornWeapon, display);
        ASSERT_GE(display.fixed_affix_slots_size(), 1);
        EXPECT_EQ(880u, display.fixed_affix_slots(0).selected().value_bps());
    }

    wornWeapon->mutable_equip()->set_enhance_level(11);
    expectHealBps(896u, 3584u);

    // 2. 四件基础装全 +11 均装 D03 治疗：3584 bps 不被旧 3200 上限吞掉，且来源明细和严格恒等。
    const Guid hatId = ForgeAndInstall(buyer, 3182, 1, 103, 9999);
    const Guid armorId = ForgeAndInstall(buyer, 3183, 1, 103, 9999);
    const Guid shoesId = ForgeAndInstall(buyer, 3184, 1, 103, 9999);
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(buyer, hatId, 4));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(buyer, armorId, 5));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(buyer, shoesId, 6));
    for (const Guid gid : {hatId, armorId, shoesId}) {
        auto* piece = equipBag.GetItemCompByGuid(gid);
        ASSERT_NE(nullptr, piece);
        piece->mutable_equip()->set_enhance_level(11);
    }
    expectHealBps(3584u, 3584u);

    // 3. 鞋子(3184) +11 速度红线：防御按 +24% 放大，基础速度(attr 11)增幅严格为 0！
    {
        const auto* wornShoes = equipBag.GetItemCompByGuid(shoesId);
        ASSERT_NE(nullptr, wornShoes);
        BagItemInfo shoesDisplay;
        PlayerEquipSystem::FillItemDisplay(*wornShoes, shoesDisplay);
        bool sawDefense = false;
        bool sawSpeed = false;
        for (const auto& line : shoesDisplay.base_attrs()) {
            if (line.attr_id() == 8u) {
                sawDefense = true;
                EXPECT_GT(line.enhance_bonus(), 0u);
            }
            if (line.attr_id() == 11u) {
                sawSpeed = true;
                EXPECT_EQ(0u, line.enhance_bonus());
            }
        }
        EXPECT_TRUE(sawDefense);
        EXPECT_TRUE(sawSpeed);
    }

    // 4. 控场命中(D05 封灵命中 105，满值 400 bps)在 +10/+11 按 25% 克制份额受益：400 -> 420 -> 424；四件 +11 -> 1696。
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Unequip(buyer, weaponId));
    const Guid ctrlWeaponId = ForgeAndInstall(buyer, 3181, 1, 105, 9999);
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(buyer, ctrlWeaponId, 3));
    auto* ctrlWeapon = equipBag.GetItemCompByGuid(ctrlWeaponId);
    ASSERT_NE(nullptr, ctrlWeapon);
    ctrlWeapon->mutable_equip()->set_enhance_level(10);
    {
        EquipmentEffects effects;
        PlayerEquipmentV1System::CollectEffects(buyer, effects);
        EXPECT_EQ(420u, effects.ailment_hit_bps().at(1));
    }
    ctrlWeapon->mutable_equip()->set_enhance_level(11);
    {
        EquipmentEffects effects;
        PlayerEquipmentV1System::CollectEffects(buyer, effects);
        EXPECT_EQ(424u, effects.ailment_hit_bps().at(1));
    }

    // 5. 纯输出 D01 物理攻击(101)在 +11 武器上不重复放大固定词条（基础伤害已享全额 +24%）。
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Unequip(buyer, ctrlWeaponId));
    const Guid dpsWeaponId = ForgeAndInstall(buyer, 3181, 1, 101, 9999);
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(buyer, dpsWeaponId, 3));
    auto* dpsWeapon = equipBag.GetItemCompByGuid(dpsWeaponId);
    ASSERT_NE(nullptr, dpsWeapon);
    dpsWeapon->mutable_equip()->set_enhance_level(11);
    {
        EquipmentEffects effects;
        PlayerEquipmentV1System::CollectEffects(buyer, effects);
        EXPECT_EQ(800u, effects.physical_attack_bps());
    }
}

TEST_F(PlayerCraftQualityTest, OfflineReputationTitleUnlockDigestAckAndWestMarketHotRankTop3) {
    // 1. 宗师已有 4 件极品（距百炼宗师 title=2 仅差 1 件），开启 24 小时代打摊位后下线。
    auto& masterCraft = tlsEcs.actorRegistry.get<PlayerCraftComp>(master);
    masterCraft.set_forge_mastery_exp(60);
    masterCraft.set_scroll_mastery_exp(60);
    masterCraft.set_supreme_forged_count(4);
    SetCraftProxyStallRequest stallReq;
    stallReq.set_open(true);
    stallReq.set_proxy_forge_fee_gold(100);
    stallReq.set_proxy_scroll_fee_gold(80);
    SetCraftProxyStallResponse stallResp;
    PlayerCraftSystem::SetProxyStall(master, stallReq, stallResp);
    ASSERT_EQ(0u, stallResp.error_message().id());

    // 模拟宗师下线离场。
    tlsEcs.playerList.erase(kMasterId);
    tlsEcs.actorRegistry.destroy(master);
    master = entt::null;

    // 2. 买家离线找宗师完成 1 单极品装备装条（手工费 100 -> 净 95）+ 1 单稀世技能书代制（手工费 80 -> 净 76）。
    GrantItem(buyer, kForgeMat, 40, false);
    GrantItem(buyer, equipcraft::kRareScrollPageConfigId, 1, false);
    GrantItem(buyer, equipcraft::kRareScrollInkConfigId, 1, true);

    EquipmentCraftChoice create;
    create.set_operation(EquipmentCraftChoice::CREATE_ITEM);
    create.set_config_id(3181);
    const auto created = CommitEquip(buyer, create);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, created.outcome());

    gForcedQualityRoll = 9999; // 5 档极品
    EquipmentCraftChoice install;
    install.set_operation(EquipmentCraftChoice::INSTALL_AFFIX);
    install.set_item_id(created.item_id());
    install.set_affix_slot(1);
    install.set_affix_id(103);
    install.set_proxy_master_player_id(kMasterId);
    const auto installed = CommitEquip(buyer, install);
    ASSERT_EQ(CommitEquipmentCraftResponse::APPLIED, installed.outcome());
    EXPECT_EQ(5u, installed.result_quality_tier());

    CraftSkillBookRequest rareReq;
    rareReq.set_mode(CraftSkillBookRequest::TARGETED_RARE);
    rareReq.set_scroll_id(2);
    rareReq.set_target_skill_table_id(10026);
    rareReq.set_expected_revision(0);
    rareReq.set_client_request_id(++nextKey);
    rareReq.set_proxy_master_player_id(kMasterId);
    CraftSkillBookResponse rareResp;
    PlayerCraftSystem::CraftSkillBook(buyer, rareReq, rareResp);
    ASSERT_EQ(0u, rareResp.error_message().id()) << rareResp.DebugString();

    // 3. 宗师尚未上线，但西市列表实时叠加待回账口碑与热榜：单数 2、极品 5、稀世 1、近 24h 2 单 1 极品、hot_rank = 1。
    ListCraftProxyStallsResponse listWhileOffline;
    PlayerCraftSystem::ListProxyStalls(buyer, listWhileOffline);
    ASSERT_EQ(1, listWhileOffline.stalls_size());
    const auto& offlineRow = listWhileOffline.stalls(0);
    EXPECT_EQ(kMasterId, offlineRow.master_player_id());
    EXPECT_EQ(2u, offlineRow.proxy_orders_completed());
    EXPECT_EQ(5u, offlineRow.supreme_forged_count());
    EXPECT_EQ(1u, offlineRow.rare_books_crafted_count());
    EXPECT_EQ(2u, offlineRow.recent_24h_orders());
    EXPECT_EQ(1u, offlineRow.recent_24h_supreme());
    EXPECT_EQ(1u, offlineRow.hot_rank());

    // 4. 宗师重新登录载入旧档（仍为 4 件极品）：先模拟存盘失败，验证回滚且待回账不丢失。
    master = SpawnPlayer(kMasterId, "欧冶子", 1000);
    auto& reloadedCraft = tlsEcs.actorRegistry.get<PlayerCraftComp>(master);
    reloadedCraft.set_forge_mastery_exp(60);
    reloadedCraft.set_scroll_mastery_exp(60);
    reloadedCraft.set_supreme_forged_count(4);
    reloadedCraft.set_proxy_stall_open(true);
    reloadedCraft.set_proxy_forge_fee_gold(100);
    reloadedCraft.set_proxy_scroll_fee_gold(80);

    PlayerCraftSystem::SetPersistForTest(&FailPersist);
    EXPECT_FALSE(PlayerCraftSystem::DrainPendingProxyReputation(master));
    EXPECT_EQ(0u, reloadedCraft.proxy_orders_completed());
    EXPECT_EQ(4u, reloadedCraft.supreme_forged_count());

    // 恢复存盘成功：GetState 自动触发回账，解锁百炼宗师(title=2)，并产出完整未读营业战报。
    PlayerCraftSystem::SetPersistForTest(&DummyPersist);
    GetSkillCraftingStateResponse masterState;
    ASSERT_EQ(kSuccess, PlayerCraftSystem::GetState(master, masterState));
    EXPECT_EQ(2u, masterState.state().proxy_orders_completed());
    EXPECT_EQ(5u, masterState.state().supreme_forged_count());
    EXPECT_EQ(1u, masterState.state().rare_books_crafted_count());
    const auto& titles = masterState.state().unlocked_craft_title_ids();
    EXPECT_NE(titles.end(), std::find(titles.begin(), titles.end(), 2u));
    ASSERT_TRUE(masterState.state().has_unread_proxy_digest());
    const auto& digest = masterState.state().unread_proxy_digest();
    EXPECT_EQ(2u, digest.orders_completed());
    EXPECT_EQ(1u, digest.supreme_forged_count());
    EXPECT_EQ(1u, digest.rare_books_crafted_count());
    EXPECT_EQ(171u, digest.net_gold_earned());
    EXPECT_EQ("侠客甲", digest.last_buyer_name());
    EXPECT_EQ(1u << 1u, digest.newly_unlocked_title_mask());

    // 再次 GetState 不重复累加。
    GetSkillCraftingStateResponse secondState;
    ASSERT_EQ(kSuccess, PlayerCraftSystem::GetState(master, secondState));
    EXPECT_EQ(2u, secondState.state().proxy_orders_completed());
    EXPECT_EQ(2u, secondState.state().unread_proxy_digest().orders_completed());

    // 5. AckCraftProxyDigest 幂等清零未读战报。
    AckCraftProxyDigestRequest ackReq;
    AckCraftProxyDigestResponse ackResp;
    PlayerCraftSystem::AckProxyDigest(master, ackReq, ackResp);
    ASSERT_EQ(0u, ackResp.error_message().id());
    EXPECT_FALSE(ackResp.state().has_unread_proxy_digest());
    AckCraftProxyDigestResponse ackReplay;
    PlayerCraftSystem::AckProxyDigest(master, ackReq, ackReplay);
    ASSERT_EQ(0u, ackReplay.error_message().id());
    EXPECT_FALSE(ackReplay.state().has_unread_proxy_digest());

    // 6. 西市极品热榜 Top 3 排序与零成交不入榜校验。
    auto addRemoteMaster = [](uint64_t id, uint32_t orders24h, uint32_t supreme24h, uint32_t totalOrders) {
        CraftProxyStallInfo info;
        info.set_master_player_id(id);
        info.set_master_name("名匠" + std::to_string(id));
        info.set_forge_mastery_tier(equipcraft::kMasteryMaster);
        info.set_proxy_forge_fee_gold(80);
        info.set_proxy_orders_completed(totalOrders);
        PlayerCraftSystem::UpsertProxyStallForTest(info);
        for (uint32_t i = 0; i < orders24h; ++i) {
            PlayerCraftSystem::RecordProxyOrderReputation(id, i < supreme24h ? 1u : 0u, 0u, 76u, "路人");
        }
    };
    addRemoteMaster(920001, 2, 2, 0); // score = 2*3 + 2 = 8 -> Rank 1
    addRemoteMaster(920002, 3, 1, 0); // score = 1*3 + 3 = 6 -> Rank 2
    // kMasterId: score = 1*3 + 2 = 5 -> Rank 3
    addRemoteMaster(920003, 1, 0, 0); // score = 1 -> Rank 0 (4th)
    addRemoteMaster(920004, 0, 0, 0); // score = 0, no orders -> Rank 0

    ListCraftProxyStallsResponse rankedList;
    PlayerCraftSystem::ListProxyStalls(buyer, rankedList);
    std::map<uint64_t, uint32_t> ranks;
    for (const auto& s : rankedList.stalls()) {
        ranks[s.master_player_id()] = s.hot_rank();
    }
    EXPECT_EQ(1u, ranks[920001]);
    EXPECT_EQ(2u, ranks[920002]);
    EXPECT_EQ(3u, ranks[kMasterId]);
    EXPECT_EQ(0u, ranks[920003]);
    EXPECT_EQ(0u, ranks[920004]);
}

} // namespace
