#include <gtest/gtest.h>

#include <array>
#include <cstdint>
#include <string>
#include <vector>

#include "modules/combat_ai/behavior_tree.h"
#include "modules/combat_ai/fsm.h"
#include "modules/combat_ai/hfsm.h"
#include "modules/combat_ai/monster_utility_ai.h"
#include "modules/combat_ai/state_mutex_matrix.h"

namespace
{

TEST(CombatAiTest, StateMutexMatrixEnforcesTwoPhaseCheckInterruptAndRefCount)
{
    enum State : uint32_t { kCombat = 0, kFollow = 1, kMounted = 2, kSilence = 3, kMax = 4 };
    enum Action : uint32_t { kUseSkill = 0, kMount = 1 };
    constexpr uint32_t kOk = 1000;
    constexpr uint32_t kTipSilenced = 7005;
    constexpr uint32_t kTipInCombat = 7004;
    constexpr uint32_t kTipUnknown = 1001;
    constexpr uint32_t kTipInvalid = 1005;

    combat_ai::StateMutexMatrix matrix;
    const std::array<combat_ai::StateRule, 5> rules{{
        {kUseSkill, kCombat, combat_ai::StateMode::kAllow, kOk},
        {kUseSkill, kFollow, combat_ai::StateMode::kInterrupt, kOk},
        {kUseSkill, kMounted, combat_ai::StateMode::kInterrupt, kOk},
        {kUseSkill, kSilence, combat_ai::StateMode::kBlock, kTipSilenced},
        {kMount, kCombat, combat_ai::StateMode::kBlock, kTipInCombat},
    }};
    ASSERT_EQ(0u, matrix.LoadRules(rules));

    combat_ai::RefCountedStateBitset states;
    EXPECT_TRUE(states.Acquire(kFollow));
    EXPECT_TRUE(states.Acquire(kMounted));
    EXPECT_TRUE(states.Acquire(kSilence));
    EXPECT_FALSE(states.Acquire(kSilence));
    EXPECT_EQ(2u, states.RefCount(kSilence));

    std::vector<uint32_t> hooksFired;
    combat_ai::StateExitHookRegistry<uint64_t> hooks;
    hooks.Register(kFollow, [&](uint64_t, combat_ai::StateExitReason reason) {
        EXPECT_EQ(combat_ai::StateExitReason::kInterrupted, reason);
        EXPECT_FALSE(states.Has(kFollow));
        hooksFired.push_back(kFollow);
    });
    hooks.Register(kMounted, [&](uint64_t, combat_ai::StateExitReason reason) {
        EXPECT_EQ(combat_ai::StateExitReason::kInterrupted, reason);
        EXPECT_FALSE(states.Has(kMounted));
        hooksFired.push_back(kMounted);
    });

    // 沉默阻塞时不得提前触发任何打断或退出钩子。
    EXPECT_EQ(kTipSilenced,
              matrix.TryPerform(
                  states.Mask(), kUseSkill, kCombat, kMax, kOk, kTipUnknown, kTipInvalid,
                  [&](uint32_t s) {
                      if (states.ClearState(s)) hooks.NotifyExited(1, s, combat_ai::StateExitReason::kInterrupted);
                  },
                  [&](uint32_t s) { states.Acquire(s); }));
    EXPECT_TRUE(hooksFired.empty());
    EXPECT_TRUE(states.Has(kFollow));
    EXPECT_TRUE(states.Has(kMounted));

    // 两层沉默全部移除后,依次打断 Follow/Mounted 并进入 Combat。
    EXPECT_FALSE(states.Release(kSilence));
    EXPECT_TRUE(states.Release(kSilence));
    EXPECT_EQ(kOk,
              matrix.TryPerform(
                  states.Mask(), kUseSkill, kCombat, kMax, kOk, kTipUnknown, kTipInvalid,
                  [&](uint32_t s) {
                      if (states.ClearState(s)) hooks.NotifyExited(1, s, combat_ai::StateExitReason::kInterrupted);
                  },
                  [&](uint32_t s) { states.Acquire(s); }));
    EXPECT_EQ((std::vector<uint32_t>{kFollow, kMounted}), hooksFired);
    EXPECT_TRUE(states.Has(kCombat));
    EXPECT_EQ(kTipInCombat, matrix.CanPerform(states.Mask(), kMount, kOk, kTipUnknown));
}

TEST(CombatAiTest, FsmRunsGuardsLifecycleHooksAndDeferredReentrancy)
{
    enum class S : uint8_t { kIdle = 0, kCast = 1, kRecover = 2, kDead = 3, kCount = 4 };
    enum class E : uint8_t { kBegin = 0, kSpell = 1, kDone = 2, kDie = 3, kCount = 4 };
    struct Ctx
    {
        bool allow{false};
        std::vector<std::string> trace{};
    };

    combat_ai::Fsm<S, E, Ctx> fsm(S::kIdle);
    fsm.AddTransition(S::kIdle, E::kBegin, S::kCast, [](const Ctx& c) { return c.allow; });
    fsm.AddTransition(S::kCast, E::kSpell, S::kRecover);
    fsm.AddTransition(S::kRecover, E::kDone, S::kIdle);
    fsm.AddAnyTransition(E::kDie, S::kDead);

    fsm.OnLeave(S::kIdle, [](Ctx& c) { c.trace.push_back("leave:idle"); });
    fsm.OnEnter(S::kCast, [](Ctx& c) { c.trace.push_back("enter:cast"); });
    fsm.OnEnter(S::kRecover, [&](Ctx& c) {
        c.trace.push_back("enter:recover");
        EXPECT_TRUE(fsm.Trigger(E::kDone, c));
    });
    fsm.OnEnter(S::kIdle, [](Ctx& c) { c.trace.push_back("enter:idle"); });

    Ctx ctx;
    EXPECT_FALSE(fsm.Trigger(E::kBegin, ctx));
    EXPECT_TRUE(ctx.trace.empty());

    ctx.allow = true;
    EXPECT_TRUE(fsm.Trigger(E::kBegin, ctx));
    EXPECT_TRUE(fsm.Is(S::kCast));
    EXPECT_TRUE(fsm.Trigger(E::kSpell, ctx));
    EXPECT_TRUE(fsm.Is(S::kIdle));
    EXPECT_TRUE(fsm.Trigger(E::kDie, ctx));
    EXPECT_TRUE(fsm.Is(S::kDead));
}

TEST(CombatAiTest, FsmSupportsLooplabMultiSrcCancelAsyncCommitAndMermaid)
{
    enum class S : uint8_t { kClosed = 0, kOpen = 1, kLocked = 2, kCount = 3 };
    enum class E : uint8_t { kOpen = 0, kClose = 1, kLock = 2, kCount = 3 };
    struct Ctx
    {
        bool cancelBefore{false};
        bool asyncLeave{false};
        std::vector<std::string> trace{};
    };

    combat_ai::Fsm<S, E, Ctx> fsm(S::kClosed);
    fsm.AddEvents({
        {E::kOpen, {S::kClosed}, S::kOpen},
        {E::kClose, {S::kOpen}, S::kClosed},
        {E::kLock, {S::kClosed, S::kOpen}, S::kLocked},
    });
    fsm.OnBeforeEvent(E::kOpen, [](auto& ev, Ctx& c) {
        c.trace.push_back("before:open");
        if (c.cancelBefore) ev.Cancel(7004);
    });
    fsm.OnLeaveState(S::kClosed, [](auto& ev, Ctx& c) {
        c.trace.push_back("leave:closed");
        if (c.asyncLeave) ev.Async();
    });
    fsm.OnEnterState(S::kOpen, [](auto&, Ctx& c) { c.trace.push_back("enter:open"); });
    fsm.OnAfterEvent(E::kOpen, [](auto&, Ctx& c) { c.trace.push_back("after:open"); });

    Ctx ctx;
    EXPECT_EQ((std::vector<E>{E::kOpen, E::kLock}), fsm.AvailableTransitions(ctx));

    ctx.cancelBefore = true;
    const auto canceled = fsm.TriggerEx(E::kOpen, ctx);
    EXPECT_EQ(combat_ai::FsmStatus::kCanceled, canceled.status);
    EXPECT_EQ(7004u, canceled.cancelError);
    EXPECT_TRUE(fsm.Is(S::kClosed));

    ctx.cancelBefore = false;
    ctx.asyncLeave = true;
    ctx.trace.clear();
    EXPECT_TRUE(fsm.TriggerEx(E::kOpen, ctx).PendingAsync());
    EXPECT_TRUE(fsm.HasPendingTransition());
    EXPECT_EQ(combat_ai::FsmStatus::kInProgress, fsm.TriggerEx(E::kLock, ctx).status);
    EXPECT_TRUE(fsm.CommitPendingTransition(ctx).Ok());
    EXPECT_TRUE(fsm.Is(S::kOpen));
    EXPECT_EQ((std::vector<std::string>{"before:open", "leave:closed", "enter:open", "after:open"}), ctx.trace);

    const std::string diagram = fsm.ExportMermaid(
        [](S s) -> std::string_view { return s == S::kClosed ? "Closed" : (s == S::kOpen ? "Open" : "Locked"); },
        [](E e) -> std::string_view { return e == E::kOpen ? "open" : (e == E::kClose ? "close" : "lock"); });
    EXPECT_NE(std::string::npos, diagram.find("Closed --> Open : open"));
    EXPECT_NE(std::string::npos, diagram.find("Open --> Locked : lock"));
}

TEST(CombatAiTest, HfsmPerformsLcaTransitionsAndEventBubbling)
{
    enum S : combat_ai::HfsmStateId { kRoot = 0, kIdle = 1, kCombat = 2, kP1 = 3, kP2 = 4, kDead = 5 };
    enum Ev : uint32_t { kAggro = 1, kKill = 2 };
    struct Ctx
    {
        int hp{100};
        int combatEnters{0};
        int combatExits{0};
    };

    combat_ai::Hfsm<Ctx, Ev> hfsm;
    hfsm.AddState(kRoot, {.parent = combat_ai::kInvalidHfsmState, .initialChild = kIdle,
                          .onEvent = [](const Ev& ev, Ctx&) {
                              return ev == kKill ? combat_ai::HfsmEventOutcome::TransitionTo(kDead)
                                                 : combat_ai::HfsmEventOutcome::Ignored();
                          }});
    hfsm.AddState(kIdle, {.parent = kRoot, .onEvent = [](const Ev& ev, Ctx&) {
                              return ev == kAggro ? combat_ai::HfsmEventOutcome::TransitionTo(kCombat)
                                                  : combat_ai::HfsmEventOutcome::Ignored();
                          }});
    hfsm.AddState(kCombat, {.parent = kRoot, .initialChild = kP1,
                            .onEnter = [](Ctx& c) { ++c.combatEnters; },
                            .onExit = [](Ctx& c) { ++c.combatExits; }});
    hfsm.AddState(kP1, {.parent = kCombat, .onUpdate = [](Ctx& c, uint32_t) -> std::optional<combat_ai::HfsmStateId> {
                            return c.hp <= 50 ? std::optional<combat_ai::HfsmStateId>{kP2} : std::nullopt;
                        }});
    hfsm.AddState(kP2, {.parent = kCombat});
    hfsm.AddState(kDead, {.parent = kRoot});

    Ctx ctx;
    ASSERT_TRUE(hfsm.Start(kRoot, ctx));
    EXPECT_EQ(kIdle, hfsm.CurrentLeaf());
    ASSERT_TRUE(hfsm.Dispatch(kAggro, ctx));
    EXPECT_EQ(kP1, hfsm.CurrentLeaf());
    EXPECT_EQ(1, ctx.combatEnters);

    ctx.hp = 40;
    hfsm.Update(ctx, 1);
    EXPECT_EQ(kP2, hfsm.CurrentLeaf());
    EXPECT_TRUE(hfsm.IsInState(kCombat));
    EXPECT_EQ(1, ctx.combatEnters);
    EXPECT_EQ(0, ctx.combatExits);

    ASSERT_TRUE(hfsm.Dispatch(kKill, ctx));
    EXPECT_EQ(kDead, hfsm.CurrentLeaf());
    EXPECT_EQ(1, ctx.combatExits);
}

TEST(CombatAiTest, SharedBehaviorTreePreemptsWithHaltAndTrainedUtilityModelConverges)
{
    struct Ctx
    {
        bool urgent{false};
        int halts{0};
        int urgentRuns{0};
    };

    combat_ai::BehaviorTreeBuilder<Ctx> b;
    const auto urgentBranch = b.Sequence({
        b.Condition([](const Ctx& c, const combat_ai::Blackboard&) { return c.urgent; }),
        b.Cooldown(2, b.Action([](Ctx& c, combat_ai::Blackboard&) {
                       ++c.urgentRuns;
                       return combat_ai::BtStatus::kSuccess;
                   })),
    });
    const auto workBranch = b.Action(
        [](Ctx&, combat_ai::Blackboard&) { return combat_ai::BtStatus::kRunning; },
        [](Ctx& c, combat_ai::Blackboard&) { ++c.halts; });
    const auto tree = b.Build(b.ReactiveSelector({urgentBranch, workBranch}));
    ASSERT_TRUE(tree.has_value());

    Ctx a;
    Ctx bCtx;
    auto stateA = tree->CreateState();
    auto stateB = tree->CreateState();
    EXPECT_EQ(combat_ai::BtStatus::kRunning, tree->Tick(a, stateA));
    EXPECT_EQ(combat_ai::BtStatus::kRunning, tree->Tick(bCtx, stateB));

    a.urgent = true;
    EXPECT_EQ(combat_ai::BtStatus::kSuccess, tree->Tick(a, stateA));
    EXPECT_EQ(1, a.halts);
    EXPECT_EQ(1, a.urgentRuns);
    EXPECT_EQ(0, bCtx.halts);

    // 离线成对排序训练 + 定点整数打分
    constexpr std::size_t kF = 3;
    const std::array<combat_ai::PairwiseTrainingSample<kF>, 4> samples{{
        {{10000, 8000, 10000}, {0, 1000, 0}, 1.0},
        {{10000, 6000, 0}, {0, 2000, 0}, 1.0},
        {{0, 9000, 10000}, {0, 1000, 0}, 1.0},
        {{10000, 5000, 10000}, {0, 5000, 0}, 1.0},
    }};
    combat_ai::PairwiseTrainMetrics metrics{};
    const auto model = combat_ai::TrainPairwiseLinearModel<kF>(samples, {}, &metrics);
    EXPECT_LT(metrics.finalLoss, metrics.initialLoss);
    EXPECT_EQ(10000u, metrics.accuracyPermyriad);

    combat_ai::QuantizedMlpModel<2, 2> mlp{};
    mlp.w1 = {{{10000, 0}, {0, 10000}}};
    mlp.w2 = {10000, 20000};
    EXPECT_GT(mlp.Score({0, 10000}), mlp.Score({10000, 0}));
}

} // namespace
