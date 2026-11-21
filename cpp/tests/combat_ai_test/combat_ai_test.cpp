#include <array>
#include <cassert>
#include <cstdint>
#include <iostream>
#include <string>
#include <vector>

#include "modules/combat_ai/behavior_tree.h"
#include "modules/combat_ai/fsm.h"
#include "modules/combat_ai/hfsm.h"
#include "modules/combat_ai/monster_utility_ai.h"
#include "modules/combat_ai/state_mutex_matrix.h"

// 纯 C++ 标准库自检测试(零第三方依赖,无裸指针成员):
// 覆盖 StateMutexMatrix / Fsm / Hfsm / BehaviorTree / MonsterUtilityAi 以及副本 Boss 组合闭环。

namespace
{

void TestStateMutexMatrix()
{
    enum State : uint32_t
    {
        kCombat = 0,
        kTeamFollow = 1,
        kMounted = 2,
        kSilence = 3,
        kStateCount = 4,
    };
    enum Action : uint32_t
    {
        kUseSkill = 0,
        kJoinFollow = 1,
        kMount = 2,
    };

    constexpr uint32_t kTipSuccess = 1000;
    constexpr uint32_t kTipSilenced = 1001;
    constexpr uint32_t kTipInCombat = 1002;
    constexpr uint32_t kTipUnknownAction = 9001;
    constexpr uint32_t kTipInvalidState = 9002;

    combat_ai::StateMutexMatrix matrix;
    const std::array<combat_ai::StateRule, 5> rules{{
        {kUseSkill, kCombat, combat_ai::StateMode::kAllow, 0},
        {kUseSkill, kTeamFollow, combat_ai::StateMode::kInterrupt, 0},
        {kUseSkill, kMounted, combat_ai::StateMode::kInterrupt, 0},
        {kUseSkill, kSilence, combat_ai::StateMode::kBlock, kTipSilenced},
        {kMount, kCombat, combat_ai::StateMode::kBlock, kTipInCombat},
    }};
    assert(matrix.LoadRules(rules) == 0);

    // 1) 多来源 Buff 引用计数位图:双层沉默移除一层仍保持沉默,全部移除才清位。
    combat_ai::RefCountedStateBitset states;
    assert(states.Acquire(kTeamFollow));
    assert(states.Acquire(kMounted));
    assert(states.Acquire(kSilence));
    assert(!states.Acquire(kSilence)); // 第二层沉默不重复翻位
    assert(states.RefCount(kSilence) == 2);

    // 2) 被沉默时放技能:两阶段门禁在只读阶段直接拒绝,不得提前打断跟随或骑乘。
    std::vector<uint32_t> exitedStates;
    combat_ai::StateExitHookRegistry<uint64_t> hooks;
    hooks.Register(kTeamFollow, [&](uint64_t /*actor*/, combat_ai::StateExitReason reason) {
        assert(reason == combat_ai::StateExitReason::kInterrupted);
        assert(!states.Has(kTeamFollow)); // 先删状态再触发钩子,防重入循环
        exitedStates.push_back(kTeamFollow);
    });
    hooks.Register(kMounted, [&](uint64_t /*actor*/, combat_ai::StateExitReason reason) {
        assert(reason == combat_ai::StateExitReason::kInterrupted);
        assert(!states.Has(kMounted));
        exitedStates.push_back(kMounted);
    });

    const uint32_t blockedTip = matrix.TryPerform(
        states.Mask(), kUseSkill, kCombat, kStateCount, kTipSuccess, kTipUnknownAction, kTipInvalidState,
        [&](uint32_t s) {
            if (states.ClearState(s))
            {
                hooks.NotifyExited(100, s, combat_ai::StateExitReason::kInterrupted);
            }
        },
        [&](uint32_t s) { states.Acquire(s); });
    assert(blockedTip == kTipSilenced);
    assert(states.Has(kTeamFollow));
    assert(states.Has(kMounted));
    assert(!states.Has(kCombat));
    assert(exitedStates.empty());

    // 3) 移除两层沉默后再施法:先自动打断 TeamFollow 与 Mounted 并触发退出钩子,再入 Combat 态。
    assert(!states.Release(kSilence));
    assert(states.Has(kSilence));
    assert(states.Release(kSilence));
    assert(!states.Has(kSilence));

    const uint32_t okTip = matrix.TryPerform(
        states.Mask(), kUseSkill, kCombat, kStateCount, kTipSuccess, kTipUnknownAction, kTipInvalidState,
        [&](uint32_t s) {
            if (states.ClearState(s))
            {
                hooks.NotifyExited(100, s, combat_ai::StateExitReason::kInterrupted);
            }
        },
        [&](uint32_t s) { states.Acquire(s); });
    assert(okTip == kTipSuccess);
    assert(!states.Has(kTeamFollow));
    assert(!states.Has(kMounted));
    assert(states.Has(kCombat));
    assert((exitedStates == std::vector<uint32_t>{kTeamFollow, kMounted}));

    // 4) 战斗中企图上马被拦截;非法入态与未知动作在副作用前拒绝。
    assert(matrix.CanPerform(states.Mask(), kMount, kTipSuccess, kTipUnknownAction) == kTipInCombat);
    assert(matrix.CanPerform(states.Mask(), 999, kTipSuccess, kTipUnknownAction) == kTipUnknownAction);
    assert(matrix.TryPerform(
               states.Mask(), kUseSkill, kStateCount, kStateCount, kTipSuccess, kTipUnknownAction, kTipInvalidState,
               [&](uint32_t) { assert(false); }, [&](uint32_t) { assert(false); }) == kTipInvalidState);
}

void TestFsm()
{
    enum class CastState : uint8_t
    {
        kIdle = 0,
        kWindup = 1,
        kChannel = 2,
        kRecovery = 3,
        kDead = 4,
        kCount = 5,
    };
    enum class CastEvent : uint8_t
    {
        kStartCast = 0,
        kWindupDone = 1,
        kChannelDone = 2,
        kRecoveryDone = 3,
        kDie = 4,
        kCount = 5,
    };

    struct CastCtx
    {
        bool hasMana{false};
        bool instantSkipRecovery{false};
        std::vector<std::string> trace{};
    };

    combat_ai::Fsm<CastState, CastEvent, CastCtx> fsm(CastState::kIdle);
    fsm.AddTransition(
        CastState::kIdle, CastEvent::kStartCast, CastState::kWindup,
        [](const CastCtx& c) { return c.hasMana; },
        [](CastState, CastState, CastEvent, CastCtx& c) { c.trace.push_back("tx:start"); });
    fsm.AddTransition(CastState::kWindup, CastEvent::kWindupDone, CastState::kChannel);
    fsm.AddTransition(CastState::kChannel, CastEvent::kChannelDone, CastState::kRecovery);
    fsm.AddTransition(CastState::kRecovery, CastEvent::kRecoveryDone, CastState::kIdle);
    fsm.AddAnyTransition(CastEvent::kDie, CastState::kDead);

    fsm.OnLeave(CastState::kIdle, [](CastCtx& c) { c.trace.push_back("leave:idle"); });
    fsm.OnEnter(CastState::kWindup, [](CastCtx& c) { c.trace.push_back("enter:windup"); });
    fsm.OnEnter(CastState::kRecovery, [&](CastCtx& c) {
        c.trace.push_back("enter:recovery");
        if (c.instantSkipRecovery)
        {
            // 测试回调内重入 Trigger:走有界延迟队列,不会打断当前切态栈。
            assert(fsm.Trigger(CastEvent::kRecoveryDone, c));
        }
    });
    fsm.OnEnter(CastState::kIdle, [](CastCtx& c) { c.trace.push_back("enter:idle"); });

    CastCtx ctx;
    assert(!fsm.CanTrigger(CastEvent::kStartCast, ctx));
    assert(!fsm.Trigger(CastEvent::kStartCast, ctx));
    assert(fsm.Is(CastState::kIdle));
    assert(ctx.trace.empty()); // Guard 拒绝零副作用

    ctx.hasMana = true;
    ctx.instantSkipRecovery = true;
    assert(fsm.Trigger(CastEvent::kStartCast, ctx));
    assert(fsm.Is(CastState::kWindup));
    assert((ctx.trace == std::vector<std::string>{"leave:idle", "tx:start", "enter:windup"}));

    assert(fsm.Trigger(CastEvent::kWindupDone, ctx));
    assert(fsm.Is(CastState::kChannel));
    assert(fsm.Trigger(CastEvent::kChannelDone, ctx));
    assert(fsm.Is(CastState::kIdle)); // Recovery 的 onEnter 重入触发 kRecoveryDone 后顺次消化回 Idle

    assert(fsm.Trigger(CastEvent::kDie, ctx));
    assert(fsm.Is(CastState::kDead));
    assert(!fsm.Trigger(CastEvent::kDie, ctx)); // allowSelf=false

    // ---- 完整 looplab/fsm 特性: 多源 AddEvents / 5 阶段钩子 / Before&Leave Cancel / Async 两阶段提交 / AvailableTransitions / Mermaid ----
    enum class DoorState : uint8_t
    {
        kClosed = 0,
        kOpen = 1,
        kLocked = 2,
        kCount = 3,
    };
    enum class DoorEvent : uint8_t
    {
        kOpen = 0,
        kClose = 1,
        kLock = 2,
        kCount = 3,
    };
    struct DoorCtx
    {
        bool cancelBefore{false};
        bool asyncLeave{false};
        std::vector<std::string> steps{};
    };

    combat_ai::Fsm<DoorState, DoorEvent, DoorCtx> door(DoorState::kClosed);
    door.AddEvents({
        {DoorEvent::kOpen, {DoorState::kClosed}, DoorState::kOpen},
        {DoorEvent::kClose, {DoorState::kOpen}, DoorState::kClosed},
        {DoorEvent::kLock, {DoorState::kClosed, DoorState::kOpen}, DoorState::kLocked},
    });
    door.OnBeforeEvent(DoorEvent::kOpen, [](auto& ev, DoorCtx& c) {
        c.steps.push_back("before:open");
        if (c.cancelBefore)
        {
            ev.Cancel(7004);
        }
    });
    door.OnBeforeAnyEvent([](auto&, DoorCtx& c) { c.steps.push_back("before:any"); });
    door.OnLeaveState(DoorState::kClosed, [](auto& ev, DoorCtx& c) {
        c.steps.push_back("leave:closed");
        if (c.asyncLeave)
        {
            ev.Async();
        }
    });
    door.OnEnterState(DoorState::kOpen, [](auto&, DoorCtx& c) { c.steps.push_back("enter:open"); });
    door.OnAfterEvent(DoorEvent::kOpen, [](auto&, DoorCtx& c) { c.steps.push_back("after:open"); });
    door.OnAfterAnyEvent([](auto&, DoorCtx& c) { c.steps.push_back("after:any"); });

    DoorCtx dctx;
    const auto avail = door.AvailableTransitions(dctx);
    assert((avail == std::vector<DoorEvent>{DoorEvent::kOpen, DoorEvent::kLock}));

    // 1) Before 阶段取消:零副作用且返回自定义错误码。
    dctx.cancelBefore = true;
    const auto canceledRes = door.TriggerEx(DoorEvent::kOpen, dctx);
    assert(canceledRes.status == combat_ai::FsmStatus::kCanceled);
    assert(canceledRes.cancelError == 7004u);
    assert(door.Is(DoorState::kClosed));
    assert((dctx.steps == std::vector<std::string>{"before:open"}));

    // 2) Leave 阶段 Async 挂起:期间新 Trigger 返回 kInProgress,Commit 后继续执行 Enter -> After。
    dctx.cancelBefore = false;
    dctx.asyncLeave = true;
    dctx.steps.clear();
    const auto asyncRes = door.TriggerEx(DoorEvent::kOpen, dctx);
    assert(asyncRes.PendingAsync());
    assert(door.HasPendingTransition());
    assert(door.Is(DoorState::kClosed));
    assert(door.TriggerEx(DoorEvent::kLock, dctx).status == combat_ai::FsmStatus::kInProgress);

    const auto commitRes = door.CommitPendingTransition(dctx);
    assert(commitRes.Ok());
    assert(!door.HasPendingTransition());
    assert(door.Is(DoorState::kOpen));
    assert((dctx.steps == std::vector<std::string>{
                              "before:open", "before:any", "leave:closed", "enter:open", "after:open", "after:any"}));

    // 3) 导出 Mermaid 流程图。
    const std::string mermaid = door.ExportMermaid(
        [](DoorState s) -> std::string_view {
            switch (s)
            {
            case DoorState::kClosed: return "Closed";
            case DoorState::kOpen: return "Open";
            case DoorState::kLocked: return "Locked";
            case DoorState::kCount: break;
            }
            return "";
        },
        [](DoorEvent e) -> std::string_view {
            switch (e)
            {
            case DoorEvent::kOpen: return "open";
            case DoorEvent::kClose: return "close";
            case DoorEvent::kLock: return "lock";
            case DoorEvent::kCount: break;
            }
            return "";
        });
    assert(mermaid.find("stateDiagram-v2") != std::string::npos);
    assert(mermaid.find("Closed --> Open : open") != std::string::npos);
    assert(mermaid.find("Open --> Locked : lock") != std::string::npos);
}

void TestHfsm()
{
    enum BossState : combat_ai::HfsmStateId
    {
        kRoot = 0,
        kPatrol = 1,
        kCombat = 2,
        kPhase1 = 3,
        kPhase2Enrage = 4,
        kDead = 5,
    };
    enum BossEvent : uint32_t
    {
        kAggro = 1,
        kTakeDamage = 2,
        kFatalDamage = 3,
    };

    struct BossCtx
    {
        int32_t hpPercent{100};
        int32_t combatEnterCount{0};
        int32_t combatExitCount{0};
        std::vector<std::string> log{};
    };

    combat_ai::Hfsm<BossCtx, BossEvent> hfsm;
    hfsm.AddState(kRoot, {
                             .parent = combat_ai::kInvalidHfsmState,
                             .initialChild = kPatrol,
                             .onEvent = [](const BossEvent& ev, BossCtx&) {
                                 return ev == kFatalDamage
                                            ? combat_ai::HfsmEventOutcome::TransitionTo(kDead)
                                            : combat_ai::HfsmEventOutcome::Ignored();
                             },
                         });
    hfsm.AddState(kPatrol, {
                               .parent = kRoot,
                               .onEnter = [](BossCtx& c) { c.log.push_back("enter:patrol"); },
                               .onExit = [](BossCtx& c) { c.log.push_back("exit:patrol"); },
                               .onEvent = [](const BossEvent& ev, BossCtx&) {
                                   return ev == kAggro
                                              ? combat_ai::HfsmEventOutcome::TransitionTo(kCombat)
                                              : combat_ai::HfsmEventOutcome::Ignored();
                               },
                           });
    hfsm.AddState(kCombat, {
                               .parent = kRoot,
                               .initialChild = kPhase1,
                               .onEnter = [](BossCtx& c) {
                                   ++c.combatEnterCount;
                                   c.log.push_back("enter:combat");
                               },
                               .onExit = [](BossCtx& c) {
                                   ++c.combatExitCount;
                                   c.log.push_back("exit:combat");
                               },
                           });
    hfsm.AddState(kPhase1, {
                               .parent = kCombat,
                               .onEnter = [](BossCtx& c) { c.log.push_back("enter:p1"); },
                               .onUpdate = [](BossCtx& c, uint32_t) -> std::optional<combat_ai::HfsmStateId> {
                                   if (c.hpPercent <= 50)
                                   {
                                       return kPhase2Enrage;
                                   }
                                   return std::nullopt;
                               },
                               .onExit = [](BossCtx& c) { c.log.push_back("exit:p1"); },
                           });
    hfsm.AddState(kPhase2Enrage, {
                                     .parent = kCombat,
                                     .onEnter = [](BossCtx& c) { c.log.push_back("enter:p2"); },
                                     .onExit = [](BossCtx& c) { c.log.push_back("exit:p2"); },
                                 });
    hfsm.AddState(kDead, {
                             .parent = kRoot,
                             .onEnter = [](BossCtx& c) { c.log.push_back("enter:dead"); },
                         });

    BossCtx ctx;
    assert(hfsm.Start(kRoot, ctx));
    assert(hfsm.CurrentLeaf() == kPatrol);
    assert(hfsm.IsInState(kRoot));
    assert(hfsm.IsInState(kPatrol));
    assert(!hfsm.IsInState(kCombat));

    // 进入 kCombat 自动下潜到初始子态 kPhase1。
    assert(hfsm.Dispatch(kAggro, ctx));
    assert(hfsm.CurrentLeaf() == kPhase1);
    assert(hfsm.IsInState(kCombat));
    assert(ctx.combatEnterCount == 1);

    // 血量跌到 40%,kPhase1 -> kPhase2Enrage 走 LCA(kCombat),共享父态 kCombat 不得重复退出重入。
    ctx.hpPercent = 40;
    hfsm.Update(ctx, 50);
    assert(hfsm.CurrentLeaf() == kPhase2Enrage);
    assert(hfsm.IsInState(kCombat));
    assert(ctx.combatEnterCount == 1);
    assert(ctx.combatExitCount == 0);

    // 叶子 kPhase2Enrage 不处理 kFatalDamage,事件向上冒泡到 kRoot 切到 kDead,完整退出 p2 -> combat。
    assert(hfsm.Dispatch(kFatalDamage, ctx));
    assert(hfsm.CurrentLeaf() == kDead);
    assert(!hfsm.IsInState(kCombat));
    assert(ctx.combatExitCount == 1);
}

void TestBehaviorTreeAndTrainedMonsterAi()
{
    // ---- 1. 训练一个副本怪物整数效用打分模型(离线训练 -> 线上纯 int64 确定性推理) ----
    // 特征(万分比 0..10000):
    //   [0] 目标是治疗职业   [1] 目标已残血(10000 - hp_ratio)
    //   [2] 目标尚未被控制   [3] 技能预计可斩杀
    constexpr std::size_t kFeatures = 4;
    const std::array<combat_ai::PairwiseTrainingSample<kFeatures>, 6> samples{{
        // 偏好:优先打/控未受控的残血治疗或斩杀目标
        {{10000, 7000, 10000, 10000}, {0, 2000, 0, 0}, 1.0},
        {{10000, 6000, 10000, 0}, {0, 3000, 0, 0}, 1.0},
        {{0, 8500, 10000, 10000}, {0, 1500, 10000, 0}, 1.0},
        {{10000, 5000, 10000, 0}, {0, 5000, 0, 0}, 1.0},
        {{0, 9000, 0, 10000}, {0, 1000, 0, 0}, 1.0},
        {{10000, 4000, 10000, 0}, {0, 4000, 10000, 0}, 1.0},
    }};

    combat_ai::PairwiseTrainMetrics metrics{};
    const auto trainedModel = combat_ai::TrainPairwiseLinearModel<kFeatures>(samples, {}, &metrics);
    assert(metrics.finalLoss < metrics.initialLoss);
    assert(metrics.accuracyPermyriad == 10000);

    // 校验确定性选招:面对"满血坦克(槽位0)" vs "残血未控奶妈(槽位1)",模型稳定选中奶妈。
    const std::array<combat_ai::ActionCandidate<kFeatures>, 2> candidates{{
        {101, 2001, 0, {0, 1000, 0, 0}},          // 普攻坦克
        {202, 2002, 1, {10000, 7500, 10000, 10000}}, // 集火斩杀奶妈
    }};
    const auto pick = combat_ai::SelectBestCandidate<kFeatures>(candidates, trainedModel);
    assert(pick.has_value());
    assert(pick->index == 1);

    // 校验同分决胜:特征完全相同时按 formationSlot 升序稳定选出,零随机数。
    const std::array<combat_ai::ActionCandidate<kFeatures>, 2> tied{{
        {101, 3002, 2, {5000, 5000, 5000, 0}},
        {101, 3001, 1, {5000, 5000, 5000, 0}},
    }};
    const auto tiedPick = combat_ai::SelectBestCandidate<kFeatures>(tied, trainedModel);
    assert(tiedPick.has_value() && tiedPick->index == 1);

    // ---- 2. 行为树共享拓扑 + 多实例黑板 + ReactiveSelector 抢占与 Halt ----
    constexpr uint32_t kBbLastAction = 1;
    constexpr uint32_t kBbLastTarget = 2;

    struct MonsterCtx
    {
        bool emergencyShieldNeeded{false};
        int32_t channelTicksRemaining{2};
        int32_t haltCount{0};
        int32_t shieldCasts{0};
    };

    combat_ai::BehaviorTreeBuilder<MonsterCtx> builder;
    const auto shieldBranch = builder.Sequence({
        builder.Condition([](const MonsterCtx& m, const combat_ai::Blackboard&) {
            return m.emergencyShieldNeeded;
        }),
        builder.Cooldown(
            3,
            builder.Action([](MonsterCtx& m, combat_ai::Blackboard& bb) {
                ++m.shieldCasts;
                bb.Set<uint64_t>(kBbLastAction, 900);
                return combat_ai::BtStatus::kSuccess;
            })),
    });

    const auto channelAndAiBranch = builder.Sequence({
        builder.Action(
            [](MonsterCtx& m, combat_ai::Blackboard&) {
                if (m.channelTicksRemaining > 1)
                {
                    --m.channelTicksRemaining;
                    return combat_ai::BtStatus::kRunning;
                }
                m.channelTicksRemaining = 2;
                return combat_ai::BtStatus::kSuccess;
            },
            [](MonsterCtx& m, combat_ai::Blackboard&) {
                ++m.haltCount;
                m.channelTicksRemaining = 2; // 被抢占时重置读条
            }),
        builder.Action([&trainedModel, &candidates](MonsterCtx&, combat_ai::Blackboard& bb) {
            const auto chosen = combat_ai::SelectBestCandidate<kFeatures>(candidates, trainedModel);
            if (!chosen.has_value())
            {
                return combat_ai::BtStatus::kFailure;
            }
            bb.Set<uint64_t>(kBbLastAction, candidates[chosen->index].actionId);
            bb.Set<uint64_t>(kBbLastTarget, candidates[chosen->index].targetId);
            return combat_ai::BtStatus::kSuccess;
        }),
    });

    const auto root = builder.ReactiveSelector({shieldBranch, channelAndAiBranch});
    const auto treeOpt = builder.Build(root);
    assert(treeOpt.has_value());
    const auto& sharedTree = *treeOpt;

    // 两只怪共享同一份 sharedTree,状态完全隔离。
    MonsterCtx mobA;
    MonsterCtx mobB;
    auto stateA = sharedTree.CreateState();
    auto stateB = sharedTree.CreateState();

    // Tick 1: mobA 开始读条 -> Running; mobB 同样开始读条 -> Running。
    assert(sharedTree.Tick(mobA, stateA) == combat_ai::BtStatus::kRunning);
    assert(sharedTree.Tick(mobB, stateB) == combat_ai::BtStatus::kRunning);

    // Tick 2: mobA 触发紧急开盾 -> ReactiveSelector 抢占并 Halt mobA 的读条;mobB 不受影响正常放出 AI 选中技能。
    mobA.emergencyShieldNeeded = true;
    assert(sharedTree.Tick(mobA, stateA) == combat_ai::BtStatus::kSuccess);
    assert(mobA.shieldCasts == 1);
    assert(mobA.haltCount == 1);
    assert(stateA.blackboard.GetOr<uint64_t>(kBbLastAction, 0) == 900u);

    assert(sharedTree.Tick(mobB, stateB) == combat_ai::BtStatus::kSuccess);
    assert(mobB.haltCount == 0);
    assert(stateB.blackboard.GetOr<uint64_t>(kBbLastAction, 0) == 202u);
    assert(stateB.blackboard.GetOr<uint64_t>(kBbLastTarget, 0) == 2002u);

    // Tick 3: mobA 的开盾还在 3 tick 冷却内,自动回落到读条分支。
    assert(sharedTree.Tick(mobA, stateA) == combat_ai::BtStatus::kRunning);
    assert(mobA.shieldCasts == 1);
}

} // namespace

int main()
{
    TestStateMutexMatrix();
    TestFsm();
    TestHfsm();
    TestBehaviorTreeAndTrainedMonsterAi();
    std::cout << "combat_ai_test passed\n";
    return 0;
}
