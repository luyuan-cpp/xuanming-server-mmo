#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "modules/bag/comp/player_bags_comp.h"
#include "modules/mission/comp/mission_comp.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/event/mission_event.pb.h"
#include "services/scene/player/system/player_data_loader.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/condition_table.h"
#include "table/code/mission_table.h"
#include "thread_context/ecs_context.h"

namespace {

// 直接调用正式生成的 PlayerAllData 装卸入口，覆盖数据库记录而非仅顶层缓存。
// 不连数据库、不写真实玩家数据，也不依赖号段分配或前一个用例的状态。
class PlayerFeaturePersistenceTest : public testing::Test {
protected:
    static constexpr uint64_t kPlayerId = (uint64_t{1} << 58) + 320;
    static constexpr uint64_t kInventoryItemId = (uint64_t{1} << 60) + 701;
    static constexpr uint64_t kWarehouseItemId = (uint64_t{1} << 60) + 702;
    static constexpr uint64_t kAcceptedAtMs = 1900000000123;

    void SetUp() override
    {
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
        ClassTableManager::Instance().Load();
        AttributePoolTableManager::Instance().Load();
        AttributeDimensionTableManager::Instance().Load();
        AttributeRuleTableManager::Instance().Load();
        tlsEcs.dispatcher.sink<OnMissionAwardEvent>().connect<&PlayerFeaturePersistenceTest::OnAward>(*this);
        tlsEcs.dispatcher.sink<AcceptMissionEvent>().connect<&PlayerFeaturePersistenceTest::OnAccept>(*this);
        tlsEcs.dispatcher.sink<OnAcceptedMissionEvent>().connect<&PlayerFeaturePersistenceTest::OnAccepted>(*this);
        tlsEcs.dispatcher.sink<ConditionEvent>().connect<&PlayerFeaturePersistenceTest::OnCondition>(*this);
        queuedAwards = tlsEcs.dispatcher.size<OnMissionAwardEvent>();
        queuedAccepts = tlsEcs.dispatcher.size<AcceptMissionEvent>();
        queuedAccepted = tlsEcs.dispatcher.size<OnAcceptedMissionEvent>();
        queuedConditions = tlsEcs.dispatcher.size<ConditionEvent>();
    }

    void TearDown() override
    {
        tlsEcs.dispatcher.sink<OnMissionAwardEvent>().disconnect<&PlayerFeaturePersistenceTest::OnAward>(*this);
        tlsEcs.dispatcher.sink<AcceptMissionEvent>().disconnect<&PlayerFeaturePersistenceTest::OnAccept>(*this);
        tlsEcs.dispatcher.sink<OnAcceptedMissionEvent>().disconnect<&PlayerFeaturePersistenceTest::OnAccepted>(*this);
        tlsEcs.dispatcher.sink<ConditionEvent>().disconnect<&PlayerFeaturePersistenceTest::OnCondition>(*this);
        for (const auto player : players)
            if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
    }

    void OnAward(const OnMissionAwardEvent&) { ++eventCount; }
    void OnAccept(const AcceptMissionEvent&) { ++eventCount; }
    void OnAccepted(const OnAcceptedMissionEvent&) { ++eventCount; }
    void OnCondition(const ConditionEvent&) { ++eventCount; }

    entt::entity NewPlayer()
    {
        const auto player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<Guid>(player, kPlayerId);
        players.push_back(player);
        return player;
    }

    entt::entity StockedPlayer()
    {
        const auto player = NewPlayer();
        tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player).set_health(100);
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(30);
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        bags.bags[kInventory].SetCapacityForRestore(80);
        bags.bags[kInventory].InsertItemForRestore(kInventoryItemId, 9, 5, 37, 101);
        bags.bags[kWarehouse].SetCapacityForRestore(140);
        bags.bags[kWarehouse].InsertItemForRestore(kWarehouseItemId, 10, 3, 121, 102);
        auto& container = tlsEcs.actorRegistry.emplace<MissionsContainerComp>(player);
        auto& main = container.GetOrCreate(MissionListComp::kPlayerMission);
        auto& mission = (*main.GetMutableMissionList().mutable_missions())[7];
        mission.set_id(7);
        mission.add_progress(5);
        (*main.GetMutableMissionList().mutable_mission_begin_time())[7] = kAcceptedAtMs;
        main.RestoreCompleted(11);
        main.RestoreCompleted(12);
        main.RestoreClaimable(12);
        auto& secondary = container.GetOrCreate(77);
        auto& secondaryMission = (*secondary.GetMutableMissionList().mutable_missions())[7];
        secondaryMission.set_id(7);
        secondaryMission.add_progress(3);
        secondary.RestoreCompleted(8);
        secondary.RestoreClaimable(8);
        return player;
    }

    void ExpectStocked(entt::entity player)
    {
        auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
        ASSERT_NE(nullptr, bags);
        auto& inventory = bags->bags[kInventory];
        auto& warehouse = bags->bags[kWarehouse];
        EXPECT_EQ(80u, inventory.Capacity());
        EXPECT_EQ(140u, warehouse.Capacity());
        EXPECT_EQ(37u, inventory.GetItemPosByGuid(kInventoryItemId));
        EXPECT_EQ(121u, warehouse.GetItemPosByGuid(kWarehouseItemId));
        const auto* inventoryItem = inventory.GetItemCompByGuid(kInventoryItemId);
        const auto* warehouseItem = warehouse.GetItemCompByGuid(kWarehouseItemId);
        ASSERT_NE(nullptr, inventoryItem);
        ASSERT_NE(nullptr, warehouseItem);
        EXPECT_EQ(kInventoryItemId, inventoryItem->item_id());
        EXPECT_EQ(kWarehouseItemId, warehouseItem->item_id());
        EXPECT_EQ(9u, inventoryItem->config_id());
        EXPECT_EQ(10u, warehouseItem->config_id());
        EXPECT_EQ(5u, inventoryItem->size());
        EXPECT_EQ(3u, warehouseItem->size());
        EXPECT_EQ(101u, inventoryItem->acquire_seq());
        EXPECT_EQ(102u, warehouseItem->acquire_seq());
        EXPECT_EQ(1u, inventory.OccupiedGridCount());
        EXPECT_EQ(1u, warehouse.OccupiedGridCount());
        EXPECT_TRUE(inventory.IsLayerConsistent());
        EXPECT_TRUE(warehouse.IsLayerConsistent());

        const auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
        ASSERT_NE(nullptr, container);
        const auto* main = container->Get(MissionListComp::kPlayerMission);
        const auto* secondary = container->Get(77);
        ASSERT_NE(nullptr, main);
        ASSERT_NE(nullptr, secondary);
        ASSERT_TRUE(main->IsAccepted(7));
        const auto& active = main->GetMissionList().missions().at(7);
        ASSERT_EQ(1, active.progress_size());
        EXPECT_EQ(5u, active.progress(0));
        EXPECT_EQ(kAcceptedAtMs, main->GetMissionList().mission_begin_time().at(7));
        EXPECT_TRUE(main->IsComplete(11));
        EXPECT_FALSE(main->IsClaimable(11));
        EXPECT_TRUE(main->IsComplete(12));
        EXPECT_TRUE(main->IsClaimable(12));
        ASSERT_TRUE(secondary->IsAccepted(7));
        ASSERT_EQ(1, secondary->GetMissionList().missions().at(7).progress_size());
        EXPECT_EQ(3u, secondary->GetMissionList().missions().at(7).progress(0));
        EXPECT_TRUE(secondary->IsComplete(8));
        EXPECT_TRUE(secondary->IsClaimable(8));

        // 加载重建生产任务分发索引，不能只恢复可显示的 progress 字段。
        const auto [row, rowError] = MissionTableManager::Instance().FindByIdSilent(7);
        ASSERT_NE(nullptr, row);
        EXPECT_TRUE(main->GetTypeFilter().contains({row->mission_type(), row->mission_sub_type()}));
        for (const auto conditionId : row->condition_id())
        {
            const auto [condition, conditionError] = ConditionTableManager::Instance().FindByIdSilent(conditionId);
            ASSERT_NE(nullptr, condition);
            const auto& index = main->GetEventMissionsClassify();
            ASSERT_TRUE(index.contains(condition->condition_category()));
            EXPECT_TRUE(index.at(condition->condition_category()).contains(7));
            EXPECT_FALSE(index.at(condition->condition_category()).contains(11));
            EXPECT_FALSE(index.at(condition->condition_category()).contains(12));
        }
    }

    void ExpectNoMissionEvents()
    {
        EXPECT_EQ(0u, eventCount);
        EXPECT_EQ(queuedAwards, tlsEcs.dispatcher.size<OnMissionAwardEvent>());
        EXPECT_EQ(queuedAccepts, tlsEcs.dispatcher.size<AcceptMissionEvent>());
        EXPECT_EQ(queuedAccepted, tlsEcs.dispatcher.size<OnAcceptedMissionEvent>());
        EXPECT_EQ(queuedConditions, tlsEcs.dispatcher.size<ConditionEvent>());
    }

    std::vector<entt::entity> players;
    size_t queuedAwards{}, queuedAccepts{}, queuedAccepted{}, queuedConditions{};
    uint32_t eventCount{};
};

TEST_F(PlayerFeaturePersistenceTest, DatabaseRecordAloneRestoresBagAssetsAndMissionRights)
{
    PlayerAllData saved;
    PlayerAllDataMessageFieldsMarshal(StockedPlayer(), saved);
    const auto& database = saved.player_database_data();
    ASSERT_TRUE(database.has_bag_component());
    ASSERT_TRUE(database.has_mission_component());
    EXPECT_EQ(2, database.bag_component().items_size());
    EXPECT_TRUE(database.mission_component().scoped_state_present());
    // 顶层是旧读者兼容镜像，不是 MySQL 落库所依赖的唯一副本。
    EXPECT_EQ(database.bag_component().SerializeAsString(), saved.bag_data().SerializeAsString());
    EXPECT_EQ(database.mission_component().SerializeAsString(), saved.quest_data().SerializeAsString());

    std::string bytes;
    ASSERT_TRUE(database.SerializeToString(&bytes));
    PlayerAllData coldSnapshot;
    ASSERT_TRUE(coldSnapshot.mutable_player_database_data()->ParseFromString(bytes));
    ASSERT_FALSE(coldSnapshot.has_bag_data());
    ASSERT_FALSE(coldSnapshot.has_quest_data());
    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnMarshal(restored, coldSnapshot);
    ExpectStocked(restored);
    ExpectNoMissionEvents();
}

TEST_F(PlayerFeaturePersistenceTest, AuthoritativeEmptyDatabaseSnapshotWinsOverStaleLegacyTopLevel)
{
    PlayerAllData saved;
    PlayerAllDataMessageFieldsMarshal(StockedPlayer(), saved);
    auto* database = saved.mutable_player_database_data();
    database->mutable_bag_component()->Clear();
    database->mutable_mission_component()->Clear();
    database->mutable_mission_component()->set_scoped_state_present(true);
    ASSERT_TRUE(database->has_bag_component());
    ASSERT_TRUE(database->has_mission_component());
    ASSERT_FALSE(saved.bag_data().items().empty());
    ASSERT_FALSE(saved.quest_data().scopes().empty());

    PlayerAllData parsed;
    ASSERT_TRUE(parsed.ParseFromString(saved.SerializeAsString()));
    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnMarshal(restored, parsed);
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(restored);
    ASSERT_NE(nullptr, bags);
    for (const auto& bag : bags->bags) EXPECT_EQ(0u, bag.OccupiedGridCount());
    const auto* missions = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(restored);
    ASSERT_NE(nullptr, missions);
    const auto* main = missions->Get(MissionListComp::kPlayerMission);
    ASSERT_NE(nullptr, main);
    EXPECT_EQ(0u, main->MissionSize());
    EXPECT_FALSE(main->IsComplete(11));
    EXPECT_FALSE(main->IsClaimable(12));
    EXPECT_EQ(nullptr, missions->Get(77));
    ExpectNoMissionEvents();
}

TEST_F(PlayerFeaturePersistenceTest, LegacyTopLevelRemainsReadableWhenNewDatabaseFieldsAreAbsent)
{
    PlayerAllData legacy;
    PlayerAllDataMessageFieldsMarshal(StockedPlayer(), legacy);
    legacy.mutable_player_database_data()->clear_bag_component();
    legacy.mutable_player_database_data()->clear_mission_component();
    PlayerAllData parsed;
    ASSERT_TRUE(parsed.ParseFromString(legacy.SerializeAsString()));
    ASSERT_FALSE(parsed.player_database_data().has_bag_component());
    ASSERT_FALSE(parsed.player_database_data().has_mission_component());
    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnMarshal(restored, parsed);
    ExpectStocked(restored);
    ExpectNoMissionEvents();

    // 旧快照读出后再保存必须写入新的数据库字段，不能永远依赖兼容分支。
    PlayerAllData resaved;
    PlayerAllDataMessageFieldsMarshal(restored, resaved);
    EXPECT_TRUE(resaved.player_database_data().has_bag_component());
    EXPECT_TRUE(resaved.player_database_data().has_mission_component());
    EXPECT_EQ(2, resaved.player_database_data().bag_component().items_size());
    EXPECT_TRUE(resaved.player_database_data().mission_component().scoped_state_present());
}

TEST_F(PlayerFeaturePersistenceTest, NewDatabaseAndLegacyTopLevelSourcesAreChosenIndependently)
{
    PlayerAllData mixed;
    PlayerAllDataMessageFieldsMarshal(StockedPlayer(), mixed);
    // 背包已有正式空记录，任务仍来自旧顶层。不能只用一个总版本开关。
    mixed.mutable_player_database_data()->mutable_bag_component()->Clear();
    mixed.mutable_player_database_data()->clear_mission_component();
    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnMarshal(restored, mixed);
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(restored);
    ASSERT_NE(nullptr, bags);
    EXPECT_EQ(0u, bags->bags[kInventory].OccupiedGridCount());
    const auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(restored);
    ASSERT_NE(nullptr, container);
    const auto* main = container->Get(MissionListComp::kPlayerMission);
    ASSERT_NE(nullptr, main);
    EXPECT_TRUE(main->IsAccepted(7));
    EXPECT_TRUE(main->IsClaimable(12));
    EXPECT_EQ(5u, main->GetMissionList().missions().at(7).progress(0));
    ExpectNoMissionEvents();
}

} // namespace
