// V1 装备按模板选择独立强化规则；旧装备、空词条与既有宝石不因新规则被改写。
// 通过正式表、装备编排、显示与背包存盘接口验证，不复用被测放大公式计算期望值。
#include <gtest/gtest.h>

#include <array>
#include <cstdint>
#include <functional>
#include <limits>
#include <string>

#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/bag_marshal.h"
#include "services/scene/player/system/player_equip.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixrule_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipattributecap_table.h"
#include "table/code/equipenhance_table.h"
#include "table/code/equipgem_table.h"
#include "table/code/equipgemsocket_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr uint32_t kV1Stone = 3002;
constexpr uint32_t kLegacyStone = 2001;
constexpr uint32_t kV1Weapon = 3181;
constexpr uint32_t kV1Hat = 3182;
constexpr uint32_t kV1Clothes = 3183;
constexpr uint32_t kV1Shoes = 3184;
constexpr uint32_t kAttackGem = 2101;
constexpr uint32_t kAttrDamage = 1;
constexpr uint32_t kAttrDefense = 8;
constexpr uint32_t kAttrSpeed = 11;
constexpr Guid kPlayerGuid = 737373;
constexpr Guid kEquipmentGuid = (uint64_t{1} << 60) + 37001;
constexpr Guid kOtherEquipmentGuid = kEquipmentGuid + 1;
constexpr Guid kStoneGuid = kEquipmentGuid + 20;
constexpr Guid kGemGuid = kEquipmentGuid + 21;
constexpr uint32_t kStoneSlot = 20;
constexpr std::array<uint32_t, 10> kV1Costs{2, 3, 4, 5, 6, 7, 9, 11, 13, 15};
constexpr std::array<uint32_t, 12> kLegacyBonuses{
    500, 1000, 1500, 2000, 2600, 3200, 3800, 4500, 5200, 6000, 7000, 8000};

bool ArmV1ItemSegment(uint64_t first, uint64_t last)
{
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false;
    options.kindName = "item";
    options.initialStep = 100000;
    if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
                       [](GuidSegmentClient::TimerKind, double, std::function<void()>) {},
                       [] { return 0.0; })) return false;
    client.Warm();
    client.OnResponse(0, first, last);
    return client.IsReady();
}

ItemComp V1Instance(Guid guid, uint32_t config, uint32_t level = 0)
{
    ItemComp item;
    item.set_item_id(guid);
    item.set_config_id(config);
    item.set_size(1);
    item.mutable_equip()->set_enhance_level(level);
    return item;
}

const EquipAttrLineInfo* BaseLine(const BagItemInfo& info, uint32_t attr)
{
    for (const auto& line : info.base_attrs())
        if (line.attr_id() == attr) return &line;
    return nullptr;
}

class PlayerEquipV1Test : public testing::Test
{
protected:
    static void LoadTables()
    {
        ItemTableManager::Instance().Load();
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

    void SetUp() override
    {
        LoadTables();
        ASSERT_TRUE(ArmV1ItemSegment(3700000, 3800000));
        BagService::SetItemInstanceInitializer({});
        PlayerEquipSystem::SetRandomSourceForTest({});
        for (const auto config : {kV1Weapon, kV1Hat, kV1Clothes, kV1Shoes, kV1Stone})
            ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(config).first)
                << "V1 source tables must be exported before this test: config=" << config;

        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayerGuid);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 1;
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(kPlayerGuid);
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(80);
        tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player);
        auto& base = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player);
        base.set_health(100);
        base.set_mana(50);
        tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(player);
        tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        SetGold(0);
    }

    void TearDown() override
    {
        BagService::SetItemInstanceInitializer({});
        PlayerEquipSystem::SetRandomSourceForTest({});
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        // 个别用例仅修改内存表行；每条用例结束重新加载，避免依赖执行顺序。
        LoadTables();
        EXPECT_TRUE(ArmV1ItemSegment(1, uint64_t{1} << 54));
    }

    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    Bag& Equipment() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kEquipment]; }

    void SetGold(uint64_t amount)
    {
        auto& currency = tlsEcs.actorRegistry.get<CurrencyComp>(player);
        currency.clear_values();
        for (uint32_t kind = 0; kind < kCurrencyMax; ++kind)
            currency.add_values(kind == kCurrencyGold ? amount : 0);
    }

    uint64_t Gold() { return CurrencySystem::GetBalance(player, kCurrencyGold); }

    void StockStones(uint32_t count, uint32_t config = kV1Stone)
    {
        Inventory().InsertItemForRestore(kStoneGuid, config, count, kStoneSlot);
        ASSERT_EQ(count, Inventory().GetTotalItemCount(config));
    }

    std::string BagBytes()
    {
        BagAllData saved;
        bag_marshal::Marshal(player, saved);
        return saved.SerializeAsString();
    }

    BagItemInfo Display(const ItemComp& item)
    {
        BagItemInfo info;
        PlayerEquipSystem::FillItemDisplay(item, info);
        return info;
    }

    uint64_t Bonus(equiprules::DerivedStat stat)
    {
        equiprules::EquipBonus result;
        PlayerEquipSystem::CollectBonus(player, result);
        return equiprules::DerivedFlatOf(result, stat);
    }

    void ExpectInterchangeablePair(uint32_t profile, uint32_t kind, uint32_t firstSlot)
    {
        // 首饰和法宝正式模板尚未开放，用内存夹具验证类别契约，不预留新的正式 ID。
        auto* row = const_cast<ItemTable*>(ItemTableManager::Instance().FindByIdSilent(kV1Weapon).first);
        ASSERT_NE(nullptr, row);
        row->set_equip_profile(profile);
        row->set_equip_kind(kind);
        row->clear_base_attr();
        ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Weapon)));
        ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kOtherEquipmentGuid, kV1Weapon)));
        ASSERT_NO_FATAL_FAILURE(StockStones(75));
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kEquipmentGuid, firstSlot));
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kOtherEquipmentGuid, firstSlot + 1));
        EXPECT_EQ(kEquipmentGuid, Equipment().Layout().At(firstSlot));
        EXPECT_EQ(kOtherEquipmentGuid, Equipment().Layout().At(firstSlot + 1));
        EXPECT_EQ(2u, Equipment().OccupiedGridCount());
        const auto before = BagBytes();
        for (const Guid guid : {kEquipmentGuid, kOtherEquipmentGuid})
        {
            SCOPED_TRACE(guid);
            PlayerEquipSystem::EnhanceOutcome result;
            EXPECT_EQ(kEquipEnhanceUnavailable, PlayerEquipSystem::Enhance(player, guid, 0, result));
            const auto* item = Equipment().GetItemCompByGuid(guid);
            ASSERT_NE(nullptr, item);
            const auto info = Display(*item);
            EXPECT_EQ(0u, info.enhance_max_level());
            EXPECT_FALSE(info.has_enhance_next());
        }
        EXPECT_EQ(before, BagBytes());
        // 同名双件合法不意味着一个实例可以同时占两槽。
        EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Equip(player, kEquipmentGuid, firstSlot + 1));
        EXPECT_EQ(before, BagBytes());
        EXPECT_TRUE(Inventory().IsLayerConsistent());
        EXPECT_TRUE(Equipment().IsLayerConsistent());
    }

    entt::entity player{entt::null};
};

TEST_F(PlayerEquipV1Test, FormalTemplatesCoverNineLevelsAndKeepLegacyProfilesSeparate)
{
    const std::array<uint32_t, 9> levels{1, 10, 20, 30, 40, 50, 60, 70, 80};
    for (std::size_t index = 0; index < levels.size(); ++index)
    {
        for (uint32_t part = 1; part <= 4; ++part)
        {
            const auto config = 3100u + static_cast<uint32_t>(index) * 10u + part;
            SCOPED_TRACE(config);
            const auto* row = ItemTableManager::Instance().FindByIdSilent(config).first;
            ASSERT_NE(nullptr, row);
            EXPECT_EQ(1u, row->equip_profile());
            EXPECT_EQ(10u + part, row->equip_kind());
            EXPECT_EQ(levels[index], row->equip_level());
            EXPECT_EQ(1u, row->max_stack_size());
            EXPECT_EQ(0u, row->affix_pool());
            EXPECT_EQ(0u, row->affix_rule());
            const auto info = Display(V1Instance(kEquipmentGuid, config));
            const auto* main = BaseLine(info, part == 1 ? kAttrDamage : kAttrDefense);
            ASSERT_NE(nullptr, main);
            const uint32_t multiplier = part == 1 ? 20u : part == 3 ? 12u : 6u;
            EXPECT_EQ(multiplier * (levels[index] + 1u), main->value());
            if (part == 4)
            {
                const auto* speed = BaseLine(info, kAttrSpeed);
                ASSERT_NE(nullptr, speed);
                EXPECT_EQ(4u * levels[index] + 8u, speed->value());
            }
        }
    }
    for (const auto config : {1101u, 1201u, 1301u, 1401u})
    {
        const auto* row = ItemTableManager::Instance().FindByIdSilent(config).first;
        ASSERT_NE(nullptr, row);
        EXPECT_EQ(0u, row->equip_profile());
    }
}

TEST_F(PlayerEquipV1Test, FourBasicPiecesEachReachTenWithExactlySeventyFiveStonesAndNoGold)
{
    SetGold(12345);
    ASSERT_NO_FATAL_FAILURE(StockStones(300));
    // 即使随机源返回区间内最大值，V1 每一级也必须成功。
    PlayerEquipSystem::SetRandomSourceForTest([](uint64_t bound) { return bound - 1; });
    uint32_t spent = 0;
    const std::array<uint32_t, 4> configs{kV1Weapon, kV1Hat, kV1Clothes, kV1Shoes};
    for (std::size_t part = 0; part < configs.size(); ++part)
    {
        SCOPED_TRACE(configs[part]);
        const Guid guid = kEquipmentGuid + part;
        ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(guid, configs[part])));
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, guid, 3u + static_cast<uint32_t>(part)));
        const auto beforePiece = spent;
        for (uint32_t level = 0; level < kV1Costs.size(); ++level)
        {
            SCOPED_TRACE(level);
            const auto* current = Equipment().GetItemCompByGuid(guid);
            ASSERT_NE(nullptr, current);
            const auto info = Display(*current);
            ASSERT_TRUE(info.has_enhance_next());
            EXPECT_EQ(11u, info.enhance_max_level());
            EXPECT_EQ(level + 1, info.enhance_next().next_level());
            EXPECT_EQ(10000u, info.enhance_next().success_rate());
            EXPECT_EQ(0u, info.enhance_next().pity_limit());
            EXPECT_EQ(0u, info.enhance_next().cost_gold());
            EXPECT_EQ(kV1Stone, info.enhance_next().material_config_id());
            EXPECT_EQ(kV1Costs[level], info.enhance_next().material_count());
            EXPECT_EQ(200u * (level + 1u), info.enhance_next().bonus_permyriad());
            PlayerEquipSystem::EnhanceOutcome result;
            ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, guid, level, result));
            EXPECT_TRUE(result.success);
            EXPECT_EQ(level + 1, result.level);
            spent += kV1Costs[level];
            EXPECT_EQ(300u - spent, Inventory().GetTotalItemCount(kV1Stone));
            EXPECT_EQ(12345u, Gold());
        }
        EXPECT_EQ(75u, spent - beforePiece);
    }
    EXPECT_EQ(300u, spent);
    EXPECT_EQ(4u, Equipment().OccupiedGridCount());
    EXPECT_TRUE(Inventory().IsLayerConsistent());
    EXPECT_TRUE(Equipment().IsLayerConsistent());
    EXPECT_EQ(1944u, Bonus(equiprules::DerivedStat::kPhysicalAttack));
    EXPECT_EQ(1944u, Bonus(equiprules::DerivedStat::kMagicAttack));
    EXPECT_EQ(2332u, Bonus(equiprules::DerivedStat::kDefense));
    EXPECT_EQ(328u, Bonus(equiprules::DerivedStat::kSpeed));
}

TEST_F(PlayerEquipV1Test, V1CanEnhanceWithZeroGold)
{
    ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Weapon)));
    ASSERT_NO_FATAL_FAILURE(StockStones(2));
    ASSERT_EQ(0u, Gold());
    PlayerEquipSystem::EnhanceOutcome result;
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 0, result));
    EXPECT_TRUE(result.success);
    EXPECT_EQ(1u, result.level);
    EXPECT_EQ(0u, Gold());
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kV1Stone));
}

TEST_F(PlayerEquipV1Test, InsufficientStonesLeaveBothBagsGoldAndLevelUnchanged)
{
    ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Weapon)));
    ASSERT_NO_FATAL_FAILURE(StockStones(1));
    SetGold(99999);
    const auto before = BagBytes();
    PlayerEquipSystem::EnhanceOutcome result;
    EXPECT_EQ(kEquipEnhanceMaterialNotEnough, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 0, result));
    EXPECT_EQ(before, BagBytes());
    EXPECT_EQ(99999u, Gold());
}

TEST_F(PlayerEquipV1Test, LegacyStonesCannotPayForV1Enhancement)
{
    ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Weapon)));
    ASSERT_NO_FATAL_FAILURE(StockStones(75, kLegacyStone));
    const auto before = BagBytes();
    PlayerEquipSystem::EnhanceOutcome result;
    EXPECT_EQ(kEquipEnhanceMaterialNotEnough, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 0, result));
    EXPECT_EQ(before, BagBytes());
}

TEST_F(PlayerEquipV1Test, BattleUsableMaterialSuppressesPreviewAndRejectsEnhancement)
{
    const auto item = V1Instance(kEquipmentGuid, kV1Weapon);
    ASSERT_EQ(kSuccess, Inventory().PutInstance(item));
    ASSERT_NO_FATAL_FAILURE(StockStones(75));
    auto* material = const_cast<ItemTable*>(ItemTableManager::Instance().FindByIdSilent(kV1Stone).first);
    ASSERT_NE(nullptr, material);
    material->set_battle_usable(1);
    const auto before = BagBytes();
    EXPECT_FALSE(Display(item).has_enhance_next());
    PlayerEquipSystem::EnhanceOutcome result;
    EXPECT_NE(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 0, result));
    EXPECT_EQ(before, BagBytes());
    EXPECT_EQ(0u, Gold());
}

TEST_F(PlayerEquipV1Test, InvalidV1RuleSuppressesPreviewAndRejectsWithoutCharging)
{
    const auto item = V1Instance(kEquipmentGuid, kV1Weapon);
    ASSERT_EQ(kSuccess, Inventory().PutInstance(item));
    ASSERT_NO_FATAL_FAILURE(StockStones(75));
    SetGold(99999);
    auto* row = const_cast<EquipEnhanceTable*>(EquipEnhanceTableManager::Instance().FindByIdSilent(1001).first);
    ASSERT_NE(nullptr, row);
    const EquipEnhanceTable original(*row);
    const auto before = BagBytes();
    for (const auto field : {"success_rate", "cost_gold", "bonus_permyriad"})
    {
        SCOPED_TRACE(field);
        row->CopyFrom(original);
        if (std::string(field) == "success_rate") row->set_success_rate(9999);
        else if (std::string(field) == "cost_gold") row->set_cost_gold(1);
        else row->set_bonus_permyriad(201);
        EXPECT_FALSE(Display(item).has_enhance_next());
        PlayerEquipSystem::EnhanceOutcome result;
        EXPECT_NE(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 0, result));
        EXPECT_EQ(before, BagBytes());
        EXPECT_EQ(99999u, Gold());
    }
}

TEST_F(PlayerEquipV1Test, MaximumLevelHasNoNextPreviewAndCostsNothing)
{
    const auto item = V1Instance(kEquipmentGuid, kV1Weapon, 11);
    ASSERT_EQ(kSuccess, Inventory().PutInstance(item));
    ASSERT_NO_FATAL_FAILURE(StockStones(75));
    const auto info = Display(item);
    EXPECT_EQ(11u, info.enhance_level());
    EXPECT_EQ(11u, info.enhance_max_level());
    EXPECT_EQ(2400u, info.enhance_bonus_permyriad());
    EXPECT_FALSE(info.has_enhance_next());
    const auto before = BagBytes();
    PlayerEquipSystem::EnhanceOutcome result;
    EXPECT_EQ(kEquipEnhanceMaxLevel, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 11, result));
    EXPECT_EQ(before, BagBytes());
}

TEST_F(PlayerEquipV1Test, TopLevelElevenUsesProbabilityWithHardPityNoDowngradeAndPersistsProgress)
{
    ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Weapon, 10)));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kEquipmentGuid, 3u));
    // 20 次 × 30 强化石 = 600 强化石，足够覆盖完整保底周期。
    ASSERT_NO_FATAL_FAILURE(StockStones(600));
    // 让随机源总是返回 9999（10% 概率不中）。
    PlayerEquipSystem::SetRandomSourceForTest([](uint64_t bound) { return bound - 1; });

    for (uint32_t attempt = 0; attempt < 19; ++attempt)
    {
        SCOPED_TRACE(attempt);
        const auto* current = Equipment().GetItemCompByGuid(kEquipmentGuid);
        ASSERT_NE(nullptr, current);
        const auto info = Display(*current);
        ASSERT_TRUE(info.has_enhance_next());
        EXPECT_EQ(11u, info.enhance_next().next_level());
        EXPECT_EQ(1000u, info.enhance_next().success_rate());
        EXPECT_EQ(2400u, info.enhance_next().bonus_permyriad());
        EXPECT_EQ(30u, info.enhance_next().material_count());
        EXPECT_EQ(20u, info.enhance_next().pity_limit());
        EXPECT_EQ(attempt, info.enhance_next().current_pity_count());
        EXPECT_FALSE(info.enhance_next().pity_guaranteed_next());

        // 传入过期 expected_pity_count 必须被拒绝且不扣强化石。
        if (attempt > 0)
        {
            const auto stonesBefore = Inventory().GetTotalItemCount(kV1Stone);
            PlayerEquipSystem::EnhanceOutcome stale;
            EXPECT_EQ(kEquipEnhanceLevelChanged,
                PlayerEquipSystem::Enhance(player, kEquipmentGuid, 10, stale, attempt - 1));
            EXPECT_EQ(stonesBefore, Inventory().GetTotalItemCount(kV1Stone));
        }

        PlayerEquipSystem::EnhanceOutcome outcome;
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 10, outcome, attempt));
        EXPECT_FALSE(outcome.success);
        EXPECT_FALSE(outcome.triggeredByPity);
        EXPECT_EQ(10u, outcome.level); // 失败不掉级
        EXPECT_EQ(attempt + 1, outcome.pityCount);
        EXPECT_EQ(20u, outcome.pityLimit);
    }

    // 存盘往返后，19/20 的保底进度必须原样保留。
    BagAllData saved;
    bag_marshal::Marshal(player, saved);
    BagAllData cold;
    ASSERT_TRUE(cold.ParseFromString(saved.SerializeAsString()));
    bag_marshal::Unmarshal(player, cold);
    const auto* restored = Equipment().GetItemCompByGuid(kEquipmentGuid);
    ASSERT_NE(nullptr, restored);
    const auto prePityInfo = Display(*restored);
    ASSERT_TRUE(prePityInfo.has_enhance_next());
    EXPECT_EQ(19u, prePityInfo.enhance_pity_count());
    EXPECT_EQ(19u, prePityInfo.enhance_next().current_pity_count());
    EXPECT_TRUE(prePityInfo.enhance_next().pity_guaranteed_next());

    // 第 20 次即使随机源继续返回最大值也必成，保底清零，武器基础伤害放大 +24%（1620 * 1.24 = 2008）。
    PlayerEquipSystem::EnhanceOutcome finalOutcome;
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 10, finalOutcome, 19u));
    EXPECT_TRUE(finalOutcome.success);
    EXPECT_TRUE(finalOutcome.triggeredByPity);
    EXPECT_EQ(11u, finalOutcome.level);
    EXPECT_EQ(0u, finalOutcome.pityCount);
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kV1Stone));
    EXPECT_EQ(2008u, Bonus(equiprules::DerivedStat::kPhysicalAttack));
    EXPECT_EQ(2008u, Bonus(equiprules::DerivedStat::kMagicAttack));
}

TEST_F(PlayerEquipV1Test, TopLevelElevenCanSucceedEarlyAndResetPity)
{
    auto item = V1Instance(kEquipmentGuid, kV1Weapon, 10);
    item.mutable_equip()->set_enhance_pity_count(5);
    ASSERT_EQ(kSuccess, Inventory().PutInstance(item));
    ASSERT_NO_FATAL_FAILURE(StockStones(30));
    // 随机源返回 0 (< 1000)，10% 直接提前成功。
    PlayerEquipSystem::SetRandomSourceForTest([](uint64_t) { return uint64_t{0}; });
    PlayerEquipSystem::EnhanceOutcome outcome;
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 10, outcome, 5u));
    EXPECT_TRUE(outcome.success);
    EXPECT_FALSE(outcome.triggeredByPity);
    EXPECT_EQ(11u, outcome.level);
    EXPECT_EQ(0u, outcome.pityCount);
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kV1Stone));
}

TEST_F(PlayerEquipV1Test, ReplayingThePreviousLevelCannotConsumeAnotherUpgrade)
{
    ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Weapon)));
    ASSERT_NO_FATAL_FAILURE(StockStones(75));
    PlayerEquipSystem::EnhanceOutcome result;
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 0, result));
    const auto before = BagBytes();
    EXPECT_EQ(kEquipEnhanceLevelChanged, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 0, result));
    EXPECT_EQ(before, BagBytes());
    EXPECT_EQ(73u, Inventory().GetTotalItemCount(kV1Stone));
}

TEST_F(PlayerEquipV1Test, ShoesKeepSpeedAtEveryLevelAndPreviewMatchesAppliedDefense)
{
    ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Shoes)));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kEquipmentGuid, 6u));
    ASSERT_NO_FATAL_FAILURE(StockStones(105));
    // 让 +11 一发命中，验证满级鞋防御放大 +24% 而速度恒为 328。
    PlayerEquipSystem::SetRandomSourceForTest([](uint64_t) { return uint64_t{0}; });
    for (uint32_t level = 0; level <= 11; ++level)
    {
        SCOPED_TRACE(level);
        const auto* item = Equipment().GetItemCompByGuid(kEquipmentGuid);
        ASSERT_NE(nullptr, item);
        const auto info = Display(*item);
        const auto* speed = BaseLine(info, kAttrSpeed);
        const auto* defense = BaseLine(info, kAttrDefense);
        ASSERT_NE(nullptr, speed);
        ASSERT_NE(nullptr, defense);
        const uint32_t bonusBps = level <= 10 ? 200u * level : 2400u;
        const auto expectedDefense = 486u * (10000u + bonusBps) / 10000u;
        EXPECT_EQ(328u, speed->value());
        EXPECT_EQ(0u, speed->enhance_bonus());
        EXPECT_EQ(expectedDefense, defense->value());
        EXPECT_EQ(expectedDefense - 486u, defense->enhance_bonus());
        EXPECT_EQ(328u, Bonus(equiprules::DerivedStat::kSpeed));
        EXPECT_EQ(expectedDefense, Bonus(equiprules::DerivedStat::kDefense));
        if (level == 11)
        {
            EXPECT_FALSE(info.has_enhance_next());
            EXPECT_EQ(602u, defense->value());
            continue;
        }
        ASSERT_TRUE(info.has_enhance_next());
        EXPECT_EQ(328u, speed->enhance_next_value());
        const uint32_t nextBonusBps = (level + 1u) <= 10 ? 200u * (level + 1u) : 2400u;
        const auto expectedNext = 486u * (10000u + nextBonusBps) / 10000u;
        EXPECT_EQ(expectedNext, defense->enhance_next_value());
        PlayerEquipSystem::EnhanceOutcome result;
        ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, level, result));
        EXPECT_EQ(expectedNext, Bonus(equiprules::DerivedStat::kDefense));
    }
}

TEST_F(PlayerEquipV1Test, EveryLegacyLevelKeepsOriginalBaseScalingIncludingShoeSpeed)
{
    for (const uint32_t config : {1101u, 1201u, 1301u, 1401u})
    {
        const auto* row = ItemTableManager::Instance().FindByIdSilent(config).first;
        ASSERT_NE(nullptr, row);
        ASSERT_EQ(0u, row->equip_profile());
        for (uint32_t level = 0; level <= 12; ++level)
        {
            SCOPED_TRACE(config);
            SCOPED_TRACE(level);
            const auto info = Display(V1Instance(kEquipmentGuid, config, level));
            const uint32_t bonus = level == 0 ? 0 : kLegacyBonuses[level - 1];
            EXPECT_EQ(12u, info.enhance_max_level());
            EXPECT_EQ(level, info.enhance_level());
            EXPECT_EQ(bonus, info.enhance_bonus_permyriad());
            for (const auto& base : row->base_attr())
            {
                if (base.base_attr_id() == 0) continue;
                const auto* actual = BaseLine(info, base.base_attr_id());
                ASSERT_NE(nullptr, actual);
                const uint64_t raw = base.base_attr_value();
                EXPECT_EQ(raw * (10000u + bonus) / 10000u, actual->value());
                if (level < 12)
                {
                    ASSERT_TRUE(info.has_enhance_next());
                    EXPECT_EQ(raw * (10000u + kLegacyBonuses[level]) / 10000u, actual->enhance_next_value());
                    EXPECT_EQ(kLegacyStone, info.enhance_next().material_config_id());
                }
            }
            if (level == 12) EXPECT_FALSE(info.has_enhance_next());
        }
    }
}

TEST_F(PlayerEquipV1Test, SameNameJewelryInstancesCanUseBothSlotsButCannotEnhance)
{
    ASSERT_NO_FATAL_FAILURE(ExpectInterchangeablePair(2, 15, 7));
}

TEST_F(PlayerEquipV1Test, SameNameArtifactInstancesCanUseBothSlotsButCannotEnhance)
{
    ASSERT_NO_FATAL_FAILURE(ExpectInterchangeablePair(3, 16, 9));
}

TEST_F(PlayerEquipV1Test, UnknownProfileDoesNotFallBackToLegacyEnhancement)
{
    auto* row = const_cast<ItemTable*>(ItemTableManager::Instance().FindByIdSilent(kV1Weapon).first);
    ASSERT_NE(nullptr, row);
    row->set_equip_profile(std::numeric_limits<uint32_t>::max());
    ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Weapon)));
    ASSERT_NO_FATAL_FAILURE(StockStones(75));
    SetGold(99999);
    const auto before = BagBytes();
    PlayerEquipSystem::EnhanceOutcome result;
    EXPECT_NE(kSuccess, PlayerEquipSystem::Enhance(player, kEquipmentGuid, 0, result));
    EXPECT_EQ(before, BagBytes());
    EXPECT_EQ(99999u, Gold());
    const auto info = Display(*Inventory().GetItemCompByGuid(kEquipmentGuid));
    EXPECT_EQ(0u, info.enhance_max_level());
    EXPECT_FALSE(info.has_enhance_next());
}

TEST_F(PlayerEquipV1Test, EmptyAffixesRemainInitializedAfterGrantWearAndSaveRoundTrip)
{
    uint32_t randomCalls = 0;
    PlayerEquipSystem::SetRandomSourceForTest([&randomCalls](uint64_t) { ++randomCalls; return uint64_t{0}; });
    BagService::SetItemInstanceInitializer(PlayerEquipSystem::RollNewInstance);
    ASSERT_EQ(kSuccess, PlayerEquipSystem::GmGrantItem(player, kV1Weapon, 1));
    Guid granted = 0;
    Inventory().ForEachItem([&granted](Guid guid, const ItemComp& item)
    {
        if (item.config_id() == kV1Weapon) granted = guid;
    });
    ASSERT_NE(0u, granted);
    const auto* item = Inventory().GetItemCompByGuid(granted);
    ASSERT_NE(nullptr, item);
    ASSERT_TRUE(item->has_equip());
    EXPECT_EQ(0, item->equip().affixes_size());
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, granted, 3u));
    BagAllData saved;
    bag_marshal::Marshal(player, saved);
    BagAllData cold;
    ASSERT_TRUE(cold.ParseFromString(saved.SerializeAsString()));
    bag_marshal::Unmarshal(player, cold);
    auto* restored = Equipment().GetItemCompByGuid(granted);
    ASSERT_NE(nullptr, restored);
    ASSERT_TRUE(restored->has_equip());
    EXPECT_EQ(0, restored->equip().affixes_size());
    const auto before = restored->SerializeAsString();
    PlayerEquipSystem::RollNewInstance(*restored);
    EXPECT_EQ(before, restored->SerializeAsString());
    EXPECT_EQ(0u, randomCalls);
    EXPECT_EQ(granted, Equipment().Layout().At(3));
    EXPECT_EQ(1u, Equipment().OccupiedGridCount());
    EXPECT_TRUE(Equipment().IsLayerConsistent());
}

TEST_F(PlayerEquipV1Test, V1RejectsNewGemInlayWithoutConsumingGemOrChangingEquipment)
{
    ASSERT_EQ(kSuccess, Inventory().PutInstance(V1Instance(kEquipmentGuid, kV1Weapon)));
    Inventory().InsertItemForRestore(kGemGuid, kAttackGem, 1, 21);
    const auto before = BagBytes();
    EXPECT_NE(kSuccess, PlayerEquipSystem::InlayGem(player, kEquipmentGuid, 0, kAttackGem));
    EXPECT_EQ(before, BagBytes());
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(kAttackGem));
}

TEST_F(PlayerEquipV1Test, ExistingGemStillContributesAndCanBeRemovedFromV1Equipment)
{
    auto item = V1Instance(kEquipmentGuid, kV1Weapon);
    auto* gem = item.mutable_equip()->add_gems();
    gem->set_socket(0);
    gem->set_gem_config_id(kAttackGem);
    const auto* gemRow = EquipGemTableManager::Instance().FindByIdSilent(kAttackGem).first;
    ASSERT_NE(nullptr, gemRow);
    const auto gemValue = gemRow->value();
    ASSERT_EQ(kSuccess, Inventory().PutInstance(item));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kEquipmentGuid, 3u));
    EXPECT_EQ(1620u + gemValue, Bonus(equiprules::DerivedStat::kPhysicalAttack));
    EXPECT_EQ(1620u + gemValue, Bonus(equiprules::DerivedStat::kMagicAttack));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kEquipmentGuid, 0));
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(kAttackGem));
    EXPECT_EQ(1620u, Bonus(equiprules::DerivedStat::kPhysicalAttack));
    EXPECT_EQ(1620u, Bonus(equiprules::DerivedStat::kMagicAttack));
    const auto* kept = Equipment().GetItemCompByGuid(kEquipmentGuid);
    ASSERT_NE(nullptr, kept);
    EXPECT_EQ(0, kept->equip().gems_size());
}
} // namespace
