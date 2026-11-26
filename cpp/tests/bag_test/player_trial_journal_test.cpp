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
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/asset_op_ledger_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/component/player_season_comp.pb.h"
#include "proto/common/component/player_trial_journal_comp.pb.h"
#include "proto/scene/player_appearance.pb.h"
#include "proto/scene/player_trial_journal.pb.h"
#include "security/token_security.h"
#include "services/battle/system/trial_journal_rules.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/asset_op_auth.h"
#include "services/scene/player/system/asset_op_system.h"
#include "services/scene/player/system/player_appearance.h"
#include "services/scene/player/system/player_season.h"
#include "services/scene/player/system/player_trial_journal.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/code/appearancerule_table.h"
#include "table/code/appearanceseries_table.h"
#include "table/code/bit_index/trialjournalmilestone_table_id_bit_index.h"
#include "table/code/bit_index/trialjournalseal_table_id_bit_index.h"
#include "table/code/characterappearance_table.h"
#include "table/code/equipartifact_table.h"
#include "table/code/item_table.h"
#include "table/code/petcosmetic_table.h"
#include "table/code/seasonrule_table.h"
#include "table/code/seasonseal_table.h"
#include "table/code/skillvfxcosmetic_table.h"
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
        prevPetCosmeticSnap = PetCosmeticTableManager::Instance().CurrentSnapshot();
        prevSkillVfxSnap = SkillVfxCosmeticTableManager::Instance().CurrentSnapshot();

        ItemTableManager::Instance().Load();
        EquipArtifactTableManager::Instance().Load();
        AppearanceSeriesTableManager::Instance().Load();
        CharacterAppearanceTableManager::Instance().Load();
        AppearanceRuleTableManager::Instance().Load();
        PetCosmeticTableManager::Instance().Load();
        SkillVfxCosmeticTableManager::Instance().Load();
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
        PetCosmeticTableManager::Instance().InstallSnapshot(prevPetCosmeticSnap);
        SkillVfxCosmeticTableManager::Instance().InstallSnapshot(prevSkillVfxSnap);
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
    std::shared_ptr<const PetCosmeticTableSnapshot> prevPetCosmeticSnap;
    std::shared_ptr<const SkillVfxCosmeticTableSnapshot> prevSkillVfxSnap;
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
    EXPECT_EQ(snap.total_permanent_seals(), 24u);
    EXPECT_EQ(snap.voucher_balance(), 0u);
    EXPECT_EQ(snap.bound_sand_balance(), 0u);
    ASSERT_EQ(snap.chapters_size(), 3);
    EXPECT_EQ(snap.chapters(0).chapter_id(), trialjournal::kChapterBreakthrough);
    EXPECT_EQ(snap.chapters(0).entries_size(), 4);
    EXPECT_EQ(snap.chapters(1).chapter_id(), trialjournal::kChapterThreeRealms);
    EXPECT_EQ(snap.chapters(1).entries_size(), 4);
    EXPECT_EQ(snap.chapters(2).chapter_id(), trialjournal::kChapterSeasonal);
    EXPECT_EQ(snap.chapters(2).season_id(), 1u);
    EXPECT_EQ(snap.chapters(2).entries_size(), 3);
    EXPECT_EQ(snap.milestones_size(), 10);
    EXPECT_EQ(snap.redeemable_artifacts_size(), 8);

    const auto* m901 = FindMilestoneView(snap, 901);
    ASSERT_NE(m901, nullptr);
    EXPECT_EQ(m901->scope_chapter_id(), trialjournal::kScopeAllPermanent);
    EXPECT_EQ(m901->required_seals(), 18u);
    EXPECT_EQ(m901->grant_appearance_id(), "20_star_formation_master_girl");
    EXPECT_EQ(m901->grant_appearance_name(), "星阵宗师");

    const auto* m105 = FindMilestoneView(snap, 105);
    ASSERT_NE(m105, nullptr);
    EXPECT_EQ(m105->grant_skill_vfx_id(), 3u);
    EXPECT_EQ(m105->grant_skill_vfx_name(), "寒星流华");

    const auto* m302 = FindMilestoneView(snap, 302);
    ASSERT_NE(m302, nullptr);
    EXPECT_EQ(m302->grant_pet_cosmetic_id(), 3u);
    EXPECT_EQ(m302->grant_pet_cosmetic_name(), "月弦师");
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

TEST_F(PlayerTrialJournalSystemTest, RejectsMilestoneGrantingPurchasablePetOrSkillCosmetic) {
    {
        auto snap = std::make_shared<TrialJournalMilestoneTableSnapshot>(
            *TrialJournalMilestoneTableManager::Instance().CurrentSnapshot());
        // PetCosmetic #1（苍嶂麟）支持百宝阁幻化符解锁，严禁配进免费试炼志里程碑。
        snap->data.mutable_data(0)->set_grant_pet_cosmetic_id(1);
        snap->idMap.clear();
        for (int i = 0; i < snap->data.data_size(); ++i) {
            const auto& row = snap->data.data(i);
            snap->idMap[row.id()] = &row;
        }
        TrialJournalMilestoneTableManager::Instance().InstallSnapshot(snap);
        EXPECT_FALSE(PlayerTrialJournalSystem::ValidateTables());
    }
    TrialJournalMilestoneTableManager::Instance().InstallSnapshot(prevMilestoneSnap);
    {
        auto snap = std::make_shared<TrialJournalMilestoneTableSnapshot>(
            *TrialJournalMilestoneTableManager::Instance().CurrentSnapshot());
        // SkillVfxCosmetic #1（鎏金符纹）支持百宝阁特效卷解锁，严禁配进免费试炼志里程碑。
        snap->data.mutable_data(0)->set_grant_skill_vfx_id(1);
        snap->idMap.clear();
        for (int i = 0; i < snap->data.data_size(); ++i) {
            const auto& row = snap->data.data(i);
            snap->idMap[row.id()] = &row;
        }
        TrialJournalMilestoneTableManager::Instance().InstallSnapshot(snap);
        EXPECT_FALSE(PlayerTrialJournalSystem::ValidateTables());
    }
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
    EXPECT_EQ(inv.GetTotalItemCount(trialjournal::kBoundReforgeSandItemId), 2u);
    EXPECT_EQ(inv.GetTotalTradableItemCount(trialjournal::kBoundReforgeSandItemId), 0u) << "里程碑重铸砂必须绑定";
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
    EXPECT_EQ(inv.GetTotalItemCount(trialjournal::kBoundArtifactVoucherItemId), 1u);
    EXPECT_EQ(inv.GetTotalTradableItemCount(trialjournal::kBoundArtifactVoucherItemId), 0u) << "里程碑法宝拓印必须绑定";
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

TEST_F(PlayerTrialJournalSystemTest, BoundTrialRewardsStackIntoBoundSlotAndAreRejectedByTradeDebit) {
    // 1) 点亮 6 印（101+102），领取 M101（2 绑定重铸砂）与 M102（1 绑定法宝拓印），并兑换 1 件绑定法宝 3301
    PlayerTrialJournalSystem::SettlementDelta d;
    ApplyBattleCredit(MakeTrialSettlement(5601, 101, 1, 6), d);
    auto win102 = MakeTrialSettlement(5602, 102, 1, 6);
    win102.mutable_trial_facts()->set_guard_stance_dispelled(true);
    ApplyBattleCredit(win102, d);

    auto& inv = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    // 先塞入 1 个绑定重铸砂，并把容量卡死到恰好 1 格（0 空位）：领 M101 时必须能并入既有绑定堆！
    InitItemParam seedBoundSand;
    seedBoundSand.itemPBComp.set_item_id(kInvalidGuid);
    seedBoundSand.itemPBComp.set_config_id(trialjournal::kBoundReforgeSandItemId);
    seedBoundSand.itemPBComp.set_size(1);
    seedBoundSand.itemPBComp.set_bound(true);
    ASSERT_EQ(inv.AddItems(std::vector<InitItemParam>{seedBoundSand}), kSuccess);
    inv.SetCapacityForRestore(inv.OccupiedGridCount());

    ClaimTrialJournalMilestoneRequest req101;
    req101.set_client_request_id(9201);
    req101.set_milestone_id(101);
    ClaimTrialJournalMilestoneResponse resp101;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, req101, resp101);
    EXPECT_EQ(resp101.error_message().id(), 0u) << "满格但存在同配置绑定堆时，绑定发奖应可并堆";
    EXPECT_EQ(inv.GetTotalItemCount(trialjournal::kBoundReforgeSandItemId), 3u);
    EXPECT_EQ(inv.GetTotalTradableItemCount(trialjournal::kBoundReforgeSandItemId), 0u);

    // 恢复容量，领 M102 并兑 3301 绑定法宝
    inv.SetCapacityForRestore(40);
    ClaimTrialJournalMilestoneRequest req102;
    req102.set_client_request_id(9202);
    req102.set_milestone_id(102);
    ClaimTrialJournalMilestoneResponse resp102;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, req102, resp102);
    ASSERT_EQ(resp102.error_message().id(), 0u);

    RedeemBoundArtifactVoucherRequest redeemReq;
    redeemReq.set_client_request_id(9203);
    redeemReq.set_artifact_item_config_id(3301);
    RedeemBoundArtifactVoucherResponse redeemResp;
    PlayerTrialJournalSystem::RedeemArtifactVoucher(player, kNowMs, redeemReq, redeemResp);
    ASSERT_EQ(redeemResp.error_message().id(), 0u);
    const Guid boundArtifactGuid = redeemResp.created_item_id();

    // 2) 接入真实 PlayerAssetOpSystem::Debit（聚宝斋寄售 TX_AUCTION_SELL）校验绑定拦截
    constexpr std::string_view kTestSecret = "trial-journal-bound-debit-secret-0123456789";
    tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerComp>(player);
    tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
    tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
    PlayerAssetOpSystem::SetNowFnForTest([]() -> int64_t { return static_cast<int64_t>(kNowMs); });
    PlayerAssetOpSystem::SetPersistFnForTest([](entt::entity) { return true; });
    PlayerAssetOpSystem::SetSecretLookupForTest([](std::string_view) { return std::string(kTestSecret); });
    struct AssetOpHookReset {
        ~AssetOpHookReset() {
            PlayerAssetOpSystem::SetNowFnForTest(nullptr);
            PlayerAssetOpSystem::SetPersistFnForTest(nullptr);
            PlayerAssetOpSystem::SetSecretLookupForTest(nullptr);
        }
    } hookReset;

    const auto tradeDebit = [&](uint64_t seq, const auto& fillBundle) {
        ::AssetOpRequest req;
        req.set_player_id(kTestPlayerId);
        req.set_stream(ASSET_OP_STREAM_TRADE_DEBIT);
        req.set_stream_epoch(1);
        req.set_seq(seq);
        req.set_correlation_id(92000 + seq);
        req.set_tx_type(static_cast<uint32_t>(TX_AUCTION_SELL));
        fillBundle(*req.mutable_bundle());
        auto* auth = req.mutable_auth();
        auth->set_caller("trade");
        auth->set_timestamp_ms(kNowMs);
        auth->set_signature_hex(token_security::HmacSha256Hex(kTestSecret, AssetOpCanonical("debit", req)));
        ::AssetOpResponse resp;
        PlayerAssetOpSystem::Debit(req, resp);
        return resp;
    };

    // 2a) 按 GUID 托管绑定法宝必须终局拒绝且法宝留在背包
    const auto boundArtifactResp = tradeDebit(1, [&](AssetBundle& b) { b.add_item_uuids(boundArtifactGuid); });
    EXPECT_EQ(boundArtifactResp.outcome(), ASSET_OP_OUTCOME_REJECTED);
    EXPECT_EQ(boundArtifactResp.reason().id(), kAssetInvalidBundle);
    EXPECT_TRUE(inv.ContainsGuid(boundArtifactGuid));

    // 2b) 按数量托管仅有绑定堆的重铸砂必须终局拒绝且数量不动
    const auto boundSandResp = tradeDebit(2, [&](AssetBundle& b) {
        auto* it = b.add_items();
        it->set_config_id(trialjournal::kBoundReforgeSandItemId);
        it->set_count(1);
    });
    EXPECT_EQ(boundSandResp.outcome(), ASSET_OP_OUTCOME_REJECTED);
    EXPECT_EQ(boundSandResp.reason().id(), kAssetInvalidBundle);
    EXPECT_EQ(inv.GetTotalItemCount(trialjournal::kBoundReforgeSandItemId), 3u);

    // 2c) 同时拥有 3 绑定 + 2 可交易重铸砂时，托管 2 个成功且仅扣除可交易堆，3 个绑定堆毫发无损
    InitItemParam tradableSand;
    tradableSand.itemPBComp.set_item_id(kInvalidGuid);
    tradableSand.itemPBComp.set_config_id(trialjournal::kBoundReforgeSandItemId);
    tradableSand.itemPBComp.set_size(2);
    tradableSand.itemPBComp.set_bound(false);
    ASSERT_EQ(inv.AddItems(std::vector<InitItemParam>{tradableSand}), kSuccess);

    const auto tradableSandResp = tradeDebit(3, [&](AssetBundle& b) {
        auto* it = b.add_items();
        it->set_config_id(trialjournal::kBoundReforgeSandItemId);
        it->set_count(2);
    });
    EXPECT_EQ(tradableSandResp.outcome(), ASSET_OP_OUTCOME_APPLIED);
    EXPECT_EQ(inv.GetTotalTradableItemCount(trialjournal::kBoundReforgeSandItemId), 0u);
    EXPECT_EQ(inv.GetTotalItemCount(trialjournal::kBoundReforgeSandItemId), 3u);
}

TEST_F(PlayerTrialJournalSystemTest, FullThreeChaptersGrandmasterM901AndSeasonalIsolation) {
    ASSERT_TRUE(PlayerTrialJournalSystem::ValidateTables());
    PlayerTrialJournalSystem::SettlementDelta d;

    // 1) 点亮卷一全部 12 印 + 卷二普通两格 6 印 = 18/24 永久印，达标终极里程碑 M901
    ApplyBattleCredit(MakeTrialSettlement(6101, 101, 1, 5), d);
    auto s102 = MakeTrialSettlement(6102, 102, 1, 5);
    s102.mutable_trial_facts()->set_guard_stance_dispelled(true);
    ApplyBattleCredit(s102, d);
    ApplyBattleCredit(MakeTrialSettlement(6103, 103, 1, 6), d);
    auto s104 = MakeTrialSettlement(6104, 104, 1, 6);
    s104.mutable_trial_facts()->set_guard_stance_dispelled(true);
    ApplyBattleCredit(s104, d);

    auto s201 = MakeTrialSettlement(6201, 201, 1, 6);
    s201.mutable_trial_facts()->set_self_triggered_shield_break(true);
    ApplyBattleCredit(s201, d);
    auto s202 = MakeTrialSettlement(6202, 202, 1, 6);
    s202.mutable_trial_facts()->set_self_triggered_anti_control_or_season_skill(true);
    ApplyBattleCredit(s202, d);

    // 2) 卷三 S1 赛季印（301 护卫救援 + 302 物法易势 + 303 人宠会武）写入 season_records，不污染永久计数与永久位图
    auto s301 = MakeTrialSettlement(6301, 301, 1, 5);
    s301.mutable_trial_facts()->set_season_id(1);
    s301.mutable_trial_facts()->set_guard_survived(true);
    ApplyBattleCredit(s301, d);
    EXPECT_EQ(d.newlyLitSealIds.size(), 2u); // 3011 + 3012

    auto s302 = MakeTrialSettlement(6302, 302, 1, 8, 5);
    s302.mutable_trial_facts()->set_season_id(1);
    s302.mutable_trial_facts()->set_team_dealt_physical_direct_damage(true);
    s302.mutable_trial_facts()->set_team_dealt_magic_direct_damage(true);
    ApplyBattleCredit(s302, d);
    EXPECT_EQ(d.newlyLitSealIds.size(), 2u); // 3021 + 3022
    EXPECT_FALSE(d.newSoloBest);
    EXPECT_FALSE(d.newCoopBest) << "PVP 卷三格只点亮印记，不写 PVE 竞速回合纪录";

    // 过季/错误 season_id 的卷三结算被忽略
    auto staleSeason = MakeTrialSettlement(6303, 303, 1, 6, 3);
    staleSeason.mutable_trial_facts()->set_season_id(99);
    staleSeason.mutable_trial_facts()->set_self_triggered_shield_break(true);
    ApplyBattleCredit(staleSeason, d);
    EXPECT_FALSE(d.changed);

    // 正式人宠会武合格负局（participated=true）可点亮 3031/3032
    auto s303 = MakeTrialSettlement(6304, 303, 1, 6, 3, BATTLE_OUTCOME_SIDE_B_WIN);
    s303.set_participated(true);
    s303.mutable_trial_facts()->set_season_id(1);
    s303.mutable_trial_facts()->set_self_triggered_shield_break(true);
    s303.mutable_trial_facts()->set_self_triggered_anti_control_or_season_skill(true);
    ApplyBattleCredit(s303, d);
    EXPECT_EQ(d.newlyLitSealIds.size(), 2u); // 3031 + 3032

    GetTrialJournalResponse snapResp;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, snapResp), kSuccess);
    EXPECT_EQ(snapResp.snapshot().total_permanent_lit_seals(), 18u) << "卷三 6 枚赛季印绝不得混入永久 18 印";
    EXPECT_EQ(snapResp.snapshot().chapters(2).lit_seal_count(), 6u);
    const auto* m901 = FindMilestoneView(snapResp.snapshot(), 901);
    ASSERT_NE(m901, nullptr);
    EXPECT_TRUE(m901->claimable());

    // 3) 领取 M901 永久免费授予「星阵宗师」20_star_formation_master_girl
    ClaimTrialJournalMilestoneRequest claim901;
    claim901.set_client_request_id(9401);
    claim901.set_milestone_id(901);
    ClaimTrialJournalMilestoneResponse resp901;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, claim901, resp901);
    EXPECT_EQ(resp901.error_message().id(), 0u);
    EXPECT_EQ(resp901.granted_appearance_id(), "20_star_formation_master_girl");

    PlayerAppearanceInfo appInfo;
    PlayerAppearanceSystem::FillInfo(player, kNowMs / 1000u, appInfo);
    bool ownedStarMaster = false;
    for (const auto& entry : appInfo.appearances()) {
        if (entry.appearance_id() == "20_star_formation_master_girl") {
            ownedStarMaster = entry.owned();
            EXPECT_FALSE(entry.card_unlockable());
        }
    }
    EXPECT_TRUE(ownedStarMaster);

    // 4) 领取卷三 S1 里程碑 302（4 印）落进 season_records，不写永久 claimed_milestone_bits，并永久解锁专属灵宠幻化 #3 月弦师
    ClaimTrialJournalMilestoneRequest claim302;
    claim302.set_client_request_id(9402);
    claim302.set_milestone_id(302);
    ClaimTrialJournalMilestoneResponse resp302;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, claim302, resp302);
    EXPECT_EQ(resp302.error_message().id(), 0u);
    EXPECT_EQ(resp302.granted_sand_count(), 1u);
    EXPECT_EQ(resp302.granted_pet_cosmetic_id(), 3u);

    ClaimTrialJournalMilestoneResponse resp302Replay;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, claim302, resp302Replay);
    EXPECT_EQ(resp302Replay.error_message().id(), 0u);
    EXPECT_TRUE(resp302Replay.replayed());
    EXPECT_EQ(resp302Replay.granted_pet_cosmetic_id(), 3u);

    const auto& comp = tlsEcs.actorRegistry.get<PlayerTrialJournalComp>(player);
    ASSERT_EQ(comp.season_records_size(), 1);
    EXPECT_EQ(comp.season_records(0).season_id(), 1u);
    EXPECT_EQ(comp.season_records(0).lit_seal_ids_size(), 6);
    ASSERT_EQ(comp.season_records(0).claimed_milestone_ids_size(), 1);
    EXPECT_EQ(comp.season_records(0).claimed_milestone_ids(0), 302u);

    // 5) 点亮卷一困难·双卫同阵（105）3 印达到卷一 15 印，领取 M105 永久解锁专属技能特效 #3 寒星流华（不自动穿戴）
    auto s105 = MakeTrialSettlement(6105, 105, 1, 6);
    s105.mutable_trial_facts()->set_guard_stance_dispelled(true);
    ApplyBattleCredit(s105, d);
    EXPECT_EQ(d.newlyLitSealIds.size(), 3u);

    ClaimTrialJournalMilestoneRequest claim105;
    claim105.set_client_request_id(9403);
    claim105.set_milestone_id(105);
    ClaimTrialJournalMilestoneResponse resp105;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, claim105, resp105);
    EXPECT_EQ(resp105.error_message().id(), 0u);
    EXPECT_EQ(resp105.granted_skill_vfx_id(), 3u);

    ClaimTrialJournalMilestoneResponse resp105Replay;
    PlayerTrialJournalSystem::ClaimMilestone(player, kNowMs, claim105, resp105Replay);
    EXPECT_EQ(resp105Replay.error_message().id(), 0u);
    EXPECT_TRUE(resp105Replay.replayed());
    EXPECT_EQ(resp105Replay.granted_skill_vfx_id(), 3u);

    PlayerAppearanceInfo cosmeticInfo;
    PlayerAppearanceSystem::FillInfo(player, kNowMs / 1000u, cosmeticInfo);
    bool ownedPet3 = false;
    for (const auto& petCos : cosmeticInfo.pet_cosmetics()) {
        if (petCos.config_id() == 3u) {
            ownedPet3 = petCos.owned();
        }
    }
    EXPECT_TRUE(ownedPet3);
    bool ownedVfx3 = false;
    for (const auto& vfxCos : cosmeticInfo.skill_vfx_cosmetics()) {
        if (vfxCos.config_id() == 3u) {
            ownedVfx3 = vfxCos.owned();
        }
    }
    EXPECT_TRUE(ownedVfx3);
    EXPECT_TRUE(cosmeticInfo.equipped_skill_vfx().empty()) << "里程碑授形只置永久拥有位图，不自动替玩家穿戴";
}

TEST_F(PlayerTrialJournalSystemTest, NormalBossTrialClearUnlocksHardWithoutJournalEntry) {
    EXPECT_FALSE(PlayerTrialJournalSystem::HasClearedAnyNormalBossTrial(player));
    GetTrialJournalResponse before;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, before), kSuccess);
    EXPECT_FALSE(before.snapshot().hard_boss_trial_unlocked());

    // 灵障首领（boss_trial_kind=3）普通难度胜局（无 TrialJournalEntry 行）也应解锁全部困难首领试炼
    BattleSettlementData barrierNormalWin;
    barrierNormalWin.set_battle_id(9901);
    barrierNormalWin.set_player_id(kTestPlayerId);
    barrierNormalWin.set_player_team_index(0);
    barrierNormalWin.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
    barrierNormalWin.set_total_rounds(5);
    barrierNormalWin.set_boss_trial_kind(3);
    barrierNormalWin.set_battle_difficulty(1);

    EXPECT_TRUE(PlayerTrialJournalSystem::HasJournalUpdate(barrierNormalWin));
    PlayerTrialJournalSystem::SettlementDelta delta;
    ApplyBattleCredit(barrierNormalWin, delta);
    EXPECT_TRUE(delta.changed);
    EXPECT_TRUE(PlayerTrialJournalSystem::HasClearedAnyNormalBossTrial(player));

    GetTrialJournalResponse after;
    ASSERT_EQ(PlayerTrialJournalSystem::GetJournal(player, kNowMs, after), kSuccess);
    EXPECT_TRUE(after.snapshot().hard_boss_trial_unlocked());
}
