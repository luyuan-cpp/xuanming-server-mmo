#include <gtest/gtest.h>
#include <limits>
#include <memory>
#include "../../nodes/scene/handler/rpc/player/player_activity_handler.h"
#include "core/type_define/player_guid_comp.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/mission/comp/mission_comp.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/bag_marshal.h"
#include "player/system/mission_marshal.h"
#include "player/system/player_activity.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_mission.h"
#include "proto/common/component/battle_comp.pb.h"
#include "rpc/service_metadata/player_activity_service_metadata.h"
#include "table/code/activityschedule_table.h"
#include "table/code/item_table.h"
#include "table/code/mission_table.h"
#include "table/code/reward_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/reward_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
class TestActivityService : public SceneActivityClientPlayer {};
ActivityVitalityRuleTable testRule;
std::vector<ActivityVitalityRewardTable> testRewards;
bool policyAvailable = true;
int persistCalls = 0;
bool LoadTestPolicy(ActivityVitalityRuleTable& rule, std::vector<ActivityVitalityRewardTable>& rewards) {
    rule = testRule; rewards = testRewards; return policyAvailable;
}
bool DeferredPersist(entt::entity) { ++persistCalls; return true; }
void RecordPersisted(entt::entity player, uint64_t playerId = 434343) {
    PlayerAllData snapshot;
    auto* db = snapshot.mutable_player_database_data();
    db->set_player_id(playerId);
    mission_marshal::Marshal(player, *db->mutable_mission_component());
    tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player).Replace(snapshot);
}
bool ArmActivitySegment(uint64_t lo = 100000, uint64_t hi = 200000) {
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options; options.kindName = "item"; options.initialStep = 100000;
    if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
        [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
    client.Warm(); client.OnResponse(0, lo, hi); return client.IsReady();
}
class PlayerActivitySystemTest : public testing::Test {
protected:
    void SetUp() override {
        MissionTableManager::Instance().Load();
        ActivityScheduleTableManager::Instance().Load();
        RewardTableManager::Instance().Load();
        ItemTableManager::Instance().Load();
        ASSERT_TRUE(ArmActivitySegment());
        testRule.Clear(); testRule.set_id(47); testRule.set_enabled(true);
        testRule.set_configuration_revision(2); testRule.set_first_period_starts_at_ms(1000);
        testRule.set_period_duration_ms(100); testRule.set_points_limit(11);
        testRewards.clear(); auto& reward = testRewards.emplace_back();
        reward.set_id(7); reward.set_rule_id(47); reward.set_required_points(7); reward.set_reward_id(1);
        policyAvailable = true; persistCalls = 0;
        PlayerActivitySystem::SetRuleLookupForTest(&LoadTestPolicy);
        PlayerActivitySystem::SetPersistForTest(&DeferredPersist);
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{434343});
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 9;
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(Guid{434343});
        ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
        request.set_rule_id(47); request.set_configuration_revision(2); request.set_period_id(1000);
        request.set_threshold_id(7); request.set_request_id("intent-a");
    }
    void TearDown() override {
        PlayerActivitySystem::SetRuleLookupForTest(nullptr);
        PlayerActivitySystem::SetPersistForTest(nullptr);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        EXPECT_TRUE(ArmActivitySegment(1, uint64_t{1} << 54));
    }
    PlayerActivityLedger& Ledger() {
        return *tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0).GetMutableMissionList().mutable_activity_ledger();
    }
    void Points(uint32_t amount = 8, uint64_t period = 1000) {
        auto* source = Ledger().add_sources(); source->set_mission_scope(0); source->set_mission_id(15);
        source->set_rule_id(47); source->set_configuration_revision(2); source->set_period_id(period); source->set_points(amount);
    }
    uint32_t Count() {
        uint32_t count = 0;
        tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory].ForEachItem(
            [&](Guid, const ItemComp& item) { count += item.size(); });
        return count;
    }
    uint32_t Claim(uint64_t nowMs = 1050) { return PlayerActivitySystem::Claim(player, nowMs, request, response); }
    entt::entity player{entt::null};
    ClaimActivityVitalityRewardRequest request;
    ClaimActivityVitalityRewardResponse response;
};

TEST_F(PlayerActivitySystemTest, PeriodUsesExplicitAnchorExclusiveEndAndRejectsOverflow) {
    uint64_t start = 99, end = 99;
    EXPECT_FALSE(PlayerActivitySystem::ResolvePeriod(testRule, 999, start, end));
    EXPECT_EQ(0u, start); EXPECT_EQ(0u, end);
    ASSERT_TRUE(PlayerActivitySystem::ResolvePeriod(testRule, 1099, start, end));
    EXPECT_EQ(1000u, start); EXPECT_EQ(1100u, end);
    ASSERT_TRUE(PlayerActivitySystem::ResolvePeriod(testRule, 1100, start, end));
    EXPECT_EQ(1100u, start); EXPECT_EQ(1200u, end);
    testRule.set_period_duration_ms(std::numeric_limits<uint64_t>::max());
    EXPECT_FALSE(PlayerActivitySystem::ResolvePeriod(testRule, 1000, start, end));
}

TEST_F(PlayerActivitySystemTest, MissingRulesAreUnavailableAndUntrustedIdentityCannotGrant) {
    policyAvailable = false;
    PlayerActivityVitalitySnapshot snapshot;
    EXPECT_EQ(kSuccess, PlayerActivitySystem::BuildVitality(player, 1050, snapshot));
    EXPECT_FALSE(snapshot.available()); EXPECT_FALSE(snapshot.unavailable_reason().empty());
    EXPECT_EQ(kFeatureUnavailable, Claim()); EXPECT_EQ(0u, Count());
    EXPECT_EQ(kThisEntityIsInvalid, PlayerActivitySystem::Claim(entt::null, 1050, request, response));
    policyAvailable = true; Points();
    for (auto mutate : {0,1,2,3}) {
        const auto original = request;
        if (mutate == 0) request.set_rule_id(48);
        if (mutate == 1) request.set_configuration_revision(3);
        if (mutate == 2) request.set_period_id(1100);
        if (mutate == 3) request.set_threshold_id(8);
        EXPECT_EQ(kInvalidParameter, Claim()); EXPECT_EQ(0u, Count());
        request = original;
    }
}

TEST_F(PlayerActivitySystemTest, SnapshotPreservesZeroAndUsesConfiguredThresholdCount) {
    PlayerActivityVitalitySnapshot snapshot;
    ASSERT_EQ(kSuccess, PlayerActivitySystem::BuildVitality(player, 1050, snapshot));
    EXPECT_TRUE(snapshot.available()); EXPECT_EQ(0u, snapshot.points()); EXPECT_EQ(11u, snapshot.points_limit());
    ASSERT_EQ(1, snapshot.rewards_size()); EXPECT_FALSE(snapshot.rewards(0).can_claim());
    Points(25);
    ASSERT_EQ(kSuccess, PlayerActivitySystem::BuildVitality(player, 1050, snapshot));
    EXPECT_EQ(11u, snapshot.points()); EXPECT_TRUE(snapshot.rewards(0).can_claim()); EXPECT_FALSE(snapshot.rewards(0).durable());
}

TEST_F(PlayerActivitySystemTest, ClaimWaitsForExactPersistedIdentityAndReplayNeverDuplicatesItems) {
    Points(); ASSERT_EQ(kSuccess, Claim()); EXPECT_TRUE(response.claimed()); EXPECT_FALSE(response.durable());
    EXPECT_EQ(4u, Count()); EXPECT_EQ(1, persistCalls);
    ASSERT_EQ(kSuccess, Claim()); EXPECT_TRUE(response.replayed()); EXPECT_FALSE(response.durable()); EXPECT_EQ(4u, Count());
    RecordPersisted(player, 999); ASSERT_EQ(kSuccess, Claim()); EXPECT_FALSE(response.durable());
    RecordPersisted(player); ASSERT_EQ(kSuccess, Claim()); EXPECT_TRUE(response.durable()); EXPECT_EQ(4u, Count());
    auto& persisted = tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player);
    persisted.snapshot->mutable_player_database_data()->mutable_mission_component()->mutable_scopes(0)
        ->mutable_mission_list()->mutable_activity_ledger()->mutable_claims(0)->set_request_id("other-intent");
    ASSERT_EQ(kSuccess, Claim()); EXPECT_FALSE(response.durable());
}

TEST_F(PlayerActivitySystemTest, TwoClientsAndRequestIdentityConflictCannotDoubleClaim) {
    Points(); ASSERT_EQ(kSuccess, Claim());
    request.set_request_id("intent-b"); EXPECT_EQ(kRewardAlreadyClaimed, Claim()); EXPECT_EQ(4u, Count());
    request.set_request_id("intent-a"); request.set_threshold_id(8);
    EXPECT_EQ(kInvalidParameter, Claim()); EXPECT_EQ(4u, Count()); EXPECT_EQ(1, Ledger().claims_size());
}

TEST_F(PlayerActivitySystemTest, ConfigurationRevisionDoesNotResetPointsOrClaimIdentity) {
    Points(); ASSERT_EQ(kSuccess, Claim());
    testRule.set_configuration_revision(3); request.set_configuration_revision(3); request.set_request_id("intent-b");
    PlayerActivityVitalitySnapshot snapshot;
    ASSERT_EQ(kSuccess, PlayerActivitySystem::BuildVitality(player, 1050, snapshot));
    EXPECT_EQ(8u, snapshot.points()); ASSERT_EQ(1, snapshot.rewards_size()); EXPECT_TRUE(snapshot.rewards(0).claimed());
    EXPECT_EQ(kRewardAlreadyClaimed, Claim()); EXPECT_EQ(4u, Count());
}

TEST_F(PlayerActivitySystemTest, OriginalClaimRecoversAfterDayBoundaryRemovalAndRestartMarshal) {
    Points(); ASSERT_EQ(kSuccess, Claim()); RecordPersisted(player);
    QuestAllData saved; mission_marshal::Marshal(player, saved);
    BagAllData savedBag; bag_marshal::Marshal(player, savedBag);
    tlsEcs.actorRegistry.destroy(player);
    player = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{434343});
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 10;
    bag_marshal::Unmarshal(player, savedBag);
    mission_marshal::Unmarshal(player, saved);
    policyAvailable = false;
    ASSERT_EQ(kSuccess, Claim(1100)); EXPECT_TRUE(response.replayed()); EXPECT_FALSE(response.durable());
    RecordPersisted(player);
    ASSERT_EQ(kSuccess, Claim(1100)); EXPECT_TRUE(response.durable());
    EXPECT_FALSE(response.snapshot().available()); EXPECT_EQ(4u, Count());
    request.set_request_id("intent-b"); EXPECT_EQ(kRewardAlreadyClaimed, Claim(1100)); EXPECT_EQ(4u, Count());
}

TEST_F(PlayerActivitySystemTest, AnotherRoleCannotRecoverOrReadOriginalPlayersClaim) {
    Points(); ASSERT_EQ(kSuccess, Claim()); RecordPersisted(player);
    const auto other = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(other, Guid{434344});
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(other).epoch = 3;
    tlsEcs.actorRegistry.emplace<PlayerBagsComp>(other);
    EXPECT_EQ(kSuccess, PlayerMissionSystem::Initialize(other));
    ClaimActivityVitalityRewardResponse otherResponse;
    EXPECT_EQ(kInvalidParameter, PlayerActivitySystem::Claim(other, 1050, request, otherResponse));
    EXPECT_FALSE(otherResponse.claimed()); EXPECT_FALSE(otherResponse.durable());
    PlayerActivityVitalitySnapshot snapshot;
    EXPECT_EQ(kSuccess, PlayerActivitySystem::BuildVitality(other, 1050, snapshot));
    EXPECT_EQ(0u, snapshot.points()); EXPECT_FALSE(snapshot.rewards(0).claimed());
    EXPECT_EQ(4u, Count());
    tlsEcs.actorRegistry.destroy(other);
}

TEST_F(PlayerActivitySystemTest, NewPeriodRejectsOldRequestButDoesNotInheritPoints) {
    Points(); EXPECT_EQ(kInvalidParameter, Claim(1100)); EXPECT_EQ(0u, Count());
    request.set_period_id(1100); EXPECT_EQ(kInvalidParameter, Claim(1100));
    PlayerActivityVitalitySnapshot snapshot;
    ASSERT_EQ(kSuccess, PlayerActivitySystem::BuildVitality(player, 1100, snapshot));
    EXPECT_EQ(0u, snapshot.points()); EXPECT_FALSE(snapshot.rewards(0).can_claim());
}

TEST_F(PlayerActivitySystemTest, FrozenUnfencedAndBattlePlayersCannotClaimOrAcknowledgeDurable) {
    Points();
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player); EXPECT_EQ(kFeatureUnavailable, Claim());
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 0; EXPECT_EQ(kFeatureUnavailable, Claim());
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 9;
    tlsEcs.actorRegistry.emplace<InBattleComp>(player); EXPECT_EQ(kFeatureUnavailable, Claim());
    tlsEcs.actorRegistry.remove<InBattleComp>(player);
    ASSERT_EQ(kSuccess, Claim()); RecordPersisted(player);
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 0;
    ASSERT_EQ(kSuccess, Claim()); EXPECT_FALSE(response.durable()); EXPECT_EQ(4u, Count());
}

TEST_F(PlayerActivitySystemTest, FullInventoryKeepsEligibilityAndRetryUsesSameIntent) {
    Points(); auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    inventory.SetCapacityForRestore(0); EXPECT_NE(kSuccess, Claim()); EXPECT_EQ(0u, Count()); EXPECT_TRUE(Ledger().claims().empty());
    inventory.SetCapacityForRestore(100); EXPECT_EQ(kSuccess, Claim()); EXPECT_EQ(4u, Count());
}

TEST_F(PlayerActivitySystemTest, OnlyCompletedServerMissionSourceCreditsOnceAcrossDays) {
    const auto [row, error] = ActivityScheduleTableManager::Instance().FindByIdSilent(15);
    ASSERT_NE(nullptr, row);
    const auto original = *row;
    auto* editable = const_cast<ActivityScheduleTable*>(row);
    editable->set_vitality_points(8);
    PlayerActivitySystem::OnMissionCompleted(player, 0, 15, 1050);
    EXPECT_TRUE(Ledger().sources().empty());
    tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0).RestoreCompleted(15);
    const auto stableRevision = editable->configuration_revision();
    EXPECT_NE(0u, stableRevision);
    editable->set_configuration_revision(0);
    PlayerActivitySystem::OnMissionCompleted(player, 0, 15, 1050);
    EXPECT_TRUE(Ledger().sources().empty());
    editable->set_configuration_revision(stableRevision);
    PlayerActivitySystem::OnMissionCompleted(player, 1, 15, 1050); EXPECT_TRUE(Ledger().sources().empty());
    PlayerActivitySystem::OnMissionCompleted(player, 0, 15, 1050);
    PlayerActivitySystem::OnMissionCompleted(player, 0, 15, 1050);
    PlayerActivitySystem::OnMissionCompleted(player, 0, 15, 1100);
    EXPECT_EQ(1, Ledger().sources_size()); EXPECT_EQ(1000u, Ledger().sources(0).period_id());
    *editable = original;
}

TEST_F(PlayerActivitySystemTest, CompletedFactsAreNotLostWhenAssetsAreTemporarilyUnfencedOrRewardsInvalid) {
    const auto [row, error] = ActivityScheduleTableManager::Instance().FindByIdSilent(15);
    ASSERT_NE(nullptr, row);
    const auto original = *row;
    auto* editable = const_cast<ActivityScheduleTable*>(row);
    editable->set_vitality_points(8);
    tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0).RestoreCompleted(15);
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 0;
    testRewards[0].set_reward_id(999999);
    PlayerActivitySystem::OnMissionCompleted(player, 0, 15, 1050);
    EXPECT_EQ(1, Ledger().sources_size()); EXPECT_EQ(8u, Ledger().sources(0).points());
    EXPECT_EQ(kFeatureUnavailable, Claim()); EXPECT_EQ(0u, Count());
    testRewards[0].set_reward_id(1);
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 10;
    EXPECT_EQ(kSuccess, Claim()); EXPECT_EQ(4u, Count());
    *editable = original;
}

TEST_F(PlayerActivitySystemTest, InvalidScheduleKeepsActualCatalogIdentity) {
    const auto [row, error] = ActivityScheduleTableManager::Instance().FindByIdSilent(15);
    ASSERT_NE(nullptr, row);
    const auto original = *row;
    auto* editable = const_cast<ActivityScheduleTable*>(row);
    editable->set_category(99);
    GetActivityListResponse list;
    EXPECT_EQ(kSuccess, PlayerActivityReadSystem::BuildList(player, 1050, list));
    bool found = false;
    for (const auto& activity : list.activities()) {
        EXPECT_NE(0u, activity.activity_id());
        if (activity.activity_id() != 15) continue;
        found = true; EXPECT_EQ(15u, activity.mission_id()); EXPECT_FALSE(activity.can_participate());
        EXPECT_FALSE(activity.unavailable_reason().empty());
    }
    EXPECT_TRUE(found);
    *editable = original;
}
} // namespace

TEST(PlayerActivityHandlerTest, GeneratedClaimDispatchReturnsIdentityAndEntityError) {
    SceneActivityClientPlayerHandler handler(std::make_unique<TestActivityService>());
    ClaimActivityVitalityRewardRequest request;
    request.set_request_id("rpc-intent");
    ClaimActivityVitalityRewardResponse response;
    response.set_claimed(true);
    response.set_durable(true);
    EXPECT_EQ(345u, SceneActivityClientPlayerClaimActivityVitalityRewardMessageId);
    handler.CallMethod(SceneActivityClientPlayerClaimActivityVitalityRewardMethod,
        entt::null, &request, &response);
    EXPECT_EQ(kThisEntityIsInvalid, response.error_message().id());
    EXPECT_EQ("rpc-intent", response.request_id());
    EXPECT_FALSE(response.claimed());
    EXPECT_FALSE(response.durable());
    EXPECT_FALSE(response.has_snapshot());
}
