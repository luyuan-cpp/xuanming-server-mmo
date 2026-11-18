// 公开接口回归：不启动服务，额度来自正式战斗候选，领取使用真实背包事务。
#include <gtest/gtest.h>
#include <array>
#include <functional>
#include <limits>
#include <memory>
#include <string>
#include <unordered_set>
#include <vector>
#include "core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/bag_marshal.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_equipment_material_reward.h"
#include "player/system/player_inbox_rules.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_gain_block_comp.pb.h"
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
using Reward = PlayerEquipmentMaterialRewardSystem;
constexpr Guid kPlayer = 849495;
constexpr uint64_t kDay = 86400000;
constexpr uint64_t kNow = 1794949200000; // 2026-11-17 21:00 UTC，游戏日新切点。
int persistCalls = 0;
bool persistReturn = false;
bool Persist(entt::entity) { ++persistCalls; return persistReturn; }

bool ArmIds() {
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false; options.kindName = "item"; options.initialStep = 100000;
    if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
        [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
    client.Warm(); client.OnResponse(0, 5300000, 5400000);
    return client.IsReady();
}
BattleSettlementData Battle(uint64_t id, uint32_t mode = match::MATCH_MODE_PVE_SOLO) {
    BattleSettlementData result;
    result.set_player_id(kPlayer); result.set_battle_id(id); result.set_match_mode(mode);
    result.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN); result.set_player_team_index(0); result.set_total_rounds(3);
    return result;
}
class EquipmentMaterialRewardTest : public testing::Test {
protected:
    void CreatePlayer() {
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayer);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 9;
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(kPlayer);
        tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerItemGainBlockComp>(player);
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(80);
        ASSERT_TRUE(tlsEcs.playerList.emplace(kPlayer, player).second);
    }
    void SetUp() override {
        ItemTableManager::Instance().Load();
        ASSERT_TRUE(ArmIds());
        blocks = GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kItem);
        for (auto id : blocks) GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, id);
        persistCalls = 0; persistReturn = false; Reward::SetPersistForTest(&Persist);
        BagService::SetItemInstanceInitializer({});
        CreatePlayer();
    }
    void TearDown() override {
        Reward::SetPersistForTest(nullptr); BagService::SetItemInstanceInitializer({});
        tlsEcs.playerList.erase(kPlayer);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        const auto currentBlocks = GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kItem);
        for (auto id : currentBlocks)
            GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, id);
        for (auto id : blocks) GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, id);
        EXPECT_TRUE(ArmIds());
    }
    ClientRequestLedgerComp& Ledger() { return tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player); }
    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    void Credit(uint64_t id = 1, uint64_t now = kNow, uint32_t mode = match::MATCH_MODE_PVE_SOLO) {
        ClientRequestLedgerComp candidate;
        ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), Battle(id, mode), now, candidate));
        Ledger().Swap(&candidate);
    }
    ClaimEquipmentMaterialRewardRequest Request(uint32_t crafting, uint32_t stone, uint64_t key = 4001) {
        ClaimEquipmentMaterialRewardRequest request;
        request.set_client_request_id(key); request.set_expected_revision(Ledger().material_reward().revision());
        request.set_crafting_material_count(crafting); request.set_enhancement_stone_count(stone);
        return request;
    }
    ClaimEquipmentMaterialRewardResponse Claim(const ClaimEquipmentMaterialRewardRequest& request) {
        ClaimEquipmentMaterialRewardResponse response;
        Reward::Claim(player, kNow, request, response);
        return response;
    }
    std::array<std::string, 2> Assets() {
        BagAllData bags; bag_marshal::Marshal(player, bags);
        return {bags.SerializeAsString(), Ledger().SerializeAsString()};
    }
    PlayerAllData Snapshot() {
        PlayerAllData snapshot;
        auto& db = *snapshot.mutable_player_database_data(); db.set_player_id(kPlayer);
        *db.mutable_client_request_ledger() = Ledger(); bag_marshal::Marshal(player, *db.mutable_bag_component());
        return snapshot;
    }
    void Persisted(const PlayerAllData& value, uint64_t epoch = 9, uint64_t seq = 17) {
        auto source = std::make_shared<PlayerAllData>(value); source->set_last_save_seq(seq);
        auto& persisted = tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player);
        persisted.RememberCommit(source, epoch, seq); persisted.Replace(*source);
    }
    entt::entity player{entt::null};
    std::unordered_set<uint32_t> blocks;
};

TEST_F(EquipmentMaterialRewardTest, QueryDoesNotCreateDailyAllowanceOrMutateLedger) {
    const auto before = Assets(); GetEquipmentMaterialRewardResponse response;
    ASSERT_EQ(kSuccess, Reward::Get(player, kNow, response));
    EXPECT_TRUE(response.snapshot().available()); EXPECT_FALSE(response.snapshot().can_claim());
    EXPECT_EQ(0u, response.snapshot().available_units()); EXPECT_EQ(0u, response.snapshot().batch_units());
    EXPECT_EQ(20u, response.snapshot().daily_limit()); EXPECT_EQ(3001u, response.snapshot().crafting_material().config_id());
    EXPECT_EQ(3002u, response.snapshot().enhancement_stone().config_id()); EXPECT_EQ(before, Assets());
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::REJECTED, Claim(Request(20, 0)).outcome());
    EXPECT_EQ(before, Assets());
}
TEST_F(EquipmentMaterialRewardTest, Utc21BoundaryAndOverflowAreExplicit) {
    uint64_t start = 0, next = 0;
    ASSERT_TRUE(Reward::ResolveDay(kNow, start, next));
    EXPECT_EQ(kNow, start); EXPECT_EQ(kNow + kDay, next);
    ASSERT_TRUE(Reward::ResolveDay(kNow - 1, start, next)); EXPECT_EQ(kNow - kDay, start); EXPECT_EQ(kNow, next);
    EXPECT_FALSE(Reward::ResolveDay(std::numeric_limits<uint64_t>::max(), start, next));
    EXPECT_FALSE(Reward::ResolveDay(0, start, next));
}
TEST_F(EquipmentMaterialRewardTest, SharedPvePvpDailyQuotaAndCrossDayUnclaimedBalance) {
    Credit(1); Credit(2, kNow, match::MATCH_MODE_1V1); Credit(3, kNow, match::MATCH_MODE_PVP_TEAM);
    EXPECT_EQ(20u, Ledger().material_reward().earned_today()); EXPECT_EQ(20u, Ledger().material_reward().available_units());
    EXPECT_EQ(0u, Ledger().material_reward().revision());
    Credit(4, kNow + kDay); EXPECT_EQ(30u, Ledger().material_reward().available_units());
    EXPECT_EQ(10u, Ledger().material_reward().earned_today());
    Credit(5, kNow); EXPECT_EQ(30u, Ledger().material_reward().available_units()); // 回拨不重置额度。
    GetEquipmentMaterialRewardResponse response;
    ASSERT_EQ(kSuccess, Reward::Get(player, kNow + kDay, response)); EXPECT_EQ(20u, response.snapshot().batch_units());
    EXPECT_EQ(30u, response.snapshot().available_units());
}
TEST_F(EquipmentMaterialRewardTest, OnlyEligibleCompletedBattlesGrantAndPrepareDoesNotMutateSource) {
    auto battle = Battle(1); ClientRequestLedgerComp candidate;
    for (const auto mode : {match::MATCH_MODE_UNSPECIFIED, match::MATCH_MODE_PVP_CHALLENGE, match::MATCH_MODE_3V3}) {
        battle.set_match_mode(mode); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    }
    battle = Battle(1); battle.set_outcome(BATTLE_OUTCOME_SIDE_B_WIN); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    battle.set_match_mode(match::MATCH_MODE_1V1); EXPECT_TRUE(Reward::IsEligibleBattle(battle));
    battle.set_outcome(BATTLE_OUTCOME_DRAW); EXPECT_TRUE(Reward::IsEligibleBattle(battle));
    battle.set_fled(true); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    battle = Battle(1); battle.set_total_rounds(0); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    battle = Battle(1); battle.set_outcome(BATTLE_OUTCOME_VOID); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    const auto before = Assets(); ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), Battle(9), kNow, candidate));
    EXPECT_EQ(10u, candidate.material_reward().available_units()); EXPECT_EQ(before, Assets());
}
TEST_F(EquipmentMaterialRewardTest, SplitClaimPublishesBothMaterialsAndLedgerTogether) {
    Credit(1); Credit(2); const auto request = Request(7, 13); const auto reply = Claim(request);
    ASSERT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, reply.outcome());
    EXPECT_FALSE(reply.durable()); EXPECT_FALSE(reply.replayed()); EXPECT_EQ(1, persistCalls);
    EXPECT_EQ(7u, Inventory().GetTotalItemCount(3001)); EXPECT_EQ(13u, Inventory().GetTotalItemCount(3002));
    EXPECT_EQ(0u, Ledger().material_reward().available_units()); EXPECT_EQ(1u, reply.result_revision());
    EXPECT_TRUE(client_request_ledger::Validate(Ledger()).empty());
    const auto after = Assets(); auto replay = Claim(request);
    EXPECT_TRUE(replay.replayed()); EXPECT_EQ(reply.result_revision(), replay.result_revision()); EXPECT_EQ(after, Assets());
    auto changed = request; changed.set_crafting_material_count(8); changed.set_enhancement_stone_count(12);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::REJECTED, Claim(changed).outcome()); EXPECT_EQ(after, Assets());
}
TEST_F(EquipmentMaterialRewardTest, AllCraftingAndAllStoneAreLegalButArbitraryTotalsAreNot) {
    Credit(); auto before = Assets();
    for (const auto count : {0u, 9u, 11u, 20u, std::numeric_limits<uint32_t>::max()}) {
        EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::REJECTED, Claim(Request(count, 0)).outcome()); EXPECT_EQ(before, Assets());
    }
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(10, 0)).outcome());
    Credit(2); EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(0, 10, 4002)).outcome());
    EXPECT_EQ(10u, Inventory().GetTotalItemCount(3001)); EXPECT_EQ(10u, Inventory().GetTotalItemCount(3002));
}
TEST_F(EquipmentMaterialRewardTest, InsufficientRoomForSecondMaterialRollsBackWholeClaim) {
    Credit(1); Credit(2); Inventory().SetCapacityForRestore(1);
    const auto before = Assets(); EXPECT_NE(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(10, 10)).outcome());
    EXPECT_EQ(before, Assets()); EXPECT_EQ(0, persistCalls);
    Inventory().SetCapacityForRestore(2);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(10, 10)).outcome());
}
TEST_F(EquipmentMaterialRewardTest, NewBattleCreditRejectsChangedBatchWithoutCreatingUnknownIntent) {
    Credit(1); const auto ten = Request(4, 6);
    Credit(2); const auto before = Assets();
    EXPECT_EQ(0u, Ledger().material_reward().revision());
    const auto reply = Claim(ten);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::REJECTED, reply.outcome());
    EXPECT_EQ(before, Assets()); EXPECT_EQ(0, persistCalls);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(8, 12, 4002)).outcome());
}
TEST_F(EquipmentMaterialRewardTest, NewCreditBeyondFullBatchKeepsConfirmedTwentyClaimValid) {
    Credit(1); Credit(2); const auto twenty = Request(7, 13);
    Credit(3, kNow + kDay);
    const auto reply = Claim(twenty);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, reply.outcome());
    EXPECT_EQ(10u, Ledger().material_reward().available_units()); EXPECT_EQ(1u, reply.result_revision());
    EXPECT_EQ(7u, Inventory().GetTotalItemCount(3001)); EXPECT_EQ(13u, Inventory().GetTotalItemCount(3002));
}
TEST_F(EquipmentMaterialRewardTest, BlockedSecondMaterialAndUnavailableIssuerPreserveAllAssets) {
    Credit(1); Credit(2); const auto request = Request(10, 10); const auto before = Assets();
    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, 3002);
    EXPECT_NE(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(request).outcome()); EXPECT_EQ(before, Assets());
    GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, 3002);
    tlsGuidSegmentRegistry.Get(GuidKind::kItem).Reset();
    EXPECT_NE(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(request).outcome()); EXPECT_EQ(before, Assets());
    EXPECT_EQ(0, persistCalls);
}
TEST_F(EquipmentMaterialRewardTest, StaleRevisionOrEvictedReceiptNeverMintsAgain) {
    Credit(1); const auto request = Request(10, 0); ASSERT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(request).outcome());
    for (uint64_t i = 0; i < client_request_ledger::kCapacity; ++i)
        ASSERT_TRUE(client_request_ledger::Remember(Ledger(), 9000 + i, ClientRequestKind::kEnhanceEquip, std::string(16, 'x'), {}));
    const auto before = Assets(); const auto unknown = Claim(request);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::UNKNOWN, unknown.outcome()); EXPECT_FALSE(unknown.durable());
    EXPECT_EQ(before, Assets()); EXPECT_EQ(10u, Inventory().GetTotalItemCount(3001));
}
TEST_F(EquipmentMaterialRewardTest, ReceiptAndBalanceSurviveSerializationAndPlayerRecreation) {
    Credit(1); Credit(2); const auto request = Request(6, 14);
    ASSERT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(request).outcome());
    auto saved = Snapshot(); std::string bytes; ASSERT_TRUE(saved.SerializeToString(&bytes));
    tlsEcs.playerList.erase(kPlayer); tlsEcs.actorRegistry.destroy(player); CreatePlayer();
    PlayerAllData restored; ASSERT_TRUE(restored.ParseFromString(bytes));
    Ledger() = restored.player_database_data().client_request_ledger();
    bag_marshal::Unmarshal(player, restored.player_database_data().bag_component());
    const auto before = Assets(); const auto reply = Claim(request);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, reply.outcome()); EXPECT_TRUE(reply.replayed());
    EXPECT_FALSE(reply.durable()); EXPECT_EQ(before, Assets());
    Persisted(restored); EXPECT_TRUE(Claim(request).durable());
}
TEST_F(EquipmentMaterialRewardTest, AppliedIsNotDurableUntilExactCurrentOwnerCommitProof) {
    Credit(); const auto before = Snapshot(); const auto request = Request(5, 5);
    persistReturn = true; ASSERT_FALSE(Claim(request).durable()); const auto applied = Snapshot();
    Persisted(before); EXPECT_FALSE(Claim(request).durable());
    Persisted(applied, 8, 17); EXPECT_FALSE(Claim(request).durable());
    Persisted(applied, 9, 0); EXPECT_FALSE(Claim(request).durable());
    auto wrong = applied; wrong.mutable_player_database_data()->set_player_id(kPlayer + 1);
    Persisted(wrong); EXPECT_FALSE(Claim(request).durable());
    tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player).Replace(applied);
    EXPECT_FALSE(Claim(request).durable());
    Persisted(applied); EXPECT_TRUE(Claim(request).durable());
}
TEST_F(EquipmentMaterialRewardTest, MissingComponentsFrozenPlayerAndSaturatedRevisionRejectWithoutWrites) {
    Credit(); const auto request = Request(10, 0); const auto before = Assets();
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::REJECTED, Claim(request).outcome()); EXPECT_EQ(before, Assets());
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 0;
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::REJECTED, Claim(request).outcome()); EXPECT_EQ(before, Assets());
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 9;
    Ledger().mutable_material_reward()->set_revision(std::numeric_limits<uint64_t>::max());
    const auto saturated = Assets();
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::REJECTED, Claim(Request(10, 0)).outcome()); EXPECT_EQ(saturated, Assets());
    ClientRequestLedgerComp candidate;
    EXPECT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), Battle(2), kNow, candidate)); EXPECT_EQ(saturated, Assets());
    Ledger().mutable_material_reward()->set_available_units(std::numeric_limits<uint64_t>::max() - 5);
    const auto overflow = Assets();
    EXPECT_NE(kSuccess, Reward::PrepareBattleCredit(Ledger(), Battle(2), kNow, candidate)); EXPECT_EQ(overflow, Assets());
    tlsEcs.actorRegistry.remove<ClientRequestLedgerComp>(player);
    GetEquipmentMaterialRewardResponse get; EXPECT_NE(kSuccess, Reward::Get(player, kNow, get));
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::UNKNOWN, Claim(request).outcome());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ClientRequestLedgerComp>(player));
}
TEST_F(EquipmentMaterialRewardTest, CorruptPersistedMaterialLedgerAndReceiptFailClosed) {
    Credit(); auto malformed = Ledger(); malformed.mutable_material_reward()->set_earned_today(21);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    malformed = Ledger(); malformed.mutable_material_reward()->set_available_units(11);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    malformed = Ledger(); malformed.mutable_material_reward()->set_day_starts_at_ms(kNow + 1);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    ASSERT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(4, 6)).outcome());
    malformed = Ledger(); malformed.mutable_records(0)->set_material_reward_revision(99);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    malformed = Ledger(); malformed.mutable_records(0)->set_enhancement_stone_count(7);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
}
TEST_F(EquipmentMaterialRewardTest, InboxWatermarkBlocksReexecutingSourceEvenAfterRecentReceiptEviction) {
    PlayerInboxLedgerComp inbox;
    auto& cursor = player_inbox_rules::MutableCursor(inbox, PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT);
    for (uint64_t seq = 1; seq <= 40; ++seq) {
        PlayerInboxDecision decision; decision.set_seq(seq); decision.set_request_id(7000 + seq);
        decision.set_msg_type(PLAYER_INBOX_MSG_BATTLE_SETTLEMENT); decision.set_outcome(PLAYER_INBOX_OUTCOME_APPLIED);
        ASSERT_TRUE(player_inbox_rules::RecordDecision(cursor, decision));
    }
    EXPECT_EQ(player_inbox_rules::DeliveryClass::kUnknown,
        player_inbox_rules::ClassifyDelivery(&cursor, 1, 7001, PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, nullptr));
    EXPECT_EQ(player_inbox_rules::DeliveryClass::kReplay,
        player_inbox_rules::ClassifyDelivery(&cursor, 40, 7040, PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, nullptr));
}
}
