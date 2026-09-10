#include <gtest/gtest.h>

#include <cstdint>
#include <limits>

#include "modules/bag/comp/player_bags_comp.h"
#include "modules/mission/comp/mission_comp.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_mission.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/system/player_feature_snapshot.h"
#include "table/code/condition_table.h"
#include "table/code/mission_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {

class PlayerFeatureSnapshotTest : public testing::Test {
protected:
    void SetUp() override {
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<Guid>(player, Guid{424200});
    }
    void TearDown() override {
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
    }
    PlayerBagsComp& AddBags() {
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(Guid{424200});
        return bags;
    }
    void LoadMissions() {
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
    }
    entt::entity player{entt::null};
};

TEST_F(PlayerFeatureSnapshotTest, ReadPreservesSparseLayoutAndWideIdentityAndCurrency) {
    auto& bags = AddBags();
    constexpr uint64_t itemId = (uint64_t{1} << 60) + 77;
    bags.bags[kInventory].InsertItemForRestore(itemId, 1, 2, 17);
    auto& currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
    currency.add_values((uint64_t{1} << 50) + 13);
    BagInfo out;
    ASSERT_EQ(kSuccess, PlayerBagSystem::BuildSnapshot(player, kInventory, out));
    ASSERT_EQ(1, out.items_size());
    EXPECT_EQ(itemId, out.items(0).item_id());
    EXPECT_EQ(2u, out.items(0).count());
    EXPECT_TRUE(out.items(0).name().empty());
    ASSERT_EQ(1, out.layout().slots_size());
    EXPECT_EQ(17u, out.layout().slots(0).slot());
    EXPECT_EQ(itemId, out.layout().slots(0).item_id());
    EXPECT_EQ(1u, out.layout().slots(0).width());
    EXPECT_EQ(1u, out.layout().slots(0).height());
    EXPECT_EQ(100u, out.layout().capacity());
    EXPECT_EQ(currency.values(0), out.currency().values(0));
    EXPECT_EQ(17u, bags.bags[kInventory].GetItemPosByGuid(itemId));
}

TEST_F(PlayerFeatureSnapshotTest, MissingBagReadDoesNotInitializeOrPublishStaleData) {
    BagInfo out;
    out.add_items()->set_item_id(55);
    EXPECT_EQ(kServiceUnavailable, PlayerBagSystem::BuildSnapshot(player, 0, out));
    EXPECT_TRUE(out.items().empty());
    EXPECT_FALSE(out.has_layout());
    EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player));
}

TEST_F(PlayerFeatureSnapshotTest, RejectsOutOfRangeBagAndInvalidEntity) {
    AddBags();
    BagInfo out;
    EXPECT_EQ(kInvalidParameter, PlayerBagSystem::BuildSnapshot(player, 4, out));
    EXPECT_EQ(kInvalidParameter, PlayerBagSystem::BuildSnapshot(player, std::numeric_limits<uint32_t>::max(), out));
    EXPECT_EQ(kThisEntityIsInvalid, PlayerBagSystem::BuildSnapshot(entt::null, 0, out));
}

TEST_F(PlayerFeatureSnapshotTest, EquipmentAndTemporaryCannotBeSorted) {
    AddBags();
    for (uint32_t bagType : {uint32_t{kEquipment}, uint32_t{kTemporary}}) {
        BagInfo out;
        ASSERT_EQ(kSuccess, PlayerBagSystem::BuildSnapshot(player, bagType, out));
        EXPECT_FALSE(out.layout().can_sort());
        bool changed = true;
        EXPECT_EQ(kInvalidParameter, PlayerBagSystem::Sort(player, bagType, out, changed));
        EXPECT_FALSE(changed);
        EXPECT_FALSE(out.has_layout());
    }
}

TEST_F(PlayerFeatureSnapshotTest, ExplicitSortReordersAndIsIdempotent) {
    auto& bag = AddBags().bags[kInventory];
    bag.InsertItemForRestore(991, 2, 1, 17);
    bag.InsertItemForRestore(992, 1, 1, 7);
    BagInfo out;
    bool changed = false;
    ASSERT_EQ(kSuccess, PlayerBagSystem::Sort(player, kInventory, out, changed));
    EXPECT_TRUE(changed);
    EXPECT_EQ(0u, bag.GetItemPosByGuid(992));
    EXPECT_EQ(1u, bag.GetItemPosByGuid(991));
    ASSERT_EQ(2, out.items_size());
    EXPECT_EQ(2u, out.items(0).count() + out.items(1).count());
    ASSERT_EQ(kSuccess, PlayerBagSystem::Sort(player, kInventory, out, changed));
    EXPECT_FALSE(changed);
}

TEST_F(PlayerFeatureSnapshotTest, FrozenSortKeepsSourceLayoutUnchanged) {
    auto& bag = AddBags().bags[kInventory];
    bag.InsertItemForRestore(993, 1, 1, 17);
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    BagInfo out;
    bool changed = true;
    EXPECT_EQ(kInvalidParameter, PlayerBagSystem::Sort(player, kInventory, out, changed));
    EXPECT_FALSE(changed);
    EXPECT_EQ(17u, bag.GetItemPosByGuid(993));
    EXPECT_FALSE(out.has_layout());
}

TEST_F(PlayerFeatureSnapshotTest, MissionCatalogueDoesNotAcceptOrInitializePlayerState) {
    LoadMissions();
    GetMissionListResponse out;
    ASSERT_EQ(kSuccess, PlayerMissionReadSystem::BuildList(player, out));
    EXPECT_EQ(MissionTableManager::Instance().FindAll().data_size(), out.missions_size());
    EXPECT_FALSE(out.state_persistent());
    EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player));
    for (const auto& mission : out.missions()) {
        EXPECT_TRUE(mission.configured());
        EXPECT_EQ(PLAYER_MISSION_NOT_ACCEPTED, mission.status());
        EXPECT_FALSE(mission.can_accept());
        EXPECT_FALSE(mission.can_claim());
        EXPECT_TRUE(mission.name().empty());
    }
}

TEST_F(PlayerFeatureSnapshotTest, MissionReadPreservesProgressAndDoesNotClaimRewards) {
    LoadMissions();
    auto& container = tlsEcs.actorRegistry.emplace<MissionsContainerComp>(player);
    auto& missions = container.GetOrCreate(MissionListComp::kPlayerMission);
    auto& active = (*missions.GetMutableMissionList().mutable_missions())[7];
    active.set_id(7);
    active.add_progress(5);
    SetBit(MissionBitMap, missions.GetClaimableRewards(), 12, true);
    const auto before = missions.GetMissionList().SerializeAsString();
    GetMissionListResponse out;
    ASSERT_EQ(kSuccess, PlayerMissionReadSystem::BuildList(player, out));
    bool sawActive = false;
    bool sawClaimable = false;
    for (const auto& mission : out.missions()) {
        if (mission.mission_id() == 7) {
            sawActive = true;
            EXPECT_EQ(PLAYER_MISSION_ACTIVE, mission.status());
            ASSERT_EQ(1, mission.objectives_size());
            EXPECT_EQ(0u, mission.objectives(0).objective_index());
            EXPECT_EQ(5u, mission.objectives(0).progress());
            EXPECT_EQ(8u, mission.objectives(0).target());
            EXPECT_FALSE(mission.objectives(0).completed());
        }
        if (mission.mission_id() == 12) {
            sawClaimable = true;
            EXPECT_EQ(PLAYER_MISSION_CLAIMABLE, mission.status());
            EXPECT_FALSE(mission.can_claim());
        }
    }
    EXPECT_TRUE(sawActive);
    EXPECT_TRUE(sawClaimable);
    EXPECT_EQ(before, missions.GetMissionList().SerializeAsString());
    EXPECT_TRUE(missions.IsClaimable(12));
}

TEST_F(PlayerFeatureSnapshotTest, RuntimeMissionMissingConfigurationRemainsVisible) {
    LoadMissions();
    auto& container = tlsEcs.actorRegistry.emplace<MissionsContainerComp>(player);
    auto& missions = container.GetOrCreate(MissionListComp::kPlayerMission);
    (*missions.GetMutableMissionList().mutable_missions())[999999].set_id(999999);
    GetMissionListResponse out;
    ASSERT_EQ(kSuccess, PlayerMissionReadSystem::BuildList(player, out));
    ASSERT_FALSE(out.missions().empty());
    const auto& last = out.missions(out.missions_size() - 1);
    EXPECT_EQ(999999u, last.mission_id());
    EXPECT_FALSE(last.configured());
    EXPECT_EQ(PLAYER_MISSION_ACTIVE, last.status());
}

TEST_F(PlayerFeatureSnapshotTest, ActivitiesOnlyExposeConfiguredEntriesWithoutInventingSchedule) {
    LoadMissions();
    GetActivityListResponse out;
    constexpr uint64_t nowMs = 1900000000123;
    ASSERT_EQ(kSuccess, PlayerActivityReadSystem::BuildList(player, nowMs, out));
    EXPECT_EQ(nowMs, out.server_time_ms());
    size_t expectedCount = 0;
    for (const auto& row : MissionTableManager::Instance().FindAll().data())
        if (row.mission_type() == 2) ++expectedCount;
    EXPECT_EQ(expectedCount, out.activities_size());
    for (const auto& activity : out.activities()) {
        const auto [row, error] = MissionTableManager::Instance().FindByIdSilent(activity.mission_id());
        ASSERT_NE(nullptr, row);
        EXPECT_EQ(2u, row->mission_type());
        EXPECT_EQ(row->reward_id(), activity.reward_id());
        EXPECT_EQ(PLAYER_ACTIVITY_UNSCHEDULED, activity.status());
        EXPECT_EQ(0u, activity.starts_at_ms());
        EXPECT_EQ(0u, activity.ends_at_ms());
        EXPECT_FALSE(activity.can_participate());
        EXPECT_TRUE(activity.name().empty());
    }
}

} // namespace
