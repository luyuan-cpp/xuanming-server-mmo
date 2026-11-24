// 五行试炼志 V2 Scene 侧单测（docs/design/long-term-progression-v2.md）。
// 覆盖：配表校验（含互斥外显拦截）、快照生成、单人/合战结算记账与战法印判定、
// 规则版本升级旧纪录兼容、未知高位保留与有效行计分、一次性里程碑领取（含满包拒领/幂等重放/外显直授/不碰核心日额度）、
// 自用法宝拓印兑换仅自用法宝（含允许同名双件/余额不足/非法法宝/满包/幂等重放）。

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "engine/core/time/system/time.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/utils/bit_index/bit_index_util.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/component/player_season_comp.pb.h"
#include "proto/common/component/player_trial_journal_comp.pb.h"
#include "proto/scene/player_appearance.pb.h"
#include "proto/scene/player_trial_journal.pb.h"
#include "services/battle/system/trial_journal_rules.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/player_appearance.h"
#include "services/scene/player/system/player_season.h"
#include "services/scene/player/system/player_trial_journal.h"
#include "table/code/appearancerule_table.h"
#include "table/code/appearanceseries_table.h"
#include "table/code/bit_index/trialjournalmilestone_table_id_bit_index.h"
#include "table/code/bit_index/trialjournalseal_table_id_bit_index.h"
#include "table/code/characterappearance_table.h"
#include "table/code/equipartifact_table.h"
#include "table/code/item_table.h"
#include "table/code/seasonrule_table.h"
#include "table/code/seasonseal_table.h"
#include "table/code/trialjournalentry_table.h"
#include "table/code/trialjournalmilestone_table.h"
#include "table/code/trialjournalseal_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/trial_journal_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {

constexpr uint64_t kTestPlayerId = 99501;
constexpr uint64_t kNowMs = 1790000000000ULL;

bool ArmTrialItemSegment(uint64_t lo, uint64_t hi) {
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

BattleSettlementData MakeTrialSettlement(uint64_t battleId,
                                         uint32_t entryId,
                                         uint32_t ruleVersion,
                                         uint32_t rounds,
                                         uint32_t humanCount = 1,
                                         eBattleOutcome outcome = BATTLE_OUTCOME_SIDE_A_WIN) {
    BattleSettlementData s;
    s.set_battle_id(battleId);
    s.set_player_id(kTestPlayerId);
    s.set_outcome(outcome);
    s.set_player_team_index(0);
    s.set_total_rounds(rounds);
    s.set_finished_at_ms(kNowMs);
    auto* facts = s.mutable_trial_facts();
    facts->set_journal_entry_id(entryId);
    facts->set_trial_rule_version(ruleVersion);
    facts->set_initial_human_count(humanCount);
    facts->set_season_id(0);
    facts->set_used_pet_table_id(1001);
    facts->add_used_artifact_effect_ids(8);
    facts->add_used_artifact_effect_ids(1);
    facts->set_used_season_seal_id(101);
    return s;
}

const TrialJournalEntryView* FindEntryView(const TrialJournalSnapshot& snapshot, uint32_t entryId) {
    for (const auto& chapter : snapshot.chapters()) {
        for (const auto& entry : chapter.entries()) {
            if (entry.entry_id() == entryId) {
                return &entry;
            }
        }
    }
    return nullptr;
}

const TrialJournalMilestoneView* FindMilestoneView(const TrialJournalSnapshot& snapshot, uint32_t milestoneId) {
    for (const auto& m : snapshot.milestones()) {
        if (m.milestone_id() == milestoneId) {
            return &m;
        }
    }
    return nullptr;
}

class PlayerTrialJournalSystemTest : public ::testing::Test {
protected:
    void SetUp() override {
        prevEntrySnap = TrialJournalEntryTableManager::Instance().CurrentSnapshot();
        prevSealSnap = TrialJournalSealTableManager::Instance().CurrentSnapshot();
        prevMilestoneSnap = TrialJournalMilestoneTableManager::Instance().CurrentSnapshot();
        prevCharAppearanceSnap = CharacterAppearanceTableManager::Instance().CurrentSnapshot();

        ItemTableManager::Instance().Load();
        EquipArtifactTableManager::Instance().Load();
        AppearanceSeriesTableManager::Instance().Load();
        CharacterAppearanceTableManager::Instance().Load();
        AppearanceRuleTableManager::Instance().Load();
        SeasonRuleTableManager::Instance().Load();
        SeasonSealTableManager::Instance().Load();
        TrialJournalEntryTableManager::Instance().Load();
        TrialJournalSealTableManager::Instance().Load();
        TrialJournalMilestoneTableManager::Instance().Load();

        ASSERT_TRUE(ArmTrialItemSegment(95000000, 95100000));

        persistCalls = 0;
        PlayerTrialJournalSystem::SetPersistForTest(&CountPersist);

        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kTestPlayerId);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 1;
        auto& profile = tlsEcs.actorRegistry.emplace<PlayerProfileComp>(player);
        profile.set_appearance_id("00_reference_topright_boy");
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        bags.bags[kInventory].SetCapacityForRestore(40);
        bags.bags[kEquipment].SetCapacityForRestore(20);
        tlsEcs.actorRegistry.emplace<PlayerItemHoldComp>(player);
        tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerSeasonComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerTrialJournalComp>(player);
        tlsEcs.playerList[kTestPlayerId] = player;
    }

    void TearDown() override {
        PlayerTrialJournalSystem::SetPersistForTest(nullptr);
        PlayerAppearanceSystem::ResetTestState();
        tlsEcs.playerList.erase(kTestPlayerId);
        if (tlsEcs.actorRegistry.valid(player)) {
            tlsEcs.actorRegistry.destroy(player);
        }
        TrialJournalEntryTableManager::Instance().InstallSnapshot(prevEntrySnap);
        TrialJournalSealTableManager::Instance().InstallSnapshot(prevSealSnap);
        TrialJournalMilestoneTableManager::Instance().InstallSnapshot(prevMilestoneSnap);
        CharacterAppearanceTableManager::Instance().InstallSnapshot(prevCharAppearanceSnap);
    }

    static bool CountPersist(entt::entity) {
        ++persistCalls;
        return true;
    }

    void ApplyBattleCredit(const BattleSettlementData& settlement,
                           PlayerTrialJournalSystem::SettlementDelta& delta) {
        auto& comp = tlsEcs.actorRegistry.get<PlayerTrialJournalComp>(player);
        PlayerTrialJournalComp candidate;
        ASSERT_EQ(PlayerTrialJournalSystem::PrepareBattleCredit(comp, settlement, kNowMs, candidate, delta), kSuccess);
        if (delta.changed) {
            comp.Swap(&candidate);
        }
    }

    static inline uint32_t persistCalls{0};
    entt::entity player{entt::null};
    std::shared_ptr<const TrialJournalEntryTableSnapshot> prevEntrySnap;
    std::shared_ptr<const TrialJournalSealTableSnapshot> prevSealSnap;
    std::shared_ptr<const TrialJournalMilestoneTableSnapshot> prevMilestoneSnap;
    std::shared_ptr<const CharacterAppearanceTableSnapshot> prevCharAppearanceSnap;
};

} // namespace

TEST_F(PlayerTrialJournalSystemTest, RealExportedTablesValidateAndPopulateInitialSnapshot) {
    ASSERT_TRUE(PlayerTrialJournalSystem::ValidateTables());

    GetTrialJournalResponse resp;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, resp), kSuccess);
    const auto& snap = resp.snapshot();
    EXPECT_TRUE(snap.available());
    EXPECT_TRUE(snap.can_operate());
    EXPECT_EQ(snap.total_permanent_lit_seals(), 0u);
    EXPECT_EQ(snap.total_permanent_seals(), 12u);
    EXPECT_EQ(snap.voucher_balance(), 0u);
    EXPECT_EQ(snap.bound_sand_balance(), 0u);
    ASSERT_EQ(snap.chapters_size(), 1);
    EXPECT_EQ(snap.chapters(0).chapter_id(), trialjournal::kChapterBreakthrough);
    EXPECT_EQ(snap.chapters(0).entries_size(), 4);
    EXPECT_EQ(snap.milestones_size(), 4);
    EXPECT_EQ(snap.redeemable_artifacts_size(), 8);
}

TEST_F(PlayerTrialJournalSystemTest, RejectsMilestoneGrantingCardUnlockableAppearance) {
    auto snap = std::make_shared<TrialJournalMilestoneTableSnapshot>(
        *TrialJournalMilestoneTableManager::Instance().CurrentSnapshot());
    ASSERT_GT(snap->data.data_size(), 0);
    // 00_reference_topright_boy 在正式表中 card_unlockable == 1，严禁作为试炼志独占外显发放。
    snap->data.mutable_data(0)->set_grant_appearance_id("00_reference_topright_boy");
    snap->idMap.clear();
    for (int i = 0; i < snap->data.data_size(); ++i) {
        const auto& row = snap->data.data(i);
        snap->idMap[row.id()] = &row;
    }
    TrialJournalMilestoneTableManager::Instance().InstallSnapshot(snap);
    EXPECT_FALSE(PlayerTrialJournalSystem::ValidateTables());
}

TEST_F(PlayerTrialJournalSystemTest, SoloClearLightsSealsUpdatesBestRoundsAndSnapshotsLoadout) {
    // 101 普通·回春：1011(破阵:通关), 1012(速破:<=7回合), 1013(断愈:首领未获护卫有效治疗)
    auto firstWin = MakeTrialSettlement(/*battleId=*/5001, /*entryId=*/101, /*ruleVersion=*/1, /*rounds=*/8);
    PlayerTrialJournalSystem::SettlementDelta delta1;
    ApplyBattleCredit(firstWin, delta1);
    EXPECT_TRUE(delta1.changed);
    EXPECT_EQ(delta1.entryId, 101u);
    EXPECT_TRUE(delta1.newSoloBest);
    EXPECT_FALSE(delta1.newCoopBest);
    EXPECT_EQ(delta1.newlyLitSealIds, (std::vector<uint32_t>{1011u, 1013u}));

    // 同等或更慢回合不覆写
    auto slowerWin = MakeTrialSettlement(5002, 101, 1, 8);
    slowerWin.mutable_trial_facts()->set_used_pet_table_id(2002);
    PlayerTrialJournalSystem::SettlementDelta deltaTie;
    ApplyBattleCredit(slowerWin, deltaTie);
    EXPECT_FALSE(deltaTie.changed);
    EXPECT_FALSE(deltaTie.newSoloBest);
    EXPECT_TRUE(deltaTie.newlyLitSealIds.empty());

    // 6 回合刷新单人最佳并补点速破印 1012；即便本局有回春治疗，旧 1013 永久保留
    auto fastWin = MakeTrialSettlement(5003, 101, 1, 6);
    fastWin.mutable_trial_facts()->set_boss_guard_heal_total(450);
    PlayerTrialJournalSystem::SettlementDelta deltaFast;
    ApplyBattleCredit(fastWin, deltaFast);
    EXPECT_TRUE(deltaFast.changed);
    EXPECT_TRUE(deltaFast.newSoloBest);
    EXPECT_EQ(deltaFast.newlyLitSealIds, (std::vector<uint32_t>{1012u}));

    GetTrialJournalResponse resp;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, resp), kSuccess);
    EXPECT_EQ(resp.snapshot().total_permanent_lit_seals(), 3u);
    const auto* entry = FindEntryView(resp.snapshot(), 101);
    ASSERT_NE(entry, nullptr);
    EXPECT_EQ(entry->lit_seal_count(), 3u);
    EXPECT_TRUE(entry->has_solo_best());
    EXPECT_EQ(entry->solo_best().best_rounds(), 6u);
    EXPECT_EQ(entry->solo_best().loadout().pet_table_id(), 1001u);
    ASSERT_EQ(entry->solo_best().loadout().artifact_effect_ids_size(), 2);
    EXPECT_EQ(entry->solo_best().loadout().artifact_effect_ids(0), 1u); // 已升序归一
    EXPECT_EQ(entry->solo_best().loadout().artifact_effect_ids(1), 8u);
    EXPECT_EQ(entry->solo_best().loadout().season_seal_id(), 101u);
    EXPECT_FALSE(entry->has_coop_best());
}

TEST_F(PlayerTrialJournalSystemTest, FlawlessNoHealBreakStanceAndCoopIsolation) {
    // 103 困难·回春：1031(破阵), 1032(无损:本人及出战宝宝未曾倒地), 1033(断愈:护卫治疗总量==0)
    auto diedAndHealed = MakeTrialSettlement(5101, 103, 1, 8, /*humanCount=*/2);
    diedAndHealed.mutable_trial_facts()->set_self_or_pet_ever_died(true);
    diedAndHealed.mutable_trial_facts()->set_boss_guard_heal_total(450);
    diedAndHealed.mutable_trial_facts()->add_teammate_names("友道甲");
    PlayerTrialJournalSystem::SettlementDelta d1;
    ApplyBattleCredit(diedAndHealed, d1);
    EXPECT_EQ(d1.newlyLitSealIds, (std::vector<uint32_t>{1031u}));
    EXPECT_FALSE(d1.newSoloBest);
    EXPECT_TRUE(d1.newCoopBest);

    // 104 困难·护势：1041(破阵), 1042(无损), 1043(破势:驱散护势或物法双系均直伤)
    auto onlyPhysDied = MakeTrialSettlement(5102, 104, 1, 9, 1);
    onlyPhysDied.mutable_trial_facts()->set_self_or_pet_ever_died(true);
    onlyPhysDied.mutable_trial_facts()->set_team_dealt_physical_direct_damage(true);
    PlayerTrialJournalSystem::SettlementDelta d2;
    ApplyBattleCredit(onlyPhysDied, d2);
    EXPECT_EQ(d2.newlyLitSealIds, (std::vector<uint32_t>{1041u}));

    auto dualHitFlawless = MakeTrialSettlement(5103, 104, 1, 8, 1);
    dualHitFlawless.mutable_trial_facts()->set_team_dealt_physical_direct_damage(true);
    dualHitFlawless.mutable_trial_facts()->set_team_dealt_magic_direct_damage(true);
    PlayerTrialJournalSystem::SettlementDelta d3;
    ApplyBattleCredit(dualHitFlawless, d3);
    EXPECT_EQ(d3.newlyLitSealIds, (std::vector<uint32_t>{1042u, 1043u}));

    // 逃跑或作废局一律不记账
    auto fled = MakeTrialSettlement(5104, 103, 1, 4, 1);
    fled.set_fled(true);
    EXPECT_FALSE(PlayerTrialJournalSystem::HasJournalUpdate(fled));

    GetTrialJournalResponse resp;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, resp), kSuccess);
    const auto* e103 = FindEntryView(resp.snapshot(), 103);
    ASSERT_NE(e103, nullptr);
    EXPECT_FALSE(e103->has_solo_best());
    EXPECT_TRUE(e103->has_coop_best());
    EXPECT_EQ(e103->coop_best().party_size(), 2u);
    ASSERT_EQ(e103->coop_best().teammate_names_size(), 1);
    EXPECT_EQ(e103->coop_best().teammate_names(0), "友道甲");
}

TEST_F(PlayerTrialJournalSystemTest, RuleVersionUpgradePreservesSealsAndIsolatesLateV1Settlement) {
    // 先按 v1 打出 8 回合（点亮 1011 通 + 1013 断愈，共 2 枚；未达 <=7 速破）
    PlayerTrialJournalSystem::SettlementDelta d1;
    ApplyBattleCredit(MakeTrialSettlement(5201, 101, /*ruleVersion=*/1, 8), d1);
    ASSERT_TRUE(d1.newSoloBest);

    // 升级配置中 101 的 rule_version -> 2
    auto snap = std::make_shared<TrialJournalEntryTableSnapshot>(
        *TrialJournalEntryTableManager::Instance().CurrentSnapshot());
    snap->idMap.clear();
    for (int i = 0; i < snap->data.data_size(); ++i) {
        auto* row = snap->data.mutable_data(i);
        if (row->id() == 101) {
            row->set_rule_version(2);
            row->set_rule_version_name("第二版");
        }
        snap->idMap[row->id()] = row;
    }
    TrialJournalEntryTableManager::Instance().InstallSnapshot(snap);

    // 快照应显示「本版暂无纪录，战法印已保留」
    GetTrialJournalResponse respBefore;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, respBefore), kSuccess);
    const auto* e101Before = FindEntryView(respBefore.snapshot(), 101);
    ASSERT_NE(e101Before, nullptr);
    EXPECT_EQ(e101Before->lit_seal_count(), 2u);
    EXPECT_FALSE(e101Before->has_solo_best());
    EXPECT_TRUE(e101Before->has_legacy_record_only());

    // 打出 v2 的 9 回合，再到达迟到的 v1 的 4 回合：迟到 v1 不得覆盖 v2，但补点速破印 1012
    PlayerTrialJournalSystem::SettlementDelta dV2;
    ApplyBattleCredit(MakeTrialSettlement(5202, 101, 2, 9), dV2);
    ASSERT_TRUE(dV2.newSoloBest);

    PlayerTrialJournalSystem::SettlementDelta dLateV1;
    ApplyBattleCredit(MakeTrialSettlement(5203, 101, 1, 4), dLateV1);
    ASSERT_TRUE(dLateV1.newSoloBest); // 更新的是 v1 纪录，同时顺带点亮速破印 1012

    GetTrialJournalResponse respAfter;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, respAfter), kSuccess);
    const auto* e101After = FindEntryView(respAfter.snapshot(), 101);
    ASSERT_NE(e101After, nullptr);
    EXPECT_EQ(e101After->lit_seal_count(), 3u);
    EXPECT_TRUE(e101After->has_solo_best());
    EXPECT_EQ(e101After->solo_best().best_rounds(), 9u) << "迟到旧版结算绝不得覆写新版纪录";
    EXPECT_FALSE(e101After->has_legacy_record_only());
}

TEST_F(PlayerTrialJournalSystemTest, UnknownHighBitsPreservedAndEffectiveRowsCounted) {
    auto& comp = tlsEcs.actorRegistry.get<PlayerTrialJournalComp>(player);
    // 写入一个高于当前全部有效位图索引的未知高位（bit 199）
    SetPackedBit(*comp.mutable_lit_seal_bits(), 199);
    SetPackedBit(*comp.mutable_claimed_milestone_bits(), 199);

    GetTrialJournalResponse resp;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, resp), kSuccess);
    EXPECT_EQ(resp.snapshot().total_permanent_lit_seals(), 0u) << "未知高位绝不计入有效分子";

    // 点亮 1011 后，未知高位 199 依然完整保留
    PlayerTrialJournalSystem::SettlementDelta d;
    ApplyBattleCredit(MakeTrialSettlement(5301, 101, 1, 8), d);
    EXPECT_TRUE(TestPackedBit(comp.lit_seal_bits(), 199));
    EXPECT_TRUE(TestPackedBit(comp.claimed_milestone_bits(), 199));
}

TEST_F(PlayerTrialJournalSystemTest, ClaimMilestoneIdempotencyBagFullAndVoucherRedemption) {
    // 先点亮 6 枚战法印（101 全 3 枚 + 102 全 3 枚）以达标 M101(3 印) 与 M102(6 印)
    PlayerTrialJournalSystem::SettlementDelta d;
    ApplyBattleCredit(MakeTrialSettlement(5401, 101, 1, 6), d);
    auto win102 = MakeTrialSettlement(5402, 102, 1, 6);
    win102.mutable_trial_facts()->set_guard_stance_dispelled(true); // 点亮 1023 破势，凑满 6 印
    ApplyBattleCredit(win102, d);

    // 1) 未达标 M103(9 印) 被拒
    ClaimTrialJournalMilestoneRequest notReachedReq;
    notReachedReq.set_client_request_id(8001);
    notReachedReq.set_milestone_id(103);
    ClaimTrialJournalMilestoneResponse notReachedResp;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, notReachedReq, notReachedResp);
    EXPECT_EQ(notReachedResp.error_message().id(), kTrialJournalMilestoneNotReached);

    // 2) 背包满时领 M101 被拒，且不置位、不扣额度、不入账
    auto& inv = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    inv.SetCapacityForRestore(0);
    ClaimTrialJournalMilestoneRequest fullReq;
    fullReq.set_client_request_id(8002);
    fullReq.set_milestone_id(101);
    ClaimTrialJournalMilestoneResponse fullResp;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, fullReq, fullResp);
    EXPECT_EQ(fullResp.error_message().id(), kTrialJournalBagFull);
    const auto* m101AfterFail = FindMilestoneView(fullResp.snapshot(), 101);
    ASSERT_NE(m101AfterFail, nullptr);
    EXPECT_FALSE(m101AfterFail->claimed());

    // 恢复容量，正式领 M101（重铸砂 2）与 M102（自用法宝拓印 1）
    inv.SetCapacityForRestore(40);
    const uint32_t materialEarnedBefore =
        tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).material_reward().earned_today();

    ClaimTrialJournalMilestoneResponse ok101;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, fullReq, ok101);
    EXPECT_EQ(ok101.error_message().id(), 0u);
    EXPECT_FALSE(ok101.replayed());
    EXPECT_EQ(ok101.granted_sand_count(), 2u);
    EXPECT_EQ(ok101.snapshot().bound_sand_balance(), 2u);
    EXPECT_EQ(persistCalls, 1u);

    // 同 client_request_id 重放返回 replayed=true 且不重复发奖
    ClaimTrialJournalMilestoneResponse replay101;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, fullReq, replay101);
    EXPECT_EQ(replay101.error_message().id(), 0u);
    EXPECT_TRUE(replay101.replayed());
    EXPECT_EQ(replay101.granted_sand_count(), 2u);
    EXPECT_EQ(replay101.snapshot().bound_sand_balance(), 2u);

    // 同键换参数拒绝
    ClaimTrialJournalMilestoneRequest mismatchReq;
    mismatchReq.set_client_request_id(8002);
    mismatchReq.set_milestone_id(102);
    ClaimTrialJournalMilestoneResponse mismatchResp;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, mismatchReq, mismatchResp);
    EXPECT_EQ(mismatchResp.error_message().id(), kInvalidParameter);

    // 换键领 M102 获得 1 张自用法宝拓印，且不碰 660 核心材料日额度
    ClaimTrialJournalMilestoneRequest req102;
    req102.set_client_request_id(8003);
    req102.set_milestone_id(102);
    ClaimTrialJournalMilestoneResponse ok102;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, req102, ok102);
    EXPECT_EQ(ok102.error_message().id(), 0u);
    EXPECT_EQ(ok102.granted_voucher_count(), 1u);
    EXPECT_EQ(ok102.snapshot().voucher_balance(), 1u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).material_reward().earned_today(),
              materialEarnedBefore);

    // 3) 兑换非法模板 ID 被拒
    RedeemBoundArtifactVoucherRequest badArtifactReq;
    badArtifactReq.set_client_request_id(9001);
    badArtifactReq.set_artifact_item_config_id(3403); // 重铸砂不是法宝
    RedeemBoundArtifactVoucherResponse badArtifactResp;
    PlayerTrialJournalSystem::RedeemArtifactVoucher(player, kNowMs, badArtifactReq, badArtifactResp);
    EXPECT_EQ(badArtifactResp.error_message().id(), kTrialJournalArtifactInvalid);

    // 4) 合法兑换法宝 3301（仅自用实例 bound==true）
    RedeemBoundArtifactVoucherRequest redeem1;
    redeem1.set_client_request_id(9002);
    redeem1.set_artifact_item_config_id(3301);
    RedeemBoundArtifactVoucherResponse redeemResp1;
    PlayerTrialJournalSystem::RedeemArtifactVoucher(player, kNowMs, redeem1, redeemResp1);
    EXPECT_EQ(redeemResp1.error_message().id(), 0u);
    EXPECT_FALSE(redeemResp1.replayed());
    EXPECT_NE(redeemResp1.created_item_id(), 0u);
    EXPECT_EQ(redeemResp1.artifact_item_config_id(), 3301u);
    EXPECT_EQ(redeemResp1.snapshot().voucher_balance(), 0u);
    const auto* createdItem = inv.GetItemCompByGuid(redeemResp1.created_item_id());
    ASSERT_NE(createdItem, nullptr);
    EXPECT_EQ(createdItem->config_id(), 3301u);
    EXPECT_TRUE(createdItem->bound());

    // 5) 兑换幂等重放
    RedeemBoundArtifactVoucherResponse redeemReplay;
    PlayerTrialJournalSystem::RedeemArtifactVoucher(player, kNowMs, redeem1, redeemReplay);
    EXPECT_EQ(redeemReplay.error_message().id(), 0u);
    EXPECT_TRUE(redeemReplay.replayed());
    EXPECT_EQ(redeemReplay.created_item_id(), redeemResp1.created_item_id());

    // 6) 拓印余额为 0 时再兑被拒
    RedeemBoundArtifactVoucherRequest noVoucherReq;
    noVoucherReq.set_client_request_id(9003);
    noVoucherReq.set_artifact_item_config_id(3301);
    RedeemBoundArtifactVoucherResponse noVoucherResp;
    PlayerTrialJournalSystem::RedeemArtifactVoucher(player, kNowMs, noVoucherReq, noVoucherResp);
    EXPECT_EQ(noVoucherResp.error_message().id(), kTrialJournalVoucherNotEnough);

    // 7) 再发 1 张拓印，允许兑换第二件同名 3301（双持同名法宝）
    inv.InsertItemForRestore((uint64_t{1} << 60) + 9910, trialjournal::kBoundArtifactVoucherItemId, 1, 0);
    RedeemBoundArtifactVoucherRequest redeem2;
    redeem2.set_client_request_id(9004);
    redeem2.set_artifact_item_config_id(3301);
    RedeemBoundArtifactVoucherResponse redeemResp2;
    PlayerTrialJournalSystem::RedeemArtifactVoucher(player, kNowMs, redeem2, redeemResp2);
    EXPECT_EQ(redeemResp2.error_message().id(), 0u);
    EXPECT_NE(redeemResp2.created_item_id(), redeemResp1.created_item_id());
    EXPECT_EQ(inv.GetTotalItemCount(3301), 2u);
}

TEST_F(PlayerTrialJournalSystemTest, ExclusiveAppearanceMilestoneGrantsAppearanceDirectly) {
    // 构造一个 card_unlockable == 0 的独占形象与指向它的试炼志里程碑
    auto appSnap = std::make_shared<CharacterAppearanceTableSnapshot>(
        *CharacterAppearanceTableManager::Instance().CurrentSnapshot());
    auto* exclRow = appSnap->data.add_data();
    exclRow->set_id(1);
    exclRow->set_appearance_id("99_trial_exclusive_test");
    exclRow->set_name("破阵玄裳");
    exclRow->set_enabled(1);
    exclRow->set_card_unlockable(0);
    // 将第一行 id=1 替换为独占测试行以便 bit_index 命中有效位
    appSnap->data.mutable_data(0)->CopyFrom(*exclRow);
    appSnap->data.mutable_data()->RemoveLast();
    appSnap->idMap.clear();
    appSnap->seriesIdIndex.clear();
    for (int i = 0; i < appSnap->data.data_size(); ++i) {
        const auto& r = appSnap->data.data(i);
        appSnap->idMap[r.id()] = &r;
        appSnap->seriesIdIndex[r.series_id()].push_back(&r);
    }
    CharacterAppearanceTableManager::Instance().InstallSnapshot(appSnap);

    auto msSnap = std::make_shared<TrialJournalMilestoneTableSnapshot>(
        *TrialJournalMilestoneTableManager::Instance().CurrentSnapshot());
    msSnap->data.mutable_data(0)->set_grant_appearance_id("99_trial_exclusive_test");
    msSnap->idMap.clear();
    for (int i = 0; i < msSnap->data.data_size(); ++i) {
        const auto& r = msSnap->data.data(i);
        msSnap->idMap[r.id()] = &r;
    }
    TrialJournalMilestoneTableManager::Instance().InstallSnapshot(msSnap);
    ASSERT_TRUE(PlayerTrialJournalSystem::ValidateTables());

    PlayerTrialJournalSystem::SettlementDelta d;
    ApplyBattleCredit(MakeTrialSettlement(5501, 101, 1, 6), d);

    ClaimTrialJournalMilestoneRequest req;
    req.set_client_request_id(9101);
    req.set_milestone_id(101);
    ClaimTrialJournalMilestoneResponse resp;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, req, resp);
    EXPECT_EQ(resp.error_message().id(), 0u);
    EXPECT_EQ(resp.granted_appearance_id(), "99_trial_exclusive_test");
    PlayerAppearanceInfo appInfo;
    PlayerAppearanceSystem::FillInfo(player, kNowMs / 1000u, appInfo);
    bool ownedExclusive = false;
    for (const auto& entry : appInfo.appearances()) {
        if (entry.appearance_id() == "99_trial_exclusive_test") {
            ownedExclusive = entry.owned();
        }
    }
    EXPECT_TRUE(ownedExclusive);
}
