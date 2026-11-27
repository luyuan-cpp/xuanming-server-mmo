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
#include "proto/data_service/data_service.pb.h"
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
BattleSettlementData Battle(uint64_t id, uint32_t mode = match::MATCH_MODE_PVE_SOLO,
                            uint32_t difficulty = 1, bool manualValid = true) {
    BattleSettlementData result;
    result.set_player_id(kPlayer); result.set_battle_id(id); result.set_match_mode(mode);
    result.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN); result.set_player_team_index(0);
    result.set_total_rounds(3);
    result.set_battle_difficulty(difficulty);
    if (manualValid) {
        result.set_manual_action_rounds(2);
        result.set_effective_combat_contribution(true);
    }
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
        Reward::SetAccountQuotaSyncTransportForTest(nullptr);
        Reward::OnPlayerDestroyed(kPlayer);
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
    void Credit(uint64_t id = 1, uint64_t now = kNow, uint32_t mode = match::MATCH_MODE_PVE_SOLO,
                uint32_t difficulty = 1, Reward::BattleCreditDelta* deltaOut = nullptr) {
        ClientRequestLedgerComp candidate;
        ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), Battle(id, mode, difficulty), now, candidate, deltaOut));
        Ledger().Swap(&candidate);
    }
    uint32_t CountByBound(uint32_t configId, bool bound) {
        uint32_t total = 0;
        Inventory().ForEachItem([&](Guid, const ItemComp& item) {
            if (item.config_id() == configId && item.bound() == bound) total += item.size();
        });
        return total;
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
TEST_F(EquipmentMaterialRewardTest, Utc21DayAndMondayWeekBoundariesAreExplicit) {
    uint64_t start = 0, next = 0;
    ASSERT_TRUE(Reward::ResolveDay(kNow, start, next));
    EXPECT_EQ(kNow, start); EXPECT_EQ(kNow + kDay, next);
    ASSERT_TRUE(Reward::ResolveDay(kNow - 1, start, next)); EXPECT_EQ(kNow - kDay, start); EXPECT_EQ(kNow, next);
    EXPECT_FALSE(Reward::ResolveDay(std::numeric_limits<uint64_t>::max(), start, next));
    EXPECT_FALSE(Reward::ResolveDay(0, start, next));

    // kNow = 2026-11-17 21:00 UTC = 2026-11-18 05:00 UTC+8（周三），本周周一 05:00 UTC+8 为 kNow - 2*kDay。
    uint64_t weekStart = 0, weekNext = 0;
    ASSERT_TRUE(Reward::ResolveWeek(kNow, weekStart, weekNext));
    EXPECT_EQ(kNow - 2 * kDay, weekStart);
    EXPECT_EQ(weekStart + 7 * kDay, weekNext);
    // 周一 05:00 前 1 毫秒仍属于上一周。
    ASSERT_TRUE(Reward::ResolveWeek(weekStart - 1, start, next));
    EXPECT_EQ(weekStart - 7 * kDay, start);
    EXPECT_EQ(weekStart, next);
    EXPECT_FALSE(Reward::ResolveWeek(0, start, next));
    EXPECT_FALSE(Reward::ResolveWeek(std::numeric_limits<uint64_t>::max(), start, next));
}
TEST_F(EquipmentMaterialRewardTest, TwoStageQuotaHasNoDailyCapInStage1AndSwitchesToWeekly150After660) {
    // 阶段一：无日限，普通 PVE +10(1可交易)，困难 PVE +15(1可交易)，PVP 胜 +15(1可交易)，同一天连续累计。
    Reward::BattleCreditDelta d1, d2, d3;
    Credit(1, kNow, match::MATCH_MODE_PVE_SOLO, 1, &d1);
    EXPECT_EQ(10u, d1.totalUnitsAdded); EXPECT_EQ(1u, d1.tradableUnitsAdded); EXPECT_FALSE(d1.pvpLossIneligible);
    Credit(2, kNow, match::MATCH_MODE_PVE_TEAM, 2, &d2);
    EXPECT_EQ(15u, d2.totalUnitsAdded); EXPECT_EQ(1u, d2.tradableUnitsAdded);
    Credit(3, kNow, match::MATCH_MODE_PVP_TEAM, 1, &d3);
    EXPECT_EQ(15u, d3.totalUnitsAdded); EXPECT_EQ(1u, d3.tradableUnitsAdded);
    EXPECT_EQ(40u, Ledger().material_reward().lifetime_earned());
    EXPECT_EQ(3u, Ledger().material_reward().tradable_earned_total());
    EXPECT_EQ(40u, Ledger().material_reward().available_units());
    EXPECT_EQ(3u, Ledger().material_reward().available_tradable_units());

    // 推满第一阶段 660 份：用困难局(+15)推进到 655（此时可交易累计 44 < 60），
    // 实测剩余 5 份尾数（<10）截断到 660 且不增可交易份数。
    uint64_t battleId = 10;
    while (Ledger().material_reward().lifetime_earned() < 655) {
        Credit(++battleId, kNow, match::MATCH_MODE_PVE_TEAM, 2);
    }
    EXPECT_EQ(655u, Ledger().material_reward().lifetime_earned());
    EXPECT_LT(Ledger().material_reward().tradable_earned_total(), 60u);
    const uint32_t tradableBeforeTail = Ledger().material_reward().tradable_earned_total();
    // 剩余 5 份尾数（<10）截断到 660 且即使未触及 60 上限也不增可交易份数。
    Reward::BattleCreditDelta tail;
    Credit(++battleId, kNow, match::MATCH_MODE_PVE_SOLO, 1, &tail);
    EXPECT_EQ(5u, tail.totalUnitsAdded);
    EXPECT_EQ(0u, tail.tradableUnitsAdded);
    EXPECT_EQ(660u, Ledger().material_reward().lifetime_earned());
    EXPECT_EQ(tradableBeforeTail, Ledger().material_reward().tradable_earned_total());

    // 进入第二阶段：自动切换为每周 150 份全绑定（0 可交易）。
    GetEquipmentMaterialRewardResponse snap;
    ASSERT_EQ(kSuccess, Reward::Get(player, kNow, snap));
    EXPECT_TRUE(snap.snapshot().weekly_phase_active());
    EXPECT_EQ(0u, snap.snapshot().earned_this_week());
    EXPECT_EQ(150u, snap.snapshot().weekly_limit());

    for (int i = 0; i < 9; ++i) {
        Reward::BattleCreditDelta w;
        Credit(++battleId, kNow, match::MATCH_MODE_PVP_TEAM, 1, &w);
        EXPECT_EQ(15u, w.totalUnitsAdded);
        EXPECT_EQ(0u, w.tradableUnitsAdded);
    }
    Reward::BattleCreditDelta wNormal;
    Credit(++battleId, kNow, match::MATCH_MODE_PVE_SOLO, 1, &wNormal);
    EXPECT_EQ(10u, wNormal.totalUnitsAdded);
    EXPECT_EQ(0u, wNormal.tradableUnitsAdded);
    EXPECT_EQ(145u, Ledger().material_reward().earned_this_week());
    // 当周剩余 5 份截断封顶至 150。
    Reward::BattleCreditDelta capWeek;
    Credit(++battleId, kNow, match::MATCH_MODE_PVP_TEAM, 1, &capWeek);
    EXPECT_EQ(5u, capWeek.totalUnitsAdded);
    EXPECT_EQ(150u, Ledger().material_reward().earned_this_week());
    // 已满本周上限不再加；时钟回拨到上一周也不重置；跨到下一周重新开放 150。
    Reward::BattleCreditDelta fullWeek;
    Credit(++battleId, kNow, match::MATCH_MODE_PVE_SOLO, 1, &fullWeek);
    EXPECT_EQ(0u, fullWeek.totalUnitsAdded);
    Reward::BattleCreditDelta rollback;
    Credit(++battleId, kNow - 7 * kDay, match::MATCH_MODE_PVE_SOLO, 1, &rollback);
    EXPECT_EQ(0u, rollback.totalUnitsAdded);
    Reward::BattleCreditDelta nextWeek;
    Credit(++battleId, kNow + 7 * kDay, match::MATCH_MODE_PVE_SOLO, 1, &nextWeek);
    EXPECT_EQ(10u, nextWeek.totalUnitsAdded);
    EXPECT_EQ(0u, nextWeek.tradableUnitsAdded);
    EXPECT_EQ(10u, Ledger().material_reward().earned_this_week());
}
TEST_F(EquipmentMaterialRewardTest, OnlyEligibleCompletedBattlesAndAntiAfkPvpLossGrant) {
    auto battle = Battle(1); ClientRequestLedgerComp candidate;
    for (const auto mode : {match::MATCH_MODE_UNSPECIFIED, match::MATCH_MODE_PVP_CHALLENGE, match::MATCH_MODE_3V3}) {
        battle.set_match_mode(mode); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    }
    // PVE 败局不给。
    battle = Battle(1); battle.set_outcome(BATTLE_OUTCOME_SIDE_B_WIN); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    // PVP 胜局（含轮换物法易势 5V5、全服赛季战队锦标赛与帮会真人帮战）达标。
    battle = Battle(1, match::MATCH_MODE_POLARITY_5V5); EXPECT_TRUE(Reward::IsEligibleBattle(battle));
    battle = Battle(1, match::MATCH_MODE_SEASON_TOURNAMENT); EXPECT_TRUE(Reward::IsEligibleBattle(battle));
    battle = Battle(1, match::MATCH_MODE_GUILD_WAR_PVP); EXPECT_TRUE(Reward::IsEligibleBattle(battle));
    Reward::BattleCreditDelta guildWarPvpWin;
    ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), battle, kNow, candidate, &guildWarPvpWin));
    EXPECT_EQ(15u, guildWarPvpWin.totalUnitsAdded);
    // 帮会守旗阵踢馆（AI 托管）：胜 10 / 负 0。
    battle = Battle(1, match::MATCH_MODE_GUILD_WAR_SIEGE); EXPECT_TRUE(Reward::IsEligibleBattle(battle));
    Reward::BattleCreditDelta guildWarSiegeWin;
    ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), battle, kNow, candidate, &guildWarSiegeWin));
    EXPECT_EQ(10u, guildWarSiegeWin.totalUnitsAdded);
    battle.set_outcome(BATTLE_OUTCOME_SIDE_B_WIN); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    // PVP 败局：满足 >=3 回合、>=2 手动回合、有效贡献才达标，且为全绑定 10 份（0 可交易）。
    battle = Battle(2, match::MATCH_MODE_1V1); battle.set_outcome(BATTLE_OUTCOME_SIDE_B_WIN);
    EXPECT_TRUE(Reward::IsEligibleBattle(battle));
    Reward::BattleCreditDelta lossOk;
    ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), battle, kNow, candidate, &lossOk));
    EXPECT_EQ(10u, lossOk.totalUnitsAdded); EXPECT_EQ(0u, lossOk.tradableUnitsAdded); EXPECT_FALSE(lossOk.pvpLossIneligible);
    // PVP 挂机败局（回合不足 / 手动回合不足 / 无有效贡献）均不达标，标记 pvpLossIneligible=true 且不占 660 上限。
    auto afkShort = battle; afkShort.set_total_rounds(2);
    EXPECT_FALSE(Reward::IsEligibleBattle(afkShort));
    Reward::BattleCreditDelta afkDelta;
    ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), afkShort, kNow, candidate, &afkDelta));
    EXPECT_EQ(0u, afkDelta.totalUnitsAdded); EXPECT_TRUE(afkDelta.pvpLossIneligible);
    auto afkAuto = battle; afkAuto.set_manual_action_rounds(1);
    EXPECT_FALSE(Reward::IsEligibleBattle(afkAuto));
    auto afkZeroContrib = battle; afkZeroContrib.set_effective_combat_contribution(false);
    EXPECT_FALSE(Reward::IsEligibleBattle(afkZeroContrib));
    // 逃跑 / 0 回合 / VOID 仍全部拒绝。
    battle.set_fled(true); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    battle = Battle(1); battle.set_total_rounds(0); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    battle = Battle(1); battle.set_outcome(BATTLE_OUTCOME_VOID); EXPECT_FALSE(Reward::IsEligibleBattle(battle));
    const auto before = Assets(); ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), Battle(9), kNow, candidate));
    EXPECT_EQ(10u, candidate.material_reward().available_units()); EXPECT_EQ(before, Assets());
}
TEST_F(EquipmentMaterialRewardTest, SplitClaimPublishesTradableAndBoundStacksSeparatelyAndNeverMerges) {
    Credit(1); Credit(2); // 累计 20 份，其中 2 份可交易 + 18 份绑定。
    const auto request = Request(7, 13); const auto reply = Claim(request);
    ASSERT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, reply.outcome());
    EXPECT_FALSE(reply.durable()); EXPECT_FALSE(reply.replayed()); EXPECT_EQ(1, persistCalls);
    // 可交易优先给打造材料 3001：3001 得 2 可交易 + 5 绑定，3002 得 13 绑定。
    EXPECT_EQ(7u, Inventory().GetTotalItemCount(3001)); EXPECT_EQ(13u, Inventory().GetTotalItemCount(3002));
    EXPECT_EQ(2u, CountByBound(3001, false)); EXPECT_EQ(5u, CountByBound(3001, true));
    EXPECT_EQ(0u, CountByBound(3002, false)); EXPECT_EQ(13u, CountByBound(3002, true));
    EXPECT_EQ(0u, Ledger().material_reward().available_units());
    EXPECT_EQ(0u, Ledger().material_reward().available_tradable_units());
    EXPECT_EQ(1u, reply.result_revision());
    EXPECT_TRUE(client_request_ledger::Validate(Ledger()).empty());
    // 整理背包不得把可交易 3001 与绑定 3001 合并。
    bool changed = false;
    ASSERT_EQ(kSuccess, BagService::MergeAndCompact(player, Inventory(), CompactPolicy::kMergeOnly, &changed));
    EXPECT_EQ(2u, CountByBound(3001, false)); EXPECT_EQ(5u, CountByBound(3001, true));
    // 消耗 6 份 3001 时必须优先扣完 5 份绑定堆，再扣 1 份可交易堆（余 1 份可交易）。
    ASSERT_EQ(kSuccess, BagService::RemoveItemsExact(player, Inventory(), {{3001, 6}}, TX_EQUIP_ENHANCE, 1));
    EXPECT_EQ(0u, CountByBound(3001, true)); EXPECT_EQ(1u, CountByBound(3001, false));
    // 重放幂等校验。
    const auto after = Assets(); auto replay = Claim(request);
    EXPECT_TRUE(replay.replayed()); EXPECT_EQ(reply.result_revision(), replay.result_revision()); EXPECT_EQ(after, Assets());
    auto changedReq = request; changedReq.set_crafting_material_count(8); changedReq.set_enhancement_stone_count(12);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::REJECTED, Claim(changedReq).outcome()); EXPECT_EQ(after, Assets());
}
TEST_F(EquipmentMaterialRewardTest, LegacyUnmigratedBalanceRemainsClaimableAsBoundWithoutEating660Cap) {
    auto* legacy = Ledger().mutable_material_reward();
    legacy->set_day_starts_at_ms(kNow);
    legacy->set_earned_today(20);
    legacy->set_available_units(20);
    legacy->set_migrated_to_v2(false);
    ASSERT_TRUE(client_request_ledger::Validate(Ledger()).empty());
    // 老余额 20 份直接作为绑定领取，且不占用新 660 额度。
    const auto reply = Claim(Request(10, 10, 4100));
    ASSERT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, reply.outcome());
    EXPECT_EQ(10u, CountByBound(3001, true)); EXPECT_EQ(0u, CountByBound(3001, false));
    EXPECT_EQ(10u, CountByBound(3002, true)); EXPECT_EQ(0u, CountByBound(3002, false));
    EXPECT_EQ(0u, Ledger().material_reward().lifetime_earned());
    EXPECT_TRUE(Ledger().material_reward().migrated_to_v2());
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
    Credit(1); Credit(2); // 2 可交易 + 18 绑定，领 (10,10) 产生 3 堆：3001可交易(2)、3001绑定(8)、3002绑定(10)。
    Inventory().SetCapacityForRestore(2);
    const auto before = Assets(); EXPECT_NE(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(10, 10)).outcome());
    EXPECT_EQ(before, Assets()); EXPECT_EQ(0, persistCalls);
    Inventory().SetCapacityForRestore(3);
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(10, 10)).outcome());
    EXPECT_EQ(2u, CountByBound(3001, false)); EXPECT_EQ(8u, CountByBound(3001, true)); EXPECT_EQ(10u, CountByBound(3002, true));
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
    Credit(); auto malformed = Ledger(); malformed.mutable_material_reward()->set_lifetime_earned(665);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    malformed = Ledger(); malformed.mutable_material_reward()->set_available_units(11);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    malformed = Ledger(); malformed.mutable_material_reward()->set_available_tradable_units(5);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    malformed = Ledger(); malformed.mutable_material_reward()->set_week_starts_at_ms(kNow + 1);
    malformed.mutable_material_reward()->set_earned_this_week(10);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    ASSERT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, Claim(Request(4, 6)).outcome());
    malformed = Ledger(); malformed.mutable_records(0)->set_material_reward_revision(99);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
    malformed = Ledger(); malformed.mutable_records(0)->set_enhancement_stone_count(7);
    EXPECT_FALSE(client_request_ledger::Validate(malformed).empty());
}
TEST_F(EquipmentMaterialRewardTest, InboxWatermarkBlocksReexecutingSourceEvenAfterRecentReceiptEviction) {
    PlayerInboxLedgerComp inbox;
    auto& cursor = player_inbox_rules::MutableCursor(inbox, player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT);
    for (uint64_t seq = 1; seq <= 40; ++seq) {
        PlayerInboxDecision decision; decision.set_seq(seq); decision.set_request_id(7000 + seq);
        decision.set_msg_type(player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT); decision.set_outcome(player_inbox::PLAYER_INBOX_OUTCOME_APPLIED);
        ASSERT_TRUE(player_inbox_rules::RecordDecision(cursor, decision));
    }
    EXPECT_EQ(player_inbox_rules::DeliveryClass::kUnknown,
        player_inbox_rules::ClassifyDelivery(&cursor, 1, 7001, player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, nullptr));
    EXPECT_EQ(player_inbox_rules::DeliveryClass::kReplay,
        player_inbox_rules::ClassifyDelivery(&cursor, 40, 7040, player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, nullptr));
}
TEST_F(EquipmentMaterialRewardTest, StageTwoLightCardUnlocksWeeklyTradableOutputWithoutIncreasingTotalQuota) {
    auto* state = Ledger().mutable_material_reward();
    state->set_migrated_to_v2(true);
    state->set_lifetime_earned(Reward::kLifetimeLimit);
    state->set_tradable_earned_total(Reward::kTradableLifetimeLimit);
    state->set_available_units(0);
    state->set_available_tradable_units(0);
    ASSERT_TRUE(client_request_ledger::Validate(Ledger()).empty());

    // 无仙途轻享卡：第二阶段每胜局获得 10 份全绑定（0 可交易）。
    ClientRequestLedgerComp noCardCandidate;
    Reward::BattleCreditDelta noCardDelta;
    ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), Battle(501), kNow, noCardCandidate, &noCardDelta, false));
    EXPECT_EQ(10u, noCardDelta.totalUnitsAdded);
    EXPECT_EQ(0u, noCardDelta.tradableUnitsAdded);
    EXPECT_EQ(0u, noCardCandidate.material_reward().available_tradable_units());
    EXPECT_TRUE(client_request_ledger::Validate(noCardCandidate).empty());

    // 持有仙途轻享卡：总额度仍严格为 10 份（不卖战力/额度），其中 1 份解锁为市场可交易产出。
    ClientRequestLedgerComp cardCandidate;
    Reward::BattleCreditDelta cardDelta;
    ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(Ledger(), Battle(502), kNow, cardCandidate, &cardDelta, true));
    EXPECT_EQ(10u, cardDelta.totalUnitsAdded);
    EXPECT_EQ(1u, cardDelta.tradableUnitsAdded);
    EXPECT_EQ(1u, cardCandidate.material_reward().available_tradable_units());
    EXPECT_TRUE(client_request_ledger::Validate(cardCandidate).empty());
}
TEST_F(EquipmentMaterialRewardTest, AccountLevelSixtyTradableCapSurvivesAltAndFailClosesUnconfirmedClaim) {
    // 模拟 data_service 的账号级共享配额（同一账号下所有角色共享 60 份，删角不重置）。
    uint32_t dirAccountUsed = 59u;
    std::unordered_map<uint64_t, uint32_t> dirCharConfirmed;
    bool deferReply = true;
    std::function<void()> flushPendingReply;

    Reward::SetAccountQuotaSyncTransportForTest(
        [&](const data_service::SyncAccountStage1TradableRequest& req, Reward::AccountQuotaSyncDone done) -> bool {
            auto deliver = [&, req, done = std::move(done)]() {
                uint32_t& prev = dirCharConfirmed[req.player_id()];
                if (req.desired_char_total() > prev && dirAccountUsed < Reward::kTradableLifetimeLimit) {
                    const uint32_t grant = std::min(req.desired_char_total() - prev,
                                                    Reward::kTradableLifetimeLimit - dirAccountUsed);
                    prev += grant;
                    dirAccountUsed += grant;
                }
                data_service::SyncAccountStage1TradableResponse resp;
                resp.set_found(true);
                resp.set_confirmed_char_total(prev);
                resp.set_account_used_total(dirAccountUsed);
                done(true, &resp);
            };
            if (deferReply) {
                flushPendingReply = std::move(deliver);
                return true;
            }
            deliver();
            return true;
        });

    // 1. 角色 1 尚未收到账号目录确认前连胜两场（本地误以为拿到 2 份可交易，但账号实际只剩 1 份额度）。
    Credit(701);
    Credit(702);
    ASSERT_EQ(2u, Ledger().material_reward().tradable_earned_total());
    ASSERT_EQ(2u, Ledger().material_reward().available_tradable_units());
    ASSERT_EQ(0u, Ledger().material_reward().stage1_tradable_confirmed());

    // 2. 未获账号目录确认前 Claim 必须 fail-closed（返回 UNKNOWN / kServiceUnavailable），绝不铸造未确认可交易材料！
    const auto beforeUnconfirmed = Assets();
    const auto blocked = Claim(Request(10, 10, 8101));
    EXPECT_EQ(ClaimEquipmentMaterialRewardResponse::UNKNOWN, blocked.outcome());
    EXPECT_EQ(kServiceUnavailable, blocked.error_message().id());
    EXPECT_EQ(beforeUnconfirmed, Assets());

    // 3. data_service 回包：账号只剩 1 份额度 -> 确认 1 份，超额 1 份平滑降级为绑定自用（总份数 20 不变）。
    ASSERT_TRUE(static_cast<bool>(flushPendingReply));
    flushPendingReply();
    EXPECT_EQ(60u, dirAccountUsed);
    EXPECT_EQ(1u, Ledger().material_reward().tradable_earned_total());
    EXPECT_EQ(1u, Ledger().material_reward().stage1_tradable_confirmed());
    EXPECT_EQ(1u, Ledger().material_reward().available_tradable_units());
    EXPECT_EQ(20u, Ledger().material_reward().available_units());

    // 4. 确认后领取成功：精确发 1 份可交易 + 19 份绑定。
    const auto okClaim = Claim(Request(10, 10, 8102));
    ASSERT_EQ(ClaimEquipmentMaterialRewardResponse::APPLIED, okClaim.outcome());
    EXPECT_EQ(1u, CountByBound(3001, false));
    EXPECT_EQ(9u, CountByBound(3001, true));
    EXPECT_EQ(0u, CountByBound(3002, false));
    EXPECT_EQ(10u, CountByBound(3002, true));

    // 5. 模拟删角并建同账号新小号 kPlayer+1：加载后从账号目录同步得到 60/60，新小号首战即 0 可交易（全绑定）。
    Reward::OnPlayerDestroyed(kPlayer);
    const Guid altId = kPlayer + 1;
    const auto alt = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(alt, altId);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(alt).epoch = 10;
    auto& altBags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(alt);
    for (auto& bag : altBags.bags) bag.SetPlayerGuid(altId);
    tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(alt);
    tlsEcs.actorRegistry.emplace<PlayerItemGainBlockComp>(alt);
    tlsEcs.actorRegistry.emplace<LevelComp>(alt).set_level(80);
    ASSERT_TRUE(tlsEcs.playerList.emplace(altId, alt).second);

    deferReply = false;
    Reward::OnPlayerLoaded(alt, kNow);
    EXPECT_EQ(60u, Reward::KnownAccountStage1TradableUsed(alt));

    GetEquipmentMaterialRewardResponse altSnap;
    ASSERT_EQ(kSuccess, Reward::Get(alt, kNow, altSnap));
    EXPECT_EQ(60u, altSnap.snapshot().tradable_earned_total());
    EXPECT_EQ(60u, altSnap.snapshot().tradable_lifetime_limit());

    ClientRequestLedgerComp altCand;
    Reward::BattleCreditDelta altDelta;
    auto altBattle = Battle(801);
    altBattle.set_player_id(altId);
    ASSERT_EQ(kSuccess, Reward::PrepareBattleCredit(
        tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(alt), altBattle, kNow, altCand, &altDelta,
        /*lightCardActive=*/false, Reward::KnownAccountStage1TradableUsed(alt)));
    EXPECT_EQ(10u, altDelta.totalUnitsAdded);
    EXPECT_EQ(0u, altDelta.tradableUnitsAdded);
    EXPECT_EQ(0u, altCand.material_reward().available_tradable_units());

    Reward::OnPlayerDestroyed(altId);
    tlsEcs.playerList.erase(altId);
    tlsEcs.actorRegistry.destroy(alt);
}

TEST_F(EquipmentMaterialRewardTest, Stage2RolloverPoolAccumulatesUpToFourWeeks600AndResistsRollback) {
    // 先把阶段一推满 660。
    uint64_t bid = 1000;
    while (Ledger().material_reward().lifetime_earned() < 660) {
        Credit(++bid, kNow, match::MATCH_MODE_PVE_TEAM, 2);
    }
    ASSERT_EQ(660u, Ledger().material_reward().lifetime_earned());

    // 第 1 周只打 50 份（结余 100 份未用）。
    for (int i = 0; i < 5; ++i) {
        Credit(++bid, kNow, match::MATCH_MODE_PVE_SOLO, 1);
    }
    EXPECT_EQ(50u, Ledger().material_reward().earned_this_week());
    EXPECT_EQ(150u, Ledger().material_reward().stage2_pool_cap());

    // 第 2 周回归：滚存 100 + 本周 150 = 250 份。
    GetEquipmentMaterialRewardResponse snapW2;
    ASSERT_EQ(kSuccess, Reward::Get(player, kNow + 7 * kDay, snapW2));
    EXPECT_EQ(0u, snapW2.snapshot().earned_this_week());
    EXPECT_EQ(250u, snapW2.snapshot().weekly_limit());

    // 跳过 2 周（第 4 周回归）：100 + 3*150 = 550 份。
    GetEquipmentMaterialRewardResponse snapW4;
    ASSERT_EQ(kSuccess, Reward::Get(player, kNow + 21 * kDay, snapW4));
    EXPECT_EQ(550u, snapW4.snapshot().weekly_limit());

    // 跳过 10 周：封顶 4 周 = 600 份，可完整刷满 600 份且账本校验合法。
    const uint64_t w11 = kNow + 70 * kDay;
    GetEquipmentMaterialRewardResponse snapW11;
    ASSERT_EQ(kSuccess, Reward::Get(player, w11, snapW11));
    EXPECT_EQ(600u, snapW11.snapshot().weekly_limit());
    for (int i = 0; i < 40; ++i) {
        Credit(++bid, w11, match::MATCH_MODE_PVP_TEAM, 1);
    }
    EXPECT_EQ(600u, Ledger().material_reward().earned_this_week());
    EXPECT_EQ(600u, Ledger().material_reward().stage2_pool_cap());
    EXPECT_TRUE(client_request_ledger::Validate(Ledger()).empty());

    // 满 600 后本周期不再增加；时钟回拨到更早周亦不增加也不重置。
    Reward::BattleCreditDelta extra, rb;
    Credit(++bid, w11, match::MATCH_MODE_PVE_SOLO, 1, &extra);
    EXPECT_EQ(0u, extra.totalUnitsAdded);
    Credit(++bid, kNow + 7 * kDay, match::MATCH_MODE_PVE_SOLO, 1, &rb);
    EXPECT_EQ(0u, rb.totalUnitsAdded);
}
}
