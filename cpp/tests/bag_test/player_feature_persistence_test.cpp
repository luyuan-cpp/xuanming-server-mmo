#include <gtest/gtest.h>
#include "actor/actor_net_id.h"
#include "services/scene/player/system/handoff_sql_barrier.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "core/system/id_generator.h"
#include "core/type_alias/player_session_type_alias.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <array>
#include <cstdint>
#include <iterator>
#include <memory>
#include <string>
#include <vector>

#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/mission/comp/mission_comp.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include "proto/common/component/time_comp.pb.h"
#include "proto/common/component/asset_op_ledger_comp.pb.h"
#include "proto/common/component/battle_settlement_ledger_comp.pb.h"
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/common/component/player_mystery_merchant_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"  // PlayerProfileComp
#include "proto/common/component/player_login_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "modules/mission/mission_events.h"
#include "services/scene/combat/skill/comp/skill_comp.h"
#include "services/scene/player/comp/last_persisted_snapshot_comp.h"
#include "services/scene/player/system/player_data_loader.h"
#include "services/scene/player/comp/player_persist_carry_comp.h"
#include "services/scene/player/system/player_lifecycle.h"
#include "services/scene/player/system/role_admission_system.h"
#include "services/scene/player/system/player_persist_marshal.h"
#include "services/scene/actor/action_state/constants/actor_state.h"
#include "services/scene/actor/attribute/comp/actor_attribute_comp.h"
#include "services/scene/actor/attribute/system/actor_attribute_calculator.h"
#include "services/scene/combat/buff/comp/buff_comp.h"
#include "services/scene/combat_state/constants/combat_state.h"
#include "services/scene/combat_state/system/combat_state.h"
#include "services/scene/generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "services/scene/spatial/comp/scene_node_scene_comp.h"
#include "entt/src/entt/signal/sigh.hpp"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/buff_table.h"
#include "table/code/class_table.h"
#include "table/code/condition_table.h"
#include "table/code/mission_table.h"
#include "table/code/skill_table.h"
#include "thread_context/ecs_context.h"

namespace {

// 真实加载成功必须落到合法场景;typed 句柄只在本作用域内持有并清理。
struct ScopedInitializationScene
{
    SceneEntity scene{tlsEcs.sceneRegistry.create()};
    ScopedInitializationScene()
    {
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_id(710001);
    }
    ~ScopedInitializationScene()
    {
        if (tlsEcs.sceneRegistry.valid(scene)) tlsEcs.sceneRegistry.destroy(scene);
    }
    PlayerEnterContext Context() const
    {
        PlayerEnterContext context;
        context.enterInfo.set_scene_id(710001);
        context.roleAdmissionGeneration = 11;
        context.ownerRevision = 7;
        return context;
    }
};



// 可选功能未使用时应保持未激活,显式初始化不能把这些空表和相位预挂给每个玩家。
// 不断言 AttributeDirtyFlagsComp 缺席:真实 Unmarshal 的 HP/MP 重算可以合法标脏。
void ExpectLazyActorStateAbsent(entt::entity actor)
{
    const auto& registry = tlsEcs.actorRegistry;
    EXPECT_FALSE((registry.any_of<CooldownTimeListComp, SkillContextCompMap, ActorStateComp>(actor)));
    EXPECT_FALSE((registry.any_of<BuffListComp, CombatStateCollectionComp>(actor)));
    EXPECT_FALSE((registry.any_of<AoiListComp, CastingTimerComp, RecoveryTimerComp,
                                  ChannelFinishTimerComp, ChannelIntervalTimerComp, TimerTaskComp>(actor)));
}

void ExpectLoadedActorProjections(entt::entity actor)
{
    const auto& registry = tlsEcs.actorRegistry;
    ASSERT_TRUE(registry.valid(actor));
    ASSERT_TRUE((registry.all_of<CombatStateFlagsComp, DerivedAttributesComp>(actor)));
    EXPECT_TRUE(registry.get<CombatStateFlagsComp>(actor).state_flags().empty());
    EXPECT_GT(registry.get<DerivedAttributesComp>(actor).max_health(), 0u)
        << "角色二级属性仍须由正式加载重算,不能被 actor 投影初始化清零";
    ExpectLazyActorStateAbsent(actor);
}

struct PlayerMapCleanup
{
    Guid playerId;
    entt::entity player;
    ~PlayerMapCleanup()
    {
        const auto it = tlsEcs.playerList.find(playerId);
        if (it != tlsEcs.playerList.end() && it->second == player)
            tlsEcs.playerList.erase(it);
    }
};


// 只给缺的组件挂默认值;用例已手挂(带用例数据)的保持原样。
template <typename Comp>
void EmplaceIfAbsent(entt::entity player)
{
    if (!tlsEcs.actorRegistry.all_of<Comp>(player))
    {
        tlsEcs.actorRegistry.emplace<Comp>(player);
    }
}

// 把手搭的实体补成「加载创建点走完」的形状。生产里这些持久化组件只由
// PlayerDatabaseMessageFieldsUnmarshal 一次性 emplace;存盘 Marshal 缺任何一个都整份拒绝,
// 所以手搭的玩家必须补齐才能被保存。新增持久化组件时这里同步加一项 —— 漏了的话下面的往返用例
// 会因 Marshal 拒绝而失败,日志里会点名缺的是哪个组件。
void CompletePersistedComps(entt::entity player)
{
    EmplaceIfAbsent<Transform>(player);
    EmplaceIfAbsent<PlayerUint64Comp>(player);
    EmplaceIfAbsent<PlayerSkillListComp>(player);
    EmplaceIfAbsent<PlayerUint32Comp>(player);
    EmplaceIfAbsent<BaseAttributesComp>(player);
    EmplaceIfAbsent<LevelComp>(player);
    EmplaceIfAbsent<PlayerAttributeComp>(player);
    EmplaceIfAbsent<PlayerPetComp>(player);
    EmplaceIfAbsent<PlayerProfileComp>(player);
    EmplaceIfAbsent<PlayerBagsComp>(player);
    EmplaceIfAbsent<MissionsContainerComp>(player);
    EmplaceIfAbsent<PlayerAssetOpLedgerComp>(player);
    EmplaceIfAbsent<BattleSettlementLedgerComp>(player);
    EmplaceIfAbsent<PlayerInboxLedgerComp>(player);
    EmplaceIfAbsent<PlayerMysteryMerchantComp>(player);
    EmplaceIfAbsent<CurrencyComp>(player);
    EmplaceIfAbsent<PlayerCurrencyComp>(player);
    // 两表顶层未知字段的载体(混版本持久字段保护 §4.2 L1),生产里由两张表的 Unmarshal 创建。
    EmplaceIfAbsent<PlayerDatabaseCarryComp>(player);
    EmplaceIfAbsent<PlayerDatabase1CarryComp>(player);
}


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
        tlsIdGeneratorManager.SetNodeId(95);
        actor_net_id::Install(tlsEcs.actorRegistry);
        RoleAdmissionSystem::Dependencies dependencies;
        auto& identity = dependencies.confirmedAdmissions.emplace_back();
        identity.set_player_id(kPlayerId);
        identity.set_generation(11);
        identity.set_owner_revision(7);
        admission = std::make_unique<RoleAdmissionSystem::ScopedDependencies>(std::move(dependencies));
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
        ClassTableManager::Instance().Load();
        AttributePoolTableManager::Instance().Load();
        AttributeDimensionTableManager::Instance().Load();
        AttributeRuleTableManager::Instance().Load();
    }

    void TearDown() override
    {
        for (const auto player : players)
            if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        admission.reset();
    }


    std::unique_ptr<RoleAdmissionSystem::ScopedDependencies> admission;

    entt::entity NewPlayer()
    {
        const auto player = tlsEcs.actorRegistry.create();
        EXPECT_NE(0, actor_net_id::Assign(tlsEcs.actorRegistry, player));
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayerId);
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
        CompletePersistedComps(player);
        return player;
    }

    // 每次只摘一个持久化组件:Marshal 必须整份拒绝(返回 false),既不把它补建回实体,
    // 也不往 message 里写任何字段 —— 尤其不能写出零值货币 / 空账本 / 空背包。
    template <typename Comp>
    void ExpectMarshalRefusedWithout(const char* name)
    {
        SCOPED_TRACE(name);
        const auto player = StockedPlayer();
        tlsEcs.actorRegistry.remove<Comp>(player);
        PlayerAllData saved;
        EXPECT_FALSE(PlayerAllDataMessageFieldsMarshal(player, saved));
        EXPECT_FALSE(tlsEcs.actorRegistry.all_of<Comp>(player)) << "存盘只读,不得补建缺失的组件";
        EXPECT_EQ(0u, saved.player_database_data().ByteSizeLong()) << "缺组件时一个字段都不写";
        EXPECT_FALSE(saved.has_player_database_data()) << "拒绝时不得创建空子消息";
        EXPECT_FALSE(saved.has_player_database_1_data());
        EXPECT_FALSE(saved.has_bag_data());
        EXPECT_FALSE(saved.has_quest_data());

        // 调用者复用的输出可能已有上一份完整快照。拒绝不能改动其内容或 presence，
        // 更不能只更新第一张表、留下混合了两个时刻的半份快照。
        PlayerAllData previous;
        previous.mutable_player_database_data()->set_player_id(kPlayerId + 1);
        previous.mutable_player_database_data()->mutable_currency()->add_values(831);
        previous.mutable_player_database_1_data()->set_player_id(kPlayerId + 2);
        previous.mutable_bag_data()->add_items()->set_item_uuid(kInventoryItemId);
        previous.mutable_quest_data()->set_scoped_state_present(true);
        const auto previousBytes = previous.SerializeAsString();
        EXPECT_FALSE(PlayerAllDataMessageFieldsMarshal(player, previous));
        EXPECT_EQ(previousBytes, previous.SerializeAsString()) << "拒绝必须保留原输出";
        EXPECT_FALSE(tlsEcs.actorRegistry.all_of<Comp>(player));
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


    std::vector<entt::entity> players;
};

TEST_F(PlayerFeaturePersistenceTest, DatabaseRecordAloneRestoresBagAssetsAndMissionRights)
{
    PlayerAllData saved;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(StockedPlayer(), saved));
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
    PlayerAllDataMessageFieldsUnmarshal(restored, coldSnapshot);
    ExpectStocked(restored);
}

TEST_F(PlayerFeaturePersistenceTest, AuthoritativeEmptyDatabaseSnapshotWinsOverStaleLegacyTopLevel)
{
    PlayerAllData saved;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(StockedPlayer(), saved));
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
    PlayerAllDataMessageFieldsUnmarshal(restored, parsed);
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
}

TEST_F(PlayerFeaturePersistenceTest, LegacyTopLevelRemainsReadableWhenNewDatabaseFieldsAreAbsent)
{
    PlayerAllData legacy;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(StockedPlayer(), legacy));
    legacy.mutable_player_database_data()->clear_bag_component();
    legacy.mutable_player_database_data()->clear_mission_component();
    PlayerAllData parsed;
    ASSERT_TRUE(parsed.ParseFromString(legacy.SerializeAsString()));
    ASSERT_FALSE(parsed.player_database_data().has_bag_component());
    ASSERT_FALSE(parsed.player_database_data().has_mission_component());
    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnmarshal(restored, parsed);
    ExpectStocked(restored);

    // 旧快照读出后再保存必须写入新的数据库字段，不能永远依赖兼容分支。
    PlayerAllData resaved;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(restored, resaved));
    EXPECT_TRUE(resaved.player_database_data().has_bag_component());
    EXPECT_TRUE(resaved.player_database_data().has_mission_component());
    EXPECT_EQ(2, resaved.player_database_data().bag_component().items_size());
    EXPECT_TRUE(resaved.player_database_data().mission_component().scoped_state_present());
}

TEST_F(PlayerFeaturePersistenceTest, NewDatabaseAndLegacyTopLevelSourcesAreChosenIndependently)
{
    PlayerAllData mixed;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(StockedPlayer(), mixed));
    // 背包已有正式空记录，任务仍来自旧顶层。不能只用一个总版本开关。
    mixed.mutable_player_database_data()->mutable_bag_component()->Clear();
    mixed.mutable_player_database_data()->clear_mission_component();
    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnmarshal(restored, mixed);
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
}

// 角色名副本(player_database.profile_component):真源是 data_service 全局库 player_name,
// 由 login 首次入场补进这条记录;scene 只读、只原样存回,所以用例里的名字一律直接写在记录上,
// 不经任何 scene 侧写路径。
TEST_F(PlayerFeaturePersistenceTest, ProfileNameRoundTripsThroughDatabaseRecord)
{
    const std::string kName = "云中君";
    PlayerAllData cold;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(StockedPlayer(), cold));
    cold.mutable_player_database_data()->mutable_profile_component()->set_name(kName);

    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnmarshal(restored, cold);
    const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(restored);
    ASSERT_NE(nullptr, profile);
    EXPECT_EQ(kName, profile->name());

    PlayerAllData saved;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(restored, saved));
    const auto& database = saved.player_database_data();
    ASSERT_TRUE(database.has_profile_component());
    EXPECT_EQ(kName, database.profile_component().name());

    // 落库走的是 player_database 的序列化字节,名字必须撑得过一次编解码。
    std::string bytes;
    ASSERT_TRUE(database.SerializeToString(&bytes));
    player_database reparsed;
    ASSERT_TRUE(reparsed.ParseFromString(bytes));
    ASSERT_TRUE(reparsed.has_profile_component());
    EXPECT_EQ(kName, reparsed.profile_component().name());
}

TEST_F(PlayerFeaturePersistenceTest, MissingProfileComponentLoadsEmptyName)
{
    // 早于名字功能的存量记录 / self-heal 恢复出的空记录都没有字段 15。
    PlayerAllData legacy;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(StockedPlayer(), legacy));
    legacy.mutable_player_database_data()->clear_profile_component();
    PlayerAllData parsed;
    ASSERT_TRUE(parsed.ParseFromString(legacy.SerializeAsString()));
    ASSERT_FALSE(parsed.player_database_data().has_profile_component());

    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnmarshal(restored, parsed);
    // 组件必须存在(战斗快照与存盘都按"有组件、名字为空"处理),而不是缺组件。
    const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(restored);
    ASSERT_NE(nullptr, profile);
    EXPECT_TRUE(profile->name().empty());

    // 空名存回不崩,且不会凭空长出名字 —— 补齐是 login 的事,scene 不写。
    PlayerAllData resaved;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(restored, resaved));
    EXPECT_TRUE(resaved.player_database_data().profile_component().name().empty());
}

// 装备实例数据(ItemEntry.equip,字段 14;docs/design/equipment-attributes.md §3.1):
// 随 player_database 这条记录落库、再从冷记录读回,属性行与 presence 都不能丢。
// presence 有语义 —— has_equip() 表示「已掷过」,读回来若变成没有,这件装备下次经过任何
// 入包路径都会被当成新铸的重掷一次;反过来普通物品也不能凭空长出一段空的 equip。
//
// 三件都放在人物背包 / 仓库而不是装备栏:这条用例钉的是存档往返,不是穿戴后的属性加成
// (那是 player_equip 的用例),放进装备栏会把还原后的属性重算也拖进来。
TEST_F(PlayerFeaturePersistenceTest, EquipInstanceDataRoundTripsThroughDatabaseRecord)
{
    constexpr uint64_t kRolledItemId = (uint64_t{1} << 60) + 711;
    constexpr uint64_t kEmptyRolledItemId = (uint64_t{1} << 60) + 712;
    constexpr uint64_t kPlainItemId = (uint64_t{1} << 60) + 713;

    const auto player = NewPlayer();
    tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player).set_health(100);
    tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(30);
    {
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);

        // 两条属性:四个字段各取不同的值,丢哪个字段都看得出来。
        ItemComp rolled;
        rolled.set_item_id(kRolledItemId);
        rolled.set_config_id(1);
        rolled.set_size(1);
        auto* blue = rolled.mutable_equip()->add_affixes();
        blue->set_attr_id(3);
        blue->set_tier(1);
        blue->set_value(17);
        blue->set_seq(0);
        auto* yellow = rolled.mutable_equip()->add_affixes();
        yellow->set_attr_id(12);
        yellow->set_tier(3);
        yellow->set_value(4);
        yellow->set_seq(1);
        // 二期(强化 + 镶嵌):强化等级与一颗宝石,孔位号故意不等于下标。
        rolled.mutable_equip()->set_enhance_level(5);
        auto* gem = rolled.mutable_equip()->add_gems();
        gem->set_socket(1);
        gem->set_gem_config_id(2201);
        bags.bags[kInventory].RestoreItems({{rolled, 5}});

        // 已初始化但一条都没掷出:只有 presence。
        ItemComp emptyRolled;
        emptyRolled.set_item_id(kEmptyRolledItemId);
        emptyRolled.set_config_id(2);
        emptyRolled.set_size(1);
        emptyRolled.mutable_equip();
        bags.bags[kWarehouse].RestoreItems({{emptyRolled, 9}});

        // 普通物品:没有 equip 段。
        ItemComp plain;
        plain.set_item_id(kPlainItemId);
        plain.set_config_id(3);
        plain.set_size(1);
        bags.bags[kInventory].RestoreItems({{plain, 6}});
    }
    CompletePersistedComps(player);

    PlayerAllData saved;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(player, saved));
    const auto& database = saved.player_database_data();
    ASSERT_TRUE(database.has_bag_component());
    ASSERT_EQ(3, database.bag_component().items_size());
    // 顶层兼容镜像与数据库记录是同一份内容,equip 段同样在里面。
    EXPECT_EQ(database.bag_component().SerializeAsString(), saved.bag_data().SerializeAsString());

    // 只拿 player_database 的字节重建(MySQL 落库的就是这一份),不带顶层镜像。
    std::string bytes;
    ASSERT_TRUE(database.SerializeToString(&bytes));
    PlayerAllData coldSnapshot;
    ASSERT_TRUE(coldSnapshot.mutable_player_database_data()->ParseFromString(bytes));
    ASSERT_FALSE(coldSnapshot.has_bag_data());
    const auto restored = NewPlayer();
    PlayerAllDataMessageFieldsUnmarshal(restored, coldSnapshot);

    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(restored);
    ASSERT_NE(nullptr, bags);
    const auto* rolled = bags->bags[kInventory].GetItemCompByGuid(kRolledItemId);
    const auto* emptyRolled = bags->bags[kWarehouse].GetItemCompByGuid(kEmptyRolledItemId);
    const auto* plain = bags->bags[kInventory].GetItemCompByGuid(kPlainItemId);
    ASSERT_NE(nullptr, rolled);
    ASSERT_NE(nullptr, emptyRolled);
    ASSERT_NE(nullptr, plain);

    ASSERT_TRUE(rolled->has_equip());
    ASSERT_EQ(2, rolled->equip().affixes_size());
    EXPECT_EQ(3u, rolled->equip().affixes(0).attr_id());
    EXPECT_EQ(1u, rolled->equip().affixes(0).tier());
    EXPECT_EQ(17u, rolled->equip().affixes(0).value());
    EXPECT_EQ(0u, rolled->equip().affixes(0).seq());
    EXPECT_EQ(12u, rolled->equip().affixes(1).attr_id());
    EXPECT_EQ(3u, rolled->equip().affixes(1).tier());
    EXPECT_EQ(4u, rolled->equip().affixes(1).value());
    EXPECT_EQ(1u, rolled->equip().affixes(1).seq());
    EXPECT_EQ(5u, rolled->equip().enhance_level());
    ASSERT_EQ(1, rolled->equip().gems_size());
    EXPECT_EQ(1u, rolled->equip().gems(0).socket());
    EXPECT_EQ(2201u, rolled->equip().gems(0).gem_config_id());
    EXPECT_EQ(1u, rolled->config_id());
    EXPECT_EQ(5u, bags->bags[kInventory].GetItemPosByGuid(kRolledItemId));

    EXPECT_TRUE(emptyRolled->has_equip());
    EXPECT_EQ(0, emptyRolled->equip().affixes_size());
    EXPECT_EQ(0u, emptyRolled->equip().enhance_level());
    EXPECT_EQ(0, emptyRolled->equip().gems_size());
    EXPECT_EQ(9u, bags->bags[kWarehouse].GetItemPosByGuid(kEmptyRolledItemId));

    EXPECT_FALSE(plain->has_equip());
    EXPECT_EQ(6u, bags->bags[kInventory].GetItemPosByGuid(kPlainItemId));
    EXPECT_TRUE(bags->bags[kInventory].IsLayerConsistent());
    EXPECT_TRUE(bags->bags[kWarehouse].IsLayerConsistent());

    // 读回后再存一次,记录里的 equip 段逐字节不变(不会越存越多 / 越存越少)。
    PlayerAllData resaved;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(restored, resaved));
    ASSERT_EQ(3, resaved.player_database_data().bag_component().items_size());
    int withEquip = 0;
    for (const auto& entry : resaved.player_database_data().bag_component().items())
    {
        if (entry.has_equip()) ++withEquip;
        if (entry.item_uuid() == kRolledItemId)
        {
            EXPECT_EQ(rolled->equip().SerializeAsString(), entry.equip().SerializeAsString());
        }
    }
    EXPECT_EQ(2, withEquip);
}

// ── 存盘只读化(docs/design/ecs-entt-misuse-audit-20261015.md F059 / F060 / F070 / F073)──
// 宿主里没有初始化 RedisSystem(GetPlayerDataRedis() 为空):下面的存盘用例都必须在碰 Redis 之前返回,
// 一旦走到写盘就是空指针崩溃 —— 这本身就是「没有 fail-closed」的证据。

// F059 / F060:旧 Marshal 对每个持久化组件 get_or_emplace,缺哪个就当场造一个零值组件写进记录。
// 现在缺任何一个都整份拒绝,且不改变实体的组件集合。逐个组件各摘一次,一个都不能漏。
TEST_F(PlayerFeaturePersistenceTest, MarshalRefusesWhenAnyPersistedComponentIsMissing)
{
    ExpectMarshalRefusedWithout<Transform>("Transform");
    ExpectMarshalRefusedWithout<PlayerUint64Comp>("PlayerUint64Comp");
    ExpectMarshalRefusedWithout<PlayerSkillListComp>("PlayerSkillListComp");
    ExpectMarshalRefusedWithout<PlayerUint32Comp>("PlayerUint32Comp");
    ExpectMarshalRefusedWithout<BaseAttributesComp>("BaseAttributesComp");
    ExpectMarshalRefusedWithout<LevelComp>("LevelComp");
    ExpectMarshalRefusedWithout<PlayerAttributeComp>("PlayerAttributeComp");
    ExpectMarshalRefusedWithout<PlayerPetComp>("PlayerPetComp");
    ExpectMarshalRefusedWithout<PlayerProfileComp>("PlayerProfileComp");
    ExpectMarshalRefusedWithout<PlayerBagsComp>("PlayerBagsComp");
    ExpectMarshalRefusedWithout<MissionsContainerComp>("MissionsContainerComp");
    ExpectMarshalRefusedWithout<PlayerAssetOpLedgerComp>("PlayerAssetOpLedgerComp");
    ExpectMarshalRefusedWithout<BattleSettlementLedgerComp>("BattleSettlementLedgerComp");
    ExpectMarshalRefusedWithout<PlayerInboxLedgerComp>("PlayerInboxLedgerComp");
    ExpectMarshalRefusedWithout<PlayerMysteryMerchantComp>("PlayerMysteryMerchantComp");
    ExpectMarshalRefusedWithout<CurrencyComp>("CurrencyComp");
    ExpectMarshalRefusedWithout<PlayerCurrencyComp>("PlayerCurrencyComp");
    // 两表顶层未知字段的载体(混版本持久字段保护 §4.2 L1):缺了同样整份拒绝,补空等于抹掉新版本写进来的整列。
    ExpectMarshalRefusedWithout<PlayerDatabaseCarryComp>("PlayerDatabaseCarryComp");
    ExpectMarshalRefusedWithout<PlayerDatabase1CarryComp>("PlayerDatabase1CarryComp");
}

// F059 落到存盘入口:组件不全的玩家在脏比较与写 Redis / 发 DBTask 之前就被拒绝(返回 false = 没写盘),
// 不补建组件。
// 预置的「上次落盘」快照就是旧 Marshal 补建空账本之后会写出的那份(StockedPlayer 的账本本来就是空的):
// 旧写法 get_or_emplace 补出空账本 → 脏比较判「盘上已是同一份」→ 快路径返回 false、不碰 Redis,于是回归时的红
// 是下面「账本被补建」这条干净的断言失败,不会走到空的 Redis 客户端把整个测试进程带崩、盖掉其他用例的结果。
TEST_F(PlayerFeaturePersistenceTest, SaveRefusesIncompletePlayerBeforeTouchingRedis)
{
    const auto player = StockedPlayer();
    PlayerAllData persisted;
    // 与 SavePlayerToRedis 比对时同一个形状:走同一个统一存盘入口(补两张子表的 player_id、盖持久 schema 世代)。
    // 不盖戳的话脏比较必然判「不同」,回归时会走到空的 Redis 客户端,而不是落在下面那条干净的断言上。
    ASSERT_TRUE(player_persist::MarshalPlayerForPersist(player, kPlayerId, persisted));
    auto& previousSnapshot = tlsEcs.actorRegistry.emplace<PlayerLastPersistedSnapshotComp>(player);
    previousSnapshot.Replace(persisted);
    const auto persistedBytes = previousSnapshot.snapshot->SerializeAsString();
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);

    EXPECT_FALSE(PlayerLifecycleSystem::SavePlayerToRedis(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerAssetOpLedgerComp>(player)) << "存盘只读,不得补建账本";
    const auto* snapshot = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
    ASSERT_NE(nullptr, snapshot);
    ASSERT_TRUE(snapshot->HasSnapshot());
    EXPECT_EQ(persistedBytes, snapshot->snapshot->SerializeAsString()) << "拒存不能刷新落盘证据";
}

// F073:缺 Guid 的实体存盘直接拒绝(返回 false = 没写盘),与 HandleExitGameNode 对缺 Guid 的容错一致。
// 修复前无守卫读取玩家身份:Debug 下 entt 断言失败,Release 下读野内存。
TEST_F(PlayerFeaturePersistenceTest, SaveWithoutGuidIsRefused)
{
    const auto player = tlsEcs.actorRegistry.create();
    players.push_back(player);
    CompletePersistedComps(player); // 只缺 Guid,确保拒绝的原因就是它

    EXPECT_FALSE(PlayerLifecycleSystem::SavePlayerToRedis(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerLastPersistedSnapshotComp>(player));
}

// 新号必需初始化不能依赖 PlayerEventHandler:宿主没有注册注册/初始化事件的业务 sink。
// 修复前默认技能与 actor 投影依赖业务 sink;不注册它们时技能为空、MoveSpeed/Flags 缺失。
TEST_F(PlayerFeaturePersistenceTest, NewPlayerInitializesDefaultSkillsAndMissionScopeWithoutPlayerEventSinks)
{
    const ScopedInitializationScene target;
    SkillTableManager::Instance().Load();
    uint32_t classId = 0;
    uint32_t defaultSkillId = 0;
    for (const auto& row : ClassTableManager::Instance().FindAll().data())
    {
        for (const auto skillId : row.skill())
        {
            if (skillId != 0 && SkillTableManager::Instance().Exists(skillId))
            {
                classId = row.id();
                defaultSkillId = skillId;
                break;
            }
        }
        if (defaultSkillId != 0) break;
    }
    ASSERT_NE(defaultSkillId, 0u) << "夹具必须包含一个表内有效的职业默认技能";
    ASSERT_FALSE(tlsEcs.playerList.contains(kPlayerId));
    // bag_test 的号段基线只启用 item;专门的 snapshot 用例会在退出时 Reset。
    // 保证真实 Init 最后的回滚快照在发 Kafka 之前因无号段退出,不引入外部 I/O。
    ASSERT_FALSE(tlsGuidSegmentRegistry.Get(GuidKind::kSnapshot).IsEnabled());
    PlayerAllData data;
    auto* database = data.mutable_player_database_data();
    database->set_player_id(kPlayerId);
    database->mutable_uint32_pb_component()->set_class_(classId);
    database->mutable_uint64_pb_component()->set_registration_timestamp(0);

    const auto created = PlayerLifecycleSystem::InitPlayerFromAllData(data, target.Context());
    PlayerMapCleanup mapCleanup{kPlayerId, created};
    if (created != entt::null) players.push_back(created);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(created));
    const auto actorNetId = actor_net_id::Of(tlsEcs.actorRegistry, created);
    ASSERT_NE(0, actorNetId);
    EXPECT_EQ(created, actor_net_id::Resolve(tlsEcs.actorRegistry, actorNetId));
    EXPECT_EQ(tlsEcs.GetPlayer(kPlayerId), created);
    const auto* registration = tlsEcs.actorRegistry.try_get<PlayerUint64Comp>(created);
    ASSERT_NE(registration, nullptr);
    EXPECT_GT(registration->registration_timestamp(), 0u);
    const auto* skills = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(created);
    ASSERT_NE(skills, nullptr);
    bool hasDefaultSkill = false;
    for (const auto& skill : skills->skill_list())
        if (skill.skill_table_id() == defaultSkillId) hasDefaultSkill = true;
    EXPECT_TRUE(hasDefaultSkill) << "默认技能必须由创建入口直接初始化";
    const auto* missions = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(created);
    ASSERT_NE(missions, nullptr);
    const auto* main = missions->Get(MissionListComp::kPlayerMission);
    ASSERT_NE(main, nullptr) << "Unmarshal 必须保证主任务 scope,不依赖初始化通知";
    EXPECT_EQ(main->GetMissionList().type(), MissionListComp::kPlayerMission);
    ExpectLoadedActorProjections(created);
}

// 存量记录不走首登默认技能分支,也必须由加载链直接建立 actor 投影。
// 修复前没有 actor 业务 sink 时缺这两个组件;只改首登分支同样无法通过本例。
TEST_F(PlayerFeaturePersistenceTest, RegisteredPlayerInitializesActorProjectionsWithoutBusinessEventSinks)
{
    const ScopedInitializationScene target;
    ASSERT_FALSE(tlsEcs.playerList.contains(kPlayerId));
    ASSERT_FALSE(tlsGuidSegmentRegistry.Get(GuidKind::kSnapshot).IsEnabled());
    const auto& classes = ClassTableManager::Instance().FindAll();
    ASSERT_GT(classes.data_size(), 0);
    PlayerAllData data;
    auto* database = data.mutable_player_database_data();
    database->set_player_id(kPlayerId);
    database->mutable_uint32_pb_component()->set_class_(classes.data(0).id());
    database->mutable_uint64_pb_component()->set_registration_timestamp(1);
    database->mutable_level_component()->set_level(30);
    database->mutable_derived_attributes_component()->set_health(123);
    database->mutable_derived_attributes_component()->set_mana(77);

    const auto created = PlayerLifecycleSystem::InitPlayerFromAllData(data, target.Context());
    PlayerMapCleanup mapCleanup{kPlayerId, created};
    if (created != entt::null) players.push_back(created);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(created));
    EXPECT_EQ(created, tlsEcs.GetPlayer(kPlayerId));
    EXPECT_EQ(1u, tlsEcs.actorRegistry.get<PlayerUint64Comp>(created).registration_timestamp());
    EXPECT_EQ(30u, tlsEcs.actorRegistry.get<LevelComp>(created).level());
    ExpectLoadedActorProjections(created);
}

TEST_F(PlayerFeaturePersistenceTest, InitAdmitsNonCreatorIntoPrivateMirror)
{
    const ScopedInitializationScene target;
    auto& info = tlsEcs.sceneRegistry.get<SceneInfoComp>(target.scene);
    info.set_mirror_config_id(1);
    (*info.mutable_creators())[kPlayerId + 1] = true;
    ASSERT_FALSE(tlsEcs.playerList.contains(kPlayerId));
    ASSERT_FALSE(tlsGuidSegmentRegistry.Get(GuidKind::kSnapshot).IsEnabled());
    const auto& classes = ClassTableManager::Instance().FindAll();
    ASSERT_GT(classes.data_size(), 0);
    PlayerAllData data;
    auto* database = data.mutable_player_database_data();
    database->set_player_id(kPlayerId);
    database->mutable_uint32_pb_component()->set_class_(classes.data(0).id());
    database->mutable_uint64_pb_component()->set_registration_timestamp(1);
    database->mutable_level_component()->set_level(30);

    const auto created = PlayerLifecycleSystem::InitPlayerFromAllData(data, target.Context());
    PlayerMapCleanup mapCleanup{kPlayerId, created};
    if (created != entt::null) players.push_back(created);

    ASSERT_NE(entt::entity{entt::null}, created);
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(created));
    EXPECT_EQ(created, tlsEcs.GetPlayer(kPlayerId));
    ASSERT_TRUE(tlsEcs.actorRegistry.all_of<SceneEntityComp>(created));
    EXPECT_EQ(target.scene, tlsEcs.actorRegistry.get<SceneEntityComp>(created).sceneEntity);
}

TEST_F(PlayerFeaturePersistenceTest, ActorCreationBuildsOnlyRequiredProjectionsWithoutDirtying)
{
    const auto actor = NewPlayer();
    ActorAttributeCalculatorSystem::InitializeOnCreate(actor);

    ASSERT_TRUE(tlsEcs.actorRegistry.all_of<CombatStateFlagsComp>(actor));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<CombatStateFlagsComp>(actor).state_flags().empty());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(actor))
        << "角色二级属性仍归玩家加载入口负责,actor helper 不应另建空缓存";
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<AttributeDirtyFlagsComp>(actor));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ActorBaseAttributesS2CDirtyMaskComp>(actor));
    ExpectLazyActorStateAbsent(actor);
}

TEST_F(PlayerFeaturePersistenceTest, InitRejectsMissingDestinationWithoutPublishingPlayerOrSession)
{
    const ScopedInitializationScene target;
    auto context = target.Context();
    context.enterInfo.set_scene_id(710002);
    constexpr SessionId session = 710005;
    context.enterInfo.set_session_id(session);
    SessionMap().insert_or_assign(session, kPlayerId);
    PlayerAllData data;
    data.mutable_player_database_data()->set_player_id(kPlayerId);
    const auto aliveBefore = tlsEcs.actorRegistry.storage<entt::entity>().free_list();

    EXPECT_TRUE(PlayerLifecycleSystem::InitPlayerFromAllData(data, context) == entt::null);

    EXPECT_EQ(aliveBefore, tlsEcs.actorRegistry.storage<entt::entity>().free_list());
    EXPECT_FALSE(tlsEcs.playerList.contains(kPlayerId));
    EXPECT_FALSE(SessionMap().contains(session));
}



TEST_F(PlayerFeaturePersistenceTest, InitFromAllDataRejectsUnallocatedActorNodeWithoutPublishingPlayer)
{
    const ScopedInitializationScene target;
    ASSERT_FALSE(tlsEcs.playerList.contains(kPlayerId));
    const auto aliveBefore = tlsEcs.actorRegistry.storage<entt::entity>().free_list();
    PlayerAllData data;
    data.mutable_player_database_data()->set_player_id(kPlayerId);
    tlsIdGeneratorManager.SetNodeId(0);

    const auto created = PlayerLifecycleSystem::InitPlayerFromAllData(data, target.Context());

    EXPECT_TRUE(created == entt::null);
    EXPECT_FALSE(tlsEcs.playerList.contains(kPlayerId));
    EXPECT_EQ(aliveBefore, tlsEcs.actorRegistry.storage<entt::entity>().free_list());
    tlsIdGeneratorManager.SetNodeId(95);
}


// F070:同一玩家已在 playerList 里时 InitPlayerFromAllData 必须拒绝,且不留下孤儿实体。
// 修复前先 create 再查重,重复时刚建的实体既无组件、也不在 playerList 里,永远不会被销毁。
// entt 3.13 的实体存储把存活实体放在 [0, free_list()) 区间,free_list() 即存活实体数。
TEST_F(PlayerFeaturePersistenceTest, InitFromAllDataRejectsDuplicateWithoutLeavingOrphanEntity)
{
    const ScopedInitializationScene target;
    const auto existing = NewPlayer();
    tlsEcs.playerList.insert_or_assign(kPlayerId, existing);
    PlayerAllData data;
    data.mutable_player_database_data()->set_player_id(kPlayerId);
    const auto aliveBefore = tlsEcs.actorRegistry.storage<entt::entity>().free_list();

    const auto created = PlayerLifecycleSystem::InitPlayerFromAllData(data, target.Context());

    EXPECT_TRUE(created == entt::null) << "重复登记必须拒绝";
    EXPECT_EQ(aliveBefore, tlsEcs.actorRegistry.storage<entt::entity>().free_list()) << "拒绝时不得留下孤儿实体";
    const auto it = tlsEcs.playerList.find(kPlayerId);
    EXPECT_TRUE(it != tlsEcs.playerList.end() && it->second == existing) << "已登记的实体保持不变";
    tlsEcs.playerList.erase(kPlayerId);
}

} // namespace

TEST_F(PlayerFeaturePersistenceTest, OldAdmissionGenerationCannotInitializeReplacementEntity)
{
    const ScopedInitializationScene target;
    auto context=target.Context();
    context.roleAdmissionGeneration=10; // 本宿主明确确认的SQL原件是11，不能借当前角色替旧ctx补身份。
    context.handoffFence=*handoff_sql_barrier::EncodeFence({8,"fixture",10,std::string(32,'a'),
        handoff_sql_barrier::FenceState::kHeld,std::string(32,'b')});
    PlayerAllData data;data.mutable_player_database_data()->set_player_id(kPlayerId);
    const auto before=tlsEcs.actorRegistry.storage<entt::entity>().free_list();
    EXPECT_TRUE(PlayerLifecycleSystem::InitPlayerFromAllData(data,context)==entt::null);
    EXPECT_EQ(before,tlsEcs.actorRegistry.storage<entt::entity>().free_list());
    EXPECT_FALSE(tlsEcs.playerList.contains(kPlayerId));
}
