#include <gtest/gtest.h>

#include <limits>
#include <set>
#include <utility>
#include "../../nodes/scene/handler/event/mission_event_handler.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/condition/condition_type.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/mission/comp/mission_comp.h"
#include "modules/mission/system/mission.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/database/bag_quest_mail_data.pb.h"
#include "proto/common/event/mission_event.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/system/bag_marshal.h"
#include "services/scene/player/system/mission_marshal.h"
#include "services/scene/player/system/player_mission.h"
#include "table/code/condition_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/monster_table.h"
#include "table/code/item_table.h"
#include "table/code/reward_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mission_error_tip.pb.h"
#include "table/proto/tip/reward_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
// Tests temporarily alter already-owned snapshot rows/maps only; no table files are written.
// Restore on every exit (including fatal assertions), before any later test sees the snapshot.
template<class T>
struct ScopedTestRestore {
    T& value;
    T saved;
    explicit ScopedTestRestore(T& target) : value(target), saved(target) {}
    ~ScopedTestRestore() { value = std::move(saved); }
};
bool ArmMissionTestItemSegment(uint64_t lo = 100000, uint64_t hi = 200000) {
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.kindName = "item";
    options.initialStep = 100000;
    if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
        [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
    client.Warm();
    client.OnResponse(0, lo, hi);
    return client.IsReady();
}

class PlayerMissionSystemTest : public testing::Test {
protected:
    void SetUp() override {
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
        RewardTableManager::Instance().Load();
        ItemTableManager::Instance().Load();
        DungeonTableManager::Instance().Load();
        MonsterTableManager::Instance().Load();
        ASSERT_TRUE(ArmMissionTestItemSegment());
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<Guid>(player, Guid{424242});
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(Guid{424242});
        ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
        MissionEventHandler::Register();
    }
    void TearDown() override {
        MissionEventHandler::UnRegister();
        tlsEcs.dispatcher.clear<AcceptMissionEvent>();
        tlsEcs.dispatcher.clear<ConditionEvent>();
        tlsEcs.dispatcher.clear<OnAcceptedMissionEvent>();
        tlsEcs.dispatcher.clear<OnMissionAwardEvent>();
        GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, 1);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        // 恢复其它背包测试使用的无 I/O 号段基线。
        EXPECT_TRUE(ArmMissionTestItemSegment(1, uint64_t{1} << 54));
    }
    MissionsComp& Missions() { return tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0); }
    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    void ReadyReward(uint32_t id = 12) { Missions().RestoreCompleted(id); Missions().RestoreClaimable(id); }
    // Rules coverage intentionally bypasses content reachability: shipped missions 1/2 reference
    // monsters 3/4 with no dungeon spawn, but still exercise ordered and damaged progress handling.
    uint32_t AcceptForRuleTest(uint32_t id) {
        AcceptMissionEvent event;
        event.set_entity(entt::to_integral(player));
        event.set_mission_id(id);
        return MissionSystem::AcceptMission(event, Missions(), MissionConfig::GetSingleton());
    }
    ConditionEvent Kill(uint32_t monster, uint32_t count = 1) {
        ConditionEvent event;
        event.set_entity(entt::to_integral(player));
        event.set_condition_type(static_cast<uint32_t>(eConditionType::kConditionKillMonster));
        event.add_condition_ids(monster);
        event.set_amount(count);
        return event;
    }
    void Progress(uint32_t monster, uint32_t count = 1) {
        MissionEventHandler::ConditionEventHandler(Kill(monster, count));
    }
    uint32_t ItemCount() {
        uint32_t count = 0;
        Inventory().ForEachItem([&count](Guid, const ItemComp& item) { count += item.size(); });
        return count;
    }
    entt::entity player{entt::null};
};

TEST_F(PlayerMissionSystemTest, ReadCheckDoesNotCreateStateAndRejectsArbitraryScope) {
    const auto other = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<Guid>(other, Guid{123});
    EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::CheckAccept(other, 0, 4, 1000));
    EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<MissionsContainerComp>(other));
    EXPECT_EQ(kInvalidParameter, PlayerMissionSystem::Accept(player, 1, 4, 1000));
    EXPECT_EQ(0u, Missions().MissionSize());
    tlsEcs.actorRegistry.destroy(other);
}

TEST_F(PlayerMissionSystemTest, AcceptRejectsUnknownEmptyAndUnsupportedContentWithoutSideEffects) {
    EXPECT_EQ(kInvalidTableId, PlayerMissionSystem::Accept(player, 0, 999999, 1000));
    EXPECT_EQ(kInvalidTableData, PlayerMissionSystem::Accept(player, 0, 3, 1000));
    EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::Accept(player, 0, 6, 1000));
    EXPECT_EQ(0u, Missions().MissionSize());
    EXPECT_EQ(0u, Missions().TypeSetSize());
    EXPECT_EQ(0u, ItemCount());
}

TEST_F(PlayerMissionSystemTest, AcceptPersistsTimestampAndRejectsDuplicateTypeAndCompletedMission) {
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 4, 123456789));
    EXPECT_EQ(123456789u, Missions().GetMissionList().mission_begin_time().at(4));
    EXPECT_EQ(kMissionIdRepeated, PlayerMissionSystem::Accept(player, 0, 4, 123456790));
    EXPECT_EQ(kMissionTypeAlreadyExists, PlayerMissionSystem::Accept(player, 0, 7, 123456790));
    Progress(1);
    ASSERT_TRUE(Missions().IsComplete(4));
    EXPECT_EQ(kMissionAlreadyCompleted, PlayerMissionSystem::Accept(player, 0, 4, 123456791));
}

TEST_F(PlayerMissionSystemTest, SuccessfulClaimGrantsWholeRewardOnceAndConsumesClaimableOnlyAfterGrant) {
    ReadyReward();
    ASSERT_EQ(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(4u, ItemCount());
    EXPECT_FALSE(Missions().IsClaimable(12));
    EXPECT_TRUE(Missions().IsComplete(12));
    EXPECT_EQ(kRewardAlreadyClaimed, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(4u, ItemCount());
    EXPECT_EQ(kInvalidParameter, PlayerMissionSystem::ClaimReward(player, 1, 12));
}

TEST_F(PlayerMissionSystemTest, PrematureAndCorruptClaimDoesNotGrant) {
    EXPECT_EQ(kMissionIdNotInRewardList, PlayerMissionSystem::ClaimReward(player, 0, 12));
    Missions().RestoreClaimable(12);
    EXPECT_EQ(kInvalidTableData, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_TRUE(Missions().IsClaimable(12));
    EXPECT_EQ(0u, ItemCount());
}

TEST_F(PlayerMissionSystemTest, FullBagRetainsEntireRewardAndRetrySucceeds) {
    ReadyReward();
    Inventory().SetCapacityForRestore(3);
    EXPECT_NE(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(0u, ItemCount());
    EXPECT_TRUE(Missions().IsClaimable(12));
    Inventory().SetCapacityForRestore(4);
    EXPECT_EQ(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(4u, ItemCount());
}

TEST_F(PlayerMissionSystemTest, BlockedRewardAndFrozenPlayerPreserveClaimAndBag) {
    ReadyReward();
    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, 1);
    EXPECT_NE(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, 1);
    auto& block = tlsEcs.actorRegistry.emplace<PlayerItemBlockList>(player);
    block.Block(1);
    EXPECT_NE(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    block.Unblock(1);
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    EXPECT_NE(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_NE(kSuccess, PlayerMissionSystem::Accept(player, 0, 4, 1000));
    EXPECT_TRUE(Missions().IsClaimable(12));
    EXPECT_EQ(0u, ItemCount());
}

TEST_F(PlayerMissionSystemTest, SegmentTailCannotPartiallyGrantReward) {
    ReadyReward();
    ASSERT_TRUE(ArmMissionTestItemSegment(500000, 500002));
    EXPECT_NE(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(0u, ItemCount());
    EXPECT_EQ(2u, tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available());
    EXPECT_TRUE(Missions().IsClaimable(12));
}

TEST_F(PlayerMissionSystemTest, MissingRewardConfigurationDoesNotConsumeClaim) {
    Missions().RestoreCompleted(999999);
    Missions().RestoreClaimable(999999);
    EXPECT_EQ(kInvalidTableData, PlayerMissionSystem::ClaimReward(player, 0, 999999));
    EXPECT_TRUE(Missions().IsClaimable(999999));
    EXPECT_EQ(0u, ItemCount());
}

TEST_F(PlayerMissionSystemTest, AutoRewardFailureRemainsClaimableAndDoesNotRepeatAfterManualRetry) {
    Inventory().SetCapacityForRestore(3);
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 4, 1000));
    Progress(1);
    ASSERT_TRUE(Missions().IsComplete(4));
    ASSERT_TRUE(Missions().IsClaimable(4));
    tlsEcs.dispatcher.update<OnMissionAwardEvent>();
    EXPECT_TRUE(Missions().IsClaimable(4));
    EXPECT_EQ(0u, ItemCount());
    Inventory().SetCapacityForRestore(4);
    ASSERT_EQ(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 4));
    OnMissionAwardEvent retry;
    retry.set_entity(entt::to_integral(player));
    retry.set_mission_id(4);
    MissionEventHandler::OnMissionAwardEventHandler(retry);
    EXPECT_EQ(4u, ItemCount());
}

TEST_F(PlayerMissionSystemTest, OrderedDuplicateTargetsRequireSeparateFacts) {
    ASSERT_EQ(kSuccess, AcceptForRuleTest(2));
    Progress(2, 2); // 第二目标不能抢跑。
    EXPECT_EQ(0u, Missions().GetMissionList().missions().at(2).progress(1));
    Progress(1);
    Progress(2, 2);
    Progress(3);
    Progress(4, 2);
    ASSERT_TRUE(Missions().IsAccepted(2));
    EXPECT_EQ(0u, Missions().GetMissionList().missions().at(2).progress(4));
    Progress(4, 2);
    EXPECT_TRUE(Missions().IsAccepted(2));
    Progress(4, 2);
    EXPECT_TRUE(Missions().IsComplete(2));
    EXPECT_TRUE(Missions().IsClaimable(2));
}

TEST_F(PlayerMissionSystemTest, HugeProgressSaturatesAndTruncatedProgressCannotComplete) {
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 7, 1000));
    Progress(1, std::numeric_limits<uint32_t>::max());
    EXPECT_TRUE(Missions().IsComplete(7));
    ASSERT_EQ(kSuccess, AcceptForRuleTest(1));
    auto& active = (*Missions().GetMutableMissionList().mutable_missions())[1];
    active.mutable_progress()->RemoveLast();
    Progress(1, std::numeric_limits<uint32_t>::max());
    EXPECT_TRUE(Missions().IsAccepted(1));
    EXPECT_FALSE(Missions().IsComplete(1));
}

TEST_F(PlayerMissionSystemTest, FrozenConditionDoesNotAdvanceMission) {
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 4, 1000));
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    Progress(1);
    EXPECT_TRUE(Missions().IsAccepted(4));
    EXPECT_EQ(0u, Missions().GetMissionList().missions().at(4).progress(0));
}

TEST_F(PlayerMissionSystemTest, MissionAndRewardSurviveRoundTripWithoutAwardOrAcceptSideEffects) {
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 7, 1234));
    Progress(1, 3);
    ReadyReward();
    QuestAllData saved;
    mission_marshal::Marshal(player, saved);
    ASSERT_TRUE(saved.scoped_state_present());
    mission_marshal::Unmarshal(player, saved);
    EXPECT_EQ(3u, Missions().GetMissionList().missions().at(7).progress(0));
    EXPECT_EQ(1234u, Missions().GetMissionList().mission_begin_time().at(7));
    EXPECT_TRUE(Missions().GetTypeFilter().contains({1, 1}));
    EXPECT_TRUE(Missions().IsClaimable(12));
    EXPECT_EQ(0u, ItemCount());
    Progress(1, 5);
    EXPECT_TRUE(Missions().IsComplete(7));
}

TEST_F(PlayerMissionSystemTest, ClaimedRewardAndStableItemIdsSurviveReloadWithoutDuplicateGrant) {
    ReadyReward();
    ASSERT_EQ(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    QuestAllData quest;
    BagAllData bag;
    mission_marshal::Marshal(player, quest);
    bag_marshal::Marshal(player, bag);
    std::set<uint64_t> before;
    for (const auto& item : bag.items()) before.insert(item.item_uuid());
    bag_marshal::Unmarshal(player, bag);
    mission_marshal::Unmarshal(player, quest);
    EXPECT_EQ(kRewardAlreadyClaimed, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(4u, ItemCount());
    BagAllData after;
    bag_marshal::Marshal(player, after);
    std::set<uint64_t> afterIds;
    for (const auto& item : after.items()) afterIds.insert(item.item_uuid());
    EXPECT_EQ(before, afterIds);
}

TEST_F(PlayerMissionSystemTest, UnknownConfigurationAndOtherScopesArePreservedWithoutOpeningClaim) {
    auto& container = tlsEcs.actorRegistry.get<MissionsContainerComp>(player);
    auto& oldScope = container.GetOrCreate(9);
    oldScope.RestoreCompleted(999999);
    oldScope.RestoreClaimable(999999);
    auto& active = (*oldScope.GetMutableMissionList().mutable_missions())[999998];
    active.set_id(999998);
    active.add_progress(123);
    QuestAllData saved;
    mission_marshal::Marshal(player, saved);
    mission_marshal::Unmarshal(player, saved);
    const auto* restored = container.Get(9);
    ASSERT_NE(nullptr, restored);
    EXPECT_TRUE(restored->IsComplete(999999));
    EXPECT_TRUE(restored->IsClaimable(999999));
    EXPECT_EQ(123u, restored->GetMissionList().missions().at(999998).progress(0));
    QuestAllData again;
    mission_marshal::Marshal(player, again);
    EXPECT_EQ(saved.SerializeAsString(), again.SerializeAsString());
    EXPECT_EQ(kInvalidParameter, PlayerMissionSystem::ClaimReward(player, 9, 999999));
}

TEST_F(PlayerMissionSystemTest, LegacySnapshotImportsProgressAndDoesNotRegrantCompletedRewards) {
    QuestAllData old;
    auto* active = old.add_active();
    active->set_config_id(7);
    active->set_progress(3);
    active->set_accepted_at_ms(321);
    old.add_completed(12);
    mission_marshal::Unmarshal(player, old);
    EXPECT_EQ(3u, Missions().GetMissionList().missions().at(7).progress(0));
    EXPECT_TRUE(Missions().IsComplete(12));
    EXPECT_FALSE(Missions().IsClaimable(12));
    EXPECT_EQ(kRewardAlreadyClaimed, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(0u, ItemCount());
}
TEST_F(PlayerMissionSystemTest, OrderedMissionRoundTripRebuildsTheCurrentObjective) {
    ASSERT_EQ(kSuccess, AcceptForRuleTest(2));
    Progress(1);
    Progress(2, 1);
    QuestAllData saved;
    mission_marshal::Marshal(player, saved);
    mission_marshal::Unmarshal(player, saved);
    Progress(3); // 第二目标仍未完成，不能跳到第三目标。
    const auto& progress = Missions().GetMissionList().missions().at(2);
    EXPECT_EQ(1u, progress.progress(0));
    EXPECT_EQ(1u, progress.progress(1));
    EXPECT_EQ(0u, progress.progress(2));
    Progress(2, 1);
    Progress(3);
    EXPECT_EQ(1u, Missions().GetMissionList().missions().at(2).progress(2));
}

TEST_F(PlayerMissionSystemTest, EmptyScopedSnapshotDoesNotImportLegacyCompletedFallback) {
    QuestAllData saved;
    saved.set_scoped_state_present(true);
    saved.add_completed(12);
    mission_marshal::Unmarshal(player, saved);
    EXPECT_FALSE(Missions().IsComplete(12));
    EXPECT_FALSE(Missions().IsClaimable(12));
    EXPECT_EQ(0u, Missions().MissionSize());
}

TEST_F(PlayerMissionSystemTest, InvalidEntityAndMissingBagDoNotConsumeReward) {
    EXPECT_EQ(kThisEntityIsInvalid, PlayerMissionSystem::Initialize(entt::null));
    EXPECT_EQ(kThisEntityIsInvalid, PlayerMissionSystem::ClaimReward(entt::null, 0, 12));
    ReadyReward();
    tlsEcs.actorRegistry.remove<PlayerBagsComp>(player);
    EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_TRUE(Missions().IsClaimable(12));
}
TEST_F(PlayerMissionSystemTest, OwnedLevelFactsReplaceProgressInsteadOfAccumulating) {
    struct LevelConfig : MissionConfig {
        google::protobuf::RepeatedField<uint32_t> conditions;
        google::protobuf::RepeatedField<uint32_t> targets;
        LevelConfig() { conditions.Add(24); targets.Add(40); }
        const google::protobuf::RepeatedField<uint32_t>& GetConditionIds(uint32_t) const override { return conditions; }
        const google::protobuf::RepeatedField<uint32_t>& GetTargetCounts(uint32_t) const override { return targets; }
        uint32_t GetRewardId(uint32_t) const override { return 0; }
    } config;
    AcceptMissionEvent accept;
    accept.set_entity(entt::to_integral(player));
    accept.set_mission_id(4);
    ASSERT_EQ(kSuccess, MissionSystem::AcceptMission(accept, Missions(), config));
    ConditionEvent level;
    level.set_entity(entt::to_integral(player));
    level.set_condition_type(static_cast<uint32_t>(eConditionType::kConditionLevelUp));
    level.add_condition_ids(20);
    level.set_amount(20);
    MissionSystem::HandleConditionEvent(level, Missions(), config);
    MissionSystem::HandleConditionEvent(level, Missions(), config);
    ASSERT_TRUE(Missions().IsAccepted(4));
    EXPECT_EQ(20u, Missions().GetMissionList().missions().at(4).progress(0));
    level.set_amount(40);
    level.set_condition_ids(0, 40);
    MissionSystem::HandleConditionEvent(level, Missions(), config);
    EXPECT_TRUE(Missions().IsComplete(4));
}
TEST_F(PlayerMissionSystemTest, CheckClaimMatchesEligibilityWithoutReservingItemsOrIds) {
    EXPECT_EQ(kMissionIdNotInRewardList, PlayerMissionSystem::CheckClaim(player, 0, 12));
    ReadyReward();
    Inventory().SetCapacityForRestore(0);
    const auto remaining = tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available();
    QuestAllData before;
    mission_marshal::Marshal(player, before);
    EXPECT_EQ(kSuccess, PlayerMissionSystem::CheckClaim(player, 0, 12));
    EXPECT_EQ(kSuccess, PlayerMissionSystem::CheckClaim(player, 0, 12));
    EXPECT_EQ(remaining, tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available());
    EXPECT_EQ(0u, ItemCount());
    QuestAllData after;
    mission_marshal::Marshal(player, after);
    EXPECT_EQ(before.SerializeAsString(), after.SerializeAsString());
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    EXPECT_EQ(kInvalidParameter, PlayerMissionSystem::CheckClaim(player, 0, 12));
    EXPECT_EQ(kInvalidParameter, PlayerMissionSystem::ClaimReward(player, 0, 12));
}
TEST_F(PlayerMissionSystemTest, ReachableDungeonMonstersGateShippedMissionCatalogWithoutMutation) {
    for (const uint32_t id : {1u, 2u, 10u, 11u})
        EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::Accept(player, 0, id, 1000)) << id;
    for (const uint32_t id : {4u, 7u, 8u, 9u, 12u, 13u})
        EXPECT_EQ(kSuccess, PlayerMissionSystem::CheckAccept(player, 0, id, 1000)) << id;
    EXPECT_EQ(0u, Missions().MissionSize());
    EXPECT_EQ(0u, Missions().TypeSetSize());
    EXPECT_EQ(0u, ItemCount());
}

TEST_F(PlayerMissionSystemTest, KillAnyAndGenericFiltersRequireAtLeastOneActuallyReachableMonster) {
    const auto [condition, error] = ConditionTableManager::Instance().FindByIdSilent(1);
    ASSERT_NE(nullptr, condition);
    auto& mutableCondition = *const_cast<ConditionTable*>(condition);
    ScopedTestRestore restore(mutableCondition);
    mutableCondition.clear_condition1();
    mutableCondition.add_condition1(3); // exists in Monster, no Dungeon references it
    mutableCondition.add_condition1(1);
    EXPECT_EQ(kSuccess, PlayerMissionSystem::CheckAccept(player, 0, 4, 1000));
    mutableCondition.set_condition1(1, 4);
    EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::CheckAccept(player, 0, 4, 1000));
    mutableCondition.clear_condition1();
    EXPECT_EQ(kSuccess, PlayerMissionSystem::CheckAccept(player, 0, 4, 1000));
    mutableCondition.add_condition2(1);
    EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::CheckAccept(player, 0, 4, 1000));
}

TEST_F(PlayerMissionSystemTest, MissingDungeonOrMonsterConfigurationFailsClosed) {
    {
        auto& ids = const_cast<DungeonTableManager::IdMapType&>(DungeonTableManager::Instance().GetIdMap());
        ScopedTestRestore restore(ids);
        ids.clear();
        EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::Accept(player, 0, 4, 1000));
    }
    {
        auto& ids = const_cast<MonsterTableManager::IdMapType&>(MonsterTableManager::Instance().GetIdMap());
        ScopedTestRestore restore(ids);
        ids.erase(1); // a Dungeon reference cannot substitute for an absent monster config
        EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::CheckAccept(player, 0, 4, 1000));
        EXPECT_EQ(kSuccess, PlayerMissionSystem::CheckAccept(player, 0, 9, 1000));
        ids.clear();
        EXPECT_EQ(kServiceUnavailable, PlayerMissionSystem::Accept(player, 0, 9, 1000));
    }
    EXPECT_EQ(kSuccess, PlayerMissionSystem::CheckAccept(player, 0, 4, 1000));
    EXPECT_EQ(0u, Missions().MissionSize());
}

TEST_F(PlayerMissionSystemTest, AcceptBackfillsSavedPrerequisitesOnceWithoutReplayingOtherMissions) {
    // Give a second real mission row the same completion dependency with a target of two.
    // This represents an already-active consumer that must not receive another mission's backfill.
    const auto [row, error] = MissionTableManager::Instance().FindByIdSilent(13);
    ASSERT_NE(nullptr, row);
    auto& mutableRow = *const_cast<MissionTable*>(row);
    ScopedTestRestore restore(mutableRow);
    mutableRow.clear_condition_id();
    mutableRow.add_condition_id(27); // completed mission 15
    mutableRow.clear_target_count();
    mutableRow.add_target_count(2);
    Missions().RestoreCompleted(15);
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 13, 1000));
    ASSERT_EQ(1u, Missions().GetMissionList().missions().at(13).progress(0));
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 14, 2000));
    EXPECT_EQ(1u, Missions().GetMissionList().missions().at(13).progress(0));
    const auto& dependency = Missions().GetMissionList().missions().at(14);
    EXPECT_EQ(1u, dependency.progress(0));
    EXPECT_EQ(0u, dependency.progress(1));
    EXPECT_EQ(kMissionIdRepeated, PlayerMissionSystem::Accept(player, 0, 14, 3000));
    EXPECT_EQ(1u, Missions().GetMissionList().missions().at(13).progress(0));
    EXPECT_EQ(0u, ItemCount());
}

TEST_F(PlayerMissionSystemTest, CompletedPrerequisitesSurviveReloadAndFinishLateAcceptedMission) {
    Missions().RestoreCompleted(15);
    Missions().RestoreCompleted(16);
    QuestAllData saved;
    mission_marshal::Marshal(player, saved);
    mission_marshal::Unmarshal(player, saved);
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 14, 1000));
    EXPECT_TRUE(Missions().IsComplete(14));
    EXPECT_TRUE(Missions().IsClaimable(14));
    EXPECT_FALSE(Missions().IsAccepted(14));
    EXPECT_EQ(kMissionAlreadyCompleted, PlayerMissionSystem::Accept(player, 0, 14, 2000));
    EXPECT_EQ(0u, ItemCount()); // completion queued the ordinary award path; backfill does not grant directly
}

TEST_F(PlayerMissionSystemTest, TargetedConditionRefreshDoesNotReplaceLiveEventFanout) {
    Missions().SetMissionTypeNotRepeated(false);
    ASSERT_EQ(kSuccess, AcceptForRuleTest(7));
    ASSERT_EQ(kSuccess, AcceptForRuleTest(8));
    MissionSystem::HandleConditionEvent(Kill(1), Missions(), MissionConfig::GetSingleton(), 7);
    EXPECT_EQ(1u, Missions().GetMissionList().missions().at(7).progress(0));
    EXPECT_EQ(0u, Missions().GetMissionList().missions().at(8).progress(0));
    MissionSystem::HandleConditionEvent(Kill(1), Missions(), MissionConfig::GetSingleton());
    EXPECT_EQ(2u, Missions().GetMissionList().missions().at(7).progress(0));
    EXPECT_EQ(1u, Missions().GetMissionList().missions().at(8).progress(0));
}
TEST_F(PlayerMissionSystemTest, LegacyDuplicateActiveEntryDoesNotAppendExtraProgressSlots) {
    QuestAllData old;
    auto* first = old.add_active();
    first->set_config_id(7);
    first->set_progress(3);
    first->set_accepted_at_ms(321);
    auto* duplicate = old.add_active();
    duplicate->set_config_id(7);
    duplicate->set_progress(5);
    duplicate->set_accepted_at_ms(999);
    mission_marshal::Unmarshal(player, old);
    const auto& active = Missions().GetMissionList().missions().at(7);
    EXPECT_EQ(1, active.progress_size());
    EXPECT_EQ(3u, active.progress(0));
    EXPECT_EQ(321u, Missions().GetMissionList().mission_begin_time().at(7));
    Progress(1, 5);
    EXPECT_TRUE(Missions().IsComplete(7));
}

TEST_F(PlayerMissionSystemTest, LegacyExplicitCompletedEntryCannotReviveClaimFromDuplicateReadyEntry) {
    for (const bool completedFirst : {false, true}) {
        QuestAllData old;
        for (const uint32_t state : completedFirst ? std::initializer_list<uint32_t>{3, 2} :
                                                     std::initializer_list<uint32_t>{2, 3}) {
            auto* entry = old.add_active();
            entry->set_config_id(12);
            entry->set_state(state);
        }
        mission_marshal::Unmarshal(player, old);
        EXPECT_TRUE(Missions().IsComplete(12));
        EXPECT_FALSE(Missions().IsClaimable(12));
        EXPECT_FALSE(Missions().IsAccepted(12));
        EXPECT_EQ(kRewardAlreadyClaimed, PlayerMissionSystem::ClaimReward(player, 0, 12));
        EXPECT_EQ(0u, ItemCount());
    }
}
TEST_F(PlayerMissionSystemTest, PartiallyFilledStacksUseExactBatchGuidBudgetWithoutPartialReward) {
    const auto [reward, error] = RewardTableManager::Instance().FindByIdSilent(1);
    ASSERT_NE(nullptr, reward);
    auto& mutableReward = *const_cast<RewardTable*>(reward);
    ScopedTestRestore restore(mutableReward);
    mutableReward.clear_reward();
    for (const uint32_t configId : {10u, 11u}) {
        const auto [item, itemError] = ItemTableManager::Instance().FindByIdSilent(configId);
        ASSERT_NE(nullptr, item);
        ASSERT_EQ(999u, item->max_stack_size());
        auto* entry = mutableReward.add_reward();
        entry->set_reward_item(configId);
        entry->set_reward_count(1998);
    }
    // Two partial stacks for each config offer 1996 free units. Each 1998-item reward
    // needs exactly one new instance, even though its no-merge upper bound is two.
    Inventory().InsertItemForRestore(900001, 10, 1, 0);
    Inventory().InsertItemForRestore(900002, 10, 1, 1);
    Inventory().InsertItemForRestore(900003, 11, 1, 2);
    Inventory().InsertItemForRestore(900004, 11, 1, 3);
    ASSERT_EQ(4u, ItemCount());
    ReadyReward();
    BagAllData before;
    bag_marshal::Marshal(player, before);
    ASSERT_TRUE(ArmMissionTestItemSegment(500000, 500001));
    EXPECT_NE(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_TRUE(Missions().IsClaimable(12));
    EXPECT_EQ(1u, tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available());
    BagAllData unchanged;
    bag_marshal::Marshal(player, unchanged);
    EXPECT_EQ(before.SerializeAsString(), unchanged.SerializeAsString());
    ASSERT_TRUE(ArmMissionTestItemSegment(500000, 500002));
    ASSERT_EQ(kSuccess, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(4000u, ItemCount());
    EXPECT_EQ(0u, tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available());
    EXPECT_FALSE(Missions().IsClaimable(12));
    EXPECT_EQ(kRewardAlreadyClaimed, PlayerMissionSystem::ClaimReward(player, 0, 12));
    EXPECT_EQ(4000u, ItemCount());
}
} // namespace
