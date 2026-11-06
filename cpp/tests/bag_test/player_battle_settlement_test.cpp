#include <gtest/gtest.h>
#include "services/scene/core/system/scene_task_scope.h"
#include "muduo/net/EventLoop.h"
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
#include "services/scene/player/system/player_feature_snapshot.h" // PlayerBagSystem::RetrieveTemporary:延后结算靠取回腾位
#include "services/scene/player/comp/player_inbox_runtime_comp.h" // 取回后定向唤醒:断言别的域的状态一点没动
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "network/node_utils.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/event/battle_event.pb.h"
#include "modules/mission/mission_events.h"
#include "services/scene/battle/system/battle_lock_rules.h"
#include "services/scene/battle/system/player_battle.h"
#include "services/scene/battle/system/battle_freeze_hold_rules.h" // 夹具注入的终局查询回答(TerminalAnswer)
// 本人近期战斗结果(docs/design/battle-result-recovery.md):持久组件、列表规则与客户端协议。
#include "proto/common/component/player_battle_result_comp.pb.h"
#include "proto/scene/player_battle_result.pb.h"
#include "services/scene/battle/system/battle_result_history.h"
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
    // PrepareBattle 写锁的同一个入口(本文件的夹具没有组快照所需的加载组件,走不到 PrepareBattle 的写锁那一步)。
    static uint64_t WriteBattleLock(uint64_t playerId, uint64_t battleId) {
        return PlayerBattleSystem::WriteBattleLockOnPrepare(playerId, battleId);
    }
    // 重建复核应答的判定与撤销,生产回调走同一个函数。
    static void RebuiltFreezeRecheckReply(uint64_t playerId, uint64_t battleId, uint32_t attemptNumber,
                                          battle_lock::ReplyShape shape, int64_t integer) {
        PlayerBattleSystem::OnRebuiltFreezeRecheckReply(playerId, battleId, attemptNumber, shape, integer);
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

// FIGHTING 冻结到期时的 SQL 终局查询(PlayerBattleSystem::SetBattleTerminalQueryForTest,2026-11-06):本文件的夹具
// 不连 match,这些局也没有 SQL 原件,一律当场回「没有可裁决原件」(NO_ORIGINAL)—— 到期照常解冻,与续冻上线之前的
// 到期收尾相同。续冻本身(COMPLETE 未应用、有原件的 NONE、上限、查询失败、资产通道 RETRY)由 battle_freeze_hold_test.cpp 盯住。
bool AnswerNoTerminalYet(uint64_t, std::function<void(battle_freeze_hold::TerminalAnswer)> done) {
    done(battle_freeze_hold::TerminalAnswer::kNoOriginal);
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

// 锁脚本观察点(PlayerBattleSystem::SetLockScriptObserverForTest)的记录:不连 Redis 也能盯住「这一次会发什么」。
// 记录只能放在文件级;单测线程独占,ScopedLockScriptRecorder 进出各清一次,不跨用例残留。
// 夹具在当前测试线程建立 loop 和任务作用域，不连接 Redis；默认拒绝重试调度，未取得应答时当场放弃，
// 续期在首发前登记的在途链随之摘除,所以每一次首发都会回调一次,不会被去重挡掉。
// 要驱动重试链(去重、门禁放弃)的用例再叠一层 ScopedLockRetryCapture(见下)。
struct LockScriptCall {
    std::string op;
    uint64_t playerId = 0;
    uint64_t battleId = 0;
    uint64_t ttlSec = 0;
    std::string ctxPayload;
};
std::vector<LockScriptCall> gLockScriptCalls;

void RecordLockScriptCall(const char* op, uint64_t playerId, uint64_t battleId, uint64_t ttlSec,
                          const std::string& ctxPayload) {
    gLockScriptCalls.push_back(LockScriptCall{op, playerId, battleId, ttlSec, ctxPayload});
}

bool RejectLockRetry(uint64_t, std::function<void()>) { return false; }

class ScopedLockScriptRecorder {
public:
    ScopedLockScriptRecorder() {
        if (!muduo::net::EventLoop::getEventLoopOfCurrentThread()) loop_ = std::make_unique<muduo::net::EventLoop>();
        if (!SceneTaskScope::IsAccepting()) tasks_ = std::make_unique<SceneTaskScope>(*muduo::net::EventLoop::getEventLoopOfCurrentThread());
        PlayerBattleSystem::SetLockRetrySchedulerForTest(&RejectLockRetry);
        gLockScriptCalls.clear();
        PlayerBattleSystem::SetLockScriptObserverForTest(&RecordLockScriptCall);
    }
    ~ScopedLockScriptRecorder() {
        tasks_.reset();
        if (loop_) { loop_->queueInLoop([loop = loop_.get()] { loop->quit(); }); loop_->loop(); }
        PlayerBattleSystem::SetLockScriptSenderForTest(nullptr);
        PlayerBattleSystem::SetLockRetrySchedulerForTest(nullptr);
        PlayerBattleSystem::SetLockScriptObserverForTest(nullptr);
        gLockScriptCalls.clear();
    }
    ScopedLockScriptRecorder(const ScopedLockScriptRecorder&) = delete;
    ScopedLockScriptRecorder& operator=(const ScopedLockScriptRecorder&) = delete;
    void Stop() { if (tasks_) tasks_->Stop(); }
private:
    std::unique_ptr<muduo::net::EventLoop> loop_;
    std::unique_ptr<SceneTaskScope> tasks_;

};

// 某一种锁脚本被发送(回调观察点)的次数。
size_t LockScriptCount(const std::string& op) {
    return static_cast<size_t>(std::count_if(gLockScriptCalls.begin(), gLockScriptCalls.end(),
        [&op](const LockScriptCall& call) { return call.op == op; }));
}

// 锁写入 / 续期重试的调度器注入(PlayerBattleSystem::SetLockRetrySchedulerForTest):收下每一次「排下一次重试」,
// 由用例手动触发,代替单测线程上不存在的 EventLoop::runAfter。同样是不带捕获的函数指针,队列只能放在文件级。
std::vector<std::function<void()>> gCapturedLockRetries;

bool CaptureLockRetry(uint64_t /*delayMs*/, std::function<void()> fire) {
    gCapturedLockRetries.push_back(std::move(fire));
    return true;
}

// 触发最早排上的一次重试。没有可触发的返回 false。
bool FireNextLockRetry() {
    if (gCapturedLockRetries.empty()) return false;
    auto fire = std::move(gCapturedLockRetries.front());
    gCapturedLockRetries.erase(gCapturedLockRetries.begin());
    fire();
    return true;
}

// 用例结束时把收下的重试链全部跑到终态:续期的在途登记是 thread_local,链不走到终态就会残留到后面的用例。
class ScopedLockRetryCapture {
public:
    ScopedLockRetryCapture() {
        gCapturedLockRetries.clear();
        PlayerBattleSystem::SetLockRetrySchedulerForTest(&CaptureLockRetry);
    }
    ~ScopedLockRetryCapture() {
        for (int guard = 0; guard < 256 && FireNextLockRetry(); ++guard) {
        }
        PlayerBattleSystem::SetLockRetrySchedulerForTest(nullptr);
        gCapturedLockRetries.clear();
    }
    ScopedLockRetryCapture(const ScopedLockRetryCapture&) = delete;
    ScopedLockRetryCapture& operator=(const ScopedLockRetryCapture&) = delete;
};

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
        // 近期战斗结果是持久组件,生产里由加载创建;结算缺它整笔 DEFER(battle-result-recovery.md §4.3)。
        tlsEcs.actorRegistry.emplace<PlayerBattleResultComp>(player);
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
        PlayerBattleSystem::SetBattleTerminalQueryForTest(&AnswerNoTerminalYet);
    }
    // 背包候选与收件箱状态机按 player_id 现取实体(tlsEcs.GetPlayer)。重建实体后必须重新登记。
    void RegisterPlayer() {
        tlsEcs.playerList.erase(playerId);
        tlsEcs.playerList.emplace(playerId, player);
    }
    void TearDown() override {
        PlayerBattleSystem::SetBattleLockReleaseSenderForTest(nullptr);
        PlayerBattleSystem::SetFollowWakeFnForTest(nullptr);
        PlayerBattleSystem::SetBattleTerminalQueryForTest(nullptr);
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
        // 近期战斗结果是持久组件,生产里由加载创建;结算缺它整笔 DEFER(battle-result-recovery.md §4.3)。
        tlsEcs.actorRegistry.emplace<PlayerBattleResultComp>(player);
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
    // 本人近期战斗结果的持久列表(含还没公开的局)与客户端查询看到的(只含已公开的局)。
    const PlayerBattleResultComp& Results() const { return tlsEcs.actorRegistry.get<PlayerBattleResultComp>(player); }
    GetRecentBattleResultsResponse QueryResults() const {
        GetRecentBattleResultsResponse response;
        EXPECT_EQ(static_cast<uint32_t>(kSuccess), PlayerBattleSystem::GetRecentBattleResults(player, response));
        return response;
    }
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
        EXPECT_EQ(0, Results().entries_size()) << "被拒的结算不记近期结果";
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

// 全服 GainBlock 与单人 GM 封禁口径相同(InboxGoldBlockSkipsOnlyGoldAndAppliesTheRest):封禁是终局业务决定,
// 被封的币种这一局整笔不入账、解封后不补发,其余效果照常应用,结算整笔算已应用(收件箱据此记 APPLIED)。
// 2026-11-05 之前是整笔延后、解封后整笔补发:封禁期间这一局到不了 APPLIED,Match 的 ready 票一直占着。
TEST_F(PlayerBattleSettlementTest, GlobalCurrencyGainBlockSkipsOnlyBlockedGoldAndAppliesTheRest) {
    struct ScopedGlobalGoldBlock {
        const uint32_t gold = static_cast<uint32_t>(kCurrencyGold);
        const bool wasBlocked =
            GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency, gold);
        ScopedGlobalGoldBlock() { GainBlockService::BlockGlobal(GainBlockService::GainType::kCurrency, gold); }
        ~ScopedGlobalGoldBlock() {
            if (!wasBlocked) GainBlockService::UnblockGlobal(GainBlockService::GainType::kCurrency, gold);
        }
    };
    ScopedGlobalGoldBlock block;
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u) << "被封的币种整笔不入账";
    EXPECT_EQ(KillProgress(), 2u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 80u);
    EXPECT_TRUE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency, block.gold)) << "结算不改封禁状态";
    ASSERT_EQ(1, Results().entries_size()) << "已应用的局照常记近期结果";
    EXPECT_EQ(0, Results().entries(0).currencies_size()) << "近期结果只记实际入账,被跳过的币种不记";
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

// battle 节点的奖励档按同一张取值表(proto/common/base/currency_type.proto)校验,本二进制不认识的币种只可能来自
// 更新版本的 battle 节点:版本错位,收件箱停用本域、实体重建后重拉。不是 DEFER:会话内永远不会认识它,本地续做只会空转。
// 整笔零副作用;同包里与版本无关的坏项(重复币种)先判出,仍按原口径延后(混版本持久字段保护设计 §12)。
TEST_F(PlayerBattleSettlementTest, InboxSettlementWithCurrencyUnknownToThisVersionReportsVersionSkew) {
    AddCurrencyGain(kCurrencyGold, 12);
    AddCurrencyGain(static_cast<uint32_t>(kCurrencyMax), 5);
    ASSERT_FALSE(IsKnownCurrencyType(static_cast<uint32_t>(kCurrencyMax)));
    const auto currencyBefore = tlsEcs.actorRegistry.get<CurrencyComp>(player).SerializeAsString();

    EXPECT_EQ(PlayerInboxExecKind::kVersionSkew, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(currencyBefore, tlsEcs.actorRegistry.get<CurrencyComp>(player).SerializeAsString()) << "已知币种也不先入账";
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0, gPersistCalls);

    AddCurrencyGain(kCurrencyGold, 3);
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind)
        << "重复币种与版本无关,先于版本错位判出";
    EXPECT_EQ(currencyBefore, tlsEcs.actorRegistry.get<CurrencyComp>(player).SerializeAsString());
}

// 单人 GM 封禁了金币:这一局的金币整笔不入账、也不抵欠款(与 AddCurrency 拒绝被封币种的口径一致),
// 解封后不补发;HP 与击杀照常,执行器回 APPLIED。2026-11-05 之前整笔 DEFER:封禁期间这一局到不了 APPLIED
// (Match 的 ready 票停在 state 3,玩家不能再排全球匹配),解封后又整笔补发,违背封禁本意。
TEST_F(PlayerBattleSettlementTest, InboxGoldBlockSkipsOnlyGoldAndAppliesTheRest) {
    ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));
    // 欠款额大于这一局的金币:万一被错误入账,欠款也只会部分抵扣、不被删除,下面的引用一直有效。
    auto& debt = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player).debts[static_cast<uint32_t>(kCurrencyGold)];
    debt.owed = 100;
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "被封的币种整笔不入账";
    EXPECT_EQ(0u, debt.paid) << "被封的币种也不抵欠款";
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_TRUE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold)) << "结算不改封禁状态";
    ASSERT_EQ(1, Results().entries_size()) << "已应用的局照常记近期结果";
    EXPECT_EQ(0, Results().entries(0).currencies_size()) << "近期结果只记实际入账,被跳过的币种不记";
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
        EXPECT_EQ(0, Results().entries_size()) << "被拒的结算不记近期结果";
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

// ---------------------------------------------------------------------------
// 本人近期战斗结果(2026-11-05,docs/design/battle-result-recovery.md)
// 缺陷:断线期间结束的一局,终局通知丢了之后玩家再也看不到胜负与奖励明细(断线重连审计 §9.3)。
// 新行为:结果与奖励同一帧写进存档里的有界列表、同一次存盘提交,确认提交之后才对客户端公开;客户端落地时查。
// ---------------------------------------------------------------------------

PlayerBattleResult BattleResultFor(uint64_t battleId)
{
    PlayerBattleResult entry;
    entry.set_battle_id(battleId);
    entry.set_outcome(static_cast<uint32_t>(BATTLE_OUTCOME_SIDE_A_WIN));
    return entry;
}

TEST(BattleResultHistory, RecordAssignsIncreasingSeqAndKeepsTheNewestEntries)
{
    PlayerBattleResultComp history;
    std::vector<uint64_t> evicted;
    for (uint64_t battleId = 1; battleId <= battle_result::kMaxEntries + 2; ++battleId) {
        auto entry = BattleResultFor(battleId);
        entry.set_result_seq(999); // 调用方填的序号与已读标记都会被覆盖
        entry.set_seen(true);
        EXPECT_EQ(battle_result::RecordOutcome::kRecorded, battle_result::Record(history, entry, evicted));
    }
    EXPECT_EQ(static_cast<int>(battle_result::kMaxEntries), history.entries_size());
    EXPECT_EQ(static_cast<uint64_t>(battle_result::kMaxEntries + 2), history.last_result_seq());
    EXPECT_EQ((std::vector<uint64_t>{1, 2}), evicted) << "按 result_seq 从小到大淘汰";
    for (const auto& entry : history.entries()) {
        EXPECT_EQ(entry.battle_id(), entry.result_seq()) << "本例 battle_id 与写入顺序一致";
        EXPECT_FALSE(entry.seen()) << "新记录一律未读";
    }
}

TEST(BattleResultHistory, RecordKeepsTheFirstEntryOfABattleAndIgnoresZeroBattleId)
{
    PlayerBattleResultComp history;
    std::vector<uint64_t> evicted;
    auto first = BattleResultFor(7);
    first.set_total_rounds(3);
    ASSERT_EQ(battle_result::RecordOutcome::kRecorded, battle_result::Record(history, first, evicted));
    const auto before = history.SerializeAsString();
    auto again = BattleResultFor(7);
    again.set_total_rounds(9);
    EXPECT_EQ(battle_result::RecordOutcome::kDuplicateBattle, battle_result::Record(history, again, evicted));
    EXPECT_EQ(battle_result::RecordOutcome::kInvalidBattle, battle_result::Record(history, BattleResultFor(0), evicted));
    EXPECT_EQ(before, history.SerializeAsString()) << "按 battle_id 去重:保留第一条,什么都不改";
    EXPECT_TRUE(evicted.empty());
}

TEST(BattleResultHistory, RecordNeverReusesASeqWhenTheLoadedWatermarkLags)
{
    // 加载来的列表 last_result_seq 落后于现存条目:新条目仍排在现存最大序号之后,不撞号。
    PlayerBattleResultComp history;
    auto loaded = BattleResultFor(5);
    loaded.set_result_seq(7);
    *history.add_entries() = loaded;
    std::vector<uint64_t> evicted;
    ASSERT_EQ(battle_result::RecordOutcome::kRecorded, battle_result::Record(history, BattleResultFor(6), evicted));
    ASSERT_EQ(2, history.entries_size());
    EXPECT_EQ(8u, history.entries(1).result_seq());
    EXPECT_EQ(8u, history.last_result_seq());
}

TEST(BattleResultHistory, VisibleOrderComesFromResultSeqNotStorageIndexAndHidesUncommitted)
{
    PlayerBattleResultComp history;
    for (const auto& [battleId, seq] : std::vector<std::pair<uint64_t, uint64_t>>{{30, 3}, {10, 1}, {20, 2}}) {
        auto entry = BattleResultFor(battleId);
        entry.set_result_seq(seq);
        *history.add_entries() = entry;
    }
    const std::vector<uint64_t> noneHidden;
    std::vector<uint64_t> order;
    for (const auto* entry : battle_result::VisibleNewestFirst(history, noneHidden)) order.push_back(entry->battle_id());
    EXPECT_EQ((std::vector<uint64_t>{30, 20, 10}), order) << "新旧看 result_seq,不看下标(AGENTS §11.6)";
    const std::vector<uint64_t> uncommitted{30};
    order.clear();
    for (const auto* entry : battle_result::VisibleNewestFirst(history, uncommitted)) order.push_back(entry->battle_id());
    EXPECT_EQ((std::vector<uint64_t>{20, 10}), order) << "还没确认提交的局不公开";
}

TEST(BattleResultHistory, MarkSeenTouchesOnlyKnownPublishedEntriesAndIsIdempotent)
{
    PlayerBattleResultComp history;
    std::vector<uint64_t> evicted;
    for (const uint64_t battleId : std::vector<uint64_t>{11, 12, 13})
        ASSERT_EQ(battle_result::RecordOutcome::kRecorded, battle_result::Record(history, BattleResultFor(battleId), evicted));
    const std::vector<uint64_t> acked{11, 13, 11, 99};
    const std::vector<uint64_t> uncommitted{13};
    const std::vector<uint64_t> noneHidden;
    EXPECT_EQ(1u, battle_result::MarkSeen(history, acked, uncommitted)) << "13 还没公开,99 不认识,11 重复只算一次";
    EXPECT_EQ(1u, battle_result::MarkSeen(history, acked, noneHidden)) << "13 公开之后才接受确认";
    EXPECT_EQ(0u, battle_result::MarkSeen(history, acked, noneHidden)) << "幂等";
    std::vector<uint64_t> unread;
    for (const auto& entry : history.entries())
        if (!entry.seen()) unread.push_back(entry.battle_id());
    EXPECT_EQ((std::vector<uint64_t>{12}), unread);
}

TEST(BattleResultHistory, FillItemLinesSortsSkipsEmptyLinesAndTruncatesAboveTheLimit)
{
    std::vector<std::pair<uint32_t, uint32_t>> items;
    for (auto configId = static_cast<uint32_t>(battle_result::kMaxItemLinesPerList + 8); configId >= 1; --configId)
        items.emplace_back(configId, 1);
    items.emplace_back(0, 5);    // 配置号 0:跳过
    items.emplace_back(1000, 0); // 数量 0:跳过
    google::protobuf::RepeatedPtrField<PlayerBattleResultItem> lines;
    EXPECT_TRUE(battle_result::FillItemLines(items, lines));
    ASSERT_EQ(static_cast<int>(battle_result::kMaxItemLinesPerList), lines.size());
    for (int i = 0; i < lines.size(); ++i)
        EXPECT_EQ(static_cast<uint32_t>(i + 1), lines.Get(i).item_table_id()) << "按配置号升序,只留最小的那些";
    google::protobuf::RepeatedPtrField<PlayerBattleResultItem> few;
    EXPECT_FALSE(battle_result::FillItemLines({{5, 2}, {3, 1}}, few));
    ASSERT_EQ(2, few.size());
    EXPECT_EQ(3u, few.Get(0).item_table_id());
    EXPECT_EQ(2u, few.Get(1).count());
}

// 与奖励同一帧写进持久列表;存档确认提交之前客户端查不到,确认提交之后与 BattleEndS2C 同一时机公开,恰好一条。
TEST_F(PlayerBattleSettlementTest, InboxBattleResultIsRecordedWithRewardsAndPublishedOnlyAfterCommit)
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
    constexpr uint64_t kFinishedAtMs = 1'900'000'000'000;
    settlement.set_finished_at_ms(kFinishedAtMs);
    settlement.set_match_mode(5);
    settlement.set_battle_config_id(6);
    PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gCompositionInboxFetches.size());
    RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);

    ASSERT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    ASSERT_EQ(1, Results().entries_size()) << "结果与奖励同一帧写入";
    const auto recorded = Results().entries(0);
    EXPECT_EQ(settlement.battle_id(), recorded.battle_id());
    EXPECT_EQ(1u, recorded.result_seq());
    EXPECT_EQ(static_cast<uint32_t>(BATTLE_OUTCOME_SIDE_A_WIN), recorded.outcome());
    EXPECT_EQ(0u, recorded.team_index());
    EXPECT_EQ(kFinishedAtMs, recorded.finished_at_ms());
    EXPECT_EQ(5u, recorded.match_mode());
    EXPECT_EQ(6u, recorded.battle_config_id());
    EXPECT_GT(recorded.settled_at_ms(), 0u);
    ASSERT_EQ(1, recorded.currencies_size());
    EXPECT_EQ(static_cast<uint32_t>(kCurrencyGold), recorded.currencies(0).currency_type());
    EXPECT_EQ(12u, recorded.currencies(0).amount());
    EXPECT_FALSE(recorded.seen());
    EXPECT_EQ(0, QueryResults().results_size()) << "存档确认提交之前不公开";
    PlayerInboxSystem::Tick();
    EXPECT_EQ(0, QueryResults().results_size()) << "再转一拍也一样";

    ASSERT_NO_FATAL_FAILURE(CommitInboxLedgerSnapshot());
    PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gCompositionInboxAcks.size());
    const auto visible = QueryResults();
    ASSERT_EQ(1, visible.results_size()) << "断线期间结束的一局,重登后能查到,而且只一条";
    EXPECT_EQ(recorded.SerializeAsString(), visible.results(0).SerializeAsString());
    EXPECT_EQ(1, Results().entries_size());
}

// 崩溃窗口(提交之后、公开之前):盘上的结果随存档加载回来就是已提交的,直接可见;收件箱回放原结局不调执行器,
// 不写第二条;确认之后照常收尾,结果不变。
TEST_F(PlayerBattleSettlementTest, ReplayAfterCrashKeepsOneBattleResultAndTheLoadedResultIsVisible)
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
    ASSERT_EQ(1, Results().entries_size());

    // 这份存档(效果、账本、结果)已提交,进程随即崩溃:新实体只从盘上带回账本与结果。
    const auto persistedLedger = tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player);
    const auto persistedResults = Results();
    tlsEcs.playerList.erase(playerId);
    tlsEcs.actorRegistry.destroy(player);
    ASSERT_NO_FATAL_FAILURE(CreatePlayerEntity());
    RegisterPlayer();
    tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player, persistedLedger);
    tlsEcs.actorRegistry.replace<PlayerBattleResultComp>(player, persistedResults);
    auto& rebuilt = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    rebuilt.set_battle_id(settlement.battle_id());
    rebuilt.set_state(IN_BATTLE_STATE_FIGHTING);
    const auto loaded = QueryResults();
    ASSERT_EQ(1, loaded.results_size()) << "盘上加载回来的结果都已提交,直接可见";
    EXPECT_EQ(settlement.battle_id(), loaded.results(0).battle_id());

    gCompositionInboxNowMs += player_inbox_rules::kMinCheckGapMs + 1;
    PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
    ASSERT_EQ(2u, gCompositionInboxFetches.size());
    RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "回放不再发奖";
    EXPECT_EQ(persistedResults.SerializeAsString(), Results().SerializeAsString()) << "回放不调执行器,不写第二条";

    ASSERT_NO_FATAL_FAILURE(CommitInboxLedgerSnapshot());
    PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gLockReleases.size());
    const auto afterReplay = QueryResults();
    ASSERT_EQ(1, afterReplay.results_size());
    EXPECT_EQ(persistedResults.entries(0).SerializeAsString(), afterReplay.results(0).SerializeAsString());
}

// 近期战斗结果是持久数据:缺了说明加载不变量已被破坏,结算整笔 DEFER、零副作用;查询与确认回 1003;都不补建。
TEST_F(PlayerBattleSettlementTest, MissingBattleResultHistoryDefersWholeSettlementWithoutCreatingIt)
{
    tlsEcs.actorRegistry.remove<PlayerBattleResultComp>(player);
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerBattleResultComp>(player)) << "结算路径不补建持久组件";

    GetRecentBattleResultsResponse response;
    EXPECT_EQ(static_cast<uint32_t>(kServiceUnavailable), PlayerBattleSystem::GetRecentBattleResults(player, response));
    EXPECT_EQ(0, response.results_size());
    AckBattleResultsRequest ack;
    ack.add_battle_ids(settlement.battle_id());
    EXPECT_EQ(static_cast<uint32_t>(kServiceUnavailable), PlayerBattleSystem::AckBattleResults(player, ack));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerBattleResultComp>(player)) << "查询与确认同样不补建";

    tlsEcs.actorRegistry.emplace<PlayerBattleResultComp>(player); // 模拟加载修复之后
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(1, Results().entries_size());
}

// 确认已展示:只认已公开的局,不认识的局忽略,幂等;冻结中不改存档(1006);超过 10 个回 1015。
TEST_F(PlayerBattleSettlementTest, AckBattleResultsMarksOnlyPublishedResultsSeenAndIsIdempotent)
{
    ASSERT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    AckBattleResultsRequest ack;
    ack.add_battle_ids(settlement.battle_id());
    ack.add_battle_ids(999999); // 不认识的局:忽略
    EXPECT_EQ(static_cast<uint32_t>(kSuccess), PlayerBattleSystem::AckBattleResults(player, ack));
    EXPECT_FALSE(Results().entries(0).seen()) << "还没公开的局不接受确认";

    EXPECT_TRUE(PlayerBattleSystem::CompleteInboxSettlement(player, settlement.SerializeAsString()));
    ASSERT_EQ(1, QueryResults().results_size());
    EXPECT_FALSE(QueryResults().results(0).seen());

    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    EXPECT_EQ(static_cast<uint32_t>(kFeatureUnavailable), PlayerBattleSystem::AckBattleResults(player, ack));
    EXPECT_FALSE(Results().entries(0).seen()) << "冻结中不改存档";
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

    EXPECT_EQ(static_cast<uint32_t>(kSuccess), PlayerBattleSystem::AckBattleResults(player, ack));
    EXPECT_TRUE(Results().entries(0).seen());
    const auto seenBytes = Results().SerializeAsString();
    EXPECT_EQ(static_cast<uint32_t>(kSuccess), PlayerBattleSystem::AckBattleResults(player, ack));
    EXPECT_EQ(seenBytes, Results().SerializeAsString()) << "重复确认无副作用";
    const auto visible = QueryResults();
    ASSERT_EQ(1, visible.results_size());
    EXPECT_TRUE(visible.results(0).seen());

    AckBattleResultsRequest tooMany;
    for (uint64_t battleId = 1; battleId <= battle_result::kMaxEntries + 1; ++battleId) tooMany.add_battle_ids(battleId);
    EXPECT_EQ(static_cast<uint32_t>(kArraySizeTooLargeInMessage), PlayerBattleSystem::AckBattleResults(player, tooMany));
    EXPECT_EQ(seenBytes, Results().SerializeAsString());
}

// 公开放在同局判断之外:应用与公开之间玩家已进了下一局,终局通知不推、下一局的冻结与锁不动,这一局的结果照样公开。
TEST_F(PlayerBattleSettlementTest, BattleResultIsPublishedEvenWhenThePlayerAlreadyEnteredTheNextBattle)
{
    ASSERT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0, QueryResults().results_size());
    auto& next = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    next.set_battle_id(settlement.battle_id() + 1);
    next.set_state(IN_BATTLE_STATE_PREPARING);
    const auto nextBytes = next.SerializeAsString();
    EXPECT_TRUE(PlayerBattleSystem::CompleteInboxSettlement(player, settlement.SerializeAsString()));
    EXPECT_EQ(nextBytes, tlsEcs.actorRegistry.get<InBattleComp>(player).SerializeAsString());
    EXPECT_TRUE(gLockReleases.empty()) << "旧局的确认不碰下一局的锁";
    const auto visible = QueryResults();
    ASSERT_EQ(1, visible.results_size());
    EXPECT_EQ(settlement.battle_id(), visible.results(0).battle_id());
}

// 记的是本人实际入账,不是终局原件:消耗按实际持有夹紧,配置行已删的掉落不记,其余照实,与背包实际变化一致。
TEST_F(PlayerBattleSettlementItemTest, InboxBattleResultRecordsActualAmountsNotTheTerminalSource)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    constexpr uint32_t kDeletedConfig = 0x7FFFFFF3u;
    ASSERT_FALSE(ItemTableManager::Instance().Exists(kDeletedConfig)) << "前置:配置行不存在";
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 1));
    SetConsumed(kSettlementStackItem, 3); // 原件要求扣 3,实际只持有 1
    SetGained(kSettlementDropItem, 2);
    SetGained(kDeletedConfig, 3);
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementDropItem));
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kSettlementStackItem));

    ASSERT_EQ(1, Results().entries_size());
    const auto& recorded = Results().entries(0);
    ASSERT_EQ(1, recorded.items_consumed_size());
    EXPECT_EQ(kSettlementStackItem, recorded.items_consumed(0).item_table_id());
    EXPECT_EQ(1u, recorded.items_consumed(0).count()) << "按实际持有夹紧,不是原件里的 3";
    ASSERT_EQ(1, recorded.items_gained_size()) << "配置行已删的掉落不记";
    EXPECT_EQ(kSettlementDropItem, recorded.items_gained(0).item_table_id());
    EXPECT_EQ(2u, recorded.items_gained(0).count());
    EXPECT_FALSE(recorded.lines_truncated());
    ASSERT_EQ(1, recorded.currencies_size());
    EXPECT_EQ(12u, recorded.currencies(0).amount());
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
    persisted.set_last_save_seq(++saveSeq);
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

// 排在最后的币种被单人封禁:只跳过这一种,前两种币、消耗、掉落、HP 与击杀在同一次提交里照常应用,每种币恰好一次。
// 2026-11-05 之前整笔 DEFER、解封后整笔补发(原名 InboxBlockedLaterCurrencyDefersWholeExchangeAndRetryCreditsEachCurrencyOnce)。
TEST_F(PlayerBattleSettlementItemTest, InboxBlockedLaterCurrencySkipsOnlyThatCurrencyAndAppliesTheWholeExchange)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    ASSERT_NO_FATAL_FAILURE(PrepareThreeCurrencyInboxExchange());
    ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyBindDiamond));
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(7u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond)) << "被封的币种整笔不入账";
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem));
    EXPECT_EQ(1u, Inventory().GetTotalItemCount(kSettlementNonStackItem)); EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_TRUE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyBindDiamond)) << "结算不改封禁状态";
    ASSERT_EQ(1, Results().entries_size());
    const auto& recorded = Results().entries(0);
    ASSERT_EQ(2, recorded.currencies_size()) << "近期结果只记实际入账的两种币";
    EXPECT_EQ(static_cast<uint32_t>(kCurrencyGold), recorded.currencies(0).currency_type());
    EXPECT_EQ(12u, recorded.currencies(0).amount());
    EXPECT_EQ(static_cast<uint32_t>(kCurrencyDiamond), recorded.currencies(1).currency_type());
    EXPECT_EQ(7u, recorded.currencies(1).amount());
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

// 审计 cpp-economy#2(2026-11-05):临时格满了不再先进先出挤掉旧物。主包与临时格都放不下整批掉落时整笔延后:
// 消耗、金币、气血、击杀都不动,临时格里原有的那件也还在 —— 改动前它会被挤掉,这一局随即记成已应用。
// 来源(收件箱条目)保留,玩家从临时格取回、腾出位置后,下一次重试即应用。
TEST_F(PlayerBattleSettlementItemTest, InboxFullTemporaryBagIsNeverEvictedAndTheWholeSettlementDefers)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    constexpr Guid kOldestTemporaryItem = 9100001;
    Inventory().SetCapacityForRestore(1); Temporary().SetCapacityForRestore(1);
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    Temporary().InsertItemForRestore(kOldestTemporaryItem, kSettlementNonStackItem, 1, 0, 1);
    SetConsumed(kSettlementStackItem, 1); SetGained(kSettlementNonStackItem, 1);
    const auto items = ItemBytes();
    const auto slots = Inventory().GridSlots();
    const auto temporarySlots = Temporary().GridSlots();
    EXPECT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_NE(nullptr, Temporary().GetItemCompByGuid(kOldestTemporaryItem)) << "the oldest temporary item is never pushed out";
    EXPECT_EQ(1u, Temporary().OccupiedGridCount());
    EXPECT_EQ(temporarySlots, Temporary().GridSlots());
    EXPECT_EQ(items, ItemBytes());
    EXPECT_EQ(slots, Inventory().GridSlots());
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, KillProgress());
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_EQ(0, Results().entries_size()) << "a deferred settlement records no recent result";
}

// 评审修正(2026-11-06):两包都放不下而延后的那一局,这一局的冻结(InBattleComp)要等结算应用并确认提交之后才摘。
// 取回若在战斗中也拒,玩家在冻结到期之前腾不出位置,这一局就一直补不上。放行之后:冻结还挂着时取回一件,
// 临时格腾出两格,下一次重试整笔应用 —— 消耗、金币、击杀、掉落都到账,取回的那件原样在人物背包。
// 顺带钉住日志限频用的「连续放不下」计数:每次延后加一,整批发放成功归 0(BagService::ConsecutiveGrantOverflowRejections)。
TEST_F(PlayerBattleSettlementItemTest, InboxDeferredForFullBagsAppliesAfterARetrieveDuringTheBattle)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    constexpr Guid kParkedTemporaryItem = 9300003; // 在两个号段之外:不会与本例铸出的号撞上
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id());
    Inventory().SetCapacityForRestore(2); Temporary().SetCapacityForRestore(2);
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));                              // 人物背包 1/2
    Temporary().InsertItemForRestore(kParkedTemporaryItem, kSettlementNonStackItem, 1, 0, 1); // 临时格 1/2
    SetConsumed(kSettlementStackItem, 1); SetGained(kSettlementNonStackItem, 2);              // 要两格:哪个包都放不下
    ASSERT_EQ(0u, BagService::ConsecutiveGrantOverflowRejections(player));
    ASSERT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    ASSERT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    // 日志节拍的计数(§6.6「日志节拍」):每次重试都放不下就加一,只给 WARN 限频,不改任何判定。
    EXPECT_EQ(1u, BagService::ConsecutiveGrantOverflowRejections(player));
    ASSERT_EQ(PlayerInboxExecKind::kDefer, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(2u, BagService::ConsecutiveGrantOverflowRejections(player));

    ASSERT_EQ(kSuccess, PlayerBagSystem::RetrieveTemporary(player, kParkedTemporaryItem, false))
        << "the battle freeze must not block the retrieve the deferred settlement is waiting for";
    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, BagService::ConsecutiveGrantOverflowRejections(player)) << "a successful grant ends the streak";
    EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(kParkedTemporaryItem)) << "the retrieved item stays in the inventory";
    EXPECT_EQ(2u, Temporary().GetTotalItemCount(kSettlementNonStackItem)) << "both drops landed in the freed temporary bag";
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem)) << "the consumption was applied once";
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
}

// 收件箱一个域此刻的调度状态:取回之后的定向唤醒(PlayerInboxSystem::OnBagSpaceFreed)不许碰的那几样。
struct InboxDomainSchedule {
    uint32_t domain{0};
    uint64_t nextCheckMs{0};
    PlayerInboxDeferReason deferReason{PlayerInboxDeferReason::kNone};
    PlayerInboxPhase phase{PlayerInboxPhase::kIdle};
    uint32_t failStreak{0};
    bool operator==(const InboxDomainSchedule&) const = default;
};

std::vector<InboxDomainSchedule> InboxSchedulesExcept(entt::entity player, uint32_t skippedDomain) {
    std::vector<InboxDomainSchedule> schedules;
    for (const auto& state : tlsEcs.actorRegistry.get<PlayerInboxRuntimeComp>(player).domains)
        if (state.domain != skippedDomain)
            schedules.push_back({state.domain, state.nextCheckMs, state.deferReason, state.phase, state.failStreak});
    return schedules;
}

// 评审修正(2026-11-06):两包都满而整笔延后之后,取回成功经 PlayerInboxSystem::OnBagSpaceFreed 把这一域的下一次尝试
// 提到现在 —— 时钟不动、下一拍就整笔应用,不用等退避;没因背包放不下而延后的域(GM 处置域,局中被前置闸延后)一点不动。
// 走的是正式的收件箱状态机与执行器,只替换时钟、传输与存盘回执(ScopedCompositionInbox)。
TEST_F(PlayerBattleSettlementItemTest, InboxRetriesABagFullDeferOnTheNextTickAfterARetrieve)
{
    ScopedCompositionInbox inbox;
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
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
    constexpr Guid kParkedTemporaryItem = 9300004; // 在两个号段之外
    Inventory().SetCapacityForRestore(2); Temporary().SetCapacityForRestore(2);
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));                              // 人物背包 1/2
    Temporary().InsertItemForRestore(kParkedTemporaryItem, kSettlementNonStackItem, 1, 0, 1); // 临时格 1/2
    SetConsumed(kSettlementStackItem, 1); SetGained(kSettlementNonStackItem, 2);              // 要两格:哪个包都放不下

    PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gCompositionInboxFetches.size());
    RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);
    ASSERT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "both bags are full: the whole settlement waits";
    PlayerInboxSystem::Tick();
    ASSERT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "without a retrieve it waits for the backoff";
    const auto othersBefore = InboxSchedulesExcept(player, kCompositionBattleDomain);
    ASSERT_FALSE(othersBefore.empty()) << "the GM sanction domain is consumed as well";

    ASSERT_EQ(kSuccess, PlayerBagSystem::RetrieveTemporary(player, kParkedTemporaryItem, false));
    EXPECT_EQ(othersBefore, InboxSchedulesExcept(player, kCompositionBattleDomain))
        << "a domain that was not deferred for bag space is left alone";
    PlayerInboxSystem::Tick(); // 时钟没动:同一时刻的下一拍
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "applied on the next tick, without waiting for the backoff";
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(2u, Temporary().GetTotalItemCount(kSettlementNonStackItem)) << "both drops landed in the freed temporary bag";
    EXPECT_EQ(1u, gCompositionInboxFetches.size()) << "the local copy was retried, nothing was fetched again";
    EXPECT_EQ(othersBefore, InboxSchedulesExcept(player, kCompositionBattleDomain));
}

// 只提前「因背包放不下」的延后:执行器为别的原因延后(这里是近期战斗结果组件缺失,在任何改动之前延后)时,
// 取回成功也不动它的退避 —— 原因已经消失,也要等退避到点才重试。
TEST_F(PlayerBattleSettlementItemTest, InboxBagSpaceFreedLeavesOtherExecutorDefersOnTheirBackoff)
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
    constexpr Guid kParkedTemporaryItem = 9300005;
    Temporary().InsertItemForRestore(kParkedTemporaryItem, kSettlementNonStackItem, 1, 0, 1); // 有东西可取回
    tlsEcs.actorRegistry.remove<PlayerBattleResultComp>(player); // 执行器在任何改动之前延后,原因与背包无关

    PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
    ASSERT_EQ(1u, gCompositionInboxFetches.size());
    RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);
    ASSERT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "deferred before any effect";
    ASSERT_EQ(0u, BagService::ConsecutiveGrantOverflowRejections(player)) << "not a bag-space deferral";
    tlsEcs.actorRegistry.emplace<PlayerBattleResultComp>(player); // 原因已经消失,但退避还没到

    ASSERT_EQ(kSuccess, PlayerBagSystem::RetrieveTemporary(player, kParkedTemporaryItem, false));
    PlayerInboxSystem::Tick();
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "a deferral for another reason keeps its backoff";

    gCompositionInboxNowMs += player_inbox_rules::kBackoffMaxMs + 1;
    PlayerInboxSystem::Tick();
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "retried once the backoff is due";
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

// 同一条回归落在 BagService 一层(审计 cpp-economy#2):临时格满了不淘汰,整笔拒绝发生在 beforeCommit(货币)之前 ——
// 回调一次都没跑,两个包逐字节不变,临时格里最早那件还在,也没有写任何流水。
TEST_F(PlayerBattleSettlementItemTest, PreparedExchangeFullTemporaryBagRefusesTheWholeGrantWithoutEvicting)
{
    ScopedPreparedExchangeSegments segments;
    ASSERT_TRUE(segments.Install());
    constexpr Guid kOldestTemporaryItem = 9100002;
    Inventory().SetCapacityForRestore(1); Temporary().SetCapacityForRestore(1);
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3));
    Temporary().InsertItemForRestore(kOldestTemporaryItem, kSettlementNonStackItem, 1, 0, 1);
    const auto items = ItemBytes();
    const auto temporarySlots = Temporary().GridSlots();
    const auto txBefore = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Available();
    int credits = 0;
    EXPECT_NE(kSuccess, BagService::ConsumeInventoryAndGrant(player,
        {{kSettlementStackItem, 1}}, {{kSettlementDropItem, 1}}, 7006, "prepared-test", [&] {
            ++credits; return CurrencySystem::AddCurrency(player, kCurrencyGold, 5);
        }));
    EXPECT_EQ(0, credits) << "the currency callback only runs after every bag step has succeeded";
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(items, ItemBytes());
    EXPECT_NE(nullptr, Temporary().GetItemCompByGuid(kOldestTemporaryItem)) << "never evicted";
    EXPECT_EQ(1u, Temporary().OccupiedGridCount());
    EXPECT_EQ(temporarySlots, Temporary().GridSlots());
    EXPECT_EQ(txBefore, tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Available()) << "no transaction log was written";
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

// 领段请求只计数、不自动回包的 item 号段(r2-battle#3 用例):Warm 的首段请求随即落下 [lo, hi)。
// step 同时是 MaxStep(minStep / maxStep 留 0 = 钉在初值上)。
int gSettlementSegmentFetches = 0;
bool ArmCountingItemSegment(uint64_t lo, uint64_t hi, uint32_t step = 100000) {
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false;
    options.kindName = "item";
    options.initialStep = step;
    gSettlementSegmentFetches = 0;
    if (!client.Enable(options, [](const std::string&, uint32_t) { ++gSettlementSegmentFetches; return true; },
        [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
    client.Warm();
    client.OnResponse(0, lo, hi);
    return client.IsReady();
}

// 审计 R2 r2-battle#3(2026-11-06):「需要量 > 剩余 > 预取阈值、又没有下一段」。预检整笔 DEFER、一个号都不发,
// 而预取只在发号之后判阈值 —— 改动前没有人续段,这笔结算永远 DEFER,玩家的票停在 state 3。
// 现在预检判出不足就请号段客户端续一段(单飞),续的那一段落下之后的重试照常应用一次。
TEST_F(PlayerBattleSettlementItemTest, ItemGuidShortageAboveThePrefetchThresholdRequestsATopUp) {
    ScopedItemSegmentBaseline restoreBaseline;
    SetGained(kSettlementNonStackItem, 3);  // 3 件不可叠加 = 3 个新实例
    ASSERT_TRUE(ArmCountingItemSegment(99700000, 99700002));  // 剩 2 个号;预取阈值 ceil(0.1 × 2) = 1,剩余在阈值之上
    ASSERT_EQ(1, gSettlementSegmentFetches) << "前置:只有 Warm 的那一次领段";

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement)) << "号不够:整笔 DEFER";
    EXPECT_EQ(2, gSettlementSegmentFetches) << "预检判出不足就续段";
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(2, gSettlementSegmentFetches) << "单飞:续段在途时重试不再多发";

    tlsGuidSegmentRegistry.Get(GuidKind::kItem).OnResponse(0, 99800000, 99900000);  // 续的那一段落下
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementNonStackItem), 3u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(KillProgress(), 2u);
}

// 评审修正(2026-11-06):掉落要的新实例比号段一段最多能给的(MaxStep)还多,续段永远凑不齐(kExceedsMaxStep)。
// 战斗结算不能拒收:奖励与消耗都在这一笔里,拒了就丢奖励或白喝药。所以整笔 DEFER(打 ERROR)、原件留在收件箱,
// 一点效果都不落、一个号都不领;与资产通道记终局拒绝不同。调大 IdSegments 的 MaxStep 之后下一次重试自愈。
TEST_F(PlayerBattleSettlementItemTest, ItemGuidNeedAboveTheMaxStepDefersTheWholeSettlementWithoutFetching) {
    ScopedItemSegmentBaseline restoreBaseline;
    SetGained(kSettlementNonStackItem, 3);  // 3 个新实例
    ASSERT_TRUE(ArmCountingItemSegment(99700000, 99700002, /*step=*/2));  // 剩 2 个号;一段最多 2 个
    ASSERT_EQ(1, gSettlementSegmentFetches);

    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement)) << "续段凑不齐也只延后,不拒收";
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(1, gSettlementSegmentFetches) << "终局结论不领段";
    EXPECT_FALSE(tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().topUpPending);
    EXPECT_EQ(tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available(), 2u) << "一个号都没发";
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementNonStackItem), 0u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_EQ(0u, KillProgress());
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

// ---------------------------------------------------------------------------
// 战斗锁写入可靠性(turn-battle-gap-closure.md §10):写锁与 ctx 一条 Lua、续期只延长、没问到有界重试、
// 已 FIGHTING 的重复确认顺带续期、重建复核只有首发回 0 才撤销。
// 单测没有 Redis:每一次发送都「没问到」。经 ScopedLockRetryCapture 收下重试、手动触发才能驱动重试链;
// 观察点(ScopedLockScriptRecorder)记录每一次真正准备发出的锁脚本。脚本本身的语义由 BattleLockContract.* 盯。
// 固定的未来时刻(2100-01-01 UTC):TTL 按发送时刻的墙钟算,用发送前后两次取时夹住,不依赖真实时间的取值。
// ---------------------------------------------------------------------------

constexpr uint64_t kFarFutureMs = 4102444800000ull;

// 写锁的每一次发送都按组件现状取值;重发前门禁:实体上的冻结已不是这一局(取消 / 结算 / 判废之后)就不再写,
// 否则会复活一把已删的锁,或者覆盖下一局的锁。
// 修复前:PrepareBattle 发两条 SET,没问到只打日志,锁缺失到战斗结束;玩家这时完整重登就重建不出冻结。
TEST_F(PlayerBattleSettlementTest, BattleLockWriteFollowsComponentAndStopsOnceFreezeIsNoLongerThisBattle) {
    ScopedLockScriptRecorder recorder;
    ScopedLockRetryCapture retries;
    const uint64_t battleId = settlement.battle_id();
    const uint64_t prepareDeadlineMs = kFarFutureMs;
    const uint64_t deadlineMs = kFarFutureMs + 300000;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(battleId);
    inBattle.set_battle_node_id(3);
    inBattle.set_state(IN_BATTLE_STATE_PREPARING);
    inBattle.set_deadline_ms(deadlineMs);
    inBattle.set_prepare_deadline_ms(prepareDeadlineMs);

    // 首发:一次 write_lock,TTL 按备战期限算,ctx 是组件此刻的序列化(含只有备战时知道的 battle_node_id);没问到排一次重试。
    const uint64_t sentNotBeforeMs = TimeSystem::NowMillisecondsUTC();
    const uint64_t ttlSec = PlayerBattleSettlementTestAccess::WriteBattleLock(playerId, battleId);
    const uint64_t sentNotAfterMs = TimeSystem::NowMillisecondsUTC();
    ASSERT_EQ(gLockScriptCalls.size(), 1u);
    const LockScriptCall first = gLockScriptCalls.front();
    EXPECT_EQ(first.op, "write_lock");
    EXPECT_EQ(first.playerId, playerId);
    EXPECT_EQ(first.battleId, battleId);
    EXPECT_EQ(first.ttlSec, ttlSec);
    EXPECT_GE(ttlSec, (prepareDeadlineMs - sentNotAfterMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec);
    EXPECT_LE(ttlSec, (prepareDeadlineMs - sentNotBeforeMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec);
    EXPECT_EQ(first.ctxPayload, inBattle.SerializeAsString());
    ASSERT_EQ(gCapturedLockRetries.size(), 1u);

    // 重发之前确认已到达:写的就是 FIGHTING 与按冻结到期时刻算的 TTL,不会把锁写回备战短期限。
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    ASSERT_TRUE(FireNextLockRetry());
    ASSERT_EQ(gLockScriptCalls.size(), 2u);
    EXPECT_EQ(gLockScriptCalls.back().op, "write_lock");
    EXPECT_EQ(gLockScriptCalls.back().ctxPayload, inBattle.SerializeAsString());
    EXPECT_GT(gLockScriptCalls.back().ttlSec, first.ttlSec);
    ASSERT_EQ(gCapturedLockRetries.size(), 1u);

    // 实体上已是别的局的冻结:不写,链到此为止。
    inBattle.set_battle_id(battleId + 1);
    ASSERT_TRUE(FireNextLockRetry());
    EXPECT_EQ(gLockScriptCalls.size(), 2u);
    EXPECT_TRUE(gCapturedLockRetries.empty());

    // 没有冻结:什么都不发,返回 0。
    tlsEcs.actorRegistry.remove<InBattleComp>(player);
    EXPECT_EQ(PlayerBattleSettlementTestAccess::WriteBattleLock(playerId, battleId), 0u);
    EXPECT_EQ(gLockScriptCalls.size(), 2u);
    EXPECT_TRUE(gCapturedLockRetries.empty());
}

// 实体已不在本节点(玩家下线):写锁重试放弃,不替一个已经不在的冻结写锁。
TEST_F(PlayerBattleSettlementTest, BattleLockWriteRetryStopsWhenPlayerLeftThisNode) {
    ScopedLockScriptRecorder recorder;
    ScopedLockRetryCapture retries;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_PREPARING);
    inBattle.set_deadline_ms(kFarFutureMs);
    PlayerBattleSettlementTestAccess::WriteBattleLock(playerId, settlement.battle_id());
    ASSERT_EQ(LockScriptCount("write_lock"), 1u);

    tlsEcs.playerList.erase(playerId);
    ASSERT_TRUE(FireNextLockRetry());
    EXPECT_EQ(LockScriptCount("write_lock"), 1u);
    EXPECT_TRUE(gCapturedLockRetries.empty());
    RegisterPlayer();
}

// 重试链有界:次数用尽就放弃(metric=battle_lock_write_gave_up),不再排。
TEST_F(PlayerBattleSettlementTest, BattleLockWriteRetryChainStopsAtMaxAttempts) {
    ScopedLockScriptRecorder recorder;
    ScopedLockRetryCapture retries;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_PREPARING);
    inBattle.set_deadline_ms(kFarFutureMs);

    PlayerBattleSettlementTestAccess::WriteBattleLock(playerId, settlement.battle_id());
    while (FireNextLockRetry()) {
    }
    EXPECT_EQ(LockScriptCount("write_lock"), size_t{battle_lock::kLockWriteMaxAttempts});
}

// 首次确认:PREPARING -> FIGHTING,续期按事件期限的冻结最晚到期时刻(含续冻上限,FreezeHoldDeadlineMs,2026-11-06)
// 算 TTL,ctx 落 FIGHTING 供登录重建。
TEST_F(PlayerBattleSettlementTest, FirstConfirmUpgradesToFightingAndRenewsFromFreezeExpiry) {
    ScopedLockScriptRecorder recorder;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_PREPARING);
    inBattle.set_deadline_ms(kFarFutureMs);
    inBattle.set_prepare_deadline_ms(kFarFutureMs - 200000);
    BattleConfirmedEvent event;
    event.set_player_id(playerId);
    event.set_battle_id(settlement.battle_id());
    event.set_deadline_ms(kFarFutureMs + 50000);

    const uint64_t sentNotBeforeMs = TimeSystem::NowMillisecondsUTC();
    PlayerBattleSystem::ConfirmBattle(event);
    const uint64_t sentNotAfterMs = TimeSystem::NowMillisecondsUTC();

    const auto& current = tlsEcs.actorRegistry.get<InBattleComp>(player);
    EXPECT_EQ(current.state(), IN_BATTLE_STATE_FIGHTING);
    EXPECT_EQ(current.deadline_ms(), event.deadline_ms());
    ASSERT_EQ(gLockScriptCalls.size(), 1u);
    const LockScriptCall renewal = gLockScriptCalls.front();
    EXPECT_EQ(renewal.op, "renew_lock");
    EXPECT_EQ(renewal.ctxPayload, current.SerializeAsString());
    const uint64_t lockUntilMs = PlayerBattleSystem::FreezeHoldDeadlineMs(event.deadline_ms());
    EXPECT_GE(renewal.ttlSec, (lockUntilMs - sentNotAfterMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec);
    EXPECT_LE(renewal.ttlSec, (lockUntilMs - sentNotBeforeMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec);
}

// 已 FIGHTING 的重复确认(Kafka 重投 / battle 开局后 180 秒内每 10 秒的周期补发):不再升级、不改期限
// (期限以首次确认写进组件的为准,不信重投事件),但顺带发一次条件续期,补上首次续期的丢失。
// 修复前:已 FIGHTING 直接返回,一条续期都不发 —— 首次续期没落地时锁在「备战期限 + 60 秒」就过期,而战斗时限默认 300 秒。
// 脚本「锁值匹配才动、只延长」本身由 BattleLockContract.RenewalOnlyExtendsKeepsCtxInStepAndNeverCreates 在真实 Redis 上盯住。
TEST_F(PlayerBattleSettlementTest, RepeatedConfirmRenewsLockOnceFromComponentStateWithoutChangingIt) {
    ScopedLockScriptRecorder recorder;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    const uint64_t deadlineMs = kFarFutureMs;
    inBattle.set_deadline_ms(deadlineMs);
    BattleConfirmedEvent event;
    event.set_player_id(playerId);
    event.set_battle_id(settlement.battle_id());
    event.set_deadline_ms(deadlineMs + 50000);  // 重投事件带的期限不同:按它算 TTL 会多出 50 秒

    const uint64_t sentNotBeforeMs = TimeSystem::NowMillisecondsUTC();
    PlayerBattleSystem::ConfirmBattle(event);
    const uint64_t sentNotAfterMs = TimeSystem::NowMillisecondsUTC();

    const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
    ASSERT_NE(current, nullptr);
    EXPECT_EQ(current->state(), IN_BATTLE_STATE_FIGHTING);
    EXPECT_EQ(current->deadline_ms(), deadlineMs);

    ASSERT_EQ(gLockScriptCalls.size(), 1u);
    const LockScriptCall renewal = gLockScriptCalls.front();
    EXPECT_EQ(renewal.op, "renew_lock");
    EXPECT_EQ(renewal.playerId, playerId);
    EXPECT_EQ(renewal.battleId, settlement.battle_id());
    // TTL = (组件期限的冻结最晚到期时刻 - 发送时刻) / 1000 + kLockExtraTtlSec(最晚到期含续冻上限,FreezeHoldDeadlineMs)。
    const uint64_t lockUntilMs = PlayerBattleSystem::FreezeHoldDeadlineMs(deadlineMs);
    EXPECT_GE(renewal.ttlSec, (lockUntilMs - sentNotAfterMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec);
    EXPECT_LE(renewal.ttlSec, (lockUntilMs - sentNotBeforeMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec);
    // ctx 就是组件此刻的序列化:解回来是这一局的 FIGHTING 与组件期限。
    EXPECT_EQ(renewal.ctxPayload, current->SerializeAsString());

    // 下一条周期补发:再发一次(单测线程没有在途的重试链,去重不挡首发)。
    PlayerBattleSystem::ConfirmBattle(event);
    EXPECT_EQ(LockScriptCount("renew_lock"), 2u);

    // 别的局的迟到确认:玩家已在这一局里,不动组件,也不发任何锁脚本。
    BattleConfirmedEvent stale = event;
    stale.set_battle_id(settlement.battle_id() + 1);
    gLockScriptCalls.clear();
    PlayerBattleSystem::ConfirmBattle(stale);
    EXPECT_TRUE(gLockScriptCalls.empty());
    EXPECT_EQ(tlsEcs.actorRegistry.get<InBattleComp>(player).battle_id(), settlement.battle_id());
}

// 同一局的续期,在途链还没走到终态时,周期补发的首发让给它(什么都不发);在途链每次重发都按组件现状重算
// TTL 与 ctx;门禁放弃(这一局在实体上已不是 FIGHTING)是终态,摘掉登记之后下一次首发照常发出。
// 摘漏了的话,最后一段的首发会被永远挡住,续期再也发不出去。
TEST_F(PlayerBattleSettlementTest, RenewChainYieldsToInFlightChainAndReleasesOnGateAbandon) {
    ScopedLockScriptRecorder recorder;
    ScopedLockRetryCapture retries;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    const uint64_t deadlineMs = kFarFutureMs;
    inBattle.set_deadline_ms(deadlineMs);
    BattleConfirmedEvent event;
    event.set_player_id(playerId);
    event.set_battle_id(settlement.battle_id());
    event.set_deadline_ms(deadlineMs);

    // 首发:发一次续期,没问到 → 排上一次重试,登记在途。
    PlayerBattleSystem::ConfirmBattle(event);
    ASSERT_EQ(LockScriptCount("renew_lock"), 1u);
    ASSERT_EQ(gCapturedLockRetries.size(), 1u);

    // 下一条周期补发:同键已有在途链 → 让路,不发、不再排重试。
    PlayerBattleSystem::ConfirmBattle(event);
    EXPECT_EQ(LockScriptCount("renew_lock"), 1u);
    EXPECT_EQ(gCapturedLockRetries.size(), 1u);

    // 在途链的重发按组件现状重算:把组件期限提前 100 秒,重发的 TTL 随之变短,ctx 是组件此刻的序列化。
    const uint64_t earlierDeadlineMs = deadlineMs - 100000;
    tlsEcs.actorRegistry.get<InBattleComp>(player).set_deadline_ms(earlierDeadlineMs);
    ASSERT_TRUE(FireNextLockRetry());
    ASSERT_EQ(LockScriptCount("renew_lock"), 2u);
    const LockScriptCall resend = gLockScriptCalls.back();
    EXPECT_EQ(resend.op, "renew_lock");
    EXPECT_LT(resend.ttlSec, gLockScriptCalls.front().ttlSec);
    EXPECT_EQ(resend.ctxPayload, tlsEcs.actorRegistry.get<InBattleComp>(player).SerializeAsString());
    ASSERT_EQ(gCapturedLockRetries.size(), 1u);  // 又没问到,排下一次

    // 门禁放弃:组件被摘(结算 / 判废已在本节点收尾),下一次重发什么都不发,链到此为止。
    tlsEcs.actorRegistry.remove<InBattleComp>(player);
    ASSERT_TRUE(FireNextLockRetry());
    EXPECT_EQ(LockScriptCount("renew_lock"), 2u);
    EXPECT_TRUE(gCapturedLockRetries.empty());

    // 登记已摘:这一局重新处于 FIGHTING 时,下一条周期补发的首发照常发出。
    auto& again = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    again.set_battle_id(settlement.battle_id());
    again.set_state(IN_BATTLE_STATE_FIGHTING);
    again.set_deadline_ms(deadlineMs);
    PlayerBattleSystem::ConfirmBattle(event);
    EXPECT_EQ(LockScriptCount("renew_lock"), 3u);
    // 剩下的这条链由 ScopedLockRetryCapture 析构时跑到次数用尽,不残留登记。
}

// 次数用尽也是终态:摘掉登记,下一条周期补发重新起链。摘漏了,Redis 恢复之后这一局的续期再也发不出去。
TEST_F(PlayerBattleSettlementTest, RenewChainReleasesAfterRetriesAreExhausted) {
    ScopedLockScriptRecorder recorder;
    ScopedLockRetryCapture retries;
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(settlement.battle_id());
    inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    inBattle.set_deadline_ms(kFarFutureMs);
    BattleConfirmedEvent event;
    event.set_player_id(playerId);
    event.set_battle_id(settlement.battle_id());
    event.set_deadline_ms(kFarFutureMs);

    PlayerBattleSystem::ConfirmBattle(event);
    while (FireNextLockRetry()) {
    }
    EXPECT_EQ(LockScriptCount("renew_lock"), size_t{battle_lock::kLockWriteMaxAttempts});

    PlayerBattleSystem::ConfirmBattle(event);
    EXPECT_EQ(LockScriptCount("renew_lock"), size_t{battle_lock::kLockWriteMaxAttempts} + 1);
}

// 重建复核只有「首发拿到 0」才撤销重建 —— 那说明锁在「读锁」与「复核」之间被结算 / 取消删掉。
// 重试拿到的 0 可能只是锁按 TTL 自然过期(重试与读锁隔着数秒到数十秒),撤销会把仍在打的玩家解冻,所以保留冻结。
TEST_F(PlayerBattleSettlementTest, RebuildRecheckRevertsOnlyOnFirstAttemptZero) {
    using battle_lock::ReplyShape;
    const uint64_t battleId = settlement.battle_id();
    const auto freeze = [this](uint64_t id) {
        auto& inBattle = tlsEcs.actorRegistry.emplace_or_replace<InBattleComp>(player);
        inBattle.set_battle_id(id);
        inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
    };

    // 重试拿到 0:保留。
    freeze(battleId);
    PlayerBattleSettlementTestAccess::RebuiltFreezeRecheckReply(playerId, battleId, 2, ReplyShape::kInteger, 0);
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));

    // 首发拿到 1(锁仍是这一局):重建成立,保留。
    PlayerBattleSettlementTestAccess::RebuiltFreezeRecheckReply(playerId, battleId, 1, ReplyShape::kInteger, 1);
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));

    // 没问到不是结论:保留(重试由发送方排)。
    PlayerBattleSettlementTestAccess::RebuiltFreezeRecheckReply(playerId, battleId, 1, ReplyShape::kNoReply, 0);
    PlayerBattleSettlementTestAccess::RebuiltFreezeRecheckReply(playerId, battleId, 1, ReplyShape::kError, 0);
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));

    // 实体上已是别的局的冻结:首发拿到 0 也不碰它。
    freeze(battleId + 1);
    PlayerBattleSettlementTestAccess::RebuiltFreezeRecheckReply(playerId, battleId, 1, ReplyShape::kInteger, 0);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));
    EXPECT_EQ(tlsEcs.actorRegistry.get<InBattleComp>(player).battle_id(), battleId + 1);

    // 首发拿到 0 且组件仍是这一局:撤销。
    freeze(battleId);
    PlayerBattleSettlementTestAccess::RebuiltFreezeRecheckReply(playerId, battleId, 1, ReplyShape::kInteger, 0);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(player));

    // 实体不在本节点(登记表里没有):什么都不做,也不崩。
    tlsEcs.playerList.erase(playerId);
    PlayerBattleSettlementTestAccess::RebuiltFreezeRecheckReply(playerId, battleId, 1, ReplyShape::kInteger, 0);
    RegisterPlayer();
}

// ---------------------------------------------------------------------------
// 战斗锁脚本契约(battle_lock_rules.h)。脚本文本契约不需要 Redis;真实 Redis 用例由 MMORPG_TEST_REDIS_ADDR 门控,
// 直接执行生产脚本,KEYS / ARGV 顺序与 player_battle.cpp 的 EVAL 一一对应,改脚本键位时这里先红。
// ---------------------------------------------------------------------------

struct RedisReplyDeleter {
    void operator()(redisReply* reply) const { freeReplyObject(reply); }
};

// 键按生产格式拼,所以在 Redis Cluster 上也同 slot。player_id 取 ≥ 2^63 的合成值:真实 player_id 在 63 位以内,
// 碰不到开发 Redis 里的真实键;每个夹具一个 nonce,重复运行互不干扰。析构时删掉本夹具碰过的两把键。
struct BattleLockRedisFixture {
    using Reply = std::unique_ptr<redisReply, RedisReplyDeleter>;
    std::unique_ptr<redisContext, decltype(&redisFree)> context{nullptr, &redisFree};
    uint64_t playerId = 0;
    std::string lockKey;
    std::string ctxKey;

    BattleLockRedisFixture() {
        const auto nonce = static_cast<uint64_t>(std::chrono::steady_clock::now().time_since_epoch().count());
        playerId = (uint64_t{1} << 63) + nonce % (uint64_t{1} << 62);
        lockKey = KeyOf(battle_lock::kBattleLockKeyFmt);
        ctxKey = KeyOf(battle_lock::kBattleCtxKeyFmt);
    }
    ~BattleLockRedisFixture() {
        if (context == nullptr || context->err != 0) return;
        (void)Run({"DEL", lockKey, ctxKey});
    }
    std::string KeyOf(const char* perPlayerFmt) const {
        char key[128];
        std::snprintf(key, sizeof(key), perPlayerFmt, static_cast<unsigned long long>(playerId));
        return key;
    }
    // endpoint = "host:port"(MMORPG_TEST_REDIS_ADDR)。
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
    // EVAL 参数顺序与 player_battle.cpp 一致:KEYS = 锁、ctx;ARGV = battle_id、TTL 秒、ctx 序列化。
    Reply RunLockScript(const char* script, const std::string& battle, const std::string& ttlSec, const std::string& ctx) {
        return Run({"EVAL", script, "2", lockKey, ctxKey, battle, ttlSec, ctx});
    }
    long long TtlOf(const std::string& key) {
        const auto reply = Run({"TTL", key});
        return (reply != nullptr && reply->type == REDIS_REPLY_INTEGER) ? reply->integer : -3;
    }
};

void ExpectInteger(const BattleLockRedisFixture::Reply& reply, long long expected) {
    ASSERT_NE(nullptr, reply);
    ASSERT_EQ(REDIS_REPLY_INTEGER, reply->type);
    EXPECT_EQ(expected, reply->integer);
}

void ExpectString(const BattleLockRedisFixture::Reply& reply, const std::string& expected) {
    ASSERT_NE(nullptr, reply);
    ASSERT_EQ(REDIS_REPLY_STRING, reply->type);
    EXPECT_EQ(expected, std::string(reply->str, reply->len));
}

void ExpectStatusOk(const BattleLockRedisFixture::Reply& reply) {
    ASSERT_NE(nullptr, reply);
    ASSERT_EQ(REDIS_REPLY_STATUS, reply->type);
}

// 续期脚本绝不写锁的值:锁值不符先返回 0,之后才动 ctx 与 TTL。补写锁会让一条迟到的确认复活被取消 / 结算 / 判废删掉的锁。
TEST(BattleLockContract, RenewalNeverWritesTheLockValue) {
    const std::string script = battle_lock::kRenewBattleLockIfMatchScript;
    EXPECT_EQ(script.find("redis.call('SET', KEYS[1]"), std::string::npos);
    const auto guard = script.find("if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end");
    ASSERT_NE(guard, std::string::npos);
    const auto ctxWrite = script.find("redis.call('SET', KEYS[2], ARGV[3], 'EX', ttl)");
    const auto lockExpire = script.find("redis.call('EXPIRE', KEYS[1], ttl)");
    ASSERT_NE(ctxWrite, std::string::npos);
    ASSERT_NE(lockExpire, std::string::npos);
    EXPECT_LT(guard, ctxWrite);
    EXPECT_LT(guard, lockExpire);
}

// 写锁脚本:锁与 ctx 在同一条脚本里、用同一个 TTL 参数,两键要么都写、要么都没写。
TEST(BattleLockContract, WriteSetsLockAndCtxWithTheSameTtl) {
    const std::string script = battle_lock::kWriteBattleLockScript;
    EXPECT_NE(script.find("redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])"), std::string::npos);
    EXPECT_NE(script.find("redis.call('SET', KEYS[2], ARGV[3], 'EX', ARGV[2])"), std::string::npos);
}

// 只延长、不缩短;ctx 与锁同 TTL;锁属于别的局两键都不动;锁不在回 0 且不补写。
TEST(BattleLockContract, RenewalOnlyExtendsKeepsCtxInStepAndNeverCreates) {
    const char* address = std::getenv("MMORPG_TEST_REDIS_ADDR");
    if (address == nullptr || *address == '\0')
        GTEST_SKIP() << "未设置 MMORPG_TEST_REDIS_ADDR，跳过真实 Redis 战斗锁续期契约";
    BattleLockRedisFixture redis;
    ASSERT_NO_FATAL_FAILURE(redis.Connect(address));
    const char* renew = battle_lock::kRenewBattleLockIfMatchScript;

    // 锁 870 还剩 500 秒;一条重复确认只要 100 秒:不缩短。
    ASSERT_NO_FATAL_FAILURE(ExpectStatusOk(redis.Run({"SET", redis.lockKey, "870", "EX", "500"})));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.RunLockScript(renew, "870", "100", "ctx-a"), 1));
    const long long keptTtl = redis.TtlOf(redis.lockKey);
    EXPECT_GT(keptTtl, 400);
    EXPECT_LE(keptTtl, 500);
    EXPECT_LE(std::llabs(keptTtl - redis.TtlOf(redis.ctxKey)), 1);
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.ctxKey}), "ctx-a"));

    // 要 900 秒:延长,ctx 跟着改写与延长。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.RunLockScript(renew, "870", "900", "ctx-b"), 1));
    EXPECT_GT(redis.TtlOf(redis.lockKey), 800);
    EXPECT_GT(redis.TtlOf(redis.ctxKey), 800);
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.ctxKey}), "ctx-b"));

    // 锁属于别的局:回 0,锁与 ctx 原样。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.RunLockScript(renew, "871", "900", "ctx-c"), 0));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.lockKey}), "870"));
    ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.ctxKey}), "ctx-b"));

    // 锁不在(已被取消 / 结算 / 判废删掉):回 0,不补写。
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"DEL", redis.lockKey, redis.ctxKey}), 2));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.RunLockScript(renew, "870", "900", "ctx-d"), 0));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"EXISTS", redis.lockKey}), 0));
    ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.Run({"EXISTS", redis.ctxKey}), 0));
}

// 写锁:两键同值同 TTL;没问到之后的重发(同值)幂等。
TEST(BattleLockContract, WriteLandsLockAndCtxTogetherAndResendIsIdempotent) {
    const char* address = std::getenv("MMORPG_TEST_REDIS_ADDR");
    if (address == nullptr || *address == '\0')
        GTEST_SKIP() << "未设置 MMORPG_TEST_REDIS_ADDR，跳过真实 Redis 战斗锁写入契约";
    BattleLockRedisFixture redis;
    ASSERT_NO_FATAL_FAILURE(redis.Connect(address));
    const char* write = battle_lock::kWriteBattleLockScript;

    for (int send = 0; send < 2; ++send) {
        ASSERT_NO_FATAL_FAILURE(ExpectInteger(redis.RunLockScript(write, "880", "120", "ctx-880"), 1)) << "send " << send;
        ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.lockKey}), "880")) << "send " << send;
        ASSERT_NO_FATAL_FAILURE(ExpectString(redis.Run({"GET", redis.ctxKey}), "ctx-880")) << "send " << send;
        const long long lockTtl = redis.TtlOf(redis.lockKey);
        EXPECT_GT(lockTtl, 0) << "send " << send;
        EXPECT_LE(lockTtl, 120) << "send " << send;
        EXPECT_LE(std::llabs(lockTtl - redis.TtlOf(redis.ctxKey)), 1) << "send " << send;
    }
}

}  // namespace

// ============================================================================
// 战斗快照的等级读点(2026-11-05;混版本持久字段保护设计 §12.2,规则见 player_level_rules.h 文件头)
// ============================================================================
#include "proto/common/component/player_network_comp.pb.h"  // PlayerSessionSnapshotComp:组快照先解析 gate 路由
#include "services/scene/player/system/player_level_rules.h"

namespace {

// 非 0 即可:组快照只要求会话号非 0。单测里没有发现过 gate 节点,gate_instance_id 留空、只告警不拒绝。
constexpr uint32_t kSnapshotLevelSessionId = 4242;

// 快照等级 = playerlevel::CappedLevel(存储等级):超上限按上限,0 按 1;LevelComp 的存储值原样携带,升回新版本后恢复。
// 引擎的伤害 / 回血 / 怪物参考等级都读快照等级:快照若照抄存储等级,超上限的玩家进了战斗就比合法的满级玩家更强。
// 本夹具默认不挂组快照要的加载组件(见 SnapshotRejectsMissingPersistentOrDerivedInputsBeforeChangingOutput),
// 这里补成能组出快照的最小玩家:空技能、空宠物、空背包都是合法的已加载状态。
TEST_F(PlayerBattleSettlementTest, SnapshotLevelIsTheCappedLevelAndTheStoredLevelIsCarried) {
    auto& registry = tlsEcs.actorRegistry;
    auto& level = registry.emplace<LevelComp>(player);
    registry.emplace<PlayerUint32Comp>(player);
    registry.emplace<PlayerProfileComp>(player);
    registry.emplace<PlayerSkillListComp>(player);
    registry.emplace<PlayerPetComp>(player);
    registry.emplace<PlayerBagsComp>(player);
    registry.emplace<PlayerSessionSnapshotComp>(player).set_gate_session_id(kSnapshotLevelSessionId);

    const auto expectSnapshotLevel = [&](uint32_t storedLevel, uint32_t effectiveLevel) {
        level.set_level(storedLevel);
        BattlePlayerSnapshot snapshot;
        uint32_t errorTipId = kSuccess;
        ASSERT_TRUE(PlayerBattleSettlementTestAccess::Snapshot(player, snapshot, errorTipId))
            << "stored level " << storedLevel << " tip " << errorTipId;
        EXPECT_EQ(playerId, snapshot.player_id());
        EXPECT_EQ(effectiveLevel, snapshot.level()) << "stored level " << storedLevel;
        EXPECT_EQ(storedLevel, level.level()) << "组快照不得改写存储等级";
    };
    // 新版本调高上限后回滚 scene、或混版本跨 zone 迁移带来的超上限等级
    expectSnapshotLevel(playerlevel::kMaxLevel + 15, playerlevel::kMaxLevel);
    // 新号加载期间(InitPlayerFromAllData 写 1 之前)的 0 级
    expectSnapshotLevel(0, 1);
    // 对照:上限本身原样生效
    expectSnapshotLevel(playerlevel::kMaxLevel, playerlevel::kMaxLevel);
}

}  // namespace

namespace {
std::vector<std::function<void(redisReply*)>> coroutineLockReplies;
int CaptureCoroutineLock(const char*, uint64_t, uint64_t, uint64_t, const std::string&, std::function<void(redisReply*)> done) {
    coroutineLockReplies.push_back(std::move(done));
    return REDIS_OK;
}
int RejectCoroutineLock(const char*, uint64_t, uint64_t, uint64_t, const std::string&, std::function<void(redisReply*)>) {
    return REDIS_ERR;
}
}

TEST_F(PlayerBattleSettlementTest, CoroutineLockWaitAndStopIgnoreLateErrorAndTimer) {
    ScopedLockScriptRecorder recorder;
    ScopedLockRetryCapture retries;
    coroutineLockReplies.clear();
    PlayerBattleSystem::SetLockScriptSenderForTest(&CaptureCoroutineLock);
    auto& freeze = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    freeze.set_battle_id(settlement.battle_id());
    freeze.set_state(IN_BATTLE_STATE_FIGHTING);
    freeze.set_deadline_ms(9000000000000ULL);
    ASSERT_GT(PlayerBattleSettlementTestAccess::WriteBattleLock(playerId, settlement.battle_id()), 0u);
    ASSERT_EQ(coroutineLockReplies.size(), 1u);
    EXPECT_TRUE(gCapturedLockRetries.empty());
    auto* loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
    bool progressed = false;
    loop->queueInLoop([&] { progressed = true; loop->quit(); });
    loop->loop();
    EXPECT_TRUE(progressed);
    recorder.Stop();
    coroutineLockReplies[0](nullptr);
    EXPECT_TRUE(gCapturedLockRetries.empty());
    EXPECT_EQ(LockScriptCount("write_lock"), 1u);
    coroutineLockReplies.clear();
}

TEST_F(PlayerBattleSettlementTest, CoroutineLockRejectRetriesThenRechecksCurrentFreeze) {
    ScopedLockScriptRecorder recorder;
    ScopedLockRetryCapture retries;
    PlayerBattleSystem::SetLockScriptSenderForTest(&RejectCoroutineLock);
    auto& freeze = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    freeze.set_battle_id(settlement.battle_id());
    freeze.set_state(IN_BATTLE_STATE_FIGHTING);
    freeze.set_deadline_ms(9000000000000ULL);
    ASSERT_GT(PlayerBattleSettlementTestAccess::WriteBattleLock(playerId, settlement.battle_id()), 0u);
    ASSERT_EQ(gCapturedLockRetries.size(), 1u);
    tlsEcs.actorRegistry.remove<InBattleComp>(player);
    ASSERT_TRUE(FireNextLockRetry());
    EXPECT_EQ(LockScriptCount("write_lock"), 1u);
    EXPECT_TRUE(gCapturedLockRetries.empty());
}

TEST_F(PlayerBattleSettlementTest, CoroutineLockIntegerIsFinalAndDuplicateReplyCannotRetry) {
    ScopedLockScriptRecorder recorder;
    ScopedLockRetryCapture retries;
    coroutineLockReplies.clear();
    PlayerBattleSystem::SetLockScriptSenderForTest(&CaptureCoroutineLock);
    auto& freeze = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    freeze.set_battle_id(settlement.battle_id());
    freeze.set_state(IN_BATTLE_STATE_FIGHTING);
    freeze.set_deadline_ms(9000000000000ULL);
    ASSERT_GT(PlayerBattleSettlementTestAccess::WriteBattleLock(playerId, settlement.battle_id()), 0u);
    auto reply = IntegerReply(0);
    coroutineLockReplies[0](&reply);
    coroutineLockReplies[0](nullptr);
    EXPECT_TRUE(gCapturedLockRetries.empty());
    EXPECT_TRUE(tlsEcs.actorRegistry.all_of<InBattleComp>(player));
    coroutineLockReplies.clear();
}

// ============================================================================
// 收件箱战斗结算遇到获得封禁(2026-11-05,负责人定):封禁是终局业务决定,只跳过被封的那部分。
// 被封的币种整笔不入账、被封的掉落不发,解封后都不补发;击杀、HP、未封的币种与掉落照常应用,结算记 APPLIED,
// Match 照常放 ready 票。与资产通道 Credit 的 REJECTED、已删除的旧结算路径的跳过同一口径。
// 2026-11-05 之前整笔 DEFER:封禁期间这一局永远到不了 APPLIED(玩家不能再排全球匹配),解封后又整笔补发。
// 冻结、溢出、满包、身份变化、版本错位仍整笔 DEFER / 停域(InboxLaterCurrencyOverflowRejectsMoneyItemsHealthAndMissionTogether、
// InboxFullBagsKeepConsumptionGoldHealthAndKillsUnchanged 等);BagService「任一项被封整批拒绝」的契约不变
// (PreparedExchangeGainBlockRejectsBeforeConsumptionOrCredit)。单人封禁与全服封禁走同一个判定,单人金币封禁见
// InboxGoldBlockSkipsOnlyGoldAndAppliesTheRest。
// ============================================================================
#include "proto/common/component/player_item_gain_block_comp.pb.h"  // PlayerItemGainBlockComp:单人物品获得封禁

namespace {

// 全服封禁一项获得,析构时还原(用例开始前就封着的不解封)。
struct ScopedGlobalGainBlock {
    ScopedGlobalGainBlock(GainBlockService::GainType blockedType, uint32_t blockedId)
        : type(blockedType), id(blockedId), wasBlocked(GainBlockService::IsGloballyBlocked(blockedType, blockedId)) {
        GainBlockService::BlockGlobal(type, id);
    }
    ~ScopedGlobalGainBlock() { if (!wasBlocked) GainBlockService::UnblockGlobal(type, id); }
    ScopedGlobalGainBlock(const ScopedGlobalGainBlock&) = delete;
    ScopedGlobalGainBlock& operator=(const ScopedGlobalGainBlock&) = delete;
    const GainBlockService::GainType type;
    const uint32_t id;
    const bool wasBlocked;
};

// 全服封禁了金币:同一笔里的钻石照常入账、HP 与击杀照常,收件箱按 APPLIED 确认(Match 只凭 APPLIED 收据放 ready 票)。
// 解封之后同一条目再投一次:收件箱按账本里的原结局回报、不再执行,被跳过的金币不补发。
TEST_F(PlayerBattleSettlementTest, InboxGlobalCurrencyBlockSkipsOnlyThatCurrencyAndUnblockDoesNotRepay)
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
    AddCurrencyGain(kCurrencyGold, 12);
    AddCurrencyGain(kCurrencyDiamond, 5);
    player_inbox::AckPlayerInboxRequest ack;
    {
        ScopedGlobalGainBlock goldBlock(GainBlockService::GainType::kCurrency, static_cast<uint32_t>(kCurrencyGold));
        PlayerInboxSystem::OnPlayerLanded(player); PlayerInboxSystem::Tick();
        ASSERT_EQ(1u, gCompositionInboxFetches.size());
        RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);
        EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "被封的币种整笔不入账";
        EXPECT_EQ(5u, CurrencySystem::GetBalance(player, kCurrencyDiamond)) << "同一笔里未封的币种照常入账";
        EXPECT_EQ(2u, KillProgress());
        EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
        ASSERT_EQ(1, Results().entries_size()) << "已应用的局照常记近期结果";
        ASSERT_EQ(1, Results().entries(0).currencies_size()) << "近期结果只记实际入账,被跳过的币种不记";
        EXPECT_EQ(static_cast<uint32_t>(kCurrencyDiamond), Results().entries(0).currencies(0).currency_type());
        EXPECT_EQ(5u, Results().entries(0).currencies(0).amount());

        ASSERT_NO_FATAL_FAILURE(CommitInboxLedgerSnapshot());
        PlayerInboxSystem::Tick();
        ASSERT_EQ(1u, gCompositionInboxAcks.size());
        ack = gCompositionInboxAcks.back();
        ASSERT_EQ(1, ack.decisions_size());
        EXPECT_EQ(player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, ack.decisions(0).outcome())
            << "封禁期间照样记 APPLIED,不再整笔 DEFER";
    }

    // 收件箱服务确认落定;金币此时已解封(goldBlock 已析构),同一条目再投一次。
    ASSERT_FALSE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency, static_cast<uint32_t>(kCurrencyGold)));
    player_inbox::AckPlayerInboxResponse acknowledged;
    acknowledged.set_player_id(playerId); acknowledged.set_domain(ack.domain());
    acknowledged.set_correlation_id(ack.correlation_id()); acknowledged.set_fence(player_inbox::PLAYER_INBOX_FENCE_MATCH);
    auto* finalized = acknowledged.add_results(); finalized->set_seq(ack.decisions(0).seq());
    finalized->set_result(player_inbox::PLAYER_INBOX_ACK_FINALIZED); finalized->set_state(player_inbox::PLAYER_INBOX_ITEM_APPLIED);
    PlayerInboxSystem::OnAckReply(playerId, kCompositionBattleDomain, ack.correlation_id(), acknowledged);
    gCompositionInboxNowMs += player_inbox_rules::kMinCheckGapMs + 1;
    PlayerInboxSystem::OnNotifyEvent(playerId, kCompositionBattleDomain); PlayerInboxSystem::Tick();
    ASSERT_EQ(2u, gCompositionInboxFetches.size());
    RespondWithBattleSettlement(gCompositionInboxFetches.back(), playerId, settlement, 10);
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "解封不补发";
    EXPECT_EQ(5u, CurrencySystem::GetBalance(player, kCurrencyDiamond)) << "重放不再执行";
    EXPECT_EQ(1, Results().entries_size()) << "重放不写第二条近期结果";
}

// 全服封禁了一种掉落:只剔除这一项(不发、也不改投临时格),其余掉落、消耗、金币、击杀与 HP 照常应用,执行器回 APPLIED。
// 号段预检按剔除之后的掉落算:被封的 3 件不可叠加物不铸号,号段只剩 1 个号也够发未封的那一堆。
TEST_F(PlayerBattleSettlementItemTest, InboxGlobalItemBlockSkipsOnlyTheBlockedDropAndAppliesTheRest)
{
    ScopedItemSegmentBaseline restoreBaseline;
    ASSERT_NO_FATAL_FAILURE(GiveItem(kSettlementStackItem, 3)); // 铺底在换号段之前,用的是基线号段
    SetConsumed(kSettlementStackItem, 1);
    SetGained(kSettlementNonStackItem, 3); // 被封:3 件不可叠加 = 3 个新实例
    SetGained(kSettlementDropItem, 2);     // 未封:可叠加,1 个新实例
    ScopedGlobalGainBlock block(GainBlockService::GainType::kItem, kSettlementNonStackItem);
    ASSERT_TRUE(ArmSettlementMissionItemSegment(99700000, 99700001)); // 只剩 1 个号

    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind)
        << "2026-11-05 之前:号段预检按原件要 4 个号、ConsumeInventoryAndGrant 整批回 kAssetBlocked,都整笔 DEFER";
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kSettlementNonStackItem)) << "被封的掉落不发";
    EXPECT_EQ(0u, Temporary().GetTotalItemCount(kSettlementNonStackItem)) << "也不改投临时格";
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementDropItem)) << "未封的掉落照常入包";
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementStackItem)) << "消耗照常扣";
    EXPECT_EQ(0u, tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available()) << "只为未封的那一堆铸了 1 个号";
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_TRUE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, kSettlementNonStackItem))
        << "结算不改封禁状态";
    ASSERT_EQ(1, Results().entries_size()) << "已应用的局照常记近期结果";
    const auto& recorded = Results().entries(0);
    ASSERT_EQ(1, recorded.items_gained_size()) << "近期结果只记实际入包的掉落";
    EXPECT_EQ(kSettlementDropItem, recorded.items_gained(0).item_table_id());
    EXPECT_EQ(2u, recorded.items_gained(0).count());
}

// 本人被 GM 封禁获得某物品(PlayerItemGainBlockComp):与全服封禁走同一个判定 GainBlockService::IsItemGainBlocked,
// 同样只剔除这一项。剔除若只看全服集合,被单人封禁的掉落仍会让 ConsumeInventoryAndGrant 整批回 kAssetBlocked、整笔无限 DEFER。
TEST_F(PlayerBattleSettlementItemTest, InboxPlayerItemBlockSkipsOnlyTheBlockedDropAndAppliesTheRest)
{
    ScopedPreparedExchangeSegments segments; ASSERT_TRUE(segments.Install());
    tlsEcs.actorRegistry.emplace<PlayerItemGainBlockComp>(player); // 持久组件,生产里由加载创建
    ASSERT_EQ(kSuccess, GainBlockService::BlockPlayerItemGain(player, kSettlementNonStackItem));
    ASSERT_FALSE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, kSettlementNonStackItem))
        << "前置:只有单人封禁";
    SetGained(kSettlementNonStackItem, 1); // 本人被封
    SetGained(kSettlementDropItem, 2);     // 未封

    EXPECT_EQ(PlayerInboxExecKind::kApplied, PlayerBattleSystem::ApplyInboxSettlement(player, settlement.SerializeAsString()).kind);
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kSettlementNonStackItem)) << "被封的掉落不发";
    EXPECT_EQ(0u, Temporary().GetTotalItemCount(kSettlementNonStackItem)) << "也不改投临时格";
    EXPECT_EQ(2u, Inventory().GetTotalItemCount(kSettlementDropItem)) << "未封的掉落照常入包";
    EXPECT_EQ(12u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(2u, KillProgress());
    EXPECT_EQ(80u, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
    EXPECT_TRUE(GainBlockService::IsPlayerItemGainBlocked(player, kSettlementNonStackItem)) << "结算不改封禁状态";
    ASSERT_EQ(1, Results().entries_size()) << "已应用的局照常记近期结果";
    ASSERT_EQ(1, Results().entries(0).items_gained_size()) << "近期结果只记实际入包的掉落";
    EXPECT_EQ(kSettlementDropItem, Results().entries(0).items_gained(0).item_table_id());
}

}  // namespace
