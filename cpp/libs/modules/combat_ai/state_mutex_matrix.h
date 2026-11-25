#pragma once

#include <array>
#include <bit>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <limits>
#include <span>
#include <unordered_map>
#include <utility>
#include <vector>

// 位图状态互斥与打断矩阵(纯 C++ 标准库,无外部依赖,无裸指针成员)。
//
// 适用场景:
// - 正交可叠加的角色状态(战斗中 / 跟随 / 骑乘 / 沉默 / 眩晕 / 施法相位等)的准入与打断。
// - 用 64 位位图取代 protobuf::Map 遍历与打断前 vector 拷贝,热路径校验为 O(1) 位运算且零堆分配。
// - 规范化规则表(StateRule: {action, state, mode, tip})为一等入口,新增状态只需加行,无需改定长槽位(cfg_slots)。
// - 显式携带 allowed 布尔与调用方传入的 successTip(兼容本仓 kSuccess = 1000 与独立单测 0,杜绝成功码错判)。
// - 两阶段执行:先纯只读校验(含入态边界),全部通过后再依次打断并触发退出钩子,最后入态,拒绝路径零副作用。

namespace combat_ai
{

inline constexpr uint32_t kMaxMutexStates = 64;
inline constexpr uint32_t kNoMutexState = std::numeric_limits<uint32_t>::max();

// 统一互斥模式枚举(与 eActionStateMode 0=互斥, 1=允许, 2=打断 一致)。
enum class StateMode : uint8_t
{
    kBlock = 0,
    kAllow = 1,
    kInterrupt = 2,
};

enum class StateExitReason : uint8_t
{
    kRemoved = 0,     // 显式移除状态
    kInterrupted = 1, // 被另一动作按互斥表打断
};

// 规范化单条互斥规则:显式携带 action 与 state 编号,消除定长槽位下标错位。
struct StateRule
{
    uint32_t action{0};
    uint32_t state{0};
    StateMode mode{StateMode::kAllow};
    uint32_t tip{0};
};

// 兼容定长槽位行(第 i 槽隐式对应 state = i)。
struct LegacySlotEntry
{
    uint32_t mode{static_cast<uint32_t>(StateMode::kAllow)};
    uint32_t tip{0};
};

// 加载期预编译后的单动作位图策略。
struct ActionMutexPolicy
{
    uint64_t blockMask{0};
    uint64_t interruptMask{0};
    std::array<uint32_t, kMaxMutexStates> blockTips{};
};

struct MutexEvaluateResult
{
    bool allowed{false};
    uint32_t tip{0};
    uint64_t interruptMask{0};

    [[nodiscard]] constexpr bool Allowed() const noexcept
    {
        return allowed;
    }
};

// 支持多来源叠加(如多个 Buff 同时施加沉默/眩晕)的引用计数位图。
// 计数 0 -> 1 置位,1 -> 0 清位;溢出按饱和截断,防止 uint16_t 回绕导致状态误消失。
class RefCountedStateBitset
{
public:
    bool Acquire(uint32_t state) noexcept
    {
        if (state >= kMaxMutexStates)
        {
            return false;
        }
        auto& count = refCounts[state];
        const bool becameActive = (count == 0);
        if (count < std::numeric_limits<uint16_t>::max())
        {
            ++count;
        }
        if (becameActive)
        {
            activeMask |= (uint64_t{1} << state);
        }
        return becameActive;
    }

    bool Release(uint32_t state) noexcept
    {
        if (state >= kMaxMutexStates)
        {
            return false;
        }
        auto& count = refCounts[state];
        if (count == 0)
        {
            return false;
        }
        --count;
        if (count == 0)
        {
            activeMask &= ~(uint64_t{1} << state);
            return true;
        }
        return false;
    }

    // 强制清零某个状态(例如被驱散或被互斥动作打断),返回原先是否处于激活态。
    bool ClearState(uint32_t state) noexcept
    {
        if (state >= kMaxMutexStates)
        {
            return false;
        }
        const bool wasActive = (refCounts[state] != 0);
        refCounts[state] = 0;
        activeMask &= ~(uint64_t{1} << state);
        return wasActive;
    }

    void Reset() noexcept
    {
        activeMask = 0;
        refCounts.fill(0);
    }

    [[nodiscard]] bool Has(uint32_t state) const noexcept
    {
        return state < kMaxMutexStates && ((activeMask & (uint64_t{1} << state)) != 0);
    }

    [[nodiscard]] uint16_t RefCount(uint32_t state) const noexcept
    {
        return state < kMaxMutexStates ? refCounts[state] : 0;
    }

    [[nodiscard]] uint64_t Mask() const noexcept
    {
        return activeMask;
    }

private:
    uint64_t activeMask{0};
    std::array<uint16_t, kMaxMutexStates> refCounts{};
};

// 状态退出钩子注册表:按状态编号 O(1) 派发子系统收尾(脱跟随 / 下马 / 停施法)。
template <typename Entity>
class StateExitHookRegistry
{
public:
    using Hook = std::function<void(Entity, StateExitReason)>;

    void Register(uint32_t state, Hook hook)
    {
        if (state >= kMaxMutexStates)
        {
            return;
        }
        hooks[state] = std::move(hook);
    }

    void Unregister(uint32_t state)
    {
        if (state >= kMaxMutexStates)
        {
            return;
        }
        hooks[state] = {};
    }

    void Clear()
    {
        for (auto& hook : hooks)
        {
            hook = {};
        }
    }

    void NotifyExited(Entity entity, uint32_t state, StateExitReason reason) const
    {
        if (state >= kMaxMutexStates)
        {
            return;
        }
        if (const auto& hook = hooks[state])
        {
            hook(entity, reason);
        }
    }

private:
    std::array<Hook, kMaxMutexStates> hooks{};
};

// 编译期/加载期构建的 O(1) 动作 × 状态互斥矩阵。
// 线程模型:加载期构建一次,热路径只读;不保存任何外部表行指针。
class StateMutexMatrix
{
public:
    void Clear()
    {
        policies.clear();
    }

    // 显式注册某动作(即使全部允许也能通过 HasAction 校验)。
    void EnsureAction(uint32_t action)
    {
        policies.try_emplace(action);
    }

    // 按显式 (action, state) 写入单条规则,不依赖槽位位置。
    bool SetRule(uint32_t action, uint32_t state, StateMode mode, uint32_t tip)
    {
        if (state >= kMaxMutexStates)
        {
            return false;
        }
        auto& policy = policies[action];
        const uint64_t bit = uint64_t{1} << state;
        policy.blockMask &= ~bit;
        policy.interruptMask &= ~bit;
        policy.blockTips[state] = 0;

        switch (mode)
        {
        case StateMode::kBlock:
            policy.blockMask |= bit;
            policy.blockTips[state] = tip;
            return true;
        case StateMode::kAllow:
            return true;
        case StateMode::kInterrupt:
            policy.interruptMask |= bit;
            return true;
        }
        return false;
    }

    // 批量装载规范化规则列表;返回非法规则条数(0 表示全部合法)。
    std::size_t LoadRules(std::span<const StateRule> rules)
    {
        std::size_t invalidCount = 0;
        for (const auto& rule : rules)
        {
            if (!SetRule(rule.action, rule.state, rule.mode, rule.tip))
            {
                ++invalidCount;
            }
        }
        return invalidCount;
    }

    // 兼容定长槽位行:第 i 槽对应 state = i。返回越界或非法 mode 的槽位数。
    std::size_t LoadLegacyActionRow(uint32_t action, std::span<const LegacySlotEntry> slots)
    {
        EnsureAction(action);
        std::size_t invalidCount = 0;
        for (std::size_t i = 0; i < slots.size(); ++i)
        {
            if (i >= kMaxMutexStates || slots[i].mode > static_cast<uint32_t>(StateMode::kInterrupt))
            {
                ++invalidCount;
                continue;
            }
            SetRule(action, static_cast<uint32_t>(i), static_cast<StateMode>(slots[i].mode), slots[i].tip);
        }
        return invalidCount;
    }

    [[nodiscard]] bool HasAction(uint32_t action) const
    {
        return policies.find(action) != policies.end();
    }

    [[nodiscard]] std::size_t ActionCount() const noexcept
    {
        return policies.size();
    }

    // 纯只读判定:O(1) 位运算,零堆分配,零副作用。
    // 当存在多个互斥状态时,按最小状态编号(最低位)稳定返回对应 tip。
    [[nodiscard]] MutexEvaluateResult Evaluate(uint64_t activeMask, uint32_t action,
                                               uint32_t successTip, uint32_t unknownActionTip) const
    {
        const auto it = policies.find(action);
        if (it == policies.end())
        {
            return MutexEvaluateResult{false, unknownActionTip, 0};
        }
        const auto& policy = it->second;
        if (const uint64_t blocked = activeMask & policy.blockMask; blocked != 0)
        {
            const auto firstBlocked = static_cast<uint32_t>(std::countr_zero(blocked));
            return MutexEvaluateResult{false, policy.blockTips[firstBlocked], 0};
        }
        return MutexEvaluateResult{true, successTip, activeMask & policy.interruptMask};
    }

    [[nodiscard]] uint32_t CanPerform(uint64_t activeMask, uint32_t action,
                                      uint32_t successTip, uint32_t unknownActionTip) const
    {
        return Evaluate(activeMask, action, successTip, unknownActionTip).tip;
    }

    // 遍历某个位掩码里的每一个置位状态编号(按升序,栈上纯整数循环,无堆分配与迭代器失效)。
    template <typename Fn>
    static void ForEachBit(uint64_t mask, Fn&& fn)
    {
        while (mask != 0)
        {
            const auto state = static_cast<uint32_t>(std::countr_zero(mask));
            mask &= (mask - 1);
            fn(state);
        }
    }

    // 两阶段原语:先纯校验(含入态边界),全部通过后才依次执行打断回调与入态回调。
    // onInterrupt(uint32_t interruptedState) 与 onEnter(uint32_t enterState) 由调用方承接实际状态写入与钩子。
    template <typename OnInterrupt, typename OnEnter>
    uint32_t TryPerform(uint64_t activeMask, uint32_t action, uint32_t enterState, uint32_t maxValidState,
                        uint32_t successTip, uint32_t unknownActionTip, uint32_t invalidStateTip,
                        OnInterrupt&& onInterrupt, OnEnter&& onEnter) const
    {
        if (enterState != kNoMutexState && (enterState >= maxValidState || enterState >= kMaxMutexStates))
        {
            return invalidStateTip;
        }
        const auto eval = Evaluate(activeMask, action, successTip, unknownActionTip);
        if (!eval.Allowed())
        {
            return eval.tip;
        }
        ForEachBit(eval.interruptMask, [&](uint32_t state) { onInterrupt(state); });
        if (enterState != kNoMutexState)
        {
            onEnter(enterState);
        }
        return successTip;
    }

private:
    std::unordered_map<uint32_t, ActionMutexPolicy> policies;
};

} // namespace combat_ai
