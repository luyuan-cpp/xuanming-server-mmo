// 瑞兽迎春限时活动场景服全链路单测（docs/design/battle-festival-beast-implementation-contract.md）。
// 覆盖：规则与时间窗边界、配表完整性门禁、发起挑战锁宠与低血/死宠判定、异步 Destroy-Before-Callback、
// PrepareBattle 二次锁宠复核与普通入口防绕过、严格资产隔离结算与共享材料账入账、跨期不累计与永久资格冻结、
// 满普通宠位经专属赠送位领宠、号源耗尽不吃资格、同/异 UUID 并发领宠收敛与落盘确认、新设备无本地 UUID 恢复、
// 以及混单交易（物品+货币+绑定神兽）整单前置拒绝零副作用。

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/mission/comp/mission_comp.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/asset/asset_op.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/asset_op_ledger_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_battle_result_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_festival_beast_comp.pb.h"
#include "proto/common/component/player_item_gain_block_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_season_comp.pb.h"
#include "proto/common/component/player_trial_journal_comp.pb.h"
#include "proto/match/match_internal.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/scene.pb.h"
#include "security/token_security.h"
#include "services/scene/battle/system/player_battle.h"
#include "services/scene/player/comp/last_persisted_snapshot_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/asset_op_auth.h"
#include "services/scene/player/system/asset_op_system.h"
#include "services/scene/player/system/festival_beast_rules.h"
#include "services/scene/player/system/player_equipment_material_reward.h"
#include "services/scene/player/system/player_feature_snapshot.h"
#include "services/scene/player/system/player_festival_beast.h"
#include "services/scene/player/system/player_pet.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/class_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/festivalbeastactivity_table.h"
#include "table/code/festivalbeasttier_table.h"
#include "table/code/item_table.h"
#include "table/code/mission_table.h"
#include "table/code/monster_table.h"
#include "table/code/pet_table.h"
#include "table/code/petrule_table.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/festival_beast_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {

constexpr uint64_t kPlayerId = 99601;
constexpr uint64_t kStartMs = 1795564800000ULL;  // 2026-11-24 00:00:00 UTC
constexpr uint64_t kEndMs = kStartMs + festival_beast_rules::kRequiredOpenDurationMs;
constexpr uint64_t kShowcaseEndMs = kEndMs + 7ULL * festival_beast_rules::kDayMs;
constexpr uint64_t kOwnerEpoch = 11;
constexpr char kUuid1[] = "11111111-2222-4333-8444-555555555551";
constexpr char kUuid2[] = "11111111-2222-4333-8444-555555555552";
constexpr char kUuid3[] = "11111111-2222-4333-8444-555555555553";
constexpr char kRewardId[] = "festival_beast_chunxishou_v1";

int gPersistCalls = 0;
bool FakePersist(entt::entity) {
    ++gPersistCalls;
    return true;
}

constexpr std::string_view kTradeTestSecret = "festival-beast-trade-secret-01234567";
int64_t gFakeAssetOpNowMs = 0;
int64_t FakeAssetOpNowMs() { return gFakeAssetOpNowMs; }
std::string FakeTradeSecretLookup(std::string_view) { return std::string(kTradeTestSecret); }

bool ArmItemSegment(uint64_t lo = 96000000, uint64_t hi = 96100000) {
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false;
    options.kindName = "item";
    options.initialStep = 100000;
    if (!client.Enable(
            options, [](const std::string&, uint32_t) { return true; },
            [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) {
        return false;
    }
    client.Warm();
    client.OnResponse(0, lo, hi);
    return client.IsReady();
}

class PlayerFestivalBeastTest : public ::testing::Test {
protected:
    void SetUp() override {
        ItemTableManager::Instance().Load();
        ClassTableManager::Instance().Load();
        SkillTableManager::Instance().Load();
        AttributePoolTableManager::Instance().Load();
        AttributeDimensionTableManager::Instance().Load();
        PetTableManager::Instance().Load();
        PetRuleTableManager::Instance().Load();
        MonsterTableManager::Instance().Load();
        DungeonTableManager::Instance().Load();
        MissionTableManager::Instance().Load();
        FestivalBeastActivityTableManager::Instance().Load();
        FestivalBeastTierTableManager::Instance().Load();

        ASSERT_TRUE(ArmItemSegment());
        gPersistCalls = 0;
        PlayerFestivalBeastSystem::SetPersistForTest(&FakePersist);
        PlayerFestivalBeastSystem::SetLaunchSenderForTest(nullptr);
        SetActivityWindow(true, kStartMs, kEndMs, 1, 1);
        CreatePlayer();
    }

    void TearDown() override {
        PlayerFestivalBeastSystem::SetPersistForTest(nullptr);
        PlayerFestivalBeastSystem::SetLaunchSenderForTest(nullptr);
        PlayerAssetOpSystem::SetSecretLookupForTest(nullptr);
        PlayerAssetOpSystem::SetNowFnForTest(nullptr);
        PlayerAssetOpSystem::SetPersistFnForTest(nullptr);
        FestivalBeastActivityTableManager::Instance().Load();
        FestivalBeastTierTableManager::Instance().Load();
        PetTableManager::Instance().Load();
        DestroyPlayer();
        EXPECT_TRUE(ArmItemSegment());
    }

    void SetActivityWindow(bool enabled, uint64_t startMs, uint64_t endMs,
                           uint32_t configRevision = 1, uint32_t rewardDefVersion = 1) {
        auto& mgr = FestivalBeastActivityTableManager::Instance();
        auto* row = const_cast<FestivalBeastActivityTable*>(mgr.FindByIdSilent(202601).first);
        ASSERT_NE(nullptr, row);
        row->set_enabled(enabled);
        row->set_effective_start_at_ms(startMs);
        row->set_effective_end_at_ms(endMs);
        row->set_config_revision(configRevision);
        row->set_reward_definition_version(rewardDefVersion);
    }

    void CreatePlayer(uint32_t level = 30) {
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayerId);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = kOwnerEpoch;
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(level);
        auto& baseAttrs = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player);
        baseAttrs.set_health(800);
        baseAttrs.set_mana(500);
        baseAttrs.set_speed(400);
        auto& derived = tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player);
        derived.set_max_health(2000);
        derived.set_max_mana(1500);
        derived.set_physical_attack(1200);
        derived.set_magic_attack(1000);
        derived.set_defense(900);
        derived.set_speed(400);
        tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(kPlayerId);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerItemGainBlockComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerItemHoldComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerBattleResultComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerSeasonComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerTrialJournalComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerFestivalBeastComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerComp>(player);
        tlsEcs.actorRegistry.emplace<MissionsContainerComp>(player);
        ASSERT_TRUE(tlsEcs.playerList.emplace(kPlayerId, player).second);
    }

    void DestroyPlayer() {
        tlsEcs.playerList.erase(kPlayerId);
        if (tlsEcs.actorRegistry.valid(player)) {
            tlsEcs.actorRegistry.destroy(player);
        }
        player = entt::null;
    }

    uint64_t GrantAndSummonStarterPet(uint64_t health = 100) {
        uint64_t petId = 0;
        EXPECT_EQ(kSuccess, PetSystem::GrantPet(player, 1, petId));
        EXPECT_NE(0u, petId);
        EXPECT_EQ(kSuccess, PetSystem::Summon(player, petId));
        auto& pets = tlsEcs.actorRegistry.get<PlayerPetComp>(player);
        for (auto& p : *pets.mutable_pets()) {
            if (p.pet_id() == petId) {
                p.set_health(health);
            }
        }
        return petId;
    }

    void CommitCurrentPersistedSnapshot(uint64_t epoch = kOwnerEpoch, uint64_t seq = 21) {
        PlayerAllData snap;
        auto& db = *snap.mutable_player_database_data();
        db.set_player_id(kPlayerId);
        *db.mutable_pet_component() = tlsEcs.actorRegistry.get<PlayerPetComp>(player);
        *db.mutable_festival_beast_component() = tlsEcs.actorRegistry.get<PlayerFestivalBeastComp>(player);
        auto source = std::make_shared<PlayerAllData>(snap);
        source->set_last_save_seq(seq);
        auto& persisted = tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player);
        persisted.RememberCommit(source, epoch, seq);
        persisted.Replace(*source);
    }

    BattleSettlementData MakeFestivalSettlement(uint64_t battleId,
                                                uint32_t instanceId,
                                                eBattleOutcome outcome,
                                                bool fled = false,
                                                uint32_t rewardVersion = 1,
                                                const std::string& reqId = kUuid1) {
        BattleSettlementData s;
        s.set_battle_id(battleId);
        s.set_player_id(kPlayerId);
        s.set_match_mode(match::MATCH_MODE_PVE_SOLO);
        s.set_battle_config_id(111);
        s.set_battle_difficulty(1);
        s.set_boss_trial_kind(7);
        s.set_outcome(outcome);
        s.set_player_team_index(0);
        s.set_total_rounds(6);
        s.set_fled(fled);
        s.set_health(1);  // 模拟局内残血：严格隔离下不得写回局外
        s.set_mana(0);
        auto* fb = s.mutable_festival_beast();
        fb->set_activity_instance_id(instanceId);
        fb->set_config_revision(1);
        fb->set_reward_identity(kRewardId);
        fb->set_reward_definition_version(rewardVersion);
        fb->set_reward_pet_table_id(101);
        fb->set_required_wins(3);
        fb->set_locked_pet_id(1);
        fb->set_client_request_id(reqId);
        return s;
    }

    entt::entity player{entt::null};
};

TEST_F(PlayerFestivalBeastTest, RulesValidateTimeBoundariesTiersAndPetConfigIntegrity) {
    // 1. 左闭右开开放区间与 7 天集中领奖展示边界。
    EXPECT_EQ(FESTIVAL_BEAST_PHASE_UNSCHEDULED, festival_beast_rules::ResolvePhase(false, kStartMs, kEndMs, 7, kStartMs));
    EXPECT_EQ(FESTIVAL_BEAST_PHASE_UPCOMING, festival_beast_rules::ResolvePhase(true, kStartMs, kEndMs, 7, kStartMs - 1));
    EXPECT_EQ(FESTIVAL_BEAST_PHASE_OPEN, festival_beast_rules::ResolvePhase(true, kStartMs, kEndMs, 7, kStartMs));
    EXPECT_EQ(FESTIVAL_BEAST_PHASE_OPEN, festival_beast_rules::ResolvePhase(true, kStartMs, kEndMs, 7, kEndMs - 1));
    EXPECT_EQ(FESTIVAL_BEAST_PHASE_CLAIM_SHOWCASE, festival_beast_rules::ResolvePhase(true, kStartMs, kEndMs, 7, kEndMs));
    EXPECT_EQ(FESTIVAL_BEAST_PHASE_CLAIM_SHOWCASE, festival_beast_rules::ResolvePhase(true, kStartMs, kEndMs, 7, kShowcaseEndMs - 1));
    EXPECT_EQ(FESTIVAL_BEAST_PHASE_HISTORY, festival_beast_rules::ResolvePhase(true, kStartMs, kEndMs, 7, kShowcaseEndMs));

    // 2. UUID 解析校验。
    uint64_t hi = 0, lo = 0;
    EXPECT_TRUE(festival_beast_rules::ParseUuid128(kUuid1, hi, lo));
    EXPECT_FALSE(festival_beast_rules::ParseUuid128("00000000-0000-0000-0000-000000000000", hi, lo));
    EXPECT_FALSE(festival_beast_rules::ParseUuid128("not-a-uuid", hi, lo));

    // 3. 档位重叠或空洞均 fail-closed 拒绝。
    std::vector<FestivalBeastTierTable> badTiers;
    for (const auto& t : FestivalBeastTierTableManager::Instance().FindAll().data()) badTiers.push_back(t);
    EXPECT_TRUE(festival_beast_rules::ValidateTiers(badTiers));
    badTiers[1].set_min_level(badTiers[0].max_level());  // 重叠
    EXPECT_FALSE(festival_beast_rules::ValidateTiers(badTiers));
    badTiers[1].set_min_level(badTiers[0].max_level() + 2);  // 空洞
    EXPECT_FALSE(festival_beast_rules::ValidateTiers(badTiers));

    // 4. 神兽表行资源引用（model_id）缺失时拒绝开放与发放。
    PetTable brokenPet = *PetTableManager::Instance().FindByIdSilent(101).first;
    EXPECT_TRUE(festival_beast_rules::IsValidRewardPetRow(&brokenPet));
    brokenPet.set_model_id(0);
    EXPECT_FALSE(festival_beast_rules::IsValidRewardPetRow(&brokenPet));
}

TEST_F(PlayerFestivalBeastTest, LaunchLocksPetAllowsLowHpRejectsDeadRechecksOnPrepareAndHandlesDestroyBeforeCallback) {
    // 无出战宠拒绝。
    StartFestivalBeastChallengeRequest req;
    req.set_client_request_id(kUuid1);
    req.set_activity_instance_id(202601);
    req.set_config_revision(1);
    StartFestivalBeastChallengeResponse resp;
    EXPECT_EQ(kFestivalBeastPetRequired, PlayerFestivalBeastSystem::StartChallenge(player, kStartMs, req, resp));

    // 0 血倒地宝宝拒绝。
    const uint64_t petId = GrantAndSummonStarterPet(/*health=*/0);
    EXPECT_EQ(kFestivalBeastPetDead, PlayerFestivalBeastSystem::StartChallenge(player, kStartMs, req, resp));

    // 低血存活宝宝（1 HP）允许进入，且按人物等级 30 选档（Tier 3）。
    for (auto& p : *tlsEcs.actorRegistry.get<PlayerPetComp>(player).mutable_pets()) {
        if (p.pet_id() == petId) p.set_health(1);
    }

    match::StartActivityBattleRequest capturedMatchReq;
    PlayerFestivalBeastSystem::LaunchReplyFn capturedCallback;
    PlayerFestivalBeastSystem::SetLaunchSenderForTest(
        [&](const match::StartActivityBattleRequest& matchReq, PlayerFestivalBeastSystem::LaunchReplyFn cb) {
            capturedMatchReq = matchReq;
            capturedCallback = std::move(cb);
            return true;
        });

    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::StartChallenge(player, kStartMs + 1000, req, resp));
    EXPECT_EQ(FESTIVAL_BEAST_LAUNCH_LAUNCHING, resp.launch_state());
    EXPECT_EQ(petId, resp.locked_pet_id());
    EXPECT_EQ(3u, resp.matched_tier_id());
    EXPECT_EQ(BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST, capturedMatchReq.activity_context().kind());
    EXPECT_EQ(petId, capturedMatchReq.activity_context().festival_beast().locked_pet_id());

    // 在途结果未知时，换不同 UUID 发起重复开局必须被拒绝。
    StartFestivalBeastChallengeRequest dupReq = req;
    dupReq.set_client_request_id(kUuid2);
    EXPECT_EQ(kFestivalBeastLaunchInFlight, PlayerFestivalBeastSystem::StartChallenge(player, kStartMs + 1100, dupReq, resp));

    // Match 返回准入结果后，新设备（无本地 UUID）调 GetFestivalBeastDetail 可直接恢复原请求与 battle_id。
    match::StartActivityBattleResponse admittedReply;
    admittedReply.set_reject(match::ACTIVITY_BATTLE_REJECT_NONE);
    admittedReply.set_battle_id(880011);
    admittedReply.set_admitted_at_ms(kStartMs + 1200);
    capturedCallback(true, &admittedReply);

    GetFestivalBeastDetailResponse detail;
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::GetDetail(player, kStartMs + 1300, GetFestivalBeastDetailRequest{}, detail));
    ASSERT_TRUE(detail.has_pending_launch());
    EXPECT_EQ(kUuid1, detail.pending_launch().client_request_id());
    EXPECT_EQ(880011u, detail.pending_launch().battle_id());
    EXPECT_EQ(petId, detail.pending_launch().locked_pet_id());
    EXPECT_EQ(FESTIVAL_BEAST_LAUNCH_ADMITTED, detail.pending_launch().launch_state());

    // PrepareBattle 二次复核：预检后收回宝宝 → 拒绝开局，返回 kFestivalBeastPetChanged。
    ASSERT_EQ(kSuccess, PetSystem::Recall(player));
    EXPECT_EQ(kFestivalBeastPetChanged,
              PlayerFestivalBeastSystem::VerifyPrepareBattlePet(player, capturedMatchReq.activity_context().festival_beast()));

    // 重新出战原低血宝宝 → 二次复核通过。
    ASSERT_EQ(kSuccess, PetSystem::Summon(player, petId));
    EXPECT_EQ(kSuccess,
              PlayerFestivalBeastSystem::VerifyPrepareBattlePet(player, capturedMatchReq.activity_context().festival_beast()));

    // Destroy-Before-Callback：发出异步请求后销毁玩家实体，回调返回不得崩溃或悬空解引用。
    PlayerFestivalBeastSystem::OnBattlePrepareCancelledOrVoid(player, 880011, kStartMs + 2000);
    StartFestivalBeastChallengeRequest req3 = req;
    req3.set_client_request_id(kUuid3);
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::StartChallenge(player, kStartMs + 2100, req3, resp));
    DestroyPlayer();
    ASSERT_NO_FATAL_FAILURE(capturedCallback(true, &admittedReply));
}

TEST_F(PlayerFestivalBeastTest, SettlementIsLosslessCreditsSharedMaterialsCreatesFrozenPermanentEligibilityAndDoesNotCarryUnderThree) {
    GrantAndSummonStarterPet(/*health=*/123);
    const uint64_t hpBefore = tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health();
    const uint64_t manaBefore = tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana();
    auto& comp = tlsEcs.actorRegistry.get<PlayerFestivalBeastComp>(player);

    // 1. 失败与逃跑不增胜场。
    PlayerFestivalBeastComp cand;
    PlayerFestivalBeastSystem::SettlementDelta delta;
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::PrepareBattleCredit(
                            comp, MakeFestivalSettlement(501, 202601, BATTLE_OUTCOME_SIDE_B_WIN), kStartMs + 1000, cand, delta));
    EXPECT_EQ(0u, delta.winsAfter);
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::PrepareBattleCredit(
                            comp, MakeFestivalSettlement(502, 202601, BATTLE_OUTCOME_SIDE_A_WIN, /*fled=*/true), kStartMs + 2000, cand, delta));
    EXPECT_EQ(0u, delta.winsAfter);

    // 2. 第 1、2 胜累计，且未满 3 胜不跨期带入 202602；同时接现有共享材料账 +10。
    for (uint64_t i = 1; i <= 2; ++i) {
        const auto s = MakeFestivalSettlement(600 + i, 202601, BATTLE_OUTCOME_SIDE_A_WIN);
        ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::PrepareBattleCredit(comp, s, kStartMs + i * 10000, cand, delta));
        comp.Swap(&cand);
        EXPECT_EQ(i, delta.winsAfter);
        EXPECT_FALSE(delta.newlyEligible);

        ClientRequestLedgerComp matCand;
        PlayerEquipmentMaterialRewardSystem::BattleCreditDelta matDelta;
        ASSERT_EQ(kSuccess, PlayerEquipmentMaterialRewardSystem::PrepareBattleCredit(
                                tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player), s, kStartMs + i * 10000, matCand, &matDelta));
        tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).Swap(&matCand);
        EXPECT_EQ(10u, matDelta.totalUnitsAdded);
    }
    EXPECT_EQ(20u, tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).material_reward().available_units());

    // 未满 3 胜查看新期次 202602 进度为 0（不跨期累计）。
    PlayerFestivalBeastComp candNextPeriod;
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::PrepareBattleCredit(
                            comp, MakeFestivalSettlement(699, 202602, BATTLE_OUTCOME_SIDE_B_WIN), kEndMs + 1000, candNextPeriod, delta));
    EXPECT_EQ(0u, delta.winsBefore);
    EXPECT_EQ(0u, delta.winsAfter);

    // 3. 截止前准入的在途第 3 胜晚于结束时刻结算，仍计入原期 202601 并生成永久资格（冻结 version=1）。
    const auto win3 = MakeFestivalSettlement(603, 202601, BATTLE_OUTCOME_SIDE_A_WIN, false, /*rewardVersion=*/1);
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::PrepareBattleCredit(comp, win3, kEndMs + 5000, cand, delta));
    comp.Swap(&cand);
    EXPECT_EQ(2u, delta.winsBefore);
    EXPECT_EQ(3u, delta.winsAfter);
    EXPECT_TRUE(delta.newlyEligible);

    // 4. 改表提高 reward_definition_version 后，已达成的永久资格仍保持冻结版本 1；局外血蓝丝毫未变。
    SetActivityWindow(true, kStartMs, kEndMs, 2, /*rewardDefVersion=*/9);
    GetFestivalBeastDetailResponse detail;
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::GetDetail(player, kShowcaseEndMs + 86400000ULL, GetFestivalBeastDetailRequest{}, detail));
    EXPECT_EQ(FESTIVAL_BEAST_PHASE_HISTORY, detail.phase());
    EXPECT_TRUE(detail.has_permanent_eligibility());
    EXPECT_EQ(1u, detail.reward_definition_version());
    EXPECT_TRUE(detail.can_claim_reward());
    EXPECT_EQ(hpBefore, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(manaBefore, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana());

    // 即使活动转入历史态，活动大厅列表仍保留未领神兽红点入口。
    GetActivityListResponse listResp;
    PlayerFestivalBeastSystem::AppendActivityCatalogEntry(player, kShowcaseEndMs + 86400000ULL, listResp);
    ASSERT_EQ(1, listResp.activities_size());
    EXPECT_EQ(PLAYER_ACTIVITY_ENTRY_FESTIVAL_BEAST, listResp.activities(0).entry_kind());
    EXPECT_TRUE(listResp.activities(0).has_claimable_reward());
}

TEST_F(PlayerFestivalBeastTest, ClaimUsesExclusiveSlotSurvivesIdFailureConvergesConcurrentUuidsAndBlocksMixedTradeBeforeSideEffects) {
    // 1. 塞满普通携带上限（max_pets），证明专属赠送位独立于普通携带量。
    const uint32_t maxPets = PetRuleTableManager::Instance().FindAll().data().Get(0).max_pets();
    ASSERT_GT(maxPets, 0u);
    for (uint32_t i = 0; i < maxPets; ++i) {
        uint64_t normalPetId = 0;
        ASSERT_EQ(kSuccess, PetSystem::GrantPet(player, 1, normalPetId));
    }
    EXPECT_EQ(0u, PetSystem::AvailableCaptureSlots(player));
    uint64_t overflowPetId = 0;
    EXPECT_EQ(kPetSlotFull, PetSystem::GrantPet(player, 1, overflowPetId));
    // 普通发放禁止铸造绑定活动神兽。
    EXPECT_EQ(kFeatureUnavailable, PetSystem::GrantPet(player, 101, overflowPetId));

    // 赋予永久待领资格。
    auto& comp = tlsEcs.actorRegistry.get<PlayerFestivalBeastComp>(player);
    auto* elig = comp.add_eligibilities();
    elig->set_reward_identity(kRewardId);
    elig->set_reward_definition_version(1);
    elig->set_reward_pet_table_id(101);
    elig->set_source_activity_instance_id(202601);
    elig->set_earned_at_ms(kStartMs + 10000);
    elig->set_earned_battle_id(7001);

    // 2. 号源耗尽时领取失败，绝不消耗永久资格或留半截回执。
    tlsGuidSegmentRegistry.Get(GuidKind::kItem).Reset();
    ClaimFestivalBeastRewardRequest claimReq1;
    claimReq1.set_client_request_id(kUuid1);
    claimReq1.set_reward_identity(kRewardId);
    ClaimFestivalBeastRewardResponse claimResp;
    EXPECT_EQ(kPetIdGenerateFailed, PlayerFestivalBeastSystem::ClaimReward(player, kShowcaseEndMs + 1000, claimReq1, claimResp));
    EXPECT_TRUE(comp.claim_receipts().empty());
    EXPECT_EQ(maxPets, static_cast<uint32_t>(tlsEcs.actorRegistry.get<PlayerPetComp>(player).pets_size()));

    // 3. 恢复号源后在历史态成功领出神兽（突破普通 max_pets 走专属赠送位），存盘未确认前 durable=false。
    ASSERT_TRUE(ArmItemSegment(97000000, 97100000));
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::ClaimReward(player, kShowcaseEndMs + 2000, claimReq1, claimResp));
    EXPECT_EQ(FESTIVAL_BEAST_CLAIM_APPLIED, claimResp.outcome());
    EXPECT_FALSE(claimResp.replayed());
    EXPECT_FALSE(claimResp.durable());
    const uint64_t springBeastPetId = claimResp.pet_id();
    ASSERT_NE(0u, springBeastPetId);
    EXPECT_EQ(101u, claimResp.reward_pet_table_id());
    EXPECT_EQ(maxPets + 1, static_cast<uint32_t>(tlsEcs.actorRegistry.get<PlayerPetComp>(player).pets_size()));

    // 校验固定首发资质模板（四维均为 12500）。
    const PetInstance* springBeast = nullptr;
    for (const auto& p : tlsEcs.actorRegistry.get<PlayerPetComp>(player).pets()) {
        if (p.pet_id() == springBeastPetId) springBeast = &p;
    }
    ASSERT_NE(nullptr, springBeast);
    ASSERT_EQ(4, springBeast->aptitude_size());
    for (const auto& [dimId, apt] : springBeast->aptitude()) {
        EXPECT_EQ(12500u, apt) << "dim=" << dimId;
    }

    // 普通携带容量仍只算普通宠，普通商店/捕捉无法占用专属位。
    EXPECT_EQ(0u, PetSystem::AvailableCaptureSlots(player));

    // 4. 确认落盘快照后，同 UUID 重放回 durable=true；不同 UUID 并发收敛至 ALREADY_CLAIMED 且回同一 pet_id。
    CommitCurrentPersistedSnapshot();
    ClaimFestivalBeastRewardResponse replayResp;
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::ClaimReward(player, kShowcaseEndMs + 3000, claimReq1, replayResp));
    EXPECT_EQ(FESTIVAL_BEAST_CLAIM_APPLIED, replayResp.outcome());
    EXPECT_TRUE(replayResp.replayed());
    EXPECT_TRUE(replayResp.durable());
    EXPECT_EQ(springBeastPetId, replayResp.pet_id());

    ClaimFestivalBeastRewardRequest claimReq2;
    claimReq2.set_client_request_id(kUuid2);
    claimReq2.set_reward_identity(kRewardId);
    ClaimFestivalBeastRewardResponse concurrentResp;
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::ClaimReward(player, kShowcaseEndMs + 4000, claimReq2, concurrentResp));
    EXPECT_EQ(FESTIVAL_BEAST_CLAIM_ALREADY_CLAIMED, concurrentResp.outcome());
    EXPECT_TRUE(concurrentResp.durable());
    EXPECT_EQ(springBeastPetId, concurrentResp.pet_id());
    EXPECT_EQ(kUuid1, concurrentResp.client_request_id());
    EXPECT_EQ(maxPets + 1, static_cast<uint32_t>(tlsEcs.actorRegistry.get<PlayerPetComp>(player).pets_size()));

    // 5. 新设备无本地 UUID 查询详情可直接恢复 claimed_pet_id 与 claim_request_id。
    GetFestivalBeastDetailResponse recoveredDetail;
    ASSERT_EQ(kSuccess, PlayerFestivalBeastSystem::GetDetail(player, kShowcaseEndMs + 5000, GetFestivalBeastDetailRequest{}, recoveredDetail));
    EXPECT_TRUE(recoveredDetail.reward_claimed());
    EXPECT_TRUE(recoveredDetail.reward_claim_durable());
    EXPECT_EQ(springBeastPetId, recoveredDetail.claimed_pet_id());
    EXPECT_EQ(kUuid1, recoveredDetail.claim_request_id());

    // 6. 绑定流转限制：直接 RemovePetForTrade 与混单 AssetDebit（普通物品 + 金币 + 春禧兽）全部前置拒绝、零副作用。
    PetInstance removedSnap;
    EXPECT_EQ(kPetNotTransferable, PetSystem::RemovePetForTrade(player, springBeastPetId, removedSnap));

    // 给玩家发金币，构造混单扣款（金币 + 绑定春禧兽）走 PlayerAssetOpSystem。
    ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 500, TX_GM_GRANT, 1));
    const int64_t goldBefore = CurrencySystem::GetBalance(player, kCurrencyGold);
    gFakeAssetOpNowMs = static_cast<int64_t>(kShowcaseEndMs + 6000);
    PlayerAssetOpSystem::SetNowFnForTest(&FakeAssetOpNowMs);
    PlayerAssetOpSystem::SetPersistFnForTest(&FakePersist);
    PlayerAssetOpSystem::SetSecretLookupForTest(&FakeTradeSecretLookup);

    AssetOpRequest debitReq;
    debitReq.set_player_id(kPlayerId);
    debitReq.set_stream(ASSET_OP_STREAM_TRADE_DEBIT);
    debitReq.set_stream_epoch(1);
    debitReq.set_seq(1);
    debitReq.set_tx_type(static_cast<uint32_t>(TX_TRADE));
    debitReq.set_correlation_id(9001);
    auto* c = debitReq.mutable_bundle()->add_currencies();
    c->set_currency_type(kCurrencyGold);
    c->set_amount(100);
    debitReq.mutable_bundle()->set_pet_id(springBeastPetId);
    auto* auth = debitReq.mutable_auth();
    auth->set_caller("trade");
    auth->set_timestamp_ms(static_cast<uint64_t>(gFakeAssetOpNowMs));
    auth->set_signature_hex(token_security::HmacSha256Hex(kTradeTestSecret, AssetOpCanonical("debit", debitReq)));

    AssetOpResponse debitResp;
    PlayerAssetOpSystem::Debit(debitReq, debitResp);
    PlayerAssetOpSystem::SetSecretLookupForTest(nullptr);
    PlayerAssetOpSystem::SetNowFnForTest(nullptr);
    PlayerAssetOpSystem::SetPersistFnForTest(nullptr);
    EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, debitResp.outcome());
    EXPECT_EQ(kPetNotTransferable, debitResp.reason().id());
    EXPECT_EQ(goldBefore, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(maxPets + 1, static_cast<uint32_t>(tlsEcs.actorRegistry.get<PlayerPetComp>(player).pets_size()));
}

}  // namespace
