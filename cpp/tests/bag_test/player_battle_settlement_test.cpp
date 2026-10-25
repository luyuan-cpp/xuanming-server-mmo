#include <gtest/gtest.h>
#include "engine/core/type_define/player_guid_comp.h"

#include <algorithm>
#include <array>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <functional>
#include <limits>
#include <map>
#include <memory>
#include <optional>
#include <string>
#include <vector>
#include <hiredis/hiredis.h>

#include "modules/condition/condition_type.h"
#include "modules/mission/comp/mission_comp.h"
#include "modules/mission/system/mission.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/item_system.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "proto/battle/battle_data.pb.h"
#include "table/code/item_table.h"
#include "table/code/pet_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/condition_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/mission_table.h"
#include "table/code/monster_table.h"
#include "table/code/reward_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_login_comp.pb.h" // LOGIN_FIRST
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/battle_settlement_ledger_comp.pb.h"
#include "services/scene/battle/system/battle_settlement_ledger.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/comp/last_persisted_snapshot_comp.h"
#include "services/scene/player/system/player_mission.h"
#include "services/scene/player/system/role_admission_system.h"
#include "services/scene/player/system/player_inbox_registry.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/event/battle_event.pb.h"
#include "modules/mission/mission_events.h"
#include "services/battle/settlement/settlement_outbox.h"
#include "services/scene/battle/system/player_battle.h"
#include "thread_context/ecs_context.h"

// 访问生产共用入口；不复制金币和任务副作用实现，不连接 Redis 或登录账号。
class PlayerBattleSettlementTestAccess {
public:
    static bool Snapshot(entt::entity player, BattlePlayerSnapshot& snapshot, uint32_t& errorTipId) {
        return PlayerBattleSystem::BuildBattleSnapshot(player, snapshot, errorTipId);
    }
    static bool Apply(entt::entity player, const BattleSettlementData& settlement) {
        return PlayerBattleSystem::ApplySettlementToEntity(player, settlement);
    }
    static bool Apply(entt::entity player, const BattleSettlementData& settlement,
                      bool& alreadyAppliedOut) {
        return PlayerBattleSystem::ApplySettlementToEntity(player, settlement, &alreadyAppliedOut);
    }
    static void ApplyPending(entt::entity player, const BattleSettlementData& settlement) {
        BattleSettlementEvent event;
        *event.mutable_settlement() = settlement;
        PlayerBattleSystem::ApplyPendingSettlement(player, event);
    }
    static void RecoverAfterFreezeExpiry(entt::entity player, uint64_t playerId) {
        PlayerBattleSystem::RecoverPendingSettlementAfterFreezeExpiry(player, playerId);
    }
    static bool LoginRecheckPending(entt::entity player) {
        return PlayerBattleSystem::IsLoginRecheckPending(player);
    }
    static void RunLoginRechecks() {
        PlayerBattleSystem::RunLoginRechecks();
    }
    static void ReleaseExpiredFightingFreeze(entt::entity player, uint64_t playerId, uint64_t battleId) {
        PlayerBattleSystem::ReleaseExpiredFightingFreeze(player, playerId, battleId);
    }
    static uint64_t ExpiredFightingBattleId(entt::entity player) {
        return PlayerBattleSystem::ExpiredFightingBattleId(player);
    }
    static bool DecideLockRebuild(entt::entity player, uint64_t lockBattleId,
                                  battle_settlement::LockRebuildDecision& out) {
        return PlayerBattleSystem::DecideLockRebuildFor(player, lockBattleId, out);
    }
};

namespace {

// 结算应用成功后会立刻压一次存盘(真实现是 PlayerLifecycleSystem::SavePlayerToRedis)。
// 单测不连 Redis,注入一个只计数的假实现;返回 false = 本次没有产生写入,
// 于是 durable 判定只能落在 PlayerLastPersistedSnapshotComp 上 —— 单测里它从不存在,
// 正是「已应用但尚未落盘」这个最关键的中间态。
int gPersistCalls = 0;
bool CountingPersist(entt::entity) {
    ++gPersistCalls;
    return false;
}

bool ArmSettlementMissionItemSegment(uint64_t lo = 100000, uint64_t hi = 200000) {
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

class PlayerMissionWithoutSinkTest : public testing::Test {
protected:
    void SetUp() override {
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
        RewardTableManager::Instance().Load();
        ItemTableManager::Instance().Load();
        DungeonTableManager::Instance().Load();
        MonsterTableManager::Instance().Load();
        ASSERT_TRUE(ArmSettlementMissionItemSegment());
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayerId);
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(kPlayerId);
        ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
        tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player).set_health(100);
        tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player).set_max_health(100);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<BattleSettlementLedgerComp>(player);
        ASSERT_TRUE(tlsEcs.playerList.emplace(kPlayerId, player).second);
        gPersistCalls = 0;
        PlayerBattleSystem::SetPersistFnForTest(&CountingPersist);
    }
    void TearDown() override {
        PlayerBattleSystem::SetPersistFnForTest(nullptr);
        tlsEcs.playerList.erase(kPlayerId);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        EXPECT_TRUE(ArmSettlementMissionItemSegment(1, uint64_t{1} << 54));
    }
    MissionsComp& Missions() { return tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0); }
    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    static constexpr Guid kPlayerId = 88448844;
    entt::entity player{entt::null};
};

TEST_F(PlayerMissionWithoutSinkTest, BattleKillCompletesAndAutoGrantsWithoutAnyDispatcherSink) {
    ASSERT_TRUE(tlsEcs.dispatcher.sink<ConditionEvent>().empty());
    ASSERT_EQ(kSuccess, PlayerMissionSystem::Accept(player, 0, 4, 1000));
    Inventory().SetCapacityForRestore(4);
    BattleSettlementData settlement;
    settlement.set_player_id(kPlayerId);
    settlement.set_battle_id(801);
    settlement.set_health(100);
    auto* defeat = settlement.add_defeated_monsters();
    defeat->set_monster_config_id(1);
    defeat->set_count(1);

    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_TRUE(Missions().IsComplete(4));
    EXPECT_FALSE(Missions().IsClaimable(4));
    uint32_t itemCount = 0;
    Inventory().ForEachItem([&itemCount](Guid, const ItemComp& item) { itemCount += item.size(); });
    EXPECT_EQ(4u, itemCount);
}

// 只为结算幂等用例调整内存表行,析构时还原;不把虚构怪物声明成正式副本来源。
class ScopedSettlementMissionRows {
public:
    bool Install() {
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
        auto* mission = const_cast<MissionTable*>(MissionTableManager::Instance().FindByIdSilent(4).first);
        auto* first = const_cast<ConditionTable*>(ConditionTableManager::Instance().FindByIdSilent(1).first);
        auto* second = const_cast<ConditionTable*>(ConditionTableManager::Instance().FindByIdSilent(2).first);
        if (mission == nullptr || first == nullptr || second == nullptr) return false;
        missionBefore = *mission;
        firstBefore = *first;
        secondBefore = *second;
        installed = true;
        mission->clear_condition_id();
        mission->add_condition_id(1);
        mission->add_condition_id(2);
        mission->clear_target_count();
        mission->add_target_count(100);
        mission->add_target_count(100);
        first->clear_condition1();
        first->add_condition1(7001);
        second->clear_condition1();
        second->add_condition1(7002);
        return true;
    }
    ~ScopedSettlementMissionRows() {
        if (!installed) return;
        *const_cast<MissionTable*>(MissionTableManager::Instance().FindByIdSilent(4).first) = missionBefore;
        *const_cast<ConditionTable*>(ConditionTableManager::Instance().FindByIdSilent(1).first) = firstBefore;
        *const_cast<ConditionTable*>(ConditionTableManager::Instance().FindByIdSilent(2).first) = secondBefore;
    }
private:
    MissionTable missionBefore;
    ConditionTable firstBefore;
    ConditionTable secondBefore;
    bool installed{false};
};

class PlayerBattleSettlementTest : public testing::Test {
protected:
    void SetUp() override {
        // 缓存跨实体生命周期，所以每个测试/重复运行分配不同真实身份。
        static uint64_t nextPlayer = 9876500;
        playerId = ++nextPlayer;
        RoleAdmissionSystem::Dependencies dependencies;
        common::base::RoleAdmissionIdentity identity;
        identity.set_player_id(playerId); identity.set_generation(1); identity.set_owner_revision(1);
        identity.set_owner_epoch(7); identity.set_scene_instance_id("battle-sql-test-scene");
        dependencies.confirmedAdmissions.push_back(identity);
        dependencies.readSqlAck = [this](Guid id, auto completed) {
            EXPECT_EQ(playerId, id);
            ++sqlReads;
            if (deferSql) pendingSql.push_back(std::move(completed));
            else completed(sqlAck);
            return true;
        };
        admission = std::make_unique<RoleAdmissionSystem::ScopedDependencies>(std::move(dependencies));
        ASSERT_TRUE(missionRows.Install());
        CreatePlayerEntity();
        RegisterPlayer();
        settlement.set_player_id(playerId);
        settlement.set_battle_id(701);
        settlement.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
        settlement.set_health(80);
        settlement.set_mana(20);
        settlement.set_gold_gain(12);
        for (const auto configId : {uint32_t{7002}, uint32_t{7001}}) {
            auto* defeat = settlement.add_defeated_monsters();
            defeat->set_monster_config_id(configId);
            defeat->set_count(1);
        }
        gPersistCalls = 0;
        PlayerBattleSystem::SetPersistFnForTest(&CountingPersist);
    }
    // 销账入口 AckSettlementPending 按 player_id 现取实体(tlsEcs.GetPlayer),不登记就走不到
    // 「活实体 + 账本」那几条规则。重建实体后必须重新登记。
    void RegisterPlayer() {
        tlsEcs.playerList.erase(playerId);
        tlsEcs.playerList.emplace(playerId, player);
    }
    void TearDown() override {
        PlayerBattleSystem::SetPersistFnForTest(nullptr);
        tlsEcs.playerList.erase(playerId);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        pendingSql.clear();
        admission.reset();
    }
    void CreatePlayerEntity() {
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{playerId});
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 7;
        tlsEcs.actorRegistry.emplace<PlayerRoleAdmissionComp>(player, std::uint64_t{1}, std::uint64_t{1});
        auto& attributes = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player);
        attributes.set_health(100);
        attributes.set_mana(50);
        auto& derived = tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player);
        derived.set_max_health(100);
        derived.set_max_mana(50);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
        // 规则入口接取受控任务;正式可达性由独立的真实任务4无sink用例验证。
        auto& missions = tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0);
        ASSERT_EQ(kSuccess, MissionSystem::AcceptMission({player, 4}, missions, MissionConfig::GetSingleton()));
        // 结算幂等账本是加载时(PlayerDatabaseMessageFieldsUnmarshal)无条件挂上的必需组件,
        // 结算路径只取不建。夹具照加载路径挂一本空账本 = 「盘上没有任何已应用条目」。
        tlsEcs.actorRegistry.emplace<BattleSettlementLedgerComp>(player);
    }
    // 两份证据由夹具分别完成；默认不伪造SQL提交。
    void SaveSettlementCandidate(bool sqlCommitted) {
        PlayerAllData persisted;
        auto* first = persisted.mutable_player_database_data();
        first->set_player_id(playerId);
        persisted.set_last_save_seq(++saveSeq);
        first->mutable_settlement_ledger()->add_applied()->set_battle_id(settlement.battle_id());
        auto* second = persisted.mutable_player_database_1_data();
        second->set_player_id(playerId);
        const auto source = std::make_shared<const PlayerAllData>(persisted);
        const auto committed = RoleAdmissionSystem::CaptureSnapshotCommit(player, source, 7, saveSeq);
        ASSERT_TRUE(static_cast<bool>(committed));
        committed();
        tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player).Replace(*source);
        if (sqlCommitted) {
            const auto q = std::to_string(saveSeq);
            const auto version = std::string(19, '0') + "7:" + std::string(20 - q.size(), '0') + q;
            sqlAck = RoleAdmissionSystem::SqlAck{"7", version, version, version};
        }
    }
    uint32_t KillProgressAt(int slot) const {
        const auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
        const auto* missions = container != nullptr ? container->Get(0) : nullptr;
        if (missions == nullptr) return 0;
        const auto& active = missions->GetMissionList().missions();
        const auto it = active.find(4);
        if (it == active.end() || slot >= it->second.progress_size()) return 0;
        return it->second.progress(slot);
    }
    uint32_t KillProgress() const { return KillProgressAt(0) + KillProgressAt(1); }
    entt::entity player{entt::null};
    uint64_t playerId{};
    BattleSettlementData settlement;
    ScopedSettlementMissionRows missionRows;
    std::unique_ptr<RoleAdmissionSystem::ScopedDependencies> admission;
    std::optional<RoleAdmissionSystem::SqlAck> sqlAck;
    std::vector<std::function<void(std::optional<RoleAdmissionSystem::SqlAck>)>> pendingSql;
    bool deferSql{false};
    int sqlReads{0};
    uint64_t saveSeq{0};
};

TEST_F(PlayerBattleSettlementTest, DuplicateSuccessfulCallbacksPayAndProgressOnce) {
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    // 模拟两个 GET 均返回同一 battle_id，DEL 尚未执行时第二个回调抵达共用入口。
    // 改造后返回 true(= 这一局已应用,调用方可以解冻并尝试销账),但出参会告诉调用方
    // 「奖是上一次发的」,据此不再推一次结算面板;钱和进度当然不能再动。
    bool alreadyApplied = false;
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement, alreadyApplied));
    EXPECT_TRUE(alreadyApplied);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(1u, KillProgressAt(0));
    EXPECT_EQ(1u, KillProgressAt(1));
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 80u);
    // 下一局不依赖 id 单调递增，正常收益仍然可应用。
    settlement.set_battle_id(700);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 24u);
    EXPECT_EQ(KillProgress(), 4u);
}

TEST_F(PlayerBattleSettlementTest, CurrencyFailureLeavesPendingSideEffectsUntouchedAndCanRetry) {
    tlsEcs.actorRegistry.remove<CurrencyComp>(player);
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id());
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana(), 50u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_TRUE(tlsEcs.actorRegistry.all_of<InBattleComp>(player));
    tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(KillProgress(), 2u);
}

// 空宠物列表是合法持久状态；整个组件缺失则是加载故障，不能跳过宝宝后仍发金币、记整笔账本。
TEST_F(PlayerBattleSettlementTest, MissingPetComponentRejectsBeforeCurrencyHealthAndLedgerChanges) {
    settlement.add_pets()->set_pet_id(901);
    ASSERT_FALSE(tlsEcs.actorRegistry.all_of<PlayerPetComp>(player));

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(50u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana());
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerPetComp>(player));
}

TEST_F(PlayerBattleSettlementTest, MissingBagComponentRejectsBeforeCurrencyHealthAndLedgerChanges) {
    auto* gained = settlement.add_items_gained();
    gained->set_item_table_id(7001);
    gained->set_count(1);
    ASSERT_FALSE(tlsEcs.actorRegistry.all_of<PlayerBagsComp>(player));

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerBagsComp>(player));
}

TEST_F(PlayerBattleSettlementTest, MissingDerivedAttributesRejectsBeforeCurrencyAndCanRetryAfterRecovery) {
    tlsEcs.actorRegistry.remove<DerivedAttributesComp>(player);

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(player));

    auto& derived = tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player);
    derived.set_max_health(100);
    derived.set_max_mana(50);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

TEST_F(PlayerBattleSettlementTest, MissingMissionComponentRejectsBeforeCurrencyProgressAndLedgerChanges) {
    tlsEcs.actorRegistry.remove<MissionsContainerComp>(player);

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<MissionsContainerComp>(player));
}

TEST_F(PlayerBattleSettlementTest, SnapshotRejectsMissingPersistentOrDerivedInputsBeforeChangingOutput) {
    auto& registry = tlsEcs.actorRegistry;
    registry.emplace<LevelComp>(player);
    registry.emplace<PlayerUint32Comp>(player);
    registry.emplace<PlayerProfileComp>(player);
    registry.emplace<PlayerSkillListComp>(player);
    registry.emplace<PlayerPetComp>(player);
    registry.emplace<PlayerBagsComp>(player);

    const auto expectRefusedWithout = [&]<typename Comp>() {
        registry.remove<Comp>(player);
        BattlePlayerSnapshot snapshot;
        snapshot.set_player_id(901);
        uint32_t errorTipId = kSuccess;
        EXPECT_FALSE(PlayerBattleSettlementTestAccess::Snapshot(player, snapshot, errorTipId));
        EXPECT_EQ(kServiceUnavailable, errorTipId);
        EXPECT_EQ(901u, snapshot.player_id());
        EXPECT_FALSE(registry.all_of<Comp>(player));
        registry.emplace<Comp>(player);
    };
    expectRefusedWithout.template operator()<LevelComp>();
    expectRefusedWithout.template operator()<PlayerUint32Comp>();
    expectRefusedWithout.template operator()<PlayerProfileComp>();
    expectRefusedWithout.template operator()<PlayerSkillListComp>();
    expectRefusedWithout.template operator()<PlayerPetComp>();
    expectRefusedWithout.template operator()<PlayerBagsComp>();
    expectRefusedWithout.template operator()<DerivedAttributesComp>();
}

// 真实重登:资产与账本来自同一份落盘 blob(不变量 I3),所以账本要和金币一起带过去。
// 登录补应用与 10s 后发件箱的再一次投递都必须被账本挡住,且不能摘掉玩家新一局的冻结。
TEST_F(PlayerBattleSettlementTest, ReloginOldPendingDoesNotPayAgainOrRemoveNewBattle) {
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    const auto currency = tlsEcs.actorRegistry.get<CurrencyComp>(player);
    const auto attributes = tlsEcs.actorRegistry.get<BaseAttributesComp>(player);
    const auto ledger = tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player);
    const auto missions = tlsEcs.actorRegistry.get<MissionsContainerComp>(player);
    tlsEcs.actorRegistry.destroy(player);
    CreatePlayerEntity();
    RegisterPlayer();
    tlsEcs.actorRegistry.get<CurrencyComp>(player) = currency;
    tlsEcs.actorRegistry.get<BaseAttributesComp>(player) = attributes;
    tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player) = ledger;
    tlsEcs.actorRegistry.get<MissionsContainerComp>(player) = missions;
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(702);
    gPersistCalls = 0;
    for (int delivery = 0; delivery < 2; ++delivery) {
        PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
        EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u) << "delivery " << delivery;
        EXPECT_EQ(KillProgress(), 2u) << "delivery " << delivery;
        const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
        ASSERT_NE(current, nullptr);
        EXPECT_EQ(current->battle_id(), 702u);
    }
    // 两次都是「已应用、未落盘」:不销账,各压一次存盘把账本推上盘。
    EXPECT_EQ(gPersistCalls, 2);
}

// 进程内缓存说应用过、实体账本里却没有:只可能是那次应用没落盘、实体被重建过 ——
// 资产也没落盘,这一局必须在**这一次**投递就重新应用(登录钩子只投这一次,不能指望下一次),
// 而且只能一次。
TEST_F(PlayerBattleSettlementTest, CacheHitButLedgerMissReappliesExactlyOnce) {
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    ASSERT_EQ(KillProgress(), 2u);
    tlsEcs.actorRegistry.destroy(player);
    CreatePlayerEntity();  // 从「没落盘」的旧 blob 重建:没有这一局的金币,也没有账本条目
    RegisterPlayer();

    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);  // 按账本清掉陈旧缓存,当场应用
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(KillProgress(), 2u);
    EXPECT_TRUE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));

    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);  // 再投一次:账本挡住
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(KillProgress(), 2u);
}

TEST_F(PlayerBattleSettlementTest, MissingAttributesCanRetryWithoutPrematureGoldCredit) {
    tlsEcs.actorRegistry.remove<BaseAttributesComp>(player);
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
    tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player).set_health(100);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementTest, WrongPlayerCannotApplyOrPoisonValidRetry) {
    settlement.set_player_id(playerId + 1);
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
    settlement.set_player_id(playerId);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}
} // namespace

// ---------------------------------------------------------------------------
// 2026-09-25 §9.3:结算幂等的持久基底。
// 改造前幂等只有 thread_local 应用缓存,进程一重启就空,于是「应用 -> 当场销账 ->
// 存盘前崩溃」会把奖励永久吞掉。现在「这一局已应用」与资产写在同一份 player_database
// blob 里,同生共死。这一组用例守的就是那条不变量。
// ---------------------------------------------------------------------------

namespace {

TEST_F(PlayerBattleSettlementTest, AppliedSettlementIsRecordedInPersistedLedger) {
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    const auto* ledger = tlsEcs.actorRegistry.try_get<BattleSettlementLedgerComp>(player);
    ASSERT_NE(ledger, nullptr);
    EXPECT_TRUE(battle_settlement::HasApplied(*ledger, settlement.battle_id()));
    // 账本条目必须与资产在同一次存盘里落地,所以应用成功后必须立刻压一次存盘。
    EXPECT_EQ(gPersistCalls, 1);
}

// 进程重启 / 实体重建之后,「已应用」只能靠从盘上回来的账本知道。
// 这里直接预置账本(= 盘上那份),应用缓存对这个 battle_id 一无所知,
// 于是唯一能拦住重复发奖的就是持久账本本身。
TEST_F(PlayerBattleSettlementTest, LedgerFromDiskBlocksReapplyAfterRestart) {
    settlement.set_battle_id(7031);
    auto& ledger = tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player);
    battle_settlement::RecordApplied(ledger, settlement.battle_id(), 1234);

    bool alreadyApplied = false;
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement, alreadyApplied));
    EXPECT_TRUE(alreadyApplied);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
}

// 冻结期必须整笔延后,而不是只挡金币和道具。
// 一笔「只有 HP + 击杀事实、没有金币也没有掉落」的 PVE 结算(打空怪,完全正常)
// 改造前会穿过那道只看 gold/items 的窄闸:任务进度被推、血量被改,而此刻存盘被硬性
// 跳过,改动随实体销毁一起没,账本却记成已应用 —— 等于把进度白送出去又丢掉。
TEST_F(PlayerBattleSettlementTest, FrozenPlayerDefersSettlementWithNoGoldOrItems) {
    settlement.set_gold_gain(0);
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    // 账本是加载时挂上的(夹具同此),这一局绝不能被登记进去。
    EXPECT_FALSE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    EXPECT_EQ(gPersistCalls, 0);

    // 解冻后照常应用,奖励一次不差 —— 延后不是丢弃。
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(KillProgress(), 2u);
}

// 归属交接一挂上,SavePlayerToRedis 开头就直接跳过写盘。此时应用等于改了一份永远落不了盘
// 的内存态,必须整笔延后,交给发件箱重投或目的地节点补应用。
TEST_F(PlayerBattleSettlementTest, TravelHandoffDefersWholeSettlement) {
    auto& handoff = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    handoff.requestedAtMs = 42;

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
}

// 审计 cpp-economy#1(data-overwrite-rollback-audit-20261018.md §2.4):GM 封锁金币获取后,AddCurrency
// 回的是终局拒绝 kAssetBlocked。修复前结算把它当暂时失败整笔返回 false,每次重投都同样被拒,这一局
// 永远应用不上:药品不扣,掉落与任务进度丢失,下一局开打时单槽 pending 还会被覆盖。
// 修复后只跳过金币,其余照常应用并登记账本(与资产通道的封禁口径一致)。解封后同一局的重投
// 被账本挡住:金币不补发,进度也不重推。
TEST_F(PlayerBattleSettlementTest, GmCurrencyBlockSkipsGoldButAppliesRestOfSettlement) {
    ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));

    bool alreadyApplied = true;
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement, alreadyApplied));  // 修复前 false
    EXPECT_FALSE(alreadyApplied);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(KillProgress(), 2u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 80u);
    EXPECT_TRUE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    EXPECT_EQ(gPersistCalls, 1);

    ASSERT_EQ(kSuccess, CurrencySystem::UnblockCurrency(player, kCurrencyGold));
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement, alreadyApplied));
    EXPECT_TRUE(alreadyApplied);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(KillProgress(), 2u);
}

// 全服 GainBlock 与单人 GM 封禁同属终局拒绝(AddCurrency 都回 kAssetBlocked),处理口径相同。
TEST_F(PlayerBattleSettlementTest, GlobalCurrencyGainBlockSkipsGoldButAppliesRestOfSettlement) {
    struct ScopedGlobalGoldBlock {
        const uint32_t gold = static_cast<uint32_t>(kCurrencyGold);
        const bool wasBlocked =
            GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency, gold);
        ScopedGlobalGoldBlock() { GainBlockService::BlockGlobal(GainBlockService::GainType::kCurrency, gold); }
        ~ScopedGlobalGoldBlock() {
            if (!wasBlocked) GainBlockService::UnblockGlobal(GainBlockService::GainType::kCurrency, gold);
        }
    } block;

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));  // 修复前 false
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(KillProgress(), 2u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 80u);
    EXPECT_TRUE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
}

// 「已应用但未落盘」时销账必须延后:这一刻销账,崩溃即永久丢奖励(§9.3)。
// 可观察的证据是销账入口又压了一次存盘(应用时一次 + 销账延后一次),账本条目也不能被摘。
TEST_F(PlayerBattleSettlementTest, AckDefersClearWhileAppliedButNotDurable) {
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id());
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);

    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(gPersistCalls, 2);
    EXPECT_TRUE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    // 冻结照常解除(锁留到落盘,但组件当场摘):玩家不会被卡在已结束的战斗里
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
}

// 原用例现在明确完成同一快照的双SQL证据；Redis删除仍由独立真Redis合同验证。
TEST_F(PlayerBattleSettlementTest, AckClearsOnceLedgerEntryIsDurable) {
    SaveSettlementCandidate(true);
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(gPersistCalls, 1); // 只有应用时那一次
    EXPECT_EQ(1, sqlReads) << "必须核双SQL，不得仅凭Redis候选销账";
}

TEST_F(PlayerBattleSettlementTest, RedisOnlySettlementRetainsLedgerAndRequestsSqlProof) {
    SaveSettlementCandidate(false);
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
    EXPECT_EQ(1, sqlReads);
    EXPECT_TRUE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
}

TEST_F(PlayerBattleSettlementTest, SqlProofForOldEntityDoesNotModifyRecreatedPlayer) {
    SaveSettlementCandidate(true);
    deferSql = true;
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
    ASSERT_EQ(1u, pendingSql.size());
    const auto old = player;
    tlsEcs.playerList.erase(playerId);
    tlsEcs.actorRegistry.destroy(player);
    CreatePlayerEntity();
    RegisterPlayer();
    const auto bytes = tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player).SerializeAsString();
    pendingSql.front()(sqlAck);
    EXPECT_NE(old, player);
    EXPECT_EQ(bytes, tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player).SerializeAsString());
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
}

// 「已有未落地存盘就不再压」依赖真实 Redis 客户端的队列状态,单测里客户端为空,
// 留给 battle-smoke 按日志核对(每场结算只应出现一次结算存盘)。

// 账本缺失 = 加载不变量被破坏(docs/design/ecs-entt-misuse-audit-20261015.md F036)。
// 修复前应用入口 get_or_emplace 一本空账本:把「缺账本」当成「没应用过」照发奖励,还把空账本
// 挂回实体(下一次存盘会拿它覆盖盘上的条目)。修复后整笔不应用、零副作用、不补建;
// 账本按加载路径回来之后照常应用一次 —— 延后不是丢弃。
TEST_F(PlayerBattleSettlementTest, MissingLedgerRejectsSettlementWithoutSideEffects) {
    tlsEcs.actorRegistry.remove<BattleSettlementLedgerComp>(player);

    // 修复前这几条都红:返回 true、金币 12、击杀 2 条、血量 80、账本被补建、压了一次存盘。
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    EXPECT_EQ(gPersistCalls, 0);
    // ASSERT 而非 EXPECT:修复前账本已被补建,下面的 emplace 会触发 entt 断言把整个测试进程带崩。
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<BattleSettlementLedgerComp>(player));

    tlsEcs.actorRegistry.emplace<BattleSettlementLedgerComp>(player);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(KillProgress(), 2u);
}

// 销账入口同理:活实体缺账本时「已应用 / 没应用」都判不了。修复前补建一本空账本并按「没应用」
// 当场销账。单测没有 Redis,销账命令本身发不出去,可观察的差别是账本有没有被补建。
TEST_F(PlayerBattleSettlementTest, MissingLedgerAckDoesNotFabricateLedger) {
    tlsEcs.actorRegistry.remove<BattleSettlementLedgerComp>(player);
    // 玩家已在下一局:这条迟到结算按 battle_id 不匹配走销账入口(ApplySettlement 的在线分支)。
    const uint64_t nextBattleId = settlement.battle_id() + 1;
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(nextBattleId);
    BattleSettlementEvent event;
    *event.mutable_settlement() = settlement;

    PlayerBattleSystem::ApplySettlement(event);

    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<BattleSettlementLedgerComp>(player));  // 修复前红
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
    ASSERT_NE(current, nullptr);
    EXPECT_EQ(current->battle_id(), nextBattleId);
}

// v1执行入口不使用可裁剪的legacy账本；重复消费由PlayerInboxSystem永久域水位验证。
TEST_F(PlayerBattleSettlementTest, InboxSettlementUsesWholeEffectInputsWithoutLegacyLedgerOrOwnSave) {
    tlsEcs.actorRegistry.remove<BattleSettlementLedgerComp>(player);
    const auto result = PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString());
    EXPECT_EQ(PlayerInboxExecKind::kApplied, result.kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<BattleSettlementLedgerComp>(player));
    EXPECT_EQ(0, gPersistCalls) << "执行器不独立落盘：由Inbox效果+永久游标同快照保存";
}

TEST_F(PlayerBattleSettlementTest, InboxGoldBlockDefersWholeSettlementAndRetryAppliesAllEffects) {
    ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    ASSERT_EQ(kSuccess, CurrencySystem::UnblockCurrency(player, kCurrencyGold));
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
}

TEST_F(PlayerBattleSettlementTest, InboxChecksNetGoldAfterActiveDebtAndNeverWrapsBalance) {
    auto& currency = tlsEcs.actorRegistry.get<CurrencyComp>(player);
    currency.mutable_values()->Resize(static_cast<int>(kCurrencyMax), 0);
    currency.set_values(static_cast<int>(kCurrencyGold), std::numeric_limits<uint64_t>::max() - 5);
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, KillProgress());
    auto& debt = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player).debts[static_cast<uint32_t>(kCurrencyGold)];
    debt.owed = 10;
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(std::numeric_limits<uint64_t>::max() - 3, CurrencySystem::GetBalance(player, kCurrencyGold));
}

TEST_F(PlayerBattleSettlementTest, InboxMissingPlayerMissionScopeCannotDropKillOrCreditGold) {
    tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.clear();
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

TEST_F(PlayerBattleSettlementTest, InboxUnownedPetSkipsOnlyRetiredPetEffect) {
    auto& pets = tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
    settlement.add_pets()->set_pet_id(911);
    const auto before = pets.SerializeAsString();
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(before, pets.SerializeAsString());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

TEST_F(PlayerBattleSettlementTest, InboxMissingPetTableSkipsOnlyThatPetEffect) {
    auto& pets = tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
    auto* pet = pets.add_pets(); pet->set_pet_id(911); pet->set_pet_table_id(UINT32_MAX); pet->set_health(17);
    settlement.add_pets()->set_pet_id(911);
    const auto before = pets.SerializeAsString();
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(before, pets.SerializeAsString());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
}

TEST_F(PlayerBattleSettlementTest, InboxMissingPetPoolSkipsOnlyThatPetEffect) {
    PetTableManager::Instance().Load(); AttributePoolTableManager::Instance().Load();
    ASSERT_GT(PetTableManager::Instance().FindAll().data_size(), 0);
    struct RestorePoolOwners {
        std::vector<std::pair<uint32_t, uint32_t>> rows;
        ~RestorePoolOwners() {
            for (const auto& [id, owner] : rows) {
                auto* row = const_cast<AttributePoolTable*>(AttributePoolTableManager::Instance().FindByIdSilent(id).first);
                EXPECT_NE(nullptr, row);
                if (row) row->set_owner_type(owner);
            }
        }
    } restore;
    for (const auto& row : AttributePoolTableManager::Instance().FindAll().data()) {
        auto* changed = const_cast<AttributePoolTable*>(&row);
        restore.rows.emplace_back(row.id(), row.owner_type()); changed->set_owner_type(0);
    }
    auto& pets = tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
    auto* pet = pets.add_pets(); pet->set_pet_id(911);
    pet->set_pet_table_id(PetTableManager::Instance().FindAll().data(0).id()); pet->set_health(17);
    settlement.add_pets()->set_pet_id(911);
    const auto before = pets.SerializeAsString();
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(before, pets.SerializeAsString());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
}

TEST_F(PlayerBattleSettlementTest, InboxMissingUnknownOrDamagedPetStateStillDefersBeforeEffects) {
    settlement.add_pets()->set_pet_id(911);
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    auto& pets = tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
    pets.GetReflection()->MutableUnknownFields(&pets)->AddVarint(900, 1);
    const auto opaque = pets.SerializeAsString();
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(opaque, pets.SerializeAsString());
    pets.Clear(); pets.add_pets()->set_pet_id(911); pets.add_pets()->set_pet_id(911);
    const auto damaged = pets.SerializeAsString();
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(damaged, pets.SerializeAsString());
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

TEST_F(PlayerBattleSettlementTest, InboxNestedUnknownFieldCannotBeSilentlyApplied) {
    auto* defeat = settlement.mutable_defeated_monsters(0);
    defeat->GetReflection()->MutableUnknownFields(defeat)->AddVarint(900, 1);
    EXPECT_EQ(PlayerInboxExecKind::kVersionSkew, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, KillProgress());
}

TEST_F(PlayerBattleSettlementTest, InboxOldBattleCannotRewriteNextBattleHealthOrFreeze) {
    auto& current = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    current.set_battle_id(settlement.battle_id() + 1);
    const auto nextBytes = current.SerializeAsString();
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_TRUE(PlayerBattleSystem::CompleteInboxSettlement(player, settlement.SerializeAsString()));
    EXPECT_EQ(nextBytes, tlsEcs.actorRegistry.get<InBattleComp>(player).SerializeAsString());
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
}

// ── 账本纯规则(无 ECS、无 Redis)────────────────────────────────────────────

TEST(BattleSettlementLedgerRule, RecordIsIdempotentAndForgetRemoves) {
    BattleSettlementLedgerComp ledger;
    EXPECT_FALSE(battle_settlement::HasApplied(ledger, 9u));
    EXPECT_FALSE(battle_settlement::RecordApplied(ledger, 9u, 100));
    EXPECT_FALSE(battle_settlement::RecordApplied(ledger, 9u, 200));
    EXPECT_EQ(ledger.applied_size(), 1);          // 重投重复登记不产生第二条
    EXPECT_EQ(ledger.applied(0).applied_at_ms(), 200u);
    EXPECT_TRUE(battle_settlement::HasApplied(ledger, 9u));
    EXPECT_TRUE(battle_settlement::ForgetApplied(ledger, 9u));
    EXPECT_FALSE(battle_settlement::ForgetApplied(ledger, 9u));
    EXPECT_FALSE(battle_settlement::HasApplied(ledger, 9u));
}

TEST(BattleSettlementLedgerRule, ZeroBattleIdIsNeverRecordedOrMatched) {
    BattleSettlementLedgerComp ledger;
    EXPECT_FALSE(battle_settlement::RecordApplied(ledger, 0u, 1));
    EXPECT_EQ(ledger.applied_size(), 0);
    EXPECT_FALSE(battle_settlement::HasApplied(ledger, 0u));
}

// 满员时淘汰**最旧**的一条,顺序由 applied_at_ms 决定而不是下标(AGENTS.md §11.6)。
// 刻意把最旧那条放在中间,下标顺序与时间顺序不一致时才能测出来。
TEST(BattleSettlementLedgerRule, EvictsOldestByTimestampNotByIndex) {
    BattleSettlementLedgerComp ledger;
    for (int i = 0; i < battle_settlement::kMaxAppliedRecords; ++i) {
        const uint64_t at = (i == 3) ? 1u : static_cast<uint64_t>(1000 + i);
        EXPECT_FALSE(battle_settlement::RecordApplied(ledger, static_cast<uint64_t>(i + 1), at));
    }
    ASSERT_EQ(ledger.applied_size(), battle_settlement::kMaxAppliedRecords);

    EXPECT_TRUE(battle_settlement::RecordApplied(ledger, 999u, 5000));  // 溢出 -> 淘汰
    EXPECT_EQ(ledger.applied_size(), battle_settlement::kMaxAppliedRecords);
    EXPECT_FALSE(battle_settlement::HasApplied(ledger, 4u));  // 下标 3、时间戳最小的那条
    EXPECT_TRUE(battle_settlement::HasApplied(ledger, 1u));
    EXPECT_TRUE(battle_settlement::HasApplied(ledger, 999u));
}

} // namespace

// ---------------------------------------------------------------------------
// 2026-09-17 G1/G3:结算道具落地(docs/design/turn-battle-gap-closure.md)
// 覆盖:消耗按实际持有夹紧、掉落真入包、道具失败不得连累金币入账。
// 物品 id 用真实 Item 表里的可叠加物(10 / 11);bag_test 的 main 已 Load 过 ItemTable
// 并装好 item 号段基线。
// ---------------------------------------------------------------------------

namespace {

constexpr uint32_t kSettlementStackItem = 10;   // 可叠加
constexpr uint32_t kSettlementDropItem = 11;    // 可叠加
constexpr uint32_t kSettlementNonStackItem = 1; // 不可叠加(max_stack_size = 1):每件一个新实例

// 把 item 号段临时换成小区间的用例在析构时还原 bag_test 的号段基线(与 PlayerMissionWithoutSinkTest 同一基线)。
struct ScopedItemSegmentBaseline {
    ~ScopedItemSegmentBaseline() { EXPECT_TRUE(ArmSettlementMissionItemSegment(1, uint64_t{1} << 54)); }
};

class PlayerBattleSettlementItemTest : public PlayerBattleSettlementTest {
protected:
    void SetUp() override {
        PlayerBattleSettlementTest::SetUp();
        ItemTableManager::Instance().Load();
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(Guid{playerId});
        bags.bags[kInventory].ExpandCapacity(20);
    }
    Bag& Inventory() {
        return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    }
    Bag& Temporary() {
        return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kTemporary];
    }
    void GiveItem(uint32_t configId, uint32_t count) {
        InitItemParam param;
        param.itemPBComp.set_config_id(configId);
        param.itemPBComp.set_size(count);
        ASSERT_EQ(kSuccess, Inventory().AddItem(param));
    }
    std::map<Guid, std::string> ItemBytes() {
        std::map<Guid, std::string> out;
        Inventory().ForEachItem([&out](Guid guid, const ItemComp& item) { out.emplace(guid, item.SerializeAsString()); });
        return out;
    }
    void SetConsumed(uint32_t configId, uint64_t count) {
        auto* entry = settlement.add_items_consumed();
        entry->set_item_table_id(configId);
        entry->set_count(count);
    }
    void SetGained(uint32_t configId, uint64_t count) {
        auto* entry = settlement.add_items_gained();
        entry->set_item_table_id(configId);
        entry->set_count(count);
    }
};

// 整笔候选的流水不得先于空间/规则/外层货币确认；用独立号段观察失败时没有流水铸号。
class ScopedPreparedExchangeSegments {
public:
    bool Install() {
        itemEnabled_ = tlsGuidSegmentRegistry.Get(GuidKind::kItem).IsEnabled();
        txEnabled_ = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).IsEnabled();
        installed_ = true;
        return Arm(GuidKind::kItem, "item", 9100000, 9200000) && Arm(GuidKind::kTxLog, "txlog", 19100000, 19200000);
    }
    ~ScopedPreparedExchangeSegments() {
        if (!installed_) return;
        tlsGuidSegmentRegistry.Get(GuidKind::kItem).Reset();
        tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Reset();
        if (itemEnabled_) EXPECT_TRUE(Arm(GuidKind::kItem, "item", 1, uint64_t{1} << 54));
        if (txEnabled_) EXPECT_TRUE(Arm(GuidKind::kTxLog, "txlog", 1, uint64_t{1} << 54));
    }
private:
    static bool Arm(GuidKind kind, const std::string& name, uint64_t low, uint64_t high) {
        auto& client = tlsGuidSegmentRegistry.Get(kind);
        client.Reset();
        GuidSegmentClient::Options options; options.kindName = name; options.initialStep = 100000;
        if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
            [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
        client.Warm(); client.OnResponse(0, low, high);
        return client.IsReady();
    }
    bool installed_{false}, itemEnabled_{false}, txEnabled_{false};
};

TEST_F(PlayerBattleSettlementItemTest, InboxFullBagsKeepConsumptionGoldHealthAndKillsUnchanged)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    Inventory().SetCapacityForRestore(1); Temporary().SetCapacityForRestore(0);
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    SetConsumed(kSettlementStackItem, 1); SetGained(kSettlementNonStackItem, 1);
    const auto slots = Inventory().GridSlots();
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(3u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(slots, Inventory().GridSlots());
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

TEST_F(PlayerBattleSettlementItemTest, InboxCommittedConsumptionSurvivesBattleUsableHotReloadAndPreservesOtherInstances)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementDropItem, 2));
    Guid consumedGuid = 0, opaqueGuid = 0;
    Inventory().ForEachItem([&](Guid guid, const ItemComp& item) {
        if (item.config_id() == kSettlementStackItem) consumedGuid = guid;
        if (item.config_id() == kSettlementDropItem) opaqueGuid = guid;
    });
    ASSERT_NE(0u, consumedGuid); ASSERT_NE(0u, opaqueGuid);
    auto* opaque = Inventory().GetItemCompByGuid(opaqueGuid);
    opaque->GetReflection()->MutableUnknownFields(opaque)->AddVarint(900, 7);
    const auto before = ItemBytes();
    const auto slots = Inventory().GridSlots();
    auto* row = const_cast<ItemTable*>(ItemTableManager::Instance().FindByIdSilent(kSettlementStackItem).first);
    ASSERT_NE(nullptr, row);
    struct RestoreBattleUsable { ItemTable& row; uint32_t old; ~RestoreBattleUsable() { row.set_battle_usable(old); } } restore{*row, row->battle_usable()};
    row->set_battle_usable(0);
    SetConsumed(kSettlementStackItem, 1);
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    const auto after = ItemBytes();
    ASSERT_EQ(before.size(), after.size()); ASSERT_EQ(2u, after.size());
    EXPECT_TRUE(after.contains(consumedGuid)); EXPECT_TRUE(after.contains(opaqueGuid));
    EXPECT_EQ(before.at(opaqueGuid), after.at(opaqueGuid));
    ItemComp expected; ASSERT_TRUE(expected.ParseFromString(before.at(consumedGuid))); expected.set_size(2);
    EXPECT_EQ(expected.SerializeAsString(), after.at(consumedGuid));
    EXPECT_EQ(slots, Inventory().GridSlots());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold)); EXPECT_EQ(2u, KillProgress());
}

TEST_F(PlayerBattleSettlementItemTest, InboxUnknownConsumedInstanceCannotBeDowngradedToOrdinaryQuantity)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    Guid guid = 0; Inventory().ForEachItem([&guid](Guid id, const ItemComp&) { guid = id; });
    ASSERT_NE(0u, guid);
    auto* item = Inventory().GetItemCompByGuid(guid);
    item->GetReflection()->MutableUnknownFields(item)->AddVarint(900, 7);
    const auto before = ItemBytes();
    const auto slots = Inventory().GridSlots();
    SetConsumed(kSettlementStackItem, 1);
    EXPECT_EQ(PlayerInboxExecKind::kVersionSkew, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(before, ItemBytes()); EXPECT_EQ(slots, Inventory().GridSlots());
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)); EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

TEST_F(PlayerBattleSettlementItemTest, InboxKnownConsumptionStillClampsToActualHeldInstances)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 1)); SetConsumed(kSettlementStackItem, 3);
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_TRUE(ItemBytes().empty()); EXPECT_EQ(0u, Inventory().OccupiedGridCount());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold)); EXPECT_EQ(2u, KillProgress());
}

TEST_F(PlayerBattleSettlementItemTest, InboxMissionRewardFailureRetainsClaimableEntitlement)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    auto* row = const_cast<MissionTable*>(MissionTableManager::Instance().FindByIdSilent(4).first);
    ASSERT_NE(nullptr, row);
    row->set_target_count(0, 1); row->set_target_count(1, 1);
    Inventory().SetCapacityForRestore(0); Temporary().SetCapacityForRestore(0);
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    auto& missions = tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(MissionListComp::kPlayerMission);
    EXPECT_TRUE(missions.IsComplete(4)); EXPECT_TRUE(missions.IsClaimable(4));
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    Inventory().SetCapacityForRestore(20);
    EXPECT_EQ(kSuccess, PlayerMissionSystem::ClaimReward(player, MissionListComp::kPlayerMission, 4));
    EXPECT_FALSE(missions.IsClaimable(4));
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "后领不重加结算金币/击杀";
}

TEST_F(PlayerBattleSettlementItemTest, PreparedExchangeCannotConsumeOrCreditWhenBothTargetsReject)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    Inventory().SetCapacityForRestore(1); Temporary().SetCapacityForRestore(0);
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    const auto slots = Inventory().GridSlots();
    const auto txBefore = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Available();
    int credits = 0;
    const auto result = BagService::ConsumeInventoryAndGrant(player,
        {{kSettlementStackItem, 1}}, {{kSettlementNonStackItem, 1}}, 7001, "prepared-test", [&] {
            ++credits; return CurrencySystem::AddCurrency(player, kCurrencyGold, 5);
        });
    EXPECT_NE(kSuccess, result);
    EXPECT_EQ(0, credits);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(3u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(slots, Inventory().GridSlots());
    EXPECT_EQ(0u, Temporary().OccupiedGridCount());
    EXPECT_EQ(txBefore, tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Available());
}

TEST_F(PlayerBattleSettlementItemTest, PreparedExchangeUsesSpaceFreedByConsumptionBeforeGrant)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    Inventory().SetCapacityForRestore(1); Temporary().SetCapacityForRestore(0);
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 2));
    int credits = 0;
    ASSERT_EQ(kSuccess, BagService::ConsumeInventoryAndGrant(player,
        {{kSettlementStackItem, 2}}, {{kSettlementNonStackItem, 1}}, 7002, "prepared-test", [&] {
            ++credits; return CurrencySystem::AddCurrency(player, kCurrencyGold, 5);
        }));
    EXPECT_EQ(1, credits);
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(kSettlementNonStackItem));
    EXPECT_TRUE(Inventory().IsLayerConsistent());
    EXPECT_EQ(0u, Temporary().OccupiedGridCount());
}

TEST_F(PlayerBattleSettlementItemTest, PreparedExchangeCommitsConsumptionAndWholeTemporaryBatchTogether)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    Inventory().SetCapacityForRestore(1); Temporary().SetCapacityForRestore(1);
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    ASSERT_EQ(kSuccess, BagService::ConsumeInventoryAndGrant(player,
        {{kSettlementStackItem, 1}}, {{kSettlementNonStackItem, 1}}, 7003, "prepared-test"));
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kSettlementNonStackItem));
    EXPECT_EQ(1u, Temporary().GetTotalItemCount(kSettlementNonStackItem));
    EXPECT_TRUE(Inventory().IsLayerConsistent()); EXPECT_TRUE(Temporary().IsLayerConsistent());
}

TEST_F(PlayerBattleSettlementItemTest, PreparedExchangeExternalCreditRefusalPublishesNoBagOrLogs)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    const auto slots = Inventory().GridSlots();
    const auto txBefore = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Available();
    int attempts = 0;
    EXPECT_NE(kSuccess, BagService::ConsumeInventoryAndGrant(player,
        {{kSettlementStackItem, 1}}, {{kSettlementDropItem, 1}}, 7004, "prepared-test", [&] {
            ++attempts; return uint32_t{kInvalidParameter};
        }));
    EXPECT_EQ(1, attempts);
    EXPECT_EQ(3u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kSettlementDropItem));
    EXPECT_EQ(slots, Inventory().GridSlots());
    EXPECT_EQ(txBefore, tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Available());
}

TEST_F(PlayerBattleSettlementItemTest, PreparedExchangeGainBlockRejectsBeforeConsumptionOrCredit)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    struct RestoreBlock {
        bool original{GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, kSettlementDropItem)};
        RestoreBlock() { GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, kSettlementDropItem); }
        ~RestoreBlock() { if (!original) GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, kSettlementDropItem); }
    } block;
    int credits = 0;
    EXPECT_NE(kSuccess, BagService::ConsumeInventoryAndGrant(player,
        {{kSettlementStackItem, 1}}, {{kSettlementDropItem, 1}}, 7005, "prepared-test", [&] {
            ++credits; return uint32_t{kSuccess};
        }));
    EXPECT_EQ(0, credits);
    EXPECT_EQ(3u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kSettlementDropItem));
}

TEST_F(PlayerBattleSettlementItemTest, SettlementMainBagFullSendsWholeDropBatchToTemporary) {
    // 铺底使用还原入口；发物和流水各有独立号段，本例显式安装二者，不依赖前序用例状态。
    auto& segment = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    auto& txSegment = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog);
    const bool txWasEnabled = txSegment.IsEnabled();
    const auto arm = [](GuidSegmentClient& client, const std::string& kind, uint64_t lo, uint64_t hi) {
        client.Reset();
        GuidSegmentClient::Options options;
        options.kindName = kind;
        options.initialStep = 100000;
        if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
                            [](GuidSegmentClient::TimerKind, double, std::function<void()>) {},
                            [] { return 0.0; })) return false;
        client.Warm();
        client.OnResponse(0, lo, hi);
        return client.IsReady();
    };
    struct RestoreSegment {
        const decltype(arm)& restore;
        GuidSegmentClient& item;
        GuidSegmentClient& tx;
        bool txEnabled;
        ~RestoreSegment() {
            EXPECT_TRUE(restore(item, "item", 1, uint64_t{1} << 54));
            tx.Reset();
            if (txEnabled) EXPECT_TRUE(restore(tx, "txlog", 1, uint64_t{1} << 54));
        }
    } restore{arm, segment, txSegment, txWasEnabled};
    ASSERT_TRUE(arm(segment, "item", 99100000, 99200000));
    ASSERT_TRUE(arm(txSegment, "txlog", 199100000, 199200000));
    // 流水计数依赖已启用的 txlog 号段；未启用时 TryNext 不计 unavailable。
    auto& logSegment = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog);
    logSegment.Reset();
    struct ResetLogSegment {
        ~ResetLogSegment() { tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Reset(); }
    } resetLogSegment;
    GuidSegmentClient::Options logOptions;
    logOptions.kindName = "txlog";
    logOptions.initialStep = 100000;
    ASSERT_TRUE(logSegment.Enable(logOptions,
        [](const std::string&, uint32_t) { return true; },
        [](GuidSegmentClient::TimerKind, double, std::function<void()>) {},
        [] { return 0.0; }));
    logSegment.Warm();
    logSegment.OnResponse(0, 700000, 800000);
    ASSERT_TRUE(logSegment.IsReady());
    Inventory().SetCapacityForRestore(1);
    constexpr Guid oldItem = (uint64_t{1} << 60) + 9911;
    Inventory().InsertItemForRestore(oldItem, 3, 1, 0, 1);
    const auto before = Inventory().GetItemCompByGuid(oldItem)->SerializeAsString();
    auto& temporary = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kTemporary];
    ASSERT_EQ(0u, temporary.OccupiedGridCount());
    settlement.set_gold_gain(0);
    settlement.clear_defeated_monsters();
    SetGained(kSettlementStackItem, 3);
    SetGained(kSettlementDropItem, 2);
    const auto logBefore = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).GetStats();

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    ASSERT_NE(nullptr, Inventory().GetItemCompByGuid(oldItem));
    EXPECT_EQ(before, Inventory().GetItemCompByGuid(oldItem)->SerializeAsString());
    EXPECT_EQ(0u, Inventory().GetItemPosByGuid(oldItem));
    EXPECT_EQ(1u, Inventory().OccupiedGridCount());
    EXPECT_EQ(3u, temporary.GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(2u, temporary.GetTotalItemCount(kSettlementDropItem));
    EXPECT_EQ(2u, temporary.OccupiedGridCount());
    EXPECT_TRUE(Inventory().IsLayerConsistent());
    EXPECT_TRUE(temporary.IsLayerConsistent());
    const auto logAfter = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).GetStats();
    // 一条流水恰好申请一个 tx_id；无 Kafka 的测试只验证发放两项各尝试一次。
    EXPECT_EQ(2u, logAfter.issued + logAfter.unavailable - logBefore.issued - logBefore.unavailable);
}

TEST_F(PlayerBattleSettlementItemTest, ConsumedIsDeductedAndGainedIsAddedToInventory) {
    GiveItem(kSettlementStackItem, 5);
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementDropItem, 3);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 3u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementDropItem), 3u);
    // 金币照常入账:道具与金币在同一次应用里
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementItemTest, ConsumedIsClampedWhenPlayerNoLongerHoldsEnough) {
    // 战斗中途玩家的药被别处扣走了(今天已被局中闸挡住,这里验的是防守姿态):
    // 契约是"按实际持有校验扣除,不足按 0",绝不能整笔结算失败。
    GiveItem(kSettlementStackItem, 1);
    SetConsumed(kSettlementStackItem, 4);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 0u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementItemTest, ConsumedForItemPlayerDoesNotHaveDoesNotFailSettlement) {
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementDropItem, 1);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 0u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementDropItem), 1u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementItemTest, ConsumedNonBattleItemIsRejectedNotDestroyed) {
    // 反向校验:快照只会把 battle_usable 的物品放进副本,账本里出现别的 id 只可能是
    // 陈旧的 battle 节点或伪造结算。放行等于让战斗服点名销毁玩家的任意物品。
    constexpr uint32_t kNonBattleItem = 1;  // Item 表里 battle_usable = 0
    GiveItem(kNonBattleItem, 3);
    SetConsumed(kNonBattleItem, 3);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kNonBattleItem), 3u);  // 一件都没被扣
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementItemTest, RepeatedApplyDoesNotDoubleConsumeOrDoubleDrop) {
    GiveItem(kSettlementStackItem, 5);
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementDropItem, 3);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    // 同 (player, battle_id) 的重投由持久账本(BattleSettlementLedgerComp)挡掉:返回 true 并报告
    // alreadyApplied,道具不能再扣一次、也不能再发一次
    bool alreadyApplied = false;
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement, alreadyApplied));
    EXPECT_TRUE(alreadyApplied);

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 3u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementDropItem), 3u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

// 审计 cpp-economy#0(data-overwrite-rollback-audit-20261018.md §2.4):物品号段暂时发不出号。
// 修复前的走向:金币照常入账、药品照扣,主背包与临时格都因发不出号整批拒绝,只打一行 ERROR,账本照样登记
// 「已应用」;在线路径随即销账,掉落永久丢失。修复后在金币之前按掉落的新实例数上界预检号段余量:
// 不够就整笔延后、零副作用,预检本身也不耗号。号段续上后重投照常应用一次 —— 延后不是丢弃。
TEST_F(PlayerBattleSettlementItemTest, ItemGuidShortageDefersWholeSettlementBeforeGold) {
    ScopedItemSegmentBaseline restoreBaseline;
    GiveItem(kSettlementStackItem, 5);  // 铺底在换号段之前,用的是基线号段
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementNonStackItem, 3);  // 3 件不可叠加 = 3 个新实例
    ASSERT_TRUE(ArmSettlementMissionItemSegment(99300000, 99300002));  // 只剩 2 个号

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));  // 修复前 true
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);           // 修复前 12
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 5u);         // 修复前 3
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementNonStackItem), 0u);
    EXPECT_EQ(Temporary().GetTotalItemCount(kSettlementNonStackItem), 0u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    EXPECT_FALSE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
    EXPECT_EQ(gPersistCalls, 0);
    EXPECT_EQ(tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available(), 2u);

    ASSERT_TRUE(ArmSettlementMissionItemSegment(99400000, 99500000));  // 续段到达
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 3u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementNonStackItem), 3u);
    EXPECT_EQ(KillProgress(), 2u);
    EXPECT_TRUE(battle_settlement::HasApplied(
        tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), settlement.battle_id()));
}

// 预检的上界也覆盖「主背包放不下 → 整批转投临时格」:主背包在空间规划阶段就拒绝,一个号都不铸,
// 转投临时格那一次最多铸 BagService::MaxNewInstancesFor 个。号段恰好只剩这么多时必须照常落地,
// 否则预检会把本可发放的掉落误判为号段不足,让结算无限延后。
TEST_F(PlayerBattleSettlementItemTest, ExactItemGuidCapacityCoversTemporaryFallback) {
    ScopedItemSegmentBaseline restoreBaseline;
    Inventory().SetCapacityForRestore(1);
    constexpr Guid oldItem = (uint64_t{1} << 60) + 9912;
    Inventory().InsertItemForRestore(oldItem, 3, 1, 0, 1);
    settlement.clear_defeated_monsters();
    SetGained(kSettlementStackItem, 3);  // 可叠加,各占 1 个新实例
    SetGained(kSettlementDropItem, 2);
    ASSERT_TRUE(ArmSettlementMissionItemSegment(99600000, 99600002));  // 恰好 2 个号

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(1u, Inventory().OccupiedGridCount());
    EXPECT_EQ(3u, Temporary().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(2u, Temporary().GetTotalItemCount(kSettlementDropItem));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available(), 0u);
}

// 上界口径:不可叠加按件计,可叠加按满堆向上取整;查不到配置行的项按 0 计
// (整批会因配置错误被拒、一个号都不铸,与号段余量无关,不能因此把结算挡成永久延后)。
TEST(BagServiceNewInstanceBound, CountsStacksPerConfigAndIgnoresUnknownRows) {
    ItemTableManager::Instance().Load();
    EXPECT_EQ(0u, BagService::MaxNewInstancesFor(ItemCountMap{}));
    EXPECT_EQ(3u, BagService::MaxNewInstancesFor(ItemCountMap{{kSettlementNonStackItem, 3}}));
    EXPECT_EQ(1u, BagService::MaxNewInstancesFor(ItemCountMap{{kSettlementStackItem, 999}}));
    EXPECT_EQ(2u, BagService::MaxNewInstancesFor(ItemCountMap{{kSettlementStackItem, 1000}}));
    EXPECT_EQ(4u, BagService::MaxNewInstancesFor(
        ItemCountMap{{kSettlementNonStackItem, 3}, {kSettlementDropItem, 5}}));
    EXPECT_EQ(0u, BagService::MaxNewInstancesFor(ItemCountMap{{999999999u, 7}}));
}

}  // namespace

// ---------------------------------------------------------------------------
// 2026-10-14 landing-recovery-sync-digest.md §7:待结算记录去掉到期删除 + 积压索引。
// 读写 pending 的 Lua 由 battle 与 scene 两端共用(settlement_outbox.h)。
// ACK 直接在专用 Redis 执行生产脚本验证原子销账；其余静态契约钉住无到期删除及键位。
// 孤儿清理的 EXISTS 与 ZREM 在同一条脚本里。键位与 battle_room_manager.cpp / player_battle.cpp 的
// EVAL 参数顺序一一对应,改脚本键位时这里会先红。
// ---------------------------------------------------------------------------

namespace {

bool ScriptContains(const char* script, const std::string& needle) {
    return std::string(script).find(needle) != std::string::npos;
}

TEST(PendingSettlementContract, WritesCarryNoExpiry) {
    for (const char* script : {battle_settlement::kSetPendingSettlementScript,
                               battle_settlement::kStorePendingSettlementIfLockMatchScript}) {
        EXPECT_FALSE(ScriptContains(script, "'EX'")) << script;
        EXPECT_FALSE(ScriptContains(script, "'PX'")) << script;
        EXPECT_FALSE(ScriptContains(script, "EXPIRE")) << script;
        // KEEPTTL 会把旧版本写入留下的 7 天 TTL 原样保住,等于没去掉。
        EXPECT_FALSE(ScriptContains(script, "KEEPTTL")) << script;
    }
}

// KEYS[3]=索引,ARGV[3]=now_ms,ARGV[4]=player_id(DispatchSettlementDurably 的 EVAL 参数顺序)。
TEST(PendingSettlementContract, BattleWriteJoinsBacklogIndexInSameScript) {
    EXPECT_TRUE(ScriptContains(battle_settlement::kSetPendingSettlementScript,
                               "redis.call('ZADD', KEYS[3], ARGV[3], ARGV[4])"));
}

// KEYS[4]=索引(StorePendingSettlementIfLockMatch 的 EVAL 参数顺序)。锁不在 / 属于别的局时提前返回,
// 什么都不写,当然也不能进索引:ZADD 必须排在两处提前返回之后。
TEST(PendingSettlementContract, SceneStagingJoinsBacklogIndexOnlyWhenWritten) {
    const std::string script = battle_settlement::kStorePendingSettlementIfLockMatchScript;
    const auto zadd = script.find("redis.call('ZADD', KEYS[4], ARGV[3], ARGV[4])");
    ASSERT_NE(zadd, std::string::npos);
    ASSERT_NE(script.find("return 0"), std::string::npos);
    ASSERT_NE(script.find("return 2"), std::string::npos);
    EXPECT_GT(zadd, script.find("return 0"));
    EXPECT_GT(zadd, script.find("return 2"));
}

namespace {
struct SettlementReplyDeleter {
    void operator()(redisReply* reply) const { freeReplyObject(reply); }
};

struct SettlementRedisFixture {
    using Reply = std::unique_ptr<redisReply, SettlementReplyDeleter>;
    std::unique_ptr<redisContext, decltype(&redisFree)> context{nullptr, &redisFree};
    // 0=pending blob,1=pending:id,2=battle:lock,3=battle:ctx,4=积压索引(kAckSettlementScript 的 KEYS 顺序)。
    std::array<std::string, 5> keys;
    // 确认模式落库标记,每局一个(kStagedSettlementMembersKeyFmt;审计 r2-battle#1)。
    std::array<std::string, 3> markers;

    SettlementRedisFixture() {
        const auto nonce = std::chrono::steady_clock::now().time_since_epoch().count();
        const auto prefix = "test:{settlement-ack-" + std::to_string(nonce) + "}:";
        for (std::size_t index = 0; index < keys.size(); ++index)
            keys[index] = prefix + std::to_string(index);
        for (std::size_t index = 0; index < markers.size(); ++index)
            markers[index] = prefix + "staged:" + std::to_string(index);
    }
    ~SettlementRedisFixture() {
        if (context == nullptr || context->err != 0) return;
        std::vector<std::string> command{"DEL"};
        command.insert(command.end(), keys.begin(), keys.end());
        command.insert(command.end(), markers.begin(), markers.end());
        (void)Run(command);
    }
    // address = "host:port"(MMORPG_TEST_REDIS_ADDR)。
    void Connect(const std::string& endpoint) {
        const auto colon = endpoint.rfind(':');
        ASSERT_NE(std::string::npos, colon);
        ASSERT_GT(colon, 0u);
        char* tail = nullptr;
        const long port = std::strtol(endpoint.c_str() + colon + 1, &tail, 10);
        ASSERT_NE(nullptr, tail);
        ASSERT_EQ('\0', *tail);
        ASSERT_GT(port, 0);
        ASSERT_LE(port, 65535);
        const timeval timeout{1, 0};
        context.reset(redisConnectWithTimeout(endpoint.substr(0, colon).c_str(), static_cast<int>(port), timeout));
        ASSERT_NE(nullptr, context);
        ASSERT_EQ(0, context->err);
        ASSERT_EQ(REDIS_OK, redisSetTimeout(context.get(), timeout));
    }
    // 生产 StagePersonalSettlements 的 EVAL 参数顺序:blob、id、积压索引、锁、标记;blob、battle、now、player、TTL。
    Reply Stage(const std::string& blob, const std::string& battle, std::size_t marker) {
        return Run({"EVAL", battle_settlement::kStagePendingSettlementOnceScript, "5",
                    keys[0], keys[1], keys[4], keys[2], markers.at(marker),
                    blob, battle, "1", "player",
                    std::to_string(battle_settlement::kStagedSettlementMembersTtlSec)});
    }
    Reply Run(const std::vector<std::string>& command) {
        std::vector<const char*> arguments;
        std::vector<std::size_t> lengths;
        for (const auto& argument : command) {
            arguments.push_back(argument.data());
            lengths.push_back(argument.size());
        }
        return Reply(static_cast<redisReply*>(redisCommandArgv(context.get(),
            static_cast<int>(arguments.size()), arguments.data(), lengths.data())));
    }
    Reply Ack(const std::string& battle) {
        std::vector<std::string> command{"EVAL", battle_settlement::kAckSettlementScript, "5"};
        command.insert(command.end(), keys.begin(), keys.end());
        command.insert(command.end(), {battle, "player"});
        return Run(command);
    }
};
}

TEST(PendingSettlementContract, AckRemovesIndexMemberOnlyTogetherWithPending) {
    const char* address = std::getenv("MMORPG_TEST_REDIS_ADDR");
    if (address == nullptr || *address == '\0')
        GTEST_SKIP() << "未设置 MMORPG_TEST_REDIS_ADDR，跳过真实 Redis ACK 契约";
    const std::string endpoint(address);
    const auto colon = endpoint.rfind(':');
    ASSERT_NE(std::string::npos, colon);
    ASSERT_GT(colon, 0u);
    char* tail = nullptr;
    const long port = std::strtol(endpoint.c_str() + colon + 1, &tail, 10);
    ASSERT_NE(nullptr, tail);
    ASSERT_EQ('\0', *tail);
    ASSERT_GT(port, 0);
    ASSERT_LE(port, 65535);
    SettlementRedisFixture redis;
    const timeval timeout{1, 0};
    redis.context.reset(redisConnectWithTimeout(endpoint.substr(0, colon).c_str(), static_cast<int>(port), timeout));
    ASSERT_NE(nullptr, redis.context);
    ASSERT_EQ(0, redis.context->err);
    ASSERT_EQ(REDIS_OK, redisSetTimeout(redis.context.get(), timeout));

    const auto expectOk = [&redis](const std::vector<std::string>& command) {
        const auto reply = redis.Run(command);
        ASSERT_NE(nullptr, reply);
        ASSERT_NE(REDIS_REPLY_ERROR, reply->type);
    };
    const auto expectValue = [&redis](std::size_t index, const std::string& expected) {
        const auto reply = redis.Run({"GET", redis.keys[index]});
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_STRING, reply->type);
        EXPECT_EQ(expected, std::string(reply->str, reply->len));
    };
    const auto expectAck = [&redis](const std::string& battle, long long expected) {
        const auto reply = redis.Ack(battle);
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_INTEGER, reply->type);
        EXPECT_EQ(expected, reply->integer);
    };
    const auto prepare = [&] {
        const std::array<std::string, 4> values{"blob", "current", "current", "binding"};
        for (std::size_t index = 0; index < values.size(); ++index)
            ASSERT_NO_FATAL_FAILURE(expectOk({"SET", redis.keys[index], values[index]}));
        ASSERT_NO_FATAL_FAILURE(expectOk({"ZADD", redis.keys[4], "1", "player"}));
    };

    ASSERT_NO_FATAL_FAILURE(prepare());
    ASSERT_NO_FATAL_FAILURE(expectAck("old", 0));
    ASSERT_NO_FATAL_FAILURE(expectValue(0, "blob"));
    ASSERT_NO_FATAL_FAILURE(expectValue(1, "current"));
    ASSERT_NO_FATAL_FAILURE(expectValue(2, "current"));
    ASSERT_NO_FATAL_FAILURE(expectValue(3, "binding"));
    auto member = redis.Run({"ZSCORE", redis.keys[4], "player"});
    ASSERT_NE(nullptr, member);
    EXPECT_EQ(REDIS_REPLY_STRING, member->type);

    // 锁属于旧局但 pending 属于新局：只能放旧锁，不能摘掉新局索引。
    ASSERT_NO_FATAL_FAILURE(expectOk({"SET", redis.keys[2], "old"}));
    ASSERT_NO_FATAL_FAILURE(expectAck("old", 2));
    ASSERT_NO_FATAL_FAILURE(expectValue(0, "blob"));
    ASSERT_NO_FATAL_FAILURE(expectValue(1, "current"));
    member = redis.Run({"ZSCORE", redis.keys[4], "player"});
    ASSERT_NE(nullptr, member);
    EXPECT_EQ(REDIS_REPLY_STRING, member->type);

    ASSERT_NO_FATAL_FAILURE(prepare());
    ASSERT_NO_FATAL_FAILURE(expectAck("current", 3));
    for (const auto& key : redis.keys) {
        const auto reply = redis.Run({"EXISTS", key});
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_INTEGER, reply->type);
        EXPECT_EQ(0, reply->integer);
    }
    ASSERT_NO_FATAL_FAILURE(expectAck("current", 0));
    // 旧回执重投不能删除之后新建的 pending、锁及索引。
    ASSERT_NO_FATAL_FAILURE(prepare());
    ASSERT_NO_FATAL_FAILURE(expectAck("old", 0));
    ASSERT_NO_FATAL_FAILURE(expectValue(0, "blob"));
    ASSERT_NO_FATAL_FAILURE(expectValue(2, "current"));
}

// ---------------------------------------------------------------------------
// 2026-10-19 审计 r2-battle#1(data-overwrite-rollback-audit-r2-20261019.md §3.3):确认模式(全球对局)
// 发结果之前的个人结算落库 kStagePendingSettlementOnceScript。
// ---------------------------------------------------------------------------

namespace {
void ExpectInteger(const SettlementRedisFixture::Reply& reply, long long expected) {
    ASSERT_NE(nullptr, reply);
    ASSERT_EQ(REDIS_REPLY_INTEGER, reply->type);
    EXPECT_EQ(expected, reply->integer);
}

void ExpectString(const SettlementRedisFixture::Reply& reply, const std::string& expected) {
    ASSERT_NE(nullptr, reply);
    ASSERT_EQ(REDIS_REPLY_STRING, reply->type);
    EXPECT_EQ(expected, std::string(reply->str, reply->len));
}

void ExpectNil(const SettlementRedisFixture::Reply& reply) {
    ASSERT_NE(nullptr, reply);
    EXPECT_EQ(REDIS_REPLY_NIL, reply->type);
}
}  // namespace

// 真实 Redis 上复现缺陷时序:第 1 轮给 A 落库,命令执行了但应答丢了(空应答 / ERROR),整轮判失败;
// 退避期间 A 的 scene 读到 pending、应用、落盘,销账脚本删掉 pending 与锁、回调摘掉账本条目;第 2 轮再写 A。
// 修复前第 2 轮是无条件 SET,pending 复活,A 下次登录时登录钩子读到它、账本里没有这一局,整笔再发一次。
// 修复后第 2 轮读到本局落库标记返回 2,什么都不写。
TEST(PendingSettlementContract, ConfirmedStagingRetryNeverRevivesAcknowledgedPending) {
    const char* address = std::getenv("MMORPG_TEST_REDIS_ADDR");
    if (address == nullptr || *address == '\0')
        GTEST_SKIP() << "未设置 MMORPG_TEST_REDIS_ADDR，跳过真实 Redis 落库契约";
    SettlementRedisFixture redis;
    ASSERT_NO_FATAL_FAILURE(redis.Connect(address));

    // PrepareBattle 写的战斗锁(值 = battle_id)。
    {
        const auto reply = redis.Run({"SET", redis.keys[2], "851"});
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_STATUS, reply->type);
    }
    // 第 1 轮:脚本执行了(调用方没拿到应答,只能当「结论未知」)。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-851", "851", 0), 1));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[0]}), "blob-851"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[1]}), "851"));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"SISMEMBER", redis.markers[0], "player"}), 1));
    {
        const auto ttl = redis.Run({"TTL", redis.markers[0]});
        ASSERT_NE(nullptr, ttl);
        ASSERT_EQ(REDIS_REPLY_INTEGER, ttl->type);
        EXPECT_GT(ttl->integer, 0);
        EXPECT_LE(ttl->integer, static_cast<long long>(battle_settlement::kStagedSettlementMembersTtlSec));
    }
    // pending 两键不带 TTL(landing-recovery §7)。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"TTL", redis.keys[0]}), -1));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"TTL", redis.keys[1]}), -1));

    // scene 应用并落盘后销账:pending 两键、索引成员与锁一起删(返回位掩码 1|2)。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Ack("851"), 3));

    // 第 2 轮重写 A:标记证明本局写过,不重写。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-851", "851", 0), 2));
    ASSERT_NO_FATAL_FAILURE(ExpectNil(redis.Run({"GET", redis.keys[0]})));  // 修复前:pending 复活
    ASSERT_NO_FATAL_FAILURE(ExpectNil(redis.Run({"GET", redis.keys[1]})));
    ASSERT_NO_FATAL_FAILURE(ExpectNil(redis.Run({"ZSCORE", redis.keys[4], "player"})));
}

// 锁在落库之前就可能合法缺席(续期没落地时战斗中途自然过期、整场期限到时 scene reaper 先删):
// 锁不在时照写,不能当成「已收尾」丢掉合法结算。锁属于别的局时不写:那一局的单槽 pending 不能被旧局覆盖。
TEST(PendingSettlementContract, ConfirmedStagingWritesWithoutLockButNeverOverAnotherBattle) {
    const char* address = std::getenv("MMORPG_TEST_REDIS_ADDR");
    if (address == nullptr || *address == '\0')
        GTEST_SKIP() << "未设置 MMORPG_TEST_REDIS_ADDR，跳过真实 Redis 落库契约";
    SettlementRedisFixture redis;
    ASSERT_NO_FATAL_FAILURE(redis.Connect(address));

    // 锁不在:照写。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-850", "850", 0), 1));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[1]}), "850"));

    // 下一局 852 拿到锁并落了自己的 pending。
    for (const auto& [index, value] : std::vector<std::pair<std::size_t, std::string>>{
             {2, "852"}, {0, "blob-852"}, {1, "852"}}) {
        const auto reply = redis.Run({"SET", redis.keys[index], value});
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_STATUS, reply->type);
    }
    // 另一局 849 此刻才落库(从没写过):锁属于 852,返回 3,不写、不进标记。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-849", "849", 1), 3));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[0]}), "blob-852"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[1]}), "852"));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"EXISTS", redis.markers[1]}), 0));

    // 锁属于本局:照写(852 自己的重试轮,第一次写)。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-852-v", "852", 2), 1));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[0]}), "blob-852-v"));
}

// 2026-10-22 评审补(r2-battle#1/#4 评审 must_fix 第 1 条):锁不在时还要看 pending 槽属于哪一局。
// 复现时序:本局 850 落库时成员一直没写进去,他的冻结在 850 截止时解除,接着打完 999 并下线;999 的 pending 两键
// 已写、还没消费,锁 999 已按 TTL 过期(这里不写锁)。850 的低频重试轮恢复后再写这名成员:
// 修复前锁不在就照写,999 的 pending 被覆盖,999 的发件箱探测到 pending:id ≠ 999 按已销账停投,999 的奖励永久丢失;
// 修复后返回 3,blob、id、索引成员都不动,标记里也没有该成员。
// 锁 == 本局时照旧覆盖(锁证明本局是当前局,槽里的只会更旧);锁不在而槽已属于本局时照写。
TEST(PendingSettlementContract, ConfirmedStagingWithoutLockNeverOverwritesAnotherBattlesPending) {
    const char* address = std::getenv("MMORPG_TEST_REDIS_ADDR");
    if (address == nullptr || *address == '\0')
        GTEST_SKIP() << "未设置 MMORPG_TEST_REDIS_ADDR，跳过真实 Redis 落库契约";
    SettlementRedisFixture redis;
    ASSERT_NO_FATAL_FAILURE(redis.Connect(address));

    // 另一局 999 已落的 pending 两键与索引成员;锁已过期。
    for (const auto& [index, value] : std::vector<std::pair<std::size_t, std::string>>{
             {0, "blob-999"}, {1, "999"}}) {
        const auto reply = redis.Run({"SET", redis.keys[index], value});
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_STATUS, reply->type);
    }
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"ZADD", redis.keys[4], "7", "player"}), 1));

    // 850 迟到的一轮:锁不在、槽属于 999 → 返回 3,什么都不写。修复前这里返回 1,下面三条断言全红。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-850", "850", 0), 3));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[0]}), "blob-999"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[1]}), "999"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"ZSCORE", redis.keys[4], "player"}), "7"));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"EXISTS", redis.markers[0]}), 0));
    ASSERT_NO_FATAL_FAILURE(ExpectNil(redis.Run({"GET", redis.keys[2]})));

    // 锁属于本局:照旧覆盖,槽里的 999 只能是更旧的一局。
    {
        const auto reply = redis.Run({"SET", redis.keys[2], "850"});
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_STATUS, reply->type);
    }
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-850", "850", 1), 1));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[0]}), "blob-850"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[1]}), "850"));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"SISMEMBER", redis.markers[1], "player"}), 1));

    // 锁不在、槽已属于本局(例如 scene 离线暂存写过同一份):照写,不误判为别的局。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"DEL", redis.keys[2]}), 1));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-850", "850", 2), 1));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[1]}), "850"));
}

// 2026-10-22 补:脚本「ZADD 放最后」的运行证据(此前只有下一条的静态顺序断言)。
// 积压索引被写坏(键类型错)时 ZADD 报 WRONGTYPE、脚本回 ERROR,但 Redis 脚本不回滚,pending 两键与本局落库标记
// 已经写下。battle 把这一轮记作「结论未知」,下一轮重写该成员时读到标记返回 2:不重写、pending 不变,落库照常收口。
// 索引只影响报数。
TEST(PendingSettlementContract, ConfirmedStagingPartialWriteOnBrokenIndexSettlesNextRound) {
    const char* address = std::getenv("MMORPG_TEST_REDIS_ADDR");
    if (address == nullptr || *address == '\0')
        GTEST_SKIP() << "未设置 MMORPG_TEST_REDIS_ADDR，跳过真实 Redis 落库契约";
    SettlementRedisFixture redis;
    ASSERT_NO_FATAL_FAILURE(redis.Connect(address));

    // 积压索引被写成字符串(运维误操作;冒烟时人为制造)。
    {
        const auto reply = redis.Run({"SET", redis.keys[4], "not-a-zset"});
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_STATUS, reply->type);
    }
    // 第 1 轮:最后一步 ZADD 报 WRONGTYPE,调用方只看到 ERROR(ClassifyStageReply → 未确认)。
    {
        const auto reply = redis.Stage("blob-853", "853", 0);
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_ERROR, reply->type);
        EXPECT_NE(std::string::npos, std::string(reply->str, reply->len).find("WRONGTYPE"));
    }
    // 它之前的写都已落地:pending 两键与标记。
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[0]}), "blob-853"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[1]}), "853"));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"SISMEMBER", redis.markers[0], "player"}), 1));
    // 第 2 轮重写该成员:读到标记返回 2。这里故意换一份字节,好让「没重写」看得见(生产里两轮字节相同)。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Stage("blob-853-retry", "853", 0), 2));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[0]}), "blob-853"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[1]}), "853"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.keys[4]}), "not-a-zset"));
}

// 判定都在第一条写之前;标记写在两键之后;ZADD 最后;EXPIRE 只作用在标记上;锁不在时只因槽位属于别的局而返回。
// 键位与 battle_room_manager.cpp StagePersonalSettlements 的 EVAL 参数顺序一一对应,改脚本时这里先红。
TEST(PendingSettlementContract, ConfirmedStagingDecidesBeforeAnyWriteAndMarksAfterPending) {
    const std::string script = battle_settlement::kStagePendingSettlementOnceScript;
    const auto marker = script.find("if redis.call('SISMEMBER', KEYS[5], ARGV[4]) == 1 then return 2 end");
    const auto lock = script.find("if lock and lock ~= ARGV[2] then return 3 end");
    // 2026-10-22 评审补:锁不在时看 pending 槽归属(KEYS[2] = pending:id),属于别的局就不写。
    const auto ownerRead = script.find("local owner = redis.call('GET', KEYS[2])");
    const auto slotGuard = script.find("if lock == false and owner and owner ~= ARGV[2] then return 3 end");
    const auto blobSet = script.find("redis.call('SET', KEYS[1], ARGV[1])");
    const auto idSet = script.find("redis.call('SET', KEYS[2], ARGV[2])");
    const auto sadd = script.find("redis.call('SADD', KEYS[5], ARGV[4])");
    const auto expire = script.find("redis.call('EXPIRE', KEYS[5], ARGV[5])");
    const auto zadd = script.find("redis.call('ZADD', KEYS[3], ARGV[3], ARGV[4])");
    for (const auto position : {marker, lock, ownerRead, slotGuard, blobSet, idSet, sadd, expire, zadd}) {
        ASSERT_NE(std::string::npos, position);
    }
    EXPECT_LT(marker, lock);
    EXPECT_LT(lock, blobSet);
    // 槽位守卫必须早于第一条写:晚了就是先覆盖别的局再返回 3。
    EXPECT_LT(lock, ownerRead);
    EXPECT_LT(ownerRead, slotGuard);
    EXPECT_LT(slotGuard, blobSet);
    EXPECT_LT(slotGuard, idSet);
    EXPECT_LT(blobSet, sadd);
    EXPECT_LT(idSet, sadd);
    EXPECT_LT(sadd, zadd);
    EXPECT_LT(expire, zadd);
    // 锁不在不能一律返回:不能出现「锁不在就返回」的分支(审计建议的纯锁纪律在全球模式下会静默丢结算)。
    // 锁不在时唯一的返回是上面的槽位守卫,它同时要求槽里有别的局(owner 存在且不等于本局)。
    EXPECT_EQ(std::string::npos, script.find("if not lock"));
    EXPECT_EQ(script.find("return 3"), lock + std::string("if lock and lock ~= ARGV[2] then ").size());
    EXPECT_EQ(script.rfind("return 3"), slotGuard + std::string("if lock == false and owner and owner ~= ARGV[2] then ").size());
    // pending 两键不过期:EXPIRE 只出现一次(标记),也不带 EX / PX / KEEPTTL。
    EXPECT_EQ(script.find("EXPIRE"), script.rfind("EXPIRE"));
    EXPECT_FALSE(ScriptContains(battle_settlement::kStagePendingSettlementOnceScript, "'EX'"));
    EXPECT_FALSE(ScriptContains(battle_settlement::kStagePendingSettlementOnceScript, "'PX'"));
    EXPECT_FALSE(ScriptContains(battle_settlement::kStagePendingSettlementOnceScript, "KEEPTTL"));
}

TEST(PendingSettlementContract, ConfirmedStagingKeysAreFormatSafeAndDistinct) {
    EXPECT_STREQ("battle:lock:%llu", battle_settlement::kBattleLockKeyFmt);
    const std::string marker = battle_settlement::kStagedSettlementMembersKeyFmt;
    EXPECT_EQ(std::string::npos, marker.find(' '));
    ASSERT_NE(std::string::npos, marker.find("%llu"));
    EXPECT_EQ(marker.find('%'), marker.rfind('%'));  // 只有一个格式说明符(battle_id)
    char staged[128];
    char pending[128];
    char pendingId[128];
    for (const unsigned long long id : {1ull, 851ull, std::numeric_limits<unsigned long long>::max()}) {
        std::snprintf(staged, sizeof(staged), battle_settlement::kStagedSettlementMembersKeyFmt, id);
        std::snprintf(pending, sizeof(pending), battle_settlement::kPendingSettlementKeyFmt, id);
        std::snprintf(pendingId, sizeof(pendingId), battle_settlement::kPendingSettlementIdKeyFmt, id);
        EXPECT_STRNE(staged, pending);
        EXPECT_STRNE(staged, pendingId);
        EXPECT_STRNE(staged, battle_settlement::kPendingSettlementIndexKey);
    }
    // 标记必须活过整个落库阶段。2026-10-22 起与 ResultCommit 总跨度(6 小时)的 4 倍余量关系由
    // turn_battle_engine_test 的 StagingSpanLeavesStagedMarkerTtlMargin 与 battle_room_manager.cpp 的 static_assert 守住,
    // 这里只留一条与 battle 节点无关的下限。
    EXPECT_GE(battle_settlement::kStagedSettlementMembersTtlSec, 3600u);
}

// 只有脚本的三种整数结论算落定;空应答、ERROR、意外形状或取值都按未确认,下一轮重写该成员。
TEST(PendingSettlementContract, ConfirmedStagingReplyClassification) {
    using battle_settlement::ClassifyStageReply;
    using battle_settlement::ReplyShape;
    using battle_settlement::StageVerdict;
    EXPECT_EQ(StageVerdict::kStored, ClassifyStageReply(ReplyShape::kInteger, 1));
    EXPECT_EQ(StageVerdict::kAlreadyStored, ClassifyStageReply(ReplyShape::kInteger, 2));
    EXPECT_EQ(StageVerdict::kSuperseded, ClassifyStageReply(ReplyShape::kInteger, 3));
    for (const int64_t unexpected : {int64_t{0}, int64_t{4}, int64_t{-1}}) {
        EXPECT_EQ(StageVerdict::kUnconfirmed, ClassifyStageReply(ReplyShape::kInteger, unexpected)) << unexpected;
    }
    for (const auto shape : {ReplyShape::kNoReply, ReplyShape::kError, ReplyShape::kNil, ReplyShape::kString,
                             ReplyShape::kStatus, ReplyShape::kArray, ReplyShape::kOther}) {
        // integer 只在 kInteger 时有意义:别的形状即使带 1 也不算写入。
        EXPECT_EQ(StageVerdict::kUnconfirmed, ClassifyStageReply(shape, 1)) << battle_settlement::ReplyShapeName(shape);
    }
    EXPECT_TRUE(battle_settlement::IsStageSettled(StageVerdict::kStored));
    EXPECT_TRUE(battle_settlement::IsStageSettled(StageVerdict::kAlreadyStored));
    EXPECT_TRUE(battle_settlement::IsStageSettled(StageVerdict::kSuperseded));
    EXPECT_FALSE(battle_settlement::IsStageSettled(StageVerdict::kUnconfirmed));
}

TEST(PendingSettlementContract, CorruptRecordCleanupAlsoRemovesIndexMember) {
    EXPECT_TRUE(ScriptContains(battle_settlement::kDeleteCorruptPendingSettlementScript,
                               "redis.call('DEL', KEYS[1], KEYS[2])"));
    EXPECT_TRUE(ScriptContains(battle_settlement::kDeleteCorruptPendingSettlementScript,
                               "redis.call('ZREM', KEYS[3], ARGV[1])"));
}

// 孤儿清理:pending:id 不在才 ZREM,判定与删除在同一条脚本里(否则两步之间写入的新一局会被误摘)。
TEST(PendingSettlementContract, OrphanSweepRemovesOnlyWhenRecordIsGone) {
    const std::string script = battle_settlement::kSweepPendingIndexOrphanScript;
    const auto exists = script.find("if redis.call('EXISTS', KEYS[2]) == 0 then");
    const auto zrem = script.find("redis.call('ZREM', KEYS[1], ARGV[1])");
    ASSERT_NE(exists, std::string::npos);
    ASSERT_NE(zrem, std::string::npos);
    EXPECT_LT(exists, zrem);
}

TEST(PendingSettlementContract, BacklogReadIsReadOnly) {
    for (const char* write : {"'ZREM'", "'ZADD'", "'DEL'", "'SET'"}) {
        EXPECT_FALSE(ScriptContains(battle_settlement::kReadPendingBacklogScript, write)) << write;
    }
    EXPECT_TRUE(ScriptContains(battle_settlement::kReadPendingBacklogScript, "'ZCARD'"));
}

// 索引键直接拼进 hiredis 的格式串(与 %llu 键格式同一种用法):不能含 '%'(会被当成格式说明符)、
// 不能含空白(hiredis 按空白切参数);也不能被按玩家的两个键格式撞上。
TEST(PendingSettlementContract, IndexKeyIsFormatSafeAndDistinctFromPerPlayerKeys) {
    const std::string index = battle_settlement::kPendingSettlementIndexKey;
    EXPECT_EQ(index, "battle:settlement:pending:index");
    EXPECT_EQ(index.find('%'), std::string::npos);
    EXPECT_EQ(index.find(' '), std::string::npos);
    EXPECT_NE(index.rfind("battle:settlement:pending:id:", 0), 0u);
    char key[128];
    for (const unsigned long long id : {1ull, 9876501ull, std::numeric_limits<unsigned long long>::max()}) {
        std::snprintf(key, sizeof(key), battle_settlement::kPendingSettlementKeyFmt, id);
        EXPECT_NE(index, key);
        std::snprintf(key, sizeof(key), battle_settlement::kPendingSettlementIdKeyFmt, id);
        EXPECT_NE(index, key);
    }
}

// 用尽日志的 reason 只看用尽那一轮能不能解析到位置。
TEST(SettlementOutboxUndelivered, ReasonFollowsTheExhaustingRound) {
    battle_settlement::RetryInput input;
    input.pendingRecordStillOurs = true;
    input.attemptsSoFar = 12;
    input.maxAttempts = 12;

    input.locationResolved = false;
    ASSERT_EQ(battle_settlement::ClassifyRetry(input), battle_settlement::RetryAction::kExhausted);
    EXPECT_EQ(battle_settlement::ClassifyUndelivered(input), battle_settlement::UndeliveredReason::kNoTarget);

    input.locationResolved = true;
    ASSERT_EQ(battle_settlement::ClassifyRetry(input), battle_settlement::RetryAction::kExhausted);
    EXPECT_EQ(battle_settlement::ClassifyUndelivered(input), battle_settlement::UndeliveredReason::kNotAcked);

    EXPECT_STREQ(battle_settlement::UndeliveredReasonName(battle_settlement::UndeliveredReason::kNoTarget), "no_target");
    EXPECT_STREQ(battle_settlement::UndeliveredReasonName(battle_settlement::UndeliveredReason::kNotAcked), "not_acked");
}

TEST(SettlementBacklog, OldestAgeIsZeroForEmptyIndexOrSkewedClock) {
    EXPECT_EQ(battle_settlement::PendingBacklogOldestAgeSec(0, 1'000'000), 0u);       // 索引为空
    EXPECT_EQ(battle_settlement::PendingBacklogOldestAgeSec(2'000, 1'000), 0u);       // 写入方墙钟快于报数方
    EXPECT_EQ(battle_settlement::PendingBacklogOldestAgeSec(1'000, 1'000), 0u);
    EXPECT_EQ(battle_settlement::PendingBacklogOldestAgeSec(1'000, 62'999), 61u);     // 向下取整到秒
    // 去掉 TTL 之后年龄可以远超原来的 7 天保留期,不能回绕。
    constexpr uint64_t kWrittenAtMs = 1'700'000'000'000ull;
    constexpr uint64_t kThirtyWeeksSec = 30ull * 7 * 24 * 3600;
    EXPECT_EQ(battle_settlement::PendingBacklogOldestAgeSec(kWrittenAtMs, kWrittenAtMs + kThirtyWeeksSec * 1000),
              kThirtyWeeksSec);
}

// 头部与轮转窗口交替;轮转窗口满批后移、不满批(扫到底)绕回 0。
TEST(SettlementBacklog, SweepAlternatesHeadAndRotatingWindowAndWraps) {
    constexpr uint32_t kBatch = 10;
    battle_settlement::PendingIndexSweepCursor cursor;
    EXPECT_EQ(battle_settlement::PendingIndexSweepStart(cursor), 0u);   // 第 0 轮:头部
    battle_settlement::AdvancePendingIndexSweep(cursor, 10, kBatch);
    EXPECT_EQ(battle_settlement::PendingIndexSweepStart(cursor), 0u);   // 第 1 轮:轮转窗口从 0 起
    battle_settlement::AdvancePendingIndexSweep(cursor, 10, kBatch);    // 满批:轮转窗口后移一批
    EXPECT_EQ(battle_settlement::PendingIndexSweepStart(cursor), 0u);   // 第 2 轮:头部
    battle_settlement::AdvancePendingIndexSweep(cursor, 10, kBatch);
    EXPECT_EQ(battle_settlement::PendingIndexSweepStart(cursor), 10u);  // 第 3 轮:轮转窗口 [10, 20)
    battle_settlement::AdvancePendingIndexSweep(cursor, 4, kBatch);     // 不满批:扫到底,绕回 0
    EXPECT_EQ(battle_settlement::PendingIndexSweepStart(cursor), 0u);   // 第 4 轮:头部
    battle_settlement::AdvancePendingIndexSweep(cursor, 10, kBatch);
    EXPECT_EQ(battle_settlement::PendingIndexSweepStart(cursor), 0u);   // 第 5 轮:轮转窗口已绕回
}

// 只查头部会卡死:打完不再上线的玩家让有效记录永久占住头部,后面的孤儿永远查不到。
// 轮转窗口必须在有限轮内走遍每个排名。
TEST(SettlementBacklog, RotatingWindowEventuallyCoversWholeIndex) {
    constexpr uint64_t kIndexSize = 95;
    constexpr uint32_t kBatch = 10;
    battle_settlement::PendingIndexSweepCursor cursor;
    std::vector<bool> covered(kIndexSize, false);
    for (int round = 0; round < 40; ++round) {
        const uint64_t start = battle_settlement::PendingIndexSweepStart(cursor);
        const uint64_t returned = start >= kIndexSize ? 0 : std::min<uint64_t>(kBatch, kIndexSize - start);
        for (uint64_t rank = start; rank < start + returned; ++rank) covered[rank] = true;
        battle_settlement::AdvancePendingIndexSweep(cursor, returned, kBatch);
    }
    for (uint64_t rank = 0; rank < kIndexSize; ++rank) {
        EXPECT_TRUE(covered[rank]) << "rank " << rank;
    }
}

TEST(SettlementBacklog, ZeroBatchLeavesCursorUntouched) {
    battle_settlement::PendingIndexSweepCursor cursor;
    cursor.round = 3;
    cursor.rotatingStart = 20;
    battle_settlement::AdvancePendingIndexSweep(cursor, 0, 0);
    EXPECT_EQ(cursor.round, 3u);
    EXPECT_EQ(cursor.rotatingStart, 20u);
}

TEST(SettlementBacklog, IndexMemberMustBePositiveDecimal) {
    uint64_t playerId = 0;
    EXPECT_TRUE(battle_settlement::ParsePendingIndexMember("9876501", playerId));
    EXPECT_EQ(playerId, 9876501u);
    EXPECT_TRUE(battle_settlement::ParsePendingIndexMember("18446744073709551615", playerId));
    EXPECT_EQ(playerId, std::numeric_limits<uint64_t>::max());
    for (const char* bad : {"", "0", "-1", "+1", " 1", "1 ", "12a", "0x10", "18446744073709551616"}) {
        playerId = 77;
        EXPECT_FALSE(battle_settlement::ParsePendingIndexMember(bad, playerId)) << '"' << bad << '"';
        EXPECT_EQ(playerId, 77u) << '"' << bad << '"';  // 失败不改出参
    }
}

// reaper 到期解冻之后的补读命中:ClearBattleFreeze 刚摘了 InBattleComp,ApplyPendingSettlement
// 照常入账一次、不凭空把玩家冻回去;同一份记录被再补读一次(下一轮 reaper / 登录钩子)由账本挡住。
TEST_F(PlayerBattleSettlementTest, PendingAppliedAfterFreezeExpiryPaysOnceWithoutRefreezing) {
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    for (int read = 0; read < 2; ++read) {
        PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
        EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u) << "read " << read;
        EXPECT_EQ(KillProgress(), 2u) << "read " << read;
        EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player)) << "read " << read;
    }
}

// 补读入口在 Redis 不可用时不应用、不改资产;2026-10-16 起记入登录复查(以前只打日志、等下次进场,
// 在线玩家一直拿不到奖励),由 reaper 节拍在 Redis 恢复后重读。
TEST_F(PlayerBattleSettlementTest, FreezeExpiryRecoveryWithoutRedisAppliesNothingAndSchedulesRecheck) {
    PlayerBattleSettlementTestAccess::RecoverAfterFreezeExpiry(player, playerId);
    EXPECT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(gPersistCalls, 0);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
}

// ---- 2026-10-16:登录钩子「没问到」进复查(单测里没有 Redis 连接,RedisReady() 恒为 false)----
//
// 空应答与 Redis 不可用是同一类「没问到」;回调里按应答形状分流的判定由 routing_identity_test 的
// SettlementLoginHook.* 盯住,这里盯调用点:没问到时挂复查标记、不当结论、不被 reaper 节拍丢掉。

// 缺陷 3:以前只打一条 WARN「等下一次登录」,玩家一直在线就一直拿不到挂起结算,冻结也不重建。
TEST_F(PlayerBattleSettlementTest, EnterSceneWithoutRedisSchedulesLoginRecheck) {
    ASSERT_FALSE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    PlayerBattleSystem::OnPlayerEnterScene(player, LOGIN_FIRST);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    // 没问到就不是结论:不补应用、不凭空冻结。
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(gPersistCalls, 0);
}

// reaper 节拍遇到 Redis 仍不可用:标记原样保留,等 Redis 恢复后的节拍再跑;重复节拍不叠加副作用。
TEST_F(PlayerBattleSettlementTest, LoginRecheckSurvivesReaperTicksWhileRedisIsDown) {
    PlayerBattleSystem::OnPlayerEnterScene(player, LOGIN_FIRST);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    for (int tick = 0; tick < 3; ++tick) {
        PlayerBattleSettlementTestAccess::RunLoginRechecks();
        EXPECT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player)) << "tick " << tick;
    }
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
}

// 缺陷 4:挂起结算补应用之后按锁重建冻结,读不了锁时以前直接 return(当「无锁」)。
// 现在补应用照常只发一次,读锁那一步进复查。
TEST_F(PlayerBattleSettlementTest, PendingAppliedButLockUnreadableSchedulesRecheck) {
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    // 复查重跑时同一份记录再补应用一次由账本挡住(PendingAppliedAfterFreezeExpiryPaysOnceWithoutRefreezing 同理)。
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

// 退出中的实体视同离线:节拍摘掉标记、不重跑(不论 Redis 是否可用),挂起结算留给下次进场。
TEST_F(PlayerBattleSettlementTest, ExitingPlayerLoginRecheckIsDroppedWithoutRerun) {
    PlayerBattleSystem::OnPlayerEnterScene(player, LOGIN_FIRST);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
    PlayerBattleSettlementTestAccess::RunLoginRechecks();
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
}

// 跨 zone 交接中:标记保留、本轮不跑 —— 重跑会在即将销毁的源实体上按锁重建冻结并推重连提示。
// 交接完成随实体销毁;交接撤销后下一个节拍再跑。
// 注意:单测里 Redis 恒不可用,这条用例分不清「交接中保留」与「Redis 不可用保留」(第二轮评审指出);
// 两条分支的区分由 routing_identity_test 的 SettlementLoginHook.RecheckEntryDecisionCoversEveryState 盯住。
TEST_F(PlayerBattleSettlementTest, TravelHandoffKeepsLoginRecheckForLater) {
    PlayerBattleSystem::OnPlayerEnterScene(player, LOGIN_FIRST);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    PlayerBattleSettlementTestAccess::RunLoginRechecks();
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
}

// 标记只挂在实体上:实体销毁即随之消失(下次进场登录钩子会重跑),不会留下指向已销毁实体的复查。
TEST_F(PlayerBattleSettlementTest, LoginRecheckDiesWithTheEntity) {
    PlayerBattleSystem::OnPlayerEnterScene(player, LOGIN_FIRST);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    tlsEcs.actorRegistry.destroy(player);
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    PlayerBattleSettlementTestAccess::RunLoginRechecks();  // 无标记可跑,也不碰已销毁实体
    player = entt::null;  // TearDown 不再 destroy
}

// ---- 2026-10-19:reaper 判作废的局不再按锁重建(第二、三轮评审 P3)----
//
// 删锁没落地时锁还指向刚作废的那一局;按锁重建的两个入口(登录重建、迟到确认重建)都先比本实体记下的作废局,
// 相等就不重建、补一次条件删锁。比较本身由 routing_identity_test 的 SettlementLoginHook.LockRebuild* 盯住
// (回调里才走得到,单测没有 Redis);这里盯「到期收尾时确实记下了作废局」这一步。

// FIGHTING 到期收尾:先记下作废局,再条件删锁、摘冻结;Redis 不可用时补读记入登录复查。不应用、不发奖。
TEST_F(PlayerBattleSettlementTest, FightingExpiryRemembersTheExpiredBattle) {
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    ASSERT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), 0u);

    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, settlement.battle_id());

    EXPECT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), settlement.battle_id());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(gPersistCalls, 0);
}

// 单槽:只留最近一局。锁还在时 match 拦着排队,只有锁没了才可能有下一局,那时旧值已无锁可比。
TEST_F(PlayerBattleSettlementTest, ExpiredBattleSlotKeepsOnlyTheLatest) {
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, 801);
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, 802);
    EXPECT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), 802u);
}

// 退出中的实体:照样记下作废局、摘冻结,但不补读(视同离线,挂起结算留给下次进场),也就不挂复查。
TEST_F(PlayerBattleSettlementTest, ExitingPlayerFightingExpiryRemembersButDoesNotRecover) {
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id());
    tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, settlement.battle_id());
    EXPECT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), settlement.battle_id());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
}

// ---- 2026-10-19(合并前独立复核 P3-2):重建入口的裁决接线 ----
//
// 两个重建入口在 Redis 回调里调同一个 DecideLockRebuild(作废槽位 + 活账本 → ClassifyLockRebuild);
// 单测经 DecideLockRebuildFor 走同一个函数,盯住「槽位确实被读进裁决」。回调里 switch 各分支本身仍只靠评审与人工场景
// (单测没有 Redis,走不进回调)。

// FIGHTING 到期记下 701 之后:锁指向 701 不重建,锁指向别的局照常重建。
TEST_F(PlayerBattleSettlementTest, LockRebuildWiringReadsThisEntitysExpiredSlot) {
    using battle_settlement::LockRebuildDecision;
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, 701);
    auto decision = LockRebuildDecision::kRebuild;
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::DecideLockRebuild(player, 701, decision));
    EXPECT_EQ(decision, LockRebuildDecision::kSkipExpired);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::DecideLockRebuild(player, 702, decision));
    EXPECT_EQ(decision, LockRebuildDecision::kRebuild);
}

// 没有作废局的实体:照常重建(作废槽位为 0 不与任何锁值相等)。
TEST_F(PlayerBattleSettlementTest, LockRebuildWiringWithoutExpiryRebuilds) {
    using battle_settlement::LockRebuildDecision;
    auto decision = LockRebuildDecision::kSkipExpired;
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::DecideLockRebuild(player, 701, decision));
    EXPECT_EQ(decision, LockRebuildDecision::kRebuild);
}

// 活账本里已有这一局(作废之后迟到结算按锁应用过):推进销账优先于「已作废」,不补删锁。
TEST_F(PlayerBattleSettlementTest, LockRebuildWiringPrefersTheLedgerOverTheExpiredSlot) {
    using battle_settlement::LockRebuildDecision;
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, 701);
    battle_settlement::RecordApplied(tlsEcs.actorRegistry.get<BattleSettlementLedgerComp>(player), 701, 1);
    auto decision = LockRebuildDecision::kRebuild;
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::DecideLockRebuild(player, 701, decision));
    EXPECT_EQ(decision, LockRebuildDecision::kAdvanceAck);
}

// 缺账本时判不了:返回 false,重建入口照旧跳过重建(fail-closed),出参不动。
TEST_F(PlayerBattleSettlementTest, LockRebuildWiringWithoutLedgerDecidesNothing) {
    using battle_settlement::LockRebuildDecision;
    tlsEcs.actorRegistry.remove<BattleSettlementLedgerComp>(player);
    auto decision = LockRebuildDecision::kAdvanceAck;
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::DecideLockRebuild(player, 701, decision));
    EXPECT_EQ(decision, LockRebuildDecision::kAdvanceAck);
}

// 槽位只挂在实体上:实体销毁(重登会新建实体)即随之消失,读口对无效实体返回 0。
TEST_F(PlayerBattleSettlementTest, ExpiredBattleSlotDiesWithTheEntity) {
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, settlement.battle_id());
    ASSERT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), settlement.battle_id());
    tlsEcs.actorRegistry.destroy(player);
    EXPECT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), 0u);
    player = entt::null;  // TearDown 不再 destroy
}

}  // namespace

// ============================================================================
// 备战与换图在途互斥(2026-10-15;turn-based-battle-server.md §23,Java 版对应 HandOffBattleExclusionTest
// 互斥表的 RESOLVING / FREEZING 两行)
// ============================================================================
#include "engine/core/time/system/time.h"
#include "proto/scene/scene.pb.h"
#include "services/scene/player/system/player_lifecycle.h"

namespace {

PrepareBattleRequest MakePrepareRequest(uint64_t playerId, uint64_t battleId) {
    PrepareBattleRequest request;
    request.set_player_id(playerId);
    request.set_battle_id(battleId);
    request.set_battle_node_id(3);
    request.set_deadline_ms(TimeSystem::NowMillisecondsUTC() + 300000);
    return request;
}

// 本夹具没挂等级 / 技能 / 背包等加载组件:过了换图闸之后会停在组快照那一步(kServiceUnavailable),
// 同样不留冻结。用它区分"被换图闸拒绝"与"放过了换图闸"。
constexpr uint32_t kRefusedAtSnapshot = kServiceUnavailable;

// 普通 EnterScene 的应答还没回(在途槽未过期):备战回 1006、不出快照、不挂冻结,也不动换图的在途记录。
// 修复前这里会放过换图闸(本夹具停在组快照;真实玩家会挂上 InBattleComp),随后落地的同节点路由
// 把战斗中的玩家换到别的场景。路由落地(EnterScene 3.1 摘在途槽)之后备战照常往下走。
TEST_F(PlayerBattleSettlementTest, PrepareRejectedWhileSceneChangeInFlight) {
    auto& pending = tlsEcs.actorRegistry.emplace<PlayerSceneChangeInFlightComp>(player);
    pending.sceneId = 2;
    pending.sentAtMs = TimeSystem::NowMillisecondsUTC();
    pending.correlationId = 77;
    ASSERT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player));

    PrepareBattleResponse response;
    PlayerBattleSystem::PrepareBattle(MakePrepareRequest(playerId, 801), response);

    EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kFeatureUnavailable));
    EXPECT_FALSE(response.has_snapshot());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    const auto* stillPending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
    ASSERT_NE(stillPending, nullptr) << "备战拒绝不得收掉换图的在途记录";
    EXPECT_EQ(stillPending->correlationId, 77u);

    tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(player);
    PrepareBattleResponse afterLanding;
    PlayerBattleSystem::PrepareBattle(MakePrepareRequest(playerId, 802), afterLanding);
    EXPECT_EQ(afterLanding.error_message().id(), kRefusedAtSnapshot);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
}

// 镜像 CreateScene 在途(自动进场的 EnterScene 还没发)同样占着换图闸:备战回 1006,镜像在途记录原样,
// 完成回调仍能按令牌认领。完成回调摘槽之后备战照常往下走。
TEST_F(PlayerBattleSettlementTest, PrepareRejectedWhileMirrorCreateInFlight) {
    auto& mirror = tlsEcs.actorRegistry.emplace<PlayerMirrorCreateInFlightComp>(player);
    mirror.token = 5;
    mirror.sentAtMs = TimeSystem::NowMillisecondsUTC();
    mirror.mirrorConfigId = 9;
    ASSERT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player));

    PrepareBattleResponse response;
    PlayerBattleSystem::PrepareBattle(MakePrepareRequest(playerId, 805), response);

    EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kFeatureUnavailable));
    EXPECT_FALSE(response.has_snapshot());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    const auto* stillMirror = tlsEcs.actorRegistry.try_get<PlayerMirrorCreateInFlightComp>(player);
    ASSERT_NE(stillMirror, nullptr) << "备战拒绝不得收掉镜像的在途记录";
    EXPECT_EQ(stillMirror->token, 5u);

    tlsEcs.actorRegistry.remove<PlayerMirrorCreateInFlightComp>(player);
    PrepareBattleResponse afterDone;
    PlayerBattleSystem::PrepareBattle(MakePrepareRequest(playerId, 806), afterDone);
    EXPECT_EQ(afterDone.error_message().id(), kRefusedAtSnapshot);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
}

// 交接冻结照旧拒绝:原来单独判 PlayerFrozenComp,改为 IsSceneChangeBusy 之后这一条不能丢。
TEST_F(PlayerBattleSettlementTest, PrepareRejectedWhileHandoffFrozen) {
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);

    PrepareBattleResponse response;
    PlayerBattleSystem::PrepareBattle(MakePrepareRequest(playerId, 803), response);

    EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kFeatureUnavailable));
    EXPECT_FALSE(response.has_snapshot());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
}

// 在途槽过了 TTL(应答与失败通知都没来),或时钟回拨(sent_at_ms 在未来):与发送侧闸同一个判据,
// 不再挡备战 —— 丢一条应答不能把玩家长期挡在战斗之外。
TEST_F(PlayerBattleSettlementTest, PrepareNotBlockedByExpiredSceneChangeSlot) {
    tlsEcs.actorRegistry.emplace<PlayerSceneChangeInFlightComp>(player);
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
    const uint64_t sentAtCases[] = {1, nowMs + 600000};
    for (const uint64_t sentAtMs : sentAtCases) {
        tlsEcs.actorRegistry.get<PlayerSceneChangeInFlightComp>(player).sentAtMs = sentAtMs;
        ASSERT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "sent_at_ms=" << sentAtMs;

        PrepareBattleResponse response;
        PlayerBattleSystem::PrepareBattle(MakePrepareRequest(playerId, 804), response);
        EXPECT_EQ(response.error_message().id(), kRefusedAtSnapshot) << "sent_at_ms=" << sentAtMs;
        EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player)) << "sent_at_ms=" << sentAtMs;
    }
}

}  // namespace
