#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <limits>
#include <optional>
#include <utility>
#include <vector>

// 分层有限状态机(移植 HFSM2 的核心思想,纯 C++ 标准库,无裸指针成员)。
//
// 核心机制:
// 1. 状态以扁平数组存放,按 StateId(uint16_t) 索引,零堆链表指针,无动态分配。
// 2. 自动下潜初始子态(initialChild):切到父态(如 kCombat)时自动落到其叶子初始态(如 kCombatApproach)。
// 3. LCA(最近公共祖先)切换:从叶子 A 切到叶子 B 时,只自底向上退出 A -> LCA 之下,
//    再自顶向下进入 LCA 之下 -> B;共享父态(如 kCombat)不重复退出重入。
// 4. 事件冒泡(Dispatch):从当前叶子态逐级向父态传递,首个处理或触发切态的节点截断冒泡。
// 5. 状态查询 O(1):维护 64 位活跃祖先位图(超过 64 个状态自动退化到祖先链遍历)。

namespace combat_ai
{

using HfsmStateId = uint16_t;
inline constexpr HfsmStateId kInvalidHfsmState = std::numeric_limits<HfsmStateId>::max();

struct HfsmEventOutcome
{
    bool handled{false};
    std::optional<HfsmStateId> transitionTarget{};

    static constexpr HfsmEventOutcome Ignored() noexcept
    {
        return HfsmEventOutcome{false, std::nullopt};
    }

    static constexpr HfsmEventOutcome Handled() noexcept
    {
        return HfsmEventOutcome{true, std::nullopt};
    }

    static constexpr HfsmEventOutcome TransitionTo(HfsmStateId target) noexcept
    {
        return HfsmEventOutcome{true, target};
    }
};

template <typename Context, typename Event = uint32_t>
class Hfsm
{
public:
    static constexpr std::size_t kMaxHierarchyDepth = 16;
    static constexpr std::size_t kMaxPendingTransitions = 16;

    using HookFn = std::function<void(Context&)>;
    using UpdateFn = std::function<std::optional<HfsmStateId>(Context&, uint32_t)>;
    using EventFn = std::function<HfsmEventOutcome(const Event&, Context&)>;

    struct StateConfig
    {
        HfsmStateId parent{kInvalidHfsmState};
        HfsmStateId initialChild{kInvalidHfsmState};
        HookFn onEnter{};
        UpdateFn onUpdate{};
        HookFn onExit{};
        EventFn onEvent{};
    };

    // 注册状态定义(允许按任意顺序注册父子态,Start 时统一做环路与深度校验)。
    bool AddState(HfsmStateId id, StateConfig config)
    {
        if (id == kInvalidHfsmState)
        {
            return false;
        }
        if (id >= states_.size())
        {
            states_.resize(static_cast<std::size_t>(id) + 1);
        }
        states_[id] = StateSlot{true, std::move(config)};
        return true;
    }

    // 启动状态机:校验层级无环后,从顶层自顶向下进入 initialState 及其初始子态叶子。
    bool Start(HfsmStateId initialState, Context& ctx)
    {
        if (!ValidateHierarchy() || !HasState(initialState))
        {
            return false;
        }
        const auto targetLeaf = ResolveLeaf(initialState);
        if (!targetLeaf.has_value())
        {
            return false;
        }

        started_ = true;
        currentLeaf_ = kInvalidHfsmState;
        activeMask_ = 0;

        std::array<HfsmStateId, kMaxHierarchyDepth> enterPath{};
        const std::size_t enterLen = BuildRootToNodePath(*targetLeaf, enterPath);
        if (enterLen == 0)
        {
            started_ = false;
            return false;
        }

        // 逐层进入:先更新活跃叶/位图再调回调,确保 onEnter 里查 IsInState 与回调内重入切态都一致。
        {
            TransitionGuard guard{inTransition_};
            for (std::size_t i = 0; i < enterLen; ++i)
            {
                currentLeaf_ = enterPath[i];
                RebuildActiveMask();
                if (const auto& fn = states_[enterPath[i]].config.onEnter)
                {
                    fn(ctx);
                }
            }
        }
        DrainPendingTransitions(ctx);
        return true;
    }

    // 按自顶向下顺序更新当前活跃链(父态先于子态),任意一层返回目标态即触发 LCA 切换。
    void Update(Context& ctx, uint32_t deltaMs = 0)
    {
        if (!started_ || currentLeaf_ == kInvalidHfsmState)
        {
            return;
        }
        std::array<HfsmStateId, kMaxHierarchyDepth> path{};
        const std::size_t len = BuildRootToNodePath(currentLeaf_, path);
        for (std::size_t i = 0; i < len; ++i)
        {
            const HfsmStateId stateId = path[i];
            if (const auto& updateFn = states_[stateId].config.onUpdate)
            {
                if (const auto next = updateFn(ctx, deltaMs); next.has_value())
                {
                    TransitionTo(*next, ctx);
                    return;
                }
                // 更新回调内部可能已通过 TransitionTo 切走当前状态,立刻结束本轮旧链更新。
                if (!IsInState(stateId))
                {
                    return;
                }
            }
        }
    }

    // 事件冒泡:从当前叶子态逐级向父态派发,首个 Handled / TransitionTo 终止冒泡。
    bool Dispatch(const Event& event, Context& ctx)
    {
        if (!started_ || currentLeaf_ == kInvalidHfsmState)
        {
            return false;
        }
        HfsmStateId cursor = currentLeaf_;
        std::size_t steps = 0;
        while (cursor != kInvalidHfsmState && steps < kMaxHierarchyDepth)
        {
            ++steps;
            const HfsmStateId parent = states_[cursor].config.parent;
            if (const auto& eventFn = states_[cursor].config.onEvent)
            {
                const HfsmEventOutcome outcome = eventFn(event, ctx);
                if (outcome.transitionTarget.has_value())
                {
                    TransitionTo(*outcome.transitionTarget, ctx);
                    return true;
                }
                if (outcome.handled)
                {
                    return true;
                }
            }
            cursor = parent;
        }
        return false;
    }

    // LCA 切换:支持目标为父态(自动下潜到其叶子初始态)或叶子态。
    // 若在 onEnter/onExit 内部重入调用,目标入队并在本次切换完成后执行。
    bool TransitionTo(HfsmStateId target, Context& ctx)
    {
        if (!started_ || !HasState(target))
        {
            return false;
        }
        if (inTransition_)
        {
            if (pendingCount_ >= kMaxPendingTransitions)
            {
                return false;
            }
            pendingTargets_[(pendingHead_ + pendingCount_) % kMaxPendingTransitions] = target;
            ++pendingCount_;
            return true;
        }

        const bool ok = ExecuteTransition(target, ctx);
        DrainPendingTransitions(ctx);
        return ok;
    }

    [[nodiscard]] bool Started() const noexcept
    {
        return started_;
    }

    [[nodiscard]] HfsmStateId CurrentLeaf() const noexcept
    {
        return currentLeaf_;
    }

    // O(1) 查询是否正处于某个状态(包括其任意父态)。
    [[nodiscard]] bool IsInState(HfsmStateId state) const noexcept
    {
        if (!started_ || currentLeaf_ == kInvalidHfsmState || !HasState(state))
        {
            return false;
        }
        if (state < 64)
        {
            return (activeMask_ & (uint64_t{1} << state)) != 0;
        }
        HfsmStateId cursor = currentLeaf_;
        std::size_t steps = 0;
        while (cursor != kInvalidHfsmState && steps < kMaxHierarchyDepth)
        {
            if (cursor == state)
            {
                return true;
            }
            cursor = states_[cursor].config.parent;
            ++steps;
        }
        return false;
    }

private:
    struct StateSlot
    {
        bool registered{false};
        StateConfig config{};
    };

    struct TransitionGuard
    {
        bool& flag;
        explicit TransitionGuard(bool& target) noexcept : flag(target) { flag = true; }
        ~TransitionGuard() noexcept { flag = false; }
    };

    [[nodiscard]] bool HasState(HfsmStateId id) const noexcept
    {
        return id < states_.size() && states_[id].registered;
    }

    // 校验所有已注册状态的 parent 与 initialChild 引用有效、深度不超限且无环路。
    [[nodiscard]] bool ValidateHierarchy() const noexcept
    {
        for (std::size_t i = 0; i < states_.size(); ++i)
        {
            if (!states_[i].registered)
            {
                continue;
            }
            // 校验向上 parent 链无环
            HfsmStateId cursor = states_[i].config.parent;
            std::size_t depth = 1;
            while (cursor != kInvalidHfsmState)
            {
                if (!HasState(cursor) || cursor == i || depth >= kMaxHierarchyDepth)
                {
                    return false;
                }
                cursor = states_[cursor].config.parent;
                ++depth;
            }
            // 校验 initialChild 必须是自己的直接子态
            const HfsmStateId child = states_[i].config.initialChild;
            if (child != kInvalidHfsmState)
            {
                if (!HasState(child) || states_[child].config.parent != static_cast<HfsmStateId>(i))
                {
                    return false;
                }
            }
        }
        return true;
    }

    [[nodiscard]] std::optional<HfsmStateId> ResolveLeaf(HfsmStateId state) const noexcept
    {
        HfsmStateId cursor = state;
        std::size_t depth = 0;
        while (HasState(cursor) && states_[cursor].config.initialChild != kInvalidHfsmState)
        {
            if (++depth >= kMaxHierarchyDepth)
            {
                return std::nullopt;
            }
            cursor = states_[cursor].config.initialChild;
        }
        return HasState(cursor) ? std::optional<HfsmStateId>{cursor} : std::nullopt;
    }

    // 生成 Root -> node 的路径,返回路径长度(0 表示非法或超深)。
    [[nodiscard]] std::size_t BuildRootToNodePath(
        HfsmStateId node, std::array<HfsmStateId, kMaxHierarchyDepth>& out) const noexcept
    {
        std::array<HfsmStateId, kMaxHierarchyDepth> rev{};
        std::size_t count = 0;
        HfsmStateId cursor = node;
        while (cursor != kInvalidHfsmState)
        {
            if (!HasState(cursor) || count >= kMaxHierarchyDepth)
            {
                return 0;
            }
            rev[count++] = cursor;
            cursor = states_[cursor].config.parent;
        }
        for (std::size_t i = 0; i < count; ++i)
        {
            out[i] = rev[count - 1 - i];
        }
        return count;
    }

    void RebuildActiveMask() noexcept
    {
        uint64_t mask = 0;
        HfsmStateId cursor = currentLeaf_;
        std::size_t steps = 0;
        while (cursor != kInvalidHfsmState && steps < kMaxHierarchyDepth)
        {
            if (cursor < 64)
            {
                mask |= (uint64_t{1} << cursor);
            }
            cursor = states_[cursor].config.parent;
            ++steps;
        }
        activeMask_ = mask;
    }

    bool ExecuteTransition(HfsmStateId target, Context& ctx)
    {
        const auto targetLeaf = ResolveLeaf(target);
        if (!targetLeaf.has_value())
        {
            return false;
        }

        std::array<HfsmStateId, kMaxHierarchyDepth> fromPath{};
        std::array<HfsmStateId, kMaxHierarchyDepth> toPath{};
        const std::size_t fromLen = BuildRootToNodePath(currentLeaf_, fromPath);
        const std::size_t toLen = BuildRootToNodePath(*targetLeaf, toPath);
        if (fromLen == 0 || toLen == 0)
        {
            return false;
        }

        // 求公共前缀长度(LCA 之上的共享祖先全部保留,不退出也不重入)。
        std::size_t lcaPrefix = 0;
        while (lcaPrefix < fromLen && lcaPrefix < toLen && fromPath[lcaPrefix] == toPath[lcaPrefix])
        {
            ++lcaPrefix;
        }

        // 显式切到当前自身(例如重置某子状态):至少退出并重入最末层状态。
        if (lcaPrefix == fromLen && lcaPrefix == toLen)
        {
            lcaPrefix = fromLen - 1;
        }

        TransitionGuard guard{inTransition_};

        // 1) 自底向上退出旧分支
        for (std::size_t i = fromLen; i > lcaPrefix; --i)
        {
            const HfsmStateId exiting = fromPath[i - 1];
            if (const auto& exitFn = states_[exiting].config.onExit)
            {
                exitFn(ctx);
            }
            currentLeaf_ = (i >= 2) ? fromPath[i - 2] : kInvalidHfsmState;
            RebuildActiveMask();
        }

        // 2) 自顶向下进入新分支
        for (std::size_t i = lcaPrefix; i < toLen; ++i)
        {
            const HfsmStateId entering = toPath[i];
            currentLeaf_ = entering;
            RebuildActiveMask();
            if (const auto& enterFn = states_[entering].config.onEnter)
            {
                enterFn(ctx);
            }
        }
        return true;
    }

    void DrainPendingTransitions(Context& ctx)
    {
        std::size_t drained = 0;
        while (pendingCount_ > 0 && drained < kMaxPendingTransitions)
        {
            const HfsmStateId next = pendingTargets_[pendingHead_];
            pendingHead_ = (pendingHead_ + 1) % kMaxPendingTransitions;
            --pendingCount_;
            ++drained;
            ExecuteTransition(next, ctx);
        }
    }

    std::vector<StateSlot> states_{};
    HfsmStateId currentLeaf_{kInvalidHfsmState};
    uint64_t activeMask_{0};
    bool started_{false};
    bool inTransition_{false};
    std::array<HfsmStateId, kMaxPendingTransitions> pendingTargets_{};
    std::size_t pendingHead_{0};
    std::size_t pendingCount_{0};
};

} // namespace combat_ai
