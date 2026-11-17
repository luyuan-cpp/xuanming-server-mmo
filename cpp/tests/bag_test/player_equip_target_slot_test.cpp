// 显式目标槽与旧装备栏容量恢复的契约测试;使用两条同 kind 旧夹具槽,不依赖新装备数值。
#include <gtest/gtest.h>

#include <cstdint>
#include <limits>
#include <memory>
#include <string>

#include "../../nodes/scene/handler/rpc/player/player_bag_handler.h"
#include "engine/core/error_handling/error_handling.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/scene/player_bag.pb.h"
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
constexpr uint32_t kSameTemplate = 1;
constexpr Guid kFirstGuid = 91001;
constexpr Guid kSecondGuid = 91002;
constexpr Guid kIncomingGuid = 91003;

ItemComp EquipmentInstance(Guid guid)
{
    ItemComp item;
    item.set_item_id(guid);
    item.set_config_id(kSameTemplate);
    item.set_size(1);
    item.mutable_equip()->set_enhance_level(2);
    return item;
}

class TargetSlotBagService : public SceneBagClientPlayer {};

class PlayerEquipTargetSlotTest : public testing::Test
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
        const auto* item = ItemTableManager::Instance().FindByIdSilent(kSameTemplate).first;
        ASSERT_NE(nullptr, item);
        ASSERT_EQ(1u, item->equip_kind());
        ASSERT_EQ(1u, item->max_stack_size());
        for (const uint32_t slot : {0u, 1u})
        {
            const auto* row = EquipSlotTableManager::Instance().FindByIdSilent(slot).first;
            ASSERT_NE(nullptr, row);
            ASSERT_EQ(1u, row->equip_kind());
        }

        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{91000});
        tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(80);
        tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player);
        auto& attributes = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player);
        attributes.set_health(100);
        attributes.set_mana(50);
        tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(player);
        tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player);
    }

    void TearDown() override
    {
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        // 少数容量用例改了槽表行;恢复进程级表,不影响其它测试。
        EquipSlotTableManager::Instance().Load();
    }

    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    Bag& Equipment() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kEquipment]; }
    BagAllData Snapshot()
    {
        BagAllData result;
        bag_marshal::Marshal(player, result);
        return result;
    }

    entt::entity player{entt::null};
    SceneBagClientPlayerHandler handler{std::make_unique<TargetSlotBagService>()};
};

TEST_F(PlayerEquipTargetSlotTest, FormalLayoutExposesEightSlotsAndInterchangeablePairs)
{
    BagLayoutInfo layout;
    layout.set_capacity(static_cast<uint32_t>(Equipment().Capacity()));
    PlayerEquipSystem::FillEquipSlots(layout);
    ASSERT_EQ(8, layout.equip_slots_size());
    for (int i = 0; i < layout.equip_slots_size(); ++i)
    {
        EXPECT_EQ(static_cast<uint32_t>(i + 3), layout.equip_slots(i).slot());
        EXPECT_FALSE(layout.equip_slots(i).name().empty());
    }
    EXPECT_EQ(15u, layout.equip_slots(4).equip_kind());
    EXPECT_EQ(15u, layout.equip_slots(5).equip_kind());
    EXPECT_EQ(16u, layout.equip_slots(6).equip_kind());
    EXPECT_EQ(16u, layout.equip_slots(7).equip_kind());
}

TEST_F(PlayerEquipTargetSlotTest, ExplicitSecondSlotDoesNotUseEmptyFirstSlot)
{
    ASSERT_EQ(kSuccess, Inventory().PutInstance(EquipmentInstance(kIncomingGuid)));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kIncomingGuid, 1u));
    EXPECT_EQ(kInvalidGuid, Equipment().Layout().At(0));
    EXPECT_EQ(kIncomingGuid, Equipment().Layout().At(1));
}

TEST_F(PlayerEquipTargetSlotTest, ExplicitOccupiedSlotIsReplacedEvenWhenAnotherSlotIsEmpty)
{
    ASSERT_EQ(kSuccess, Equipment().PutInstance(EquipmentInstance(kSecondGuid), 1u));
    ASSERT_EQ(kSuccess, Inventory().PutInstance(EquipmentInstance(kIncomingGuid)));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kIncomingGuid, 1u));
    EXPECT_EQ(kInvalidGuid, Equipment().Layout().At(0));
    EXPECT_EQ(kIncomingGuid, Equipment().Layout().At(1));
    EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(kSecondGuid));
}

TEST_F(PlayerEquipTargetSlotTest, SameTemplateInstancesCoexistAndOnlyChosenOccupantIsReplaced)
{
    ASSERT_EQ(kSuccess, Equipment().PutInstance(EquipmentInstance(kFirstGuid), 0u));
    ASSERT_EQ(kSuccess, Equipment().PutInstance(EquipmentInstance(kSecondGuid), 1u));
    const ItemComp incoming = EquipmentInstance(kIncomingGuid);
    ASSERT_EQ(kSuccess, Inventory().PutInstance(incoming));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kIncomingGuid, 1u));
    EXPECT_EQ(kFirstGuid, Equipment().Layout().At(0));
    EXPECT_EQ(kIncomingGuid, Equipment().Layout().At(1));
    EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(kSecondGuid));
    const auto* stored = Equipment().GetItemCompByGuid(kIncomingGuid);
    ASSERT_NE(nullptr, stored);
    EXPECT_EQ(incoming.equip().SerializeAsString(), stored->equip().SerializeAsString());
    EXPECT_TRUE(Inventory().IsLayerConsistent());
    EXPECT_TRUE(Equipment().IsLayerConsistent());
}

TEST_F(PlayerEquipTargetSlotTest, InvalidOrMismatchedSlotRejectsWithoutChangingEitherBag)
{
    ASSERT_EQ(kSuccess, Equipment().PutInstance(EquipmentInstance(kFirstGuid), 0u));
    ASSERT_EQ(kSuccess, Inventory().PutInstance(EquipmentInstance(kIncomingGuid)));
    const std::string before = Snapshot().SerializeAsString();
    // 槽 2 存在但 kind 不匹配;容量边界及 UINT32_MAX 均不能回退到自动选槽。
    for (const uint32_t slot : {2u, static_cast<uint32_t>(Equipment().Capacity()),
                               std::numeric_limits<uint32_t>::max()})
    {
        EXPECT_EQ(kEquipNoSlot, PlayerEquipSystem::Equip(player, kIncomingGuid, slot));
        EXPECT_EQ(before, Snapshot().SerializeAsString());
    }
}

TEST_F(PlayerEquipTargetSlotTest, ExistingSlotOutsideCurrentCapacityRejectsBeforeMutation)
{
    Equipment().SetCapacityForRestore(1);
    ASSERT_EQ(kSuccess, Inventory().PutInstance(EquipmentInstance(kIncomingGuid)));
    const std::string before = Snapshot().SerializeAsString();
    EXPECT_EQ(kEquipNoSlot, PlayerEquipSystem::Equip(player, kIncomingGuid, 1u));
    EXPECT_EQ(before, Snapshot().SerializeAsString());
}

TEST_F(PlayerEquipTargetSlotTest, OmittedSlotStillUsesFirstEmptyThenReplacesLowest)
{
    for (const Guid guid : {kFirstGuid, kSecondGuid, kIncomingGuid})
        ASSERT_EQ(kSuccess, Inventory().PutInstance(EquipmentInstance(guid)));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kFirstGuid));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kSecondGuid));
    EXPECT_EQ(kFirstGuid, Equipment().Layout().At(0));
    EXPECT_EQ(kSecondGuid, Equipment().Layout().At(1));
    ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kIncomingGuid));
    EXPECT_EQ(kIncomingGuid, Equipment().Layout().At(0));
    EXPECT_EQ(kSecondGuid, Equipment().Layout().At(1));
    EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(kFirstGuid));
}

TEST_F(PlayerEquipTargetSlotTest, HandlerDistinguishesExplicitZeroFromOmittedSlot)
{
    ASSERT_EQ(kSuccess, Equipment().PutInstance(EquipmentInstance(kFirstGuid), 0u));
    ASSERT_EQ(kSuccess, Inventory().PutInstance(EquipmentInstance(kIncomingGuid)));
    EquipItemRequest request;
    request.set_item_id(kIncomingGuid);
    request.set_target_slot(0);
    ASSERT_TRUE(request.has_target_slot());
    EquipItemResponse response;
    handler.EquipItem(player, &request, &response);
    EXPECT_FALSE(response.has_error_message());
    EXPECT_EQ(kIncomingGuid, Equipment().Layout().At(0));
    EXPECT_EQ(kInvalidGuid, Equipment().Layout().At(1));
    EXPECT_TRUE(response.has_bag());
    EXPECT_TRUE(response.has_equipment());
}

TEST_F(PlayerEquipTargetSlotTest, HandlerWithoutSlotKeepsLegacySelection)
{
    ASSERT_EQ(kSuccess, Equipment().PutInstance(EquipmentInstance(kFirstGuid), 0u));
    ASSERT_EQ(kSuccess, Inventory().PutInstance(EquipmentInstance(kIncomingGuid)));
    EquipItemRequest request;
    request.set_item_id(kIncomingGuid);
    ASSERT_FALSE(request.has_target_slot());
    EquipItemResponse response;
    handler.EquipItem(player, &request, &response);
    EXPECT_FALSE(response.has_error_message());
    EXPECT_EQ(kFirstGuid, Equipment().Layout().At(0));
    EXPECT_EQ(kIncomingGuid, Equipment().Layout().At(1));
}

TEST_F(PlayerEquipTargetSlotTest, OldCapacityGrowsAndDuplicateTemplateSlotsSurviveRepeatedRoundTrips)
{
    ASSERT_GE(Equipment().Capacity(), 11u);
    ASSERT_EQ(kSuccess, Equipment().PutInstance(EquipmentInstance(kFirstGuid), 0u));
    ASSERT_EQ(kSuccess, Equipment().PutInstance(EquipmentInstance(kSecondGuid), 1u));
    BagAllData old = Snapshot();
    old.set_capacities(kEquipment, 10);
    bag_marshal::Unmarshal(player, old);
    EXPECT_GE(Equipment().Capacity(), 11u);
    const BagAllData expanded = Snapshot();
    bag_marshal::Unmarshal(player, expanded);
    EXPECT_EQ(expanded.SerializeAsString(), Snapshot().SerializeAsString());
    EXPECT_EQ(kFirstGuid, Equipment().Layout().At(0));
    EXPECT_EQ(kSecondGuid, Equipment().Layout().At(1));
    ASSERT_NE(nullptr, Equipment().GetItemCompByGuid(kFirstGuid));
    ASSERT_NE(nullptr, Equipment().GetItemCompByGuid(kSecondGuid));
    EXPECT_EQ(2u, Equipment().GetItemCompByGuid(kSecondGuid)->equip().enhance_level());
}

TEST_F(PlayerEquipTargetSlotTest, LargerSavedCapacityIsPreserved)
{
    BagAllData saved = Snapshot();
    saved.set_capacities(kEquipment, 40);
    bag_marshal::Unmarshal(player, saved);
    EXPECT_EQ(40u, Equipment().Capacity());
}

TEST_F(PlayerEquipTargetSlotTest, FormalSlotBeyondDefaultExtendsCapacityEvenWhenSnapshotOmitsCapacities)
{
    const auto* row = EquipSlotTableManager::Instance().FindByIdSilent(6).first;
    ASSERT_NE(nullptr, row);
    ASSERT_FALSE(row->name().empty());
    // 恢复按表中的正式槽需求计算,而非把八槽的当前数字写死在恢复函数里。
    const_cast<EquipSlotTable*>(row)->set_id(18);
    BagAllData saved;
    bag_marshal::Unmarshal(player, saved);
    EXPECT_EQ(19u, Equipment().Capacity());
}

TEST_F(PlayerEquipTargetSlotTest, UnnamedFixtureRowDoesNotGrowFormalCapacity)
{
    const auto* row = EquipSlotTableManager::Instance().FindByIdSilent(2).first;
    ASSERT_NE(nullptr, row);
    ASSERT_TRUE(row->name().empty());
    const_cast<EquipSlotTable*>(row)->set_id(50);
    BagAllData saved = Snapshot();
    saved.set_capacities(kEquipment, 10);
    bag_marshal::Unmarshal(player, saved);
    EXPECT_EQ(11u, Equipment().Capacity());
}
} // namespace
