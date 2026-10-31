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
#include <string_view>
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
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/comp/last_persisted_snapshot_comp.h"
#include "services/scene/player/system/player_mission.h"
#include "services/scene/player/system/role_admission_system.h"
#include "services/scene/player/system/player_inbox_registry.h"
#include "services/scene/player/system/player_inbox_rules.h"
#include "services/scene/player/system/player_inbox_system.h"
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "network/node_utils.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/event/battle_event.pb.h"
#include "modules/mission/mission_events.h"
#include "services/scene/battle/system/battle_lock_rules.h"
#include "services/scene/battle/system/player_battle.h"
#include "thread_context/ecs_context.h"
// 冻结窗口内资产通道仍被战斗闸挡下的回归(2026-10-30):真实 Debit 入口 + 签名 + 资产通道账本。
#include "proto/common/component/asset_op_ledger_comp.pb.h"
#include "security/token_security.h"
#include "services/scene/player/system/asset_op_auth.h"
#include "services/scene/player/system/asset_op_system.h"
#include "table/proto/tip/asset_error_tip.pb.h"

// 访问生产共用入口；不复制金币和任务副作用实现，不连接 Redis 或登录账号。
class PlayerBattleSettlementTestAccess {
public:
    static bool Snapshot(entt::entity player, BattlePlayerSnapshot& snapshot, uint32_t& errorTipId) {
        return PlayerBattleSystem::BuildBattleSnapshot(player, snapshot, errorTipId);
    }
    static bool Apply(entt::entity player, const BattleSettlementData& settlement) {
        return PlayerBattleSystem::ApplySettlementToEntity(player, settlement);
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
    static void ReapExpiredFreezes(uint64_t nowMs) {
        PlayerBattleSystem::ReapExpiredFreezes(nowMs);
    }
    static uint64_t ExpiredFightingBattleId(entt::entity player) {
        return PlayerBattleSystem::ExpiredFightingBattleId(player);
    }
    static battle_lock::LockRebuildDecision DecideLockRebuild(entt::entity player, uint64_t lockBattleId) {
        return PlayerBattleSystem::DecideLockRebuildFor(player, lockBattleId);
    }
};

namespace {

// 收件箱记账之后会请求存盘(真实现是 PlayerLifecycleSystem::SavePlayer)。
// 单测不连 Redis,注入一个只计数的假实现;返回 false = 本次没有产生写入,
// 于是「已提交」只能落在 PlayerLastPersistedSnapshotComp 上 —— 用例不提交就正是「效果已写、还没落库」这个最关键的中间态。
int gPersistCalls = 0;
bool CountingPersist(entt::entity) {
    ++gPersistCalls;
    return false;
}

// 条件删锁命令的注入发送器(PlayerBattleSystem::SetBattleLockReleaseSenderForTest):只记下 (player_id, battle_id) 与回包入口,
// 不连 Redis。用例自己决定回灌什么样的回包,走的是与生产同一个具名回包处理函数。
struct RecordedLockRelease {
    uint64_t playerId{0};
    uint64_t battleId{0};
    std::function<void(redisReply*)> onReply;
};
std::vector<RecordedLockRelease> gLockReleases;
bool RecordLockRelease(uint64_t playerId, uint64_t battleId, std::function<void(redisReply*)> onReply) {
    gLockReleases.push_back({playerId, battleId, std::move(onReply)});
    return true;
}

// 组队跟随唤醒的注入(PlayerBattleSystem::SetFollowWakeFnForTest):记下被唤醒的实体,以及那一刻已经发出的删锁命令数
// (跟随链读 battle:lock fail-closed,删锁命令必须先于它进 hiredis 管道)。
std::vector<entt::entity> gFollowWakes;
std::vector<size_t> gLockReleasesAtFollowWake;
void RecordFollowWake(entt::entity player) {
    gFollowWakes.push_back(player);
    gLockReleasesAtFollowWake.push_back(gLockReleases.size());
}

// 条件删锁脚本的整数回包(1 = 锁是这一次删掉的,0 = 锁不在或已易主)。
redisReply IntegerReply(long long value) {
    redisReply reply{};
    reply.type = REDIS_REPLY_INTEGER;
    reply.integer = value;
    return reply;
}

bool ArmSettlementMissionItemSegment(uint64_t lo = 100000, uint64_t hi = 200000) {
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false;
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
        // 结算入口要核对已确认的入场身份(ApplySettlementToEntity)。
        RoleAdmissionSystem::Dependencies dependencies;
        common::base::RoleAdmissionIdentity identity;
        identity.set_player_id(kPlayerId); identity.set_generation(1); identity.set_owner_revision(1);
        identity.set_owner_epoch(7); identity.set_scene_instance_id("battle-sql-test-scene");
        dependencies.confirmedAdmissions.push_back(identity);
        admission = std::make_unique<RoleAdmissionSystem::ScopedDependencies>(std::move(dependencies));
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayerId);
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(kPlayerId);
        ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
        tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player).set_health(100);
        tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player).set_max_health(100);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        ASSERT_TRUE(tlsEcs.playerList.emplace(kPlayerId, player).second);
    }
    void TearDown() override {
        tlsEcs.playerList.erase(kPlayerId);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        admission.reset();
        EXPECT_TRUE(ArmSettlementMissionItemSegment(1, uint64_t{1} << 54));
    }
    MissionsComp& Missions() { return tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0); }
    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    static constexpr Guid kPlayerId = 88448844;
    entt::entity player{entt::null};
    std::unique_ptr<RoleAdmissionSystem::ScopedDependencies> admission;
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
        // 删锁命令与跟随唤醒一律走记录器:单测不连 Redis,也要能断言「发没发、发给谁、回包之后做了什么」。
        gLockReleases.clear();
        gFollowWakes.clear();
        gLockReleasesAtFollowWake.clear();
        PlayerBattleSystem::SetBattleLockReleaseSenderForTest(&RecordLockRelease);
        PlayerBattleSystem::SetFollowWakeFnForTest(&RecordFollowWake);
    }
    // 背包候选与收件箱状态机按 player_id 现取实体(tlsEcs.GetPlayer)。重建实体后必须重新登记。
    void RegisterPlayer() {
        tlsEcs.playerList.erase(playerId);
        tlsEcs.playerList.emplace(playerId, player);
    }
    void TearDown() override {
        PlayerBattleSystem::SetBattleLockReleaseSenderForTest(nullptr);
        PlayerBattleSystem::SetFollowWakeFnForTest(nullptr);
        gLockReleases.clear();
        gFollowWakes.clear();
        gLockReleasesAtFollowWake.clear();
        tlsEcs.playerList.erase(playerId);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
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
    }
    // 模拟收件箱账本落进一次提交:快照只在提交之后才替换。
    void CommitInboxLedgerSnapshot() {
        PlayerAllData persisted;
        auto* first = persisted.mutable_player_database_data();
        first->set_player_id(playerId);
        first->mutable_inbox_ledger()->CopyFrom(tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player));
        persisted.mutable_player_database_1_data()->set_player_id(playerId);
        persisted.set_last_save_seq(++saveSeq);
        const auto source = std::make_shared<const PlayerAllData>(persisted);
        const auto committed = RoleAdmissionSystem::CaptureSnapshotCommit(player, source, 7, saveSeq);
        ASSERT_TRUE(static_cast<bool>(committed));
        committed();
        tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player).Replace(*source);
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
    void AddCurrencyGain(uint32_t type, uint64_t amount) {
        auto* gain = settlement.add_currency_gains();
        gain->set_currency_type(type);
        gain->set_amount(amount);
    }
    void ExpectCurrencySettlementRejectedWithoutEffects() {
        const auto currencyBefore = tlsEcs.actorRegistry.get<CurrencyComp>(player).SerializeAsString();
        EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
        EXPECT_EQ(currencyBefore, tlsEcs.actorRegistry.get<CurrencyComp>(player).SerializeAsString());
        EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
        EXPECT_EQ(50u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana());
        EXPECT_EQ(0u, KillProgress());
        EXPECT_EQ(0, gPersistCalls);
    }
    entt::entity player{entt::null};
    uint64_t playerId{};
    BattleSettlementData settlement;
    ScopedSettlementMissionRows missionRows;
    std::unique_ptr<RoleAdmissionSystem::ScopedDependencies> admission;
    uint64_t saveSeq{0};
};

TEST_F(PlayerBattleSettlementTest, NewCurrencyGainsDoNotImplicitlyAddLegacyGold) {
    AddCurrencyGain(kCurrencyDiamond, 5);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
}

TEST_F(PlayerBattleSettlementTest, LegacyGoldGainStillAppliesWithoutNewCurrencyGains) {
    ASSERT_EQ(0, settlement.currency_gains_size());
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond));
}

// 故意把坏币种放在有效金币之后，证明预检覆盖整笔，而不是边校验边入账。
TEST_F(PlayerBattleSettlementTest, InvalidLaterCurrencyRejectsAllEffectsAndCanRetryAfterCorrection) {
    AddCurrencyGain(kCurrencyGold, 12);
    AddCurrencyGain(kCurrencyMax, 5);
    ExpectCurrencySettlementRejectedWithoutEffects();

    settlement.mutable_currency_gains(1)->set_currency_type(kCurrencyDiamond);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
}

TEST_F(PlayerBattleSettlementTest, DuplicateCurrencyRejectsBeforeAnyCredit) {
    AddCurrencyGain(kCurrencyGold, 12);
    AddCurrencyGain(kCurrencyGold, 3);
    ExpectCurrencySettlementRejectedWithoutEffects();
}

TEST_F(PlayerBattleSettlementTest, OversizedLaterCurrencyRejectsBeforeAnyCredit) {
    AddCurrencyGain(kCurrencyGold, 12);
    AddCurrencyGain(kCurrencyDiamond,
        static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) + 1);
    ExpectCurrencySettlementRejectedWithoutEffects();
}

TEST_F(PlayerBattleSettlementTest, LegacyGoldAboveSignedCreditRangeRejectsAllEffects) {
    settlement.set_gold_gain(static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) + 1);
    ExpectCurrencySettlementRejectedWithoutEffects();
}

TEST_F(PlayerBattleSettlementTest, LargestSignedCurrencyGainIsCreditedWithoutNarrowing) {
    const auto amount = static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
    AddCurrencyGain(kCurrencyDiamond, amount);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(amount, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
}

TEST_F(PlayerBattleSettlementTest, LaterCurrencyBalanceOverflowRejectsBeforeEarlierCredit) {
    const auto largestCredit = std::numeric_limits<int64_t>::max();
    ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyDiamond, largestCredit));
    ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyDiamond, largestCredit));
    ASSERT_EQ(std::numeric_limits<uint64_t>::max() - 1,
              CurrencySystem::GetBalance(player, kCurrencyDiamond));
    AddCurrencyGain(kCurrencyGold, 12);
    AddCurrencyGain(kCurrencyDiamond, 2);
    ExpectCurrencySettlementRejectedWithoutEffects();
}

TEST_F(PlayerBattleSettlementTest, MissingCurrencyDebtComponentRejectsAllCurrencyGainsWithoutCreatingIt) {
    AddCurrencyGain(kCurrencyGold, 12);
    AddCurrencyGain(kCurrencyDiamond, 5);
    tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player);
    ExpectCurrencySettlementRejectedWithoutEffects();
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerCurrencyComp>(player));

    tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
}

TEST_F(PlayerBattleSettlementTest, ZeroCurrencyGainSuppressesLegacyGoldWithoutCallingCredit) {
    AddCurrencyGain(kCurrencyDiamond, 0);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(2u, KillProgress());
}

TEST_F(PlayerBattleSettlementTest, CurrencyFailureLeavesSideEffectsAndFreezeUntouchedAndCanRetry) {
    tlsEcs.actorRegistry.remove<CurrencyComp>(player);
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id());
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana(), 50u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_TRUE(tlsEcs.actorRegistry.all_of<InBattleComp>(player));
    tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(KillProgress(), 2u);
}

// 空宠物列表是合法持久状态；整个组件缺失则是加载故障，不能跳过宝宝后仍发金币、记整笔水位。
TEST_F(PlayerBattleSettlementTest, MissingPetComponentRejectsBeforeCurrencyAndHealthChanges) {
    settlement.add_pets()->set_pet_id(901);
    ASSERT_FALSE(tlsEcs.actorRegistry.all_of<PlayerPetComp>(player));

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(50u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana());
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerPetComp>(player));
}

TEST_F(PlayerBattleSettlementTest, MissingBagComponentRejectsBeforeCurrencyAndHealthChanges) {
    auto* gained = settlement.add_items_gained();
    gained->set_item_table_id(7001);
    gained->set_count(1);
    ASSERT_FALSE(tlsEcs.actorRegistry.all_of<PlayerBagsComp>(player));

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerBagsComp>(player));
}

TEST_F(PlayerBattleSettlementTest, MissingDerivedAttributesRejectsBeforeCurrencyAndCanRetryAfterRecovery) {
    tlsEcs.actorRegistry.remove<DerivedAttributesComp>(player);

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(player));

    auto& derived = tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player);
    derived.set_max_health(100);
    derived.set_max_mana(50);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

TEST_F(PlayerBattleSettlementTest, MissingMissionComponentRejectsBeforeCurrencyAndProgressChanges) {
    tlsEcs.actorRegistry.remove<MissionsContainerComp>(player);

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, KillProgress());
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
// 整笔可应用判据,以及收件箱执行入口(ApplyInboxSettlement)。
// 结算只走收件箱:效果与永久域水位在同一份快照里,同生共死;去重在 PlayerInboxSystem。
// ---------------------------------------------------------------------------

namespace {

// 冻结期必须整笔延后,而不是只挡金币和道具。
// 一笔「只有 HP + 击杀事实、没有金币也没有掉落」的 PVE 结算(打空怪,完全正常)
// 若穿过一道只看 gold/items 的窄闸:任务进度被推、血量被改,而此刻存盘被硬性跳过,改动随实体销毁一起没。
TEST_F(PlayerBattleSettlementTest, FrozenPlayerDefersSettlementWithNoGoldOrItems) {
    settlement.set_gold_gain(0);
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    EXPECT_EQ(gPersistCalls, 0);

    // 解冻后照常应用,奖励一次不差 —— 延后不是丢弃。
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(KillProgress(), 2u);
}

// 归属交接一挂上,存盘入口开头就直接跳过写盘。此时应用等于改了一份永远落不了盘
// 的内存态,必须整笔延后,条目留在收件箱,由目的地节点重拉。
TEST_F(PlayerBattleSettlementTest, TravelHandoffDefersWholeSettlement) {
    auto& handoff = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    handoff.requestedAtMs = 42;

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
}

// 全服 GainBlock 与单人 GM 封禁口径相同(InboxGoldBlockDefersWholeSettlementAndRetryAppliesAllEffects):整笔延后、零副作用,解封后照常应用。
TEST_F(PlayerBattleSettlementTest, GlobalCurrencyGainBlockDefersWholeSettlementAndRetryApplies) {
    struct ScopedGlobalGoldBlock {
        const uint32_t gold = static_cast<uint32_t>(kCurrencyGold);
        const bool wasBlocked =
            GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency, gold);
        ScopedGlobalGoldBlock() { GainBlockService::BlockGlobal(GainBlockService::GainType::kCurrency, gold); }
        ~ScopedGlobalGoldBlock() {
            if (!wasBlocked) GainBlockService::UnblockGlobal(GainBlockService::GainType::kCurrency, gold);
        }
    };
    {
        ScopedGlobalGoldBlock block;
        EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
        EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
        EXPECT_EQ(0u, KillProgress());
        EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    }
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(KillProgress(), 2u);
}

// 重复消费由PlayerInboxSystem永久域水位验证。
TEST_F(PlayerBattleSettlementTest, InboxSettlementUsesWholeEffectInputsWithoutOwnSave) {
    const auto result = PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString());
    EXPECT_EQ(PlayerInboxExecKind::kApplied, result.kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
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

// C-3(2026-10-30):载荷上限只读类型规格(proto 的 max_payload_bytes 是唯一事实源),执行器里不再手写 16384。
// 超过上限的原件按版本错位拒执行、零副作用。去掉这道闸,下面这份能正常解码、语义完整的载荷会被当成 APPLIED。
TEST_F(PlayerBattleSettlementTest, InboxPayloadAboveTypeSpecLimitIsRefusedWithoutEffects) {
    const uint32_t limit = player_inbox_registry::MaxPayloadBytes(player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT);
    ASSERT_EQ(16384u, limit);
    while (settlement.ByteSizeLong() <= limit) {
        // monster_config_id = 0 的击杀事实不推任务,只占字节。
        for (int i = 0; i < 512; ++i) settlement.add_defeated_monsters()->set_count(1);
    }
    EXPECT_EQ(PlayerInboxExecKind::kVersionSkew, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

TEST_F(PlayerBattleSettlementTest, InboxOldBattleCannotRewriteNextBattleHealthOrFreeze) {
    auto& current = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    current.set_battle_id(settlement.battle_id() + 1);
    const auto nextBytes = current.SerializeAsString();
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_TRUE(PlayerBattleSystem::CompleteInboxSettlement(player, settlement.SerializeAsString()));
    EXPECT_EQ(nextBytes, tlsEcs.actorRegistry.get<InBattleComp>(player).SerializeAsString());
    EXPECT_TRUE(gLockReleases.empty()) << "玩家在下一局里:旧局的确认不放锁(条件删本来也删不到,这里连命令都不发)";
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
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
    void PrepareThreeCurrencyInboxExchange() {
        tlsEcs.actorRegistry.get<CurrencyComp>(player).mutable_values()->Resize(static_cast<int>(kCurrencyMax), 0);
        AddCurrencyGain(kCurrencyGold, 12);
        AddCurrencyGain(kCurrencyDiamond, 7);
        AddCurrencyGain(kCurrencyBindDiamond, 3);
        settlement.set_gold_gain(999); // 新集合存在时旧金币不能叠加，亦不能被Inbox旧入口单独解释。
        ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
        SetConsumed(kSettlementStackItem, 1);
        SetGained(kSettlementNonStackItem, 1);
    }
    void ExpectInboxExchangeRejectedWithoutEffects(PlayerInboxExecKind kind) {
        const auto money = tlsEcs.actorRegistry.get<CurrencyComp>(player).SerializeAsString();
        const auto items = ItemBytes();
        const auto slots = Inventory().GridSlots();
        EXPECT_EQ(kind, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
        EXPECT_EQ(money, tlsEcs.actorRegistry.get<CurrencyComp>(player).SerializeAsString());
        EXPECT_EQ(items, ItemBytes()); EXPECT_EQ(slots, Inventory().GridSlots());
        EXPECT_EQ(0u, Temporary().OccupiedGridCount());
        EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
        EXPECT_EQ(50u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana());
        EXPECT_EQ(0u, KillProgress()); EXPECT_EQ(0, gPersistCalls);
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
        GuidSegmentClient::Options options; options.mixIds = false; options.kindName = name; options.initialStep = 100000;
        if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
            [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
        client.Warm(); client.OnResponse(0, low, high);
        return client.IsReady();
    }
    bool installed_{false}, itemEnabled_{false}, txEnabled_{false};
};

// 复用正式Inbox状态机/执行器，依赖仅替换时钟、传输与存盘回执；不自行实现去重。
constexpr uint32_t kCompositionBattleDomain = player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT;
uint64_t gCompositionInboxNowMs = 1'000'000;
std::vector<player_inbox::FetchPlayerInboxRequest> gCompositionInboxFetches;
std::vector<player_inbox::AckPlayerInboxRequest> gCompositionInboxAcks;
NodeInfo gCompositionInboxNode;
const NodeInfo* gCompositionPreviousNode = nullptr;
uint64_t CompositionInboxNow() { return gCompositionInboxNowMs; }
bool CaptureCompositionInboxFetch(const player_inbox::FetchPlayerInboxRequest& request) {
    if (static_cast<uint32_t>(request.domain()) != kCompositionBattleDomain) return false;
    gCompositionInboxFetches.push_back(request); return true;
}
bool CaptureCompositionInboxAck(const player_inbox::AckPlayerInboxRequest& request) {
    if (static_cast<uint32_t>(request.domain()) != kCompositionBattleDomain) return false;
    gCompositionInboxAcks.push_back(request); return true;
}
class ScopedCompositionInbox {
public:
    ScopedCompositionInbox() {
        gCompositionInboxNowMs = 1'000'000;
        gCompositionInboxFetches.clear(); gCompositionInboxAcks.clear();
        const auto& old = GetNodeInfo();
        gCompositionPreviousNode = &old == &NodeInfo::default_instance() ? nullptr : &old;
        gCompositionInboxNode.Clear(); gCompositionInboxNode.set_node_id(4242);
        InstallLocalNodeInfo(&gCompositionInboxNode);
        PlayerInboxSystem::SetNowFnForTest(&CompositionInboxNow);
        PlayerInboxSystem::SetPersistFnForTest(&CountingPersist);
        PlayerInboxSystem::SetTransportForTest(&CaptureCompositionInboxFetch, &CaptureCompositionInboxAck);
        PlayerInboxSystem::SetConsumerConfigForTest(true, true, 0);
    }
    ~ScopedCompositionInbox() {
        PlayerInboxSystem::SetConsumerConfigForTest(false, false, 0);
        PlayerInboxSystem::SetTransportForTest(nullptr, nullptr);
        PlayerInboxSystem::SetPersistFnForTest(nullptr); PlayerInboxSystem::SetNowFnForTest(nullptr);
        InstallLocalNodeInfo(gCompositionPreviousNode); gCompositionPreviousNode = nullptr;
        gCompositionInboxFetches.clear(); gCompositionInboxAcks.clear();
    }
};

// 把一条战斗结算当作 Fetch 的回包喂给正式的收件箱状态机。request_id = battle_id(match 发件箱的口径)。
void RespondWithBattleSettlement(const player_inbox::FetchPlayerInboxRequest& fetch, const uint64_t playerId,
                                 const BattleSettlementData& settlement, const uint64_t seq) {
    player_inbox::FetchPlayerInboxResponse response;
    response.set_player_id(playerId); response.set_domain(fetch.domain());
    response.set_correlation_id(fetch.correlation_id()); response.set_fence(player_inbox::PLAYER_INBOX_FENCE_MATCH);
    auto* item = response.add_items(); item->set_seq(seq); item->set_request_id(settlement.battle_id());
    item->set_msg_type(player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT);
    item->set_payload(settlement.SerializeAsString()); item->set_delivery_count(1); item->set_created_ms(1);
    PlayerInboxSystem::OnFetchReply(playerId, kCompositionBattleDomain, fetch.correlation_id(), response);
}

// 终局协议 v1 的放锁时机(player-data-tidb-scaling-plan.md §5.4):效果已写、还没提交时,冻结与 battle:lock 都不动 ——
// 此刻放开,资产通道与下一局排队就会抢在这份存档落库之前;提交之后恰好放一次,再发收件箱确认。
TEST_F(PlayerBattleSettlementTest, InboxKeepsFreezeAndLockUntilSettlementCommitThenReleasesOnce)
{
    ScopedCompositionInbox inbox;
    const auto confirmed = RoleAdmissionSystem::ConfirmedIdentity(playerId);
    ASSERT_TRUE(confirmed.has_value());
    RoleAdmissionSystem::Dependencies dependencies;
    dependencies.now = [] { return RoleAdmissionSystem::Clock::time_point(std::chrono::milliseconds(gCompositionInboxNowMs)); };
    dependencies.localNode = [] { return RoleAdmissionSystem::LocalNode{"battle-sql-test-scene", "4242", 1}; };
    dependencies.confirmedAdmissions.push_back(*confirmed);
    RoleAdmissionSystem::ScopedDependencies proof(std::move(dependencies));
    tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player);
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gCompositionInboxFetches.size());
    RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);

    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "没提交不解冻";
    EXPECT_TRUE(gLockReleases.empty()) << "没提交不放锁";
    EXPECT_TRUE(gFollowWakes.empty());
    EXPECT_TRUE(gCompositionInboxAcks.empty()) << "没提交不确认";
    PlayerInboxSystem::Tick();
    EXPECT_TRUE(gLockReleases.empty()) << "再转一拍也一样";

    ASSERT_NO_FATAL_FAILURE(CommitInboxLedgerSnapshot());
    PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(playerId, gLockReleases.front().playerId);
    EXPECT_EQ(settlement.battle_id(), gLockReleases.front().battleId);
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    ASSERT_EQ(1u, gFollowWakes.size());
    EXPECT_EQ(1u, gLockReleasesAtFollowWake.front()) << "删锁命令必须先于跟随链读锁";
    ASSERT_EQ(1u, gCompositionInboxAcks.size());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "放锁不重复任何效果";
}

// 崩溃窗口:结算已随存档提交、还没来得及放锁,进程就没了。重登时按锁重建出冻结(组件不落库),收件箱重放这一条:
// 账本里已有原结局,效果不再执行;提交确认之后照常解冻放锁,玩家不会一直卡到冻结到期。
TEST_F(PlayerBattleSettlementTest, ReplayAfterCrashReleasesRebuiltFreezeWithoutPayingAgain)
{
    ScopedCompositionInbox inbox;
    const auto confirmed = RoleAdmissionSystem::ConfirmedIdentity(playerId);
    ASSERT_TRUE(confirmed.has_value());
    RoleAdmissionSystem::Dependencies dependencies;
    dependencies.now = [] { return RoleAdmissionSystem::Clock::time_point(std::chrono::milliseconds(gCompositionInboxNowMs)); };
    dependencies.localNode = [] { return RoleAdmissionSystem::LocalNode{"battle-sql-test-scene", "4242", 1}; };
    dependencies.confirmedAdmissions.push_back(*confirmed);
    RoleAdmissionSystem::ScopedDependencies proof(std::move(dependencies));
    tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player);
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id());
    PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gCompositionInboxFetches.size());
    RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);
    ASSERT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    const auto persistedLedger = tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player);
    ASSERT_TRUE(gLockReleases.empty());

    // 盘上的账本带着这一条,资产也在盘上;新实体只从盘上带回账本,用金币与进度为 0 证明效果没有再执行一遍。
    tlsEcs.playerList.erase(playerId);
    tlsEcs.actorRegistry.destroy(player);
    ASSERT_NO_FATAL_FAILURE(CreatePlayerEntity());
    RegisterPlayer();
    tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player, persistedLedger);
    auto& rebuilt = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    rebuilt.set_battle_id(settlement.battle_id());
    rebuilt.set_state(IN_BATTLE_STATE_FIGHTING);

    gCompositionInboxNowMs += player_inbox_rules::kMinCheckGapMs + 1;
    PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
    ASSERT_EQ(2u, gCompositionInboxFetches.size());
    RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "重放只报告账本里的事实,不再发奖";
    EXPECT_EQ(0u, KillProgress());
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player));
    EXPECT_TRUE(gLockReleases.empty());

    ASSERT_NO_FATAL_FAILURE(CommitInboxLedgerSnapshot());
    PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(settlement.battle_id(), gLockReleases.front().battleId);
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
}

TEST_F(PlayerBattleSettlementItemTest, InboxThreeCurrenciesUseOneAtomicCreditAndPermanentReplayIgnoresLegacyGold)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(PrepareThreeCurrencyInboxExchange());
    ScopedCompositionInbox inbox;
    const auto confirmed = RoleAdmissionSystem::ConfirmedIdentity(playerId);
    ASSERT_TRUE(confirmed.has_value());
    RoleAdmissionSystem::Dependencies dependencies;
    dependencies.now = [] { return RoleAdmissionSystem::Clock::time_point(std::chrono::milliseconds(gCompositionInboxNowMs)); };
    dependencies.localNode = [] { return RoleAdmissionSystem::LocalNode{"battle-sql-test-scene", "4242", 1}; };
    dependencies.confirmedAdmissions.push_back(*confirmed);
    RoleAdmissionSystem::ScopedDependencies proof(std::move(dependencies));
    tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player);
    PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gCompositionInboxFetches.size());
    const auto respond = [&](const player_inbox::FetchPlayerInboxRequest& fetch) {
        player_inbox::FetchPlayerInboxResponse response;
        response.set_player_id(playerId); response.set_domain(fetch.domain());
        response.set_correlation_id(fetch.correlation_id()); response.set_fence(player_inbox::PLAYER_INBOX_FENCE_MATCH);
        auto* item = response.add_items(); item->set_seq(10); item->set_request_id(settlement.battle_id());
        item->set_msg_type(player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT);
        item->set_payload(settlement.SerializeAsString()); item->set_delivery_count(1); item->set_created_ms(1);
        PlayerInboxSystem::OnFetchReply(playerId, kCompositionBattleDomain, fetch.correlation_id(), response);
    };
    respond(gCompositionInboxFetches.back());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(7u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(3u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond));
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(kSettlementNonStackItem));
    EXPECT_EQ(2u, KillProgress()); EXPECT_TRUE(gCompositionInboxAcks.empty());

    // 按既有Inbox夹具完成一次提交，不把persist函数返回值当证明。
    PlayerAllData persisted;
    auto* first = persisted.mutable_player_database_data(); first->set_player_id(playerId);
    first->mutable_currency()->CopyFrom(tlsEcs.actorRegistry.get<CurrencyComp>(player));
    first->mutable_derived_attributes_component()->CopyFrom(tlsEcs.actorRegistry.get<BaseAttributesComp>(player));
    first->mutable_inbox_ledger()->CopyFrom(tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player));
    persisted.mutable_player_database_1_data()->set_player_id(playerId); persisted.set_last_save_seq(++saveSeq);
    const auto source = std::make_shared<const PlayerAllData>(persisted);
    const auto committed = RoleAdmissionSystem::CaptureSnapshotCommit(player, source, 7, saveSeq);
    ASSERT_TRUE(static_cast<bool>(committed)); committed();
    tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player).Replace(*source);
    PlayerInboxSystem::Tick(); ASSERT_EQ(1u, gCompositionInboxAcks.size());
    const auto ack = gCompositionInboxAcks.back();
    ASSERT_EQ(1, ack.decisions_size());
    EXPECT_EQ(player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, ack.decisions(0).outcome());
    player_inbox::AckPlayerInboxResponse acknowledged;
    acknowledged.set_player_id(playerId); acknowledged.set_domain(ack.domain());
    acknowledged.set_correlation_id(ack.correlation_id()); acknowledged.set_fence(player_inbox::PLAYER_INBOX_FENCE_MATCH);
    auto* finalized = acknowledged.add_results(); finalized->set_seq(ack.decisions(0).seq());
    finalized->set_result(player_inbox::PLAYER_INBOX_ACK_FINALIZED); finalized->set_state(player_inbox::PLAYER_INBOX_ITEM_APPLIED);
    PlayerInboxSystem::OnAckReply(playerId, kCompositionBattleDomain, ack.correlation_id(), acknowledged);

    const auto after = ItemBytes();
    tlsEcs.actorRegistry.get<BaseAttributesComp>(player).set_health(90);
    gCompositionInboxNowMs += player_inbox_rules::kMinCheckGapMs + 1;
    PlayerInboxSystem::OnNotifyEvent(playerId, kCompositionBattleDomain); PlayerInboxSystem::Tick();
    ASSERT_EQ(2u, gCompositionInboxFetches.size()); respond(gCompositionInboxFetches.back());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(7u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(3u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond));
    EXPECT_EQ(after, ItemBytes()); EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(90u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    ASSERT_EQ(2u, gCompositionInboxAcks.size());
}

TEST_F(PlayerBattleSettlementItemTest, InboxLaterCurrencyOverflowRejectsMoneyItemsHealthAndMissionTogether)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(PrepareThreeCurrencyInboxExchange());
    tlsEcs.actorRegistry.get<CurrencyComp>(player).set_values(
        static_cast<int>(kCurrencyBindDiamond), std::numeric_limits<uint64_t>::max() - 2);
    ASSERT_NO_FATAL_FAILURE(ExpectInboxExchangeRejectedWithoutEffects(PlayerInboxExecKind::kDefer));
    tlsEcs.actorRegistry.get<CurrencyComp>(player).set_values(static_cast<int>(kCurrencyBindDiamond), 0);
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(7u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(3u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond));
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(kSettlementNonStackItem)); EXPECT_EQ(2u, KillProgress());
}

TEST_F(PlayerBattleSettlementItemTest, InboxBlockedLaterCurrencyDefersWholeExchangeAndRetryCreditsEachCurrencyOnce)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(PrepareThreeCurrencyInboxExchange());
    ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyBindDiamond));
    ASSERT_NO_FATAL_FAILURE(ExpectInboxExchangeRejectedWithoutEffects(PlayerInboxExecKind::kDefer));
    ASSERT_EQ(kSuccess, CurrencySystem::UnblockCurrency(player, kCurrencyBindDiamond));
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(7u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(3u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond));
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(kSettlementNonStackItem)); EXPECT_EQ(2u, KillProgress());
}

TEST_F(PlayerBattleSettlementItemTest, InboxUnknownDebtOnLaterCurrencyStopsBeforeAnyCreditOrConsumption)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(PrepareThreeCurrencyInboxExchange());
    CurrencyDebtEntry unknown;
    unknown.GetReflection()->MutableUnknownFields(&unknown)->AddVarint(900, 1);
    const auto carried = unknown_field_carry::Capture(unknown);
    auto& debt = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player).debts[static_cast<uint32_t>(kCurrencyBindDiamond)];
    debt.owed = 1; debt.unknownFields = carried;
    ASSERT_TRUE(CurrencySystem::DebtCarriesUnknownFields(player, kCurrencyBindDiamond));
    ASSERT_NO_FATAL_FAILURE(ExpectInboxExchangeRejectedWithoutEffects(PlayerInboxExecKind::kVersionSkew));
    EXPECT_EQ(0u, debt.paid); EXPECT_EQ(carried, debt.unknownFields);
}

TEST_F(PlayerBattleSettlementItemTest, InboxAdmissionChangeDuringCandidateInitializationCannotPartiallyCreditCurrencies)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(PrepareThreeCurrencyInboxExchange());
    const auto before = RoleAdmissionSystem::ConfirmedIdentity(playerId);
    ASSERT_TRUE(before.has_value());
    auto replacement = *before; replacement.set_owner_revision(before->owner_revision() + 1);
    int initialized = 0;
    struct ResetInitializer { ~ResetInitializer() { BagService::SetItemInstanceInitializer({}); } } reset;
    const auto installAdmission = [&](const common::base::RoleAdmissionIdentity& identity) {
        admission.reset();
        RoleAdmissionSystem::Dependencies dependencies;
        dependencies.confirmedAdmissions.push_back(identity);
        admission = std::make_unique<RoleAdmissionSystem::ScopedDependencies>(std::move(dependencies));
        auto& loaded = tlsEcs.actorRegistry.get<PlayerRoleAdmissionComp>(player);
        loaded.generation = identity.generation(); loaded.ownerRevision = identity.owner_revision();
    };
    BagService::SetItemInstanceInitializer([&](ItemComp&) {
        ++initialized;
        installAdmission(replacement); // 对抗性重入：容器候选未发布，最终提交必须再核入场原件。
    });
    ASSERT_NO_FATAL_FAILURE(ExpectInboxExchangeRejectedWithoutEffects(PlayerInboxExecKind::kDefer));
    EXPECT_EQ(1, initialized);
    const auto changed = RoleAdmissionSystem::ConfirmedIdentity(playerId);
    ASSERT_TRUE(changed.has_value()); EXPECT_EQ(replacement.owner_revision(), changed->owner_revision());
    BagService::SetItemInstanceInitializer({}); installAdmission(*before);
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(7u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(3u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond));
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(kSettlementNonStackItem)); EXPECT_EQ(2u, KillProgress());
}

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

// C-2(2026-10-30):掉落物的配置行已被热更删掉(这里用一个表里不存在的配置号,对执行器而言与删行等价)。
// 改动前 ConsumeInventoryAndGrant 对查不到行的配置整笔回 kInvalidParameter,执行器回 DEFER,这一局的金币、任务、HP
// 与其余掉落永远卡在收件箱里。现在只剔除这一项(记 ERROR metric=battle_inbox_item_effect_skipped),其余照常 APPLIED。
TEST_F(PlayerBattleSettlementItemTest, InboxDropWithDeletedConfigRowSkipsOnlyThatDrop)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    constexpr uint32_t kDeletedConfig = 0x7FFFFFF1u;
    ASSERT_FALSE(ItemTableManager::Instance().Exists(kDeletedConfig)) << "前置:配置行不存在";
    SetGained(kSettlementDropItem, 2);
    SetGained(kDeletedConfig, 3);
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementDropItem)) << "其余掉落照常入包";
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kDeletedConfig));
    EXPECT_EQ(0u, Temporary().OccupiedGridCount());
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
}

// C-2:消耗物的配置行已被热更删掉。改动前 HasUnsupportedInboxConsumption 把「行不存在」判成版本错位,执行器回
// kVersionSkew,整个战斗结算域对这名玩家停用。现在只剔除这一项、不停域,其余效果(含另一种已知消耗)照常到账。
// 行还在而玩家实例带未知字段,仍按版本错位(InboxUnknownConsumedInstanceCannotBeDowngradedToOrdinaryQuantity)。
TEST_F(PlayerBattleSettlementItemTest, InboxConsumptionWithDeletedConfigRowDoesNotHaltDomain)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    constexpr uint32_t kDeletedConfig = 0x7FFFFFF2u;
    ASSERT_FALSE(ItemTableManager::Instance().Exists(kDeletedConfig)) << "前置:配置行不存在";
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    SetConsumed(kSettlementStackItem, 1);
    SetConsumed(kDeletedConfig, 2);
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind)
        << "改动前是 kVersionSkew:停用整个战斗结算域";
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem)) << "已知消耗照常扣";
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
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

TEST_F(PlayerBattleSettlementItemTest, ConsumedForItemPlayerDoesNotHaveDoesNotFailSettlement) {
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementDropItem, 1);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 0u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementDropItem), 1u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

// 审计 cpp-economy#0(data-overwrite-rollback-audit-20261018.md §2.4):物品号段暂时发不出号。
// 在金币之前按掉落的新实例数上界预检号段余量:不够就整笔延后、零副作用,预检本身也不耗号。
// 号段续上后续做照常应用一次 —— 延后不是丢弃。
TEST_F(PlayerBattleSettlementItemTest, ItemGuidShortageDefersWholeSettlementBeforeGold) {
    ScopedItemSegmentBaseline restoreBaseline;
    GiveItem(kSettlementStackItem, 5);  // 铺底在换号段之前,用的是基线号段
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementNonStackItem, 3);  // 3 件不可叠加 = 3 个新实例
    ASSERT_TRUE(ArmSettlementMissionItemSegment(99300000, 99300002));  // 只剩 2 个号

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 5u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementNonStackItem), 0u);
    EXPECT_EQ(Temporary().GetTotalItemCount(kSettlementNonStackItem), 0u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    EXPECT_EQ(gPersistCalls, 0);
    EXPECT_EQ(tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available(), 2u);

    ASSERT_TRUE(ArmSettlementMissionItemSegment(99400000, 99500000));  // 续段到达
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 3u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementNonStackItem), 3u);
    EXPECT_EQ(KillProgress(), 2u);
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

namespace {

// ---- 2026-10-16:登录钩子「没问到」进复查(单测里没有 Redis 连接,RedisReady() 恒为 false)----
//
// 空应答与 Redis 不可用是同一类「没问到」;回调里按应答形状分流的判定由 routing_identity_test 的
// BattleLockLoginHook.* 盯住,这里盯调用点:没问到时挂复查标记、不当结论、不被 reaper 节拍丢掉。

// 读不了锁不等于没有锁:不重建冻结,等 Redis 恢复后的节拍重读。
TEST_F(PlayerBattleSettlementTest, EnterSceneWithoutRedisSchedulesLoginRecheck) {
    ASSERT_FALSE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    PlayerBattleSystem::OnPlayerEnterScene(player, LOGIN_FIRST);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    // 没问到就不是结论:不凭空冻结。
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

// 退出中的实体视同离线:节拍摘掉标记、不重跑(不论 Redis 是否可用),下次进场登录钩子重读。
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
// 两条分支的区分由 routing_identity_test 的 BattleLockLoginHook.RecheckEntryDecisionCoversEveryState 盯住。
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
// 相等就不重建、补一次条件删锁。比较本身由 routing_identity_test 的 BattleLockLoginHook.LockRebuild* 盯住
// (回调里才走得到,单测没有 Redis);这里盯「到期收尾时确实记下了作废局」这一步。

// FIGHTING 到期收尾:先记下作废局,再条件删锁、摘冻结。不应用、不发奖,也不再去 Redis 读任何结算。
TEST_F(PlayerBattleSettlementTest, FightingExpiryRemembersTheExpiredBattle) {
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    ASSERT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), 0u);

    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, settlement.battle_id());

    EXPECT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), settlement.battle_id());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::LoginRecheckPending(player));
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(playerId, gLockReleases.front().playerId);
    EXPECT_EQ(settlement.battle_id(), gLockReleases.front().battleId);
    ASSERT_EQ(1u, gFollowWakes.size());
    EXPECT_EQ(1u, gLockReleasesAtFollowWake.front()) << "删锁命令必须先于跟随链读锁";
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(gPersistCalls, 0);
}

// 冻结到期之后结算才到(收件箱迟迟没投到):照常应用一次、不把玩家冻回去;提交确认时条件删锁再发一次(幂等)。
TEST_F(PlayerBattleSettlementTest, SettlementArrivingAfterFreezeExpiryAppliesWithoutRefreezing) {
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, settlement.battle_id());
    ASSERT_EQ(1u, gLockReleases.size());

    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));

    EXPECT_TRUE(PlayerBattleSystem::CompleteInboxSettlement(player, settlement.SerializeAsString()));
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    ASSERT_EQ(2u, gLockReleases.size());
    EXPECT_EQ(settlement.battle_id(), gLockReleases.back().battleId);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
}

// 组件已不在时解冻早已发生,跟随链当时读到锁还在会跳过:只有这条命令确实删掉了锁才补一次跟随,落在现任实体上。
// 回包没落地 / 没删到 / 玩家已备战下一局都不补。摘组件那条路径不走这里,不会多跟随一次。
TEST_F(PlayerBattleSettlementTest, CompleteWithoutFreezeWakesFollowOnlyWhenThisCommandDeletedTheLock) {
    ASSERT_TRUE(PlayerBattleSystem::CompleteInboxSettlement(player, settlement.SerializeAsString()));
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(settlement.battle_id(), gLockReleases.front().battleId);
    EXPECT_TRUE(gFollowWakes.empty()) << "回包之前不补";

    gLockReleases.front().onReply(nullptr);
    redisReply error{};
    error.type = REDIS_REPLY_ERROR;
    gLockReleases.front().onReply(&error);
    auto notDeleted = IntegerReply(0);
    gLockReleases.front().onReply(&notDeleted);
    EXPECT_TRUE(gFollowWakes.empty()) << "没落地的回包不能当成锁已放掉";

    auto deleted = IntegerReply(1);
    gLockReleases.front().onReply(&deleted);
    ASSERT_EQ(1u, gFollowWakes.size());
    EXPECT_EQ(player, gFollowWakes.front());

    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id() + 1);
    gLockReleases.front().onReply(&deleted);
    EXPECT_EQ(1u, gFollowWakes.size()) << "已备战下一局:那一局解冻时自会触发跟随";
}

// 同一局的冻结还在:条件删锁恰好一次,且先于摘组件(跟随链读 battle:lock fail-closed);摘组件已触发跟随,回包不再补第二次。
TEST_F(PlayerBattleSettlementTest, CompleteReleasesLockBeforeRemovingFreezeOfTheSameBattle) {
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id());
    ASSERT_TRUE(PlayerBattleSystem::CompleteInboxSettlement(player, settlement.SerializeAsString()));
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    ASSERT_EQ(1u, gLockReleases.size());
    ASSERT_EQ(1u, gFollowWakes.size());
    EXPECT_EQ(1u, gLockReleasesAtFollowWake.front());
    auto deleted = IntegerReply(1);
    gLockReleases.front().onReply(&deleted);
    EXPECT_EQ(1u, gFollowWakes.size());
}

// 单槽:只留最近一局。锁还在时 match 拦着排队,只有锁没了才可能有下一局,那时旧值已无锁可比。
TEST_F(PlayerBattleSettlementTest, ExpiredBattleSlotKeepsOnlyTheLatest) {
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, 801);
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, 802);
    EXPECT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), 802u);
}

// ---- 2026-10-19(合并前独立复核 P3-2):重建入口的裁决接线 ----
//
// 两个重建入口在 Redis 回调里调同一个 DecideLockRebuild(作废槽位 → ClassifyLockRebuild);
// 单测经 DecideLockRebuildFor 走同一个函数,盯住「槽位确实被读进裁决」。回调里的分支本身仍只靠评审与人工场景
// (单测没有 Redis,走不进回调)。

// FIGHTING 到期记下 701 之后:锁指向 701 不重建,锁指向别的局照常重建。
TEST_F(PlayerBattleSettlementTest, LockRebuildWiringReadsThisEntitysExpiredSlot) {
    using battle_lock::LockRebuildDecision;
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, 701);
    EXPECT_EQ(PlayerBattleSettlementTestAccess::DecideLockRebuild(player, 701), LockRebuildDecision::kSkipExpired);
    EXPECT_EQ(PlayerBattleSettlementTestAccess::DecideLockRebuild(player, 702), LockRebuildDecision::kRebuild);
}

// 没有作废局的实体:照常重建(作废槽位为 0 不与任何锁值相等)。
TEST_F(PlayerBattleSettlementTest, LockRebuildWiringWithoutExpiryRebuilds) {
    EXPECT_EQ(PlayerBattleSettlementTestAccess::DecideLockRebuild(player, 701), battle_lock::LockRebuildDecision::kRebuild);
}

// 槽位只挂在实体上:实体销毁(重登会新建实体)即随之消失,读口对无效实体返回 0。
TEST_F(PlayerBattleSettlementTest, ExpiredBattleSlotDiesWithTheEntity) {
    PlayerBattleSettlementTestAccess::ReleaseExpiredFightingFreeze(player, playerId, settlement.battle_id());
    ASSERT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), settlement.battle_id());
    tlsEcs.actorRegistry.destroy(player);
    EXPECT_EQ(PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player), 0u);
    player = entt::null;  // TearDown 不再 destroy
}

// ---- 2026-10-30:FIGHTING 冻结等到终局提交宽限 + 收件箱投递余量之后才算到期 ----
//
// Battle 在 deadline 才强制平局,Match 还允许 deadline 之后 60s 内提交 COMPLETE,结算再经收件箱投递到本节点。
// 改动前 reaper 在 deadline 一过就摘冻结:资产通道的战斗闸(只看 InBattleComp)提前放开,玩家能先把本局用掉的药
// 挂托管扣走,迟到的结算只能按当时持有量夹紧 —— 可逐局刷的有界资产复制。

// 纯判据:FIGHTING = deadline + 宽限 + 余量;PREPARING 判据不变;没有期限不判到期;上溢饱和。
TEST(BattleFreezeExpiry, FightingWaitsForTerminalGraceAndInboxMarginPreparingDoesNot) {
    EXPECT_EQ(60000u, PlayerBattleSystem::kTerminalOutcomeGraceMs)
        << "须与 go/match/internal/authority/completion.go terminalOutcomeGraceMs、Java TERMINAL_OUTCOME_GRACE_MS 同值";
    EXPECT_EQ(60000u, PlayerBattleSystem::kInboxSettlementDeliveryMarginMs);
    constexpr uint64_t kDeadlineMs = 1'800'000'000'000ULL;
    constexpr uint64_t kWindowMs =
        PlayerBattleSystem::kTerminalOutcomeGraceMs + PlayerBattleSystem::kInboxSettlementDeliveryMarginMs;
    EXPECT_EQ(kDeadlineMs + kWindowMs, PlayerBattleSystem::FreezeExpiresAtMs(false, kDeadlineMs, 0));
    EXPECT_EQ(kDeadlineMs + kWindowMs, PlayerBattleSystem::FreezeExpiresAtMs(false, kDeadlineMs, kDeadlineMs - 5000))
        << "FIGHTING 不看备战期限";
    EXPECT_EQ(0u, PlayerBattleSystem::FreezeExpiresAtMs(false, 0, 0)) << "没有期限:不判到期";
    EXPECT_EQ(kDeadlineMs - 5000, PlayerBattleSystem::FreezeExpiresAtMs(true, kDeadlineMs, kDeadlineMs - 5000))
        << "PREPARING 只看备战期限,不加宽限";
    EXPECT_EQ(kDeadlineMs, PlayerBattleSystem::FreezeExpiresAtMs(true, kDeadlineMs, 0))
        << "旧版 match 不填备战期限时退回 deadline";
    EXPECT_EQ(UINT64_MAX, PlayerBattleSystem::FreezeExpiresAtMs(false, UINT64_MAX - 1, 0)) << "上溢饱和,不回绕成很早的时刻";
    static_assert(PlayerBattleSystem::FreezeExpiresAtMs(false, 1, 0) == 1 + kWindowMs, "判据是编译期可求值的纯函数");
}

// reaper 拍落在 deadline 与 deadline + 宽限 + 余量之间:FIGHTING 冻结保留(作废槽位也不记);越过之后照常作废释放。
TEST_F(PlayerBattleSettlementTest, ReaperKeepsFightingFreezeUntilTerminalGraceAndInboxMarginPass) {
    constexpr uint64_t kDeadlineMs = 1'800'000'000'000ULL;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    inBattle.set_deadline_ms(kDeadlineMs);
    const uint64_t expiresAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kDeadlineMs, 0);
    for (const uint64_t nowMs : {kDeadlineMs + 1, kDeadlineMs + PlayerBattleSystem::kTerminalOutcomeGraceMs + 1, expiresAtMs}) {
        PlayerBattleSettlementTestAccess::ReapExpiredFreezes(nowMs);
        EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "now = deadline + " << (nowMs - kDeadlineMs) << "ms";
        EXPECT_EQ(0u, PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player));
        EXPECT_TRUE(gLockReleases.empty()) << "冻结还在,锁不能先放";
    }
    PlayerBattleSettlementTestAccess::ReapExpiredFreezes(expiresAtMs + 1);
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    EXPECT_EQ(settlement.battle_id(), PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player));
    ASSERT_EQ(1u, gLockReleases.size()) << "FIGHTING 到期:条件删锁,玩家不会卡在这一局";
    EXPECT_EQ(settlement.battle_id(), gLockReleases.front().battleId);
}

// PREPARING 判据不变:备战期限一过就摘组件(没有终局可等),也不记作废槽位(锁留到 TTL 给迟到确认重建)。
TEST_F(PlayerBattleSettlementTest, ReaperStillExpiresPreparingFreezeAtPrepareDeadline) {
    constexpr uint64_t kPrepareDeadlineMs = 1'800'000'000'000ULL;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_PREPARING);
    inBattle.set_deadline_ms(kPrepareDeadlineMs + 600000);
    inBattle.set_prepare_deadline_ms(kPrepareDeadlineMs);
    PlayerBattleSettlementTestAccess::ReapExpiredFreezes(kPrepareDeadlineMs);
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "恰好到点还不算过期(严格大于才摘)";
    PlayerBattleSettlementTestAccess::ReapExpiredFreezes(kPrepareDeadlineMs + 1);
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    EXPECT_EQ(0u, PlayerBattleSettlementTestAccess::ExpiredFightingBattleId(player));
    EXPECT_TRUE(gLockReleases.empty()) << "PREPARING 到期不删锁,留给迟到的确认按锁重建";
}

// 资产通道注入:时钟、存盘、密钥都替掉,只为在冻结窗口里打一次真实的 Debit(签名与资产通道单测同一套路)。
constexpr std::string_view kBattleAssetOpSecret = "battle-asset-op-fixture-secret-0123456789";
int64_t gAssetOpNowMs = 0;
int64_t AssetOpNowMs() { return gAssetOpNowMs; }
bool AssetOpPersist(entt::entity) { return true; }
std::string AssetOpSecret(std::string_view) { return std::string(kBattleAssetOpSecret); }
struct ScopedAssetOpFakes {
    ScopedAssetOpFakes() {
        PlayerAssetOpSystem::SetNowFnForTest(&AssetOpNowMs);
        PlayerAssetOpSystem::SetPersistFnForTest(&AssetOpPersist);
        PlayerAssetOpSystem::SetSecretLookupForTest(&AssetOpSecret);
    }
    ~ScopedAssetOpFakes() {
        PlayerAssetOpSystem::SetNowFnForTest(nullptr);
        PlayerAssetOpSystem::SetPersistFnForTest(nullptr);
        PlayerAssetOpSystem::SetSecretLookupForTest(nullptr);
    }
};
// 帮会捐献流扣金币(GUILD_DEBIT / TX_GUILD_DONATE,caller = guild)。
::AssetOpResponse GuildDebitGold(uint64_t playerId, uint64_t seq, uint64_t amount) {
    ::AssetOpRequest request;
    request.set_player_id(playerId);
    request.set_stream(ASSET_OP_STREAM_GUILD_DEBIT);
    request.set_seq(seq);
    request.set_correlation_id(77);
    request.set_tx_type(static_cast<uint32_t>(TX_GUILD_DONATE));
    request.set_stream_epoch(100);
    auto* currency = request.mutable_bundle()->add_currencies();
    currency->set_currency_type(kCurrencyGold);
    currency->set_amount(amount);
    auto* auth = request.mutable_auth();
    auth->set_caller("guild");
    auth->set_timestamp_ms(static_cast<uint64_t>(gAssetOpNowMs));
    auth->set_signature_hex(token_security::HmacSha256Hex(kBattleAssetOpSecret, AssetOpCanonical("debit", request)));
    ::AssetOpResponse response;
    PlayerAssetOpSystem::Debit(request, response);
    return response;
}

// 宽限 + 余量窗口内,资产通道扣款照旧被战斗闸挡下(RETRY + kAssetInBattle、不记账、不扣钱);
// 冻结按新判据作废释放之后,同一 seq 重投照常扣款。
TEST_F(PlayerBattleSettlementTest, AssetDebitGetsInBattleRetryUntilFightingFreezeExpires) {
    ScopedAssetOpFakes fakes;
    tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerComp>(player); // 加载时无条件挂上的资产通道账本
    ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    constexpr uint64_t kDeadlineMs = 1'800'000'000'000ULL;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    inBattle.set_deadline_ms(kDeadlineMs);
    const uint64_t expiresAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kDeadlineMs, 0);

    const uint64_t inWindowMs = kDeadlineMs + PlayerBattleSystem::kTerminalOutcomeGraceMs + 1;
    gAssetOpNowMs = static_cast<int64_t>(inWindowMs);
    PlayerBattleSettlementTestAccess::ReapExpiredFreezes(inWindowMs);
    ASSERT_TRUE(PlayerBattleSystem::IsInBattle(player));
    const auto blocked = GuildDebitGold(playerId, 1, 30);
    EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, blocked.outcome());
    EXPECT_EQ(static_cast<uint32_t>(kAssetInBattle), blocked.reason().id());
    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyGold));

    gAssetOpNowMs = static_cast<int64_t>(expiresAtMs + 1);
    PlayerBattleSettlementTestAccess::ReapExpiredFreezes(expiresAtMs + 1);
    ASSERT_FALSE(PlayerBattleSystem::IsInBattle(player));
    const auto applied = GuildDebitGold(playerId, 1, 30);
    EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, applied.outcome());
    EXPECT_EQ(70u, CurrencySystem::GetBalance(player, kCurrencyGold));
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
