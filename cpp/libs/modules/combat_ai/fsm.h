#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <initializer_list>
#include <optional>
#include <string>
#include <string_view>
#include <type_traits>
#include <utility>
#include <vector>

// 事件驱动有限状态机(完整移植 Go looplab/fsm 的核心设计,纯 C++ 标准库,无裸指针成员)。
//
// 完整保留 looplab/fsm 的 5 项核心能力并做 C++ 强类型与零分配改造:
// 1. 声明式多源转移表(AddEvent / AddEvents):一条规则支持多个 Src 状态映射到同一 Dst,另支持 AddAnyTransition。
// 2. 完整 5 阶段回调管线:
//      BeforeEvent / BeforeAnyEvent  (可 ev.Cancel(errCode) 零副作用取消)
//        -> LeaveState / LeaveAnyState (可 ev.Cancel(errCode) 或 ev.Async() 挂起异步迁移)
//        -> 状态提交(current = dst) + 规则 onTransition
//        -> EnterState / EnterAnyState
//        -> AfterEvent / AfterAnyEvent
// 3. 异步/两阶段迁移(ev.Async() + CommitPendingTransition / CancelPendingTransition):
//    离开旧态需等待异步 RPC / 存盘 / 动画结束时挂起迁移,期间新 Trigger fail-closed 返回 kInProgress。
// 4. 自省与可视化:CanTrigger / AvailableTransitions / ExportMermaid(直接产出 stateDiagram-v2 流程图)。
// 5. 重入保护:同步回调内再调 Trigger 进入有界延迟队列顺序消化,避免递归栈破坏状态不变量。

namespace combat_ai
{

enum class FsmStatus : uint8_t
{
    kOk = 0,           // 同步转移完成
    kAsyncPending = 1, // Leave 回调调用了 ev.Async(),等待 CommitPendingTransition / CancelPendingTransition
    kNoTransition = 2, // 无匹配转移或 Guard 未通过
    kCanceled = 3,     // Before 或 Leave 回调调用了 ev.Cancel(errCode)
    kInProgress = 4,   // 已有挂起的异步转移未收尾,拒绝并发新事件(fail-closed)
    kQueueFull = 5,    // 回调内重入队列已满(fail-closed)
};

struct FsmResult
{
    FsmStatus status{FsmStatus::kNoTransition};
    uint32_t cancelError{0};

    [[nodiscard]] constexpr bool Ok() const noexcept
    {
        return status == FsmStatus::kOk;
    }

    [[nodiscard]] constexpr bool PendingAsync() const noexcept
    {
        return status == FsmStatus::kAsyncPending;
    }
};

template <typename StateEnum, typename EventEnum>
class FsmEventView
{
public:
    FsmEventView(EventEnum event, StateEnum src, StateEnum dst) noexcept
        : event(event)
        , src(src)
        , dst(dst)
    {
    }

    [[nodiscard]] EventEnum Event() const noexcept { return event; }
    [[nodiscard]] StateEnum Src() const noexcept { return src; }
    [[nodiscard]] StateEnum Dst() const noexcept { return dst; }

    // 在 Before* 或 Leave* 阶段中止本次转移(fail-closed,状态不改变,不进入 Enter/After)。
    void Cancel(uint32_t errorCode = 0) noexcept
    {
        if (!allowCancel)
        {
            return;
        }
        canceled = true;
        cancelError = errorCode;
        async = false;
    }

    // 在 Leave* 阶段声明异步挂起:等待外部完成异步动作后再调用 CommitPendingTransition / CancelPendingTransition。
    void Async() noexcept
    {
        if (!allowAsync || canceled)
        {
            return;
        }
        async = true;
    }

    [[nodiscard]] bool Canceled() const noexcept { return canceled; }
    [[nodiscard]] uint32_t CancelError() const noexcept { return cancelError; }
    [[nodiscard]] bool IsAsync() const noexcept { return async; }

    void SetPhasePermissions(bool allowCancel, bool allowAsync) noexcept
    {
        this->allowCancel = allowCancel;
        this->allowAsync = allowAsync;
    }

private:
    EventEnum event{};
    StateEnum src{};
    StateEnum dst{};
    bool allowCancel{false};
    bool allowAsync{false};
    bool canceled{false};
    bool async{false};
    uint32_t cancelError{0};
};

template <typename StateEnum, typename EventEnum, typename Context>
class Fsm
{
    static_assert(std::is_enum_v<StateEnum>, "StateEnum must be an enum type");
    static_assert(std::is_enum_v<EventEnum>, "EventEnum must be an enum type");

public:
    static constexpr std::size_t kStateCount = static_cast<std::size_t>(StateEnum::kCount);
    static constexpr std::size_t kEventCount = static_cast<std::size_t>(EventEnum::kCount);
    static constexpr std::size_t kMaxDeferredEvents = 16;

    using EventView = FsmEventView<StateEnum, EventEnum>;
    using GuardFn = std::function<bool(const Context&)>;
    using StateHookFn = std::function<void(Context&)>;
    using EventHookFn = std::function<void(EventView&, Context&)>;
    using TransitionHookFn = std::function<void(StateEnum, StateEnum, EventEnum, Context&)>;

    struct TransitionRule
    {
        StateEnum from{};
        EventEnum event{};
        StateEnum to{};
        GuardFn guard{};
        TransitionHookFn onTransition{};
    };

    // looplab/fsm 风格的声明式事件描述:一条描述即可把多个源状态映射到同一目标状态。
    struct EventDesc
    {
        EventEnum event{};
        std::vector<StateEnum> srcs{};
        StateEnum dst{};
        GuardFn guard{};
        TransitionHookFn onTransition{};
    };

    explicit Fsm(StateEnum initialState) noexcept
        : current(initialState)
    {
    }

    // 注册一条显式状态转移。同一 (from, event) 允许配多条不同 Guard 的规则,按注册顺序取首条命中。
    void AddTransition(StateEnum from, EventEnum event, StateEnum to,
                       GuardFn guard = {}, TransitionHookFn onTransition = {})
    {
        const auto fromIdx = ToIndex(from);
        const auto eventIdx = ToIndex(event);
        if (fromIdx >= kStateCount || eventIdx >= kEventCount || ToIndex(to) >= kStateCount)
        {
            return;
        }
        transitions[Slot(fromIdx, eventIdx)].push_back(
            TransitionRule{from, event, to, std::move(guard), std::move(onTransition)});
    }

    // looplab/fsm 风格:一条事件批量绑定多个源状态 -> 目标状态。
    void AddEvent(const EventDesc& desc)
    {
        for (const StateEnum src : desc.srcs)
        {
            AddTransition(src, desc.event, desc.dst, desc.guard, desc.onTransition);
        }
    }

    void AddEvents(std::initializer_list<EventDesc> descs)
    {
        for (const auto& desc : descs)
        {
            AddEvent(desc);
        }
    }

    // 注册任意态都可响应的全局转移(例如:任何状态收到 kEventDie 都切到 kStateDead)。
    // 优先级低于当前态的显式规则;allowSelf=false 时若已在目标态则忽略。
    void AddAnyTransition(EventEnum event, StateEnum to, GuardFn guard = {},
                          TransitionHookFn onTransition = {}, bool allowSelf = false)
    {
        const auto eventIdx = ToIndex(event);
        if (eventIdx >= kEventCount || ToIndex(to) >= kStateCount)
        {
            return;
        }
        anyTransitions[eventIdx].push_back(
            AnyTransitionRule{to, std::move(guard), std::move(onTransition), allowSelf});
    }

    // ---- 回调注册(5 阶段: Before -> Leave -> Commit -> Enter -> After) ----

    void OnBeforeEvent(EventEnum event, EventHookFn hook)
    {
        if (const auto idx = ToIndex(event); idx < kEventCount)
        {
            onBeforeEvent[idx] = std::move(hook);
        }
    }

    void OnBeforeAnyEvent(EventHookFn hook)
    {
        onBeforeAnyEvent = std::move(hook);
    }

    void OnLeave(StateEnum state, StateHookFn hook)
    {
        if (const auto idx = ToIndex(state); idx < kStateCount)
        {
            onLeave[idx] = std::move(hook);
        }
    }

    void OnLeaveState(StateEnum state, EventHookFn hook)
    {
        if (const auto idx = ToIndex(state); idx < kStateCount)
        {
            onLeaveEvent[idx] = std::move(hook);
        }
    }

    void OnLeaveAnyState(EventHookFn hook)
    {
        onLeaveAnyState = std::move(hook);
    }

    void OnEnter(StateEnum state, StateHookFn hook)
    {
        if (const auto idx = ToIndex(state); idx < kStateCount)
        {
            onEnter[idx] = std::move(hook);
        }
    }

    void OnEnterState(StateEnum state, EventHookFn hook)
    {
        if (const auto idx = ToIndex(state); idx < kStateCount)
        {
            onEnterEvent[idx] = std::move(hook);
        }
    }

    void OnEnterAnyState(EventHookFn hook)
    {
        onEnterAnyState = std::move(hook);
    }

    void OnAfterEvent(EventEnum event, EventHookFn hook)
    {
        if (const auto idx = ToIndex(event); idx < kEventCount)
        {
            onAfterEvent[idx] = std::move(hook);
        }
    }

    void OnAfterAnyEvent(EventHookFn hook)
    {
        onAfterAnyEvent = std::move(hook);
    }

    // ---- 查询与自省 ----

    // 纯只读判定:此刻触发 event 是否有合法且满足 Guard 的转移(存在挂起异步转移时返回 false)。
    [[nodiscard]] bool CanTrigger(EventEnum event, const Context& ctx) const
    {
        if (pendingAsync.has_value())
        {
            return false;
        }
        return FindMatch(current, event, ctx).matched;
    }

    // 返回当前状态下所有可触发的事件列表(对应 looplab/fsm 的 AvailableTransitions)。
    [[nodiscard]] std::vector<EventEnum> AvailableTransitions(const Context& ctx) const
    {
        std::vector<EventEnum> result;
        if (pendingAsync.has_value())
        {
            return result;
        }
        for (std::size_t e = 0; e < kEventCount; ++e)
        {
            const auto ev = static_cast<EventEnum>(e);
            if (FindMatch(current, ev, ctx).matched)
            {
                result.push_back(ev);
            }
        }
        return result;
    }

    // 直接从状态转移表导出 Mermaid stateDiagram-v2 文本,便于文档与排障。
    template <typename StateNameFn, typename EventNameFn>
    [[nodiscard]] std::string ExportMermaid(StateNameFn&& stateName, EventNameFn&& eventName) const
    {
        std::string out = "stateDiagram-v2\n";
        for (std::size_t s = 0; s < kStateCount; ++s)
        {
            const auto from = static_cast<StateEnum>(s);
            for (std::size_t e = 0; e < kEventCount; ++e)
            {
                const auto ev = static_cast<EventEnum>(e);
                for (const auto& rule : transitions[Slot(s, e)])
                {
                    out += "    ";
                    out += std::string(stateName(from));
                    out += " --> ";
                    out += std::string(stateName(rule.to));
                    out += " : ";
                    out += std::string(eventName(ev));
                    out += "\n";
                }
            }
        }
        for (std::size_t e = 0; e < kEventCount; ++e)
        {
            const auto ev = static_cast<EventEnum>(e);
            for (const auto& rule : anyTransitions[e])
            {
                out += "    [*] --> ";
                out += std::string(stateName(rule.to));
                out += " : ";
                out += std::string(eventName(ev));
                out += " (any)\n";
            }
        }
        return out;
    }

    // ---- 触发与异步收尾 ----

    // 完整语义触发:返回 FsmResult(区分 kOk / kAsyncPending / kNoTransition / kCanceled / kInProgress / kQueueFull)。
    FsmResult TriggerEx(EventEnum event, Context& ctx)
    {
        if (pendingAsync.has_value())
        {
            return FsmResult{FsmStatus::kInProgress, 0};
        }
        if (inTransition)
        {
            if (deferredCount >= kMaxDeferredEvents)
            {
                return FsmResult{FsmStatus::kQueueFull, 0};
            }
            deferredQueue[(deferredHead + deferredCount) % kMaxDeferredEvents] = event;
            ++deferredCount;
            return FsmResult{FsmStatus::kOk, 0};
        }

        const FsmResult first = ExecuteSingle(event, ctx);
        if (first.status == FsmStatus::kAsyncPending)
        {
            return first;
        }
        DrainDeferredQueue(ctx);
        return first;
    }

    // 简洁 bool 接口:同步完成或合法进入异步挂起均返回 true。
    bool Trigger(EventEnum event, Context& ctx)
    {
        const FsmResult res = TriggerEx(event, ctx);
        return res.Ok() || res.PendingAsync();
    }

    [[nodiscard]] bool HasPendingTransition() const noexcept
    {
        return pendingAsync.has_value();
    }

    // 完成先前由 ev.Async() 挂起的迁移(对应 looplab/fsm 的 fsm.Transition()):
    // 提交状态 -> 执行规则 onTransition -> Enter* -> After* -> 消化重入队列。
    FsmResult CommitPendingTransition(Context& ctx)
    {
        if (!pendingAsync.has_value())
        {
            return FsmResult{FsmStatus::kNoTransition, 0};
        }
        if (inTransition)
        {
            return FsmResult{FsmStatus::kInProgress, 0};
        }
        const PendingAsync pending = *pendingAsync;
        pendingAsync.reset();

        FinishCommitAndAfter(pending.from, pending.match, pending.event, ctx);
        DrainDeferredQueue(ctx);
        return FsmResult{FsmStatus::kOk, 0};
    }

    // 放弃先前由 ev.Async() 挂起的迁移,保持原状态不变。
    bool CancelPendingTransition() noexcept
    {
        if (!pendingAsync.has_value())
        {
            return false;
        }
        pendingAsync.reset();
        deferredHead = 0;
        deferredCount = 0;
        return true;
    }

    // 复位到指定状态(不触发旧态 OnLeave,用于对象池复用或测试复位)。
    void Reset(StateEnum state) noexcept
    {
        if (ToIndex(state) < kStateCount)
        {
            current = state;
        }
        inTransition = false;
        pendingAsync.reset();
        deferredHead = 0;
        deferredCount = 0;
    }

    [[nodiscard]] StateEnum Current() const noexcept
    {
        return current;
    }

    [[nodiscard]] bool Is(StateEnum state) const noexcept
    {
        return current == state;
    }

private:
    struct AnyTransitionRule
    {
        StateEnum to{};
        GuardFn guard{};
        TransitionHookFn onTransition{};
        bool allowSelf{false};
    };

    struct MatchResult
    {
        bool matched{false};
        bool isAnyRule{false};
        std::size_t ruleIndex{0};
        StateEnum to{};
    };

    struct PendingAsync
    {
        StateEnum from{};
        EventEnum event{};
        MatchResult match{};
    };

    struct TransitionGuard
    {
        bool& flag;
        explicit TransitionGuard(bool& target) noexcept : flag(target) { flag = true; }
        ~TransitionGuard() noexcept { flag = false; }
    };

    static constexpr std::size_t ToIndex(StateEnum s) noexcept
    {
        return static_cast<std::size_t>(s);
    }

    static constexpr std::size_t ToIndex(EventEnum e) noexcept
    {
        return static_cast<std::size_t>(e);
    }

    static constexpr std::size_t Slot(std::size_t stateIdx, std::size_t eventIdx) noexcept
    {
        return stateIdx * kEventCount + eventIdx;
    }

    [[nodiscard]] MatchResult FindMatch(StateEnum from, EventEnum event, const Context& ctx) const
    {
        const auto fromIdx = ToIndex(from);
        const auto eventIdx = ToIndex(event);
        if (fromIdx >= kStateCount || eventIdx >= kEventCount)
        {
            return {};
        }

        const auto& specific = transitions[Slot(fromIdx, eventIdx)];
        for (std::size_t i = 0; i < specific.size(); ++i)
        {
            if (!specific[i].guard || specific[i].guard(ctx))
            {
                return MatchResult{true, false, i, specific[i].to};
            }
        }

        const auto& globals = anyTransitions[eventIdx];
        for (std::size_t i = 0; i < globals.size(); ++i)
        {
            if (!globals[i].allowSelf && globals[i].to == from)
            {
                continue;
            }
            if (!globals[i].guard || globals[i].guard(ctx))
            {
                return MatchResult{true, true, i, globals[i].to};
            }
        }
        return {};
    }

    FsmResult ExecuteSingle(EventEnum event, Context& ctx)
    {
        const StateEnum from = current;
        const MatchResult match = FindMatch(from, event, ctx);
        if (!match.matched)
        {
            return FsmResult{FsmStatus::kNoTransition, 0};
        }

        const auto fromIdx = ToIndex(from);
        const auto eventIdx = ToIndex(event);
        EventView ev(event, from, match.to);

        {
            TransitionGuard guard{inTransition};

            // 阶段 1: BeforeEvent -> BeforeAnyEvent (允许 Cancel,不允许 Async)
            ev.SetPhasePermissions(/*allowCancel=*/true, /*allowAsync=*/false);
            if (const auto& hook = onBeforeEvent[eventIdx])
            {
                hook(ev, ctx);
                if (ev.Canceled())
                {
                    return FsmResult{FsmStatus::kCanceled, ev.CancelError()};
                }
            }
            if (onBeforeAnyEvent)
            {
                onBeforeAnyEvent(ev, ctx);
                if (ev.Canceled())
                {
                    return FsmResult{FsmStatus::kCanceled, ev.CancelError()};
                }
            }

            // 阶段 2: LeaveState -> LeaveAnyState -> simpleLeave (允许 Cancel 与 Async;先跑可取消钩子)
            ev.SetPhasePermissions(/*allowCancel=*/true, /*allowAsync=*/true);
            if (const auto& hook = onLeaveEvent[fromIdx])
            {
                hook(ev, ctx);
                if (ev.Canceled())
                {
                    return FsmResult{FsmStatus::kCanceled, ev.CancelError()};
                }
            }
            if (onLeaveAnyState)
            {
                onLeaveAnyState(ev, ctx);
                if (ev.Canceled())
                {
                    return FsmResult{FsmStatus::kCanceled, ev.CancelError()};
                }
            }
            if (const auto& simpleLeave = onLeave[fromIdx])
            {
                simpleLeave(ctx);
            }

            if (ev.IsAsync())
            {
                pendingAsync = PendingAsync{from, event, match};
                return FsmResult{FsmStatus::kAsyncPending, 0};
            }
        }

        // 阶段 3~5: Commit -> Enter -> After
        FinishCommitAndAfter(from, match, event, ctx);
        return FsmResult{FsmStatus::kOk, 0};
    }

    void FinishCommitAndAfter(StateEnum from, const MatchResult& match, EventEnum event, Context& ctx)
    {
        const auto fromIdx = ToIndex(from);
        const auto toIdx = ToIndex(match.to);
        const auto eventIdx = ToIndex(event);

        EventView ev(event, from, match.to);
        ev.SetPhasePermissions(/*allowCancel=*/false, /*allowAsync=*/false);

        TransitionGuard guard{inTransition};

        // 阶段 3: 提交新状态 + 规则转移回调
        current = match.to;
        if (match.isAnyRule)
        {
            if (const auto& hook = anyTransitions[eventIdx][match.ruleIndex].onTransition)
            {
                hook(from, match.to, event, ctx);
            }
        }
        else
        {
            if (const auto& hook = transitions[Slot(fromIdx, eventIdx)][match.ruleIndex].onTransition)
            {
                hook(from, match.to, event, ctx);
            }
        }

        // 阶段 4: EnterState -> EnterAnyState
        if (const auto& simpleEnter = onEnter[toIdx])
        {
            simpleEnter(ctx);
        }
        if (const auto& hook = onEnterEvent[toIdx])
        {
            hook(ev, ctx);
        }
        if (onEnterAnyState)
        {
            onEnterAnyState(ev, ctx);
        }

        // 阶段 5: AfterEvent -> AfterAnyEvent
        if (const auto& hook = onAfterEvent[eventIdx])
        {
            hook(ev, ctx);
        }
        if (onAfterAnyEvent)
        {
            onAfterAnyEvent(ev, ctx);
        }
    }

    void DrainDeferredQueue(Context& ctx)
    {
        std::size_t drained = 0;
        while (deferredCount > 0 && drained < kMaxDeferredEvents && !pendingAsync.has_value())
        {
            const EventEnum next = deferredQueue[deferredHead];
            deferredHead = (deferredHead + 1) % kMaxDeferredEvents;
            --deferredCount;
            ++drained;
            const FsmResult res = ExecuteSingle(next, ctx);
            if (res.PendingAsync())
            {
                break;
            }
        }
    }

    StateEnum current{};
    bool inTransition{false};
    std::optional<PendingAsync> pendingAsync{};
    std::array<EventEnum, kMaxDeferredEvents> deferredQueue{};
    std::size_t deferredHead{0};
    std::size_t deferredCount{0};
    std::array<std::vector<TransitionRule>, kStateCount * kEventCount> transitions{};
    std::array<std::vector<AnyTransitionRule>, kEventCount> anyTransitions{};
    std::array<EventHookFn, kEventCount> onBeforeEvent{};
    EventHookFn onBeforeAnyEvent{};
    std::array<StateHookFn, kStateCount> onLeave{};
    std::array<EventHookFn, kStateCount> onLeaveEvent{};
    EventHookFn onLeaveAnyState{};
    std::array<StateHookFn, kStateCount> onEnter{};
    std::array<EventHookFn, kStateCount> onEnterEvent{};
    EventHookFn onEnterAnyState{};
    std::array<EventHookFn, kEventCount> onAfterEvent{};
    EventHookFn onAfterAnyEvent{};
};

} // namespace combat_ai
