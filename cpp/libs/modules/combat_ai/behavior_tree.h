#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <limits>
#include <optional>
#include <unordered_map>
#include <utility>
#include <variant>
#include <vector>

// 数据驱动行为树(融合 BehaviorTree.CPP 的响应式抢占/Halt 与 behavior3go 的静态树+实例黑板分离,
// 纯 C++ 标准库实现,无裸指针成员,无每节点堆对象)。
//
// MMO 场景关键设计:
// 1. 静态拓扑与运行态分离:1000 只同种副本怪共享一份不可变 BehaviorTreeDef,
//    每只怪仅持有轻量 BehaviorTreeState(扁平节点状态数组 + 整型键黑板),内存占用极小。
// 2. 扁平索引仓库(NodeIndex = uint32_t):节点与子节点表全为连续 std::vector,零裸指针、缓存友好。
// 3. 响应式抢占(ReactiveSequence / ReactiveSelector):每 tick 从高优先级子节点重算,
//    一旦高优先级条件变化并抢占,立即级联调用被抢占子树的 Halt 钩子清理读条/追击等中途状态。
// 4. 冷却装饰器按 tick 计数,不读挂钟时间,保证单测与回放 100% 确定性。

namespace combat_ai
{

enum class BtStatus : uint8_t
{
    kSuccess = 0,
    kFailure = 1,
    kRunning = 2,
};

using BtNodeId = uint32_t;
inline constexpr BtNodeId kInvalidBtNode = std::numeric_limits<BtNodeId>::max();

// 整型键类型安全黑板:规避热路径字符串分配与哈希。
class Blackboard
{
public:
    using Value = std::variant<int64_t, uint64_t, double, bool>;

    template <typename T>
    void Set(uint32_t key, T value)
    {
        entries_[key] = Value{value};
    }

    template <typename T>
    [[nodiscard]] std::optional<T> Get(uint32_t key) const
    {
        const auto it = entries_.find(key);
        if (it == entries_.end())
        {
            return std::nullopt;
        }
        if (const auto* val = std::get_if<T>(&it->second))
        {
            return *val;
        }
        return std::nullopt;
    }

    template <typename T>
    [[nodiscard]] T GetOr(uint32_t key, T fallback) const
    {
        return Get<T>(key).value_or(fallback);
    }

    [[nodiscard]] bool Has(uint32_t key) const
    {
        return entries_.find(key) != entries_.end();
    }

    void Erase(uint32_t key)
    {
        entries_.erase(key);
    }

    void Clear()
    {
        entries_.clear();
    }

private:
    std::unordered_map<uint32_t, Value> entries_;
};

// 每只怪 / 每个实体独享的运行态快照。
struct BehaviorTreeState
{
    struct NodeMemory
    {
        uint32_t runningChild{0};
        uint32_t repeatCount{0};
        uint64_t cooldownReadyTick{0};
        bool isRunning{false};
    };

    uint64_t tickCount{0};
    std::vector<NodeMemory> memory{};
    Blackboard blackboard{};

    void ResetRuntime()
    {
        for (auto& slot : memory)
        {
            slot = NodeMemory{};
        }
    }
};

enum class BtNodeKind : uint8_t
{
    kSequence = 0,         // 顺序执行,记住 Running 子节点
    kSelector = 1,         // 备选回退,记住 Running 子节点
    kReactiveSequence = 2, // 每 tick 从头复核条件,前序失败/挂起时 Halt 后序正在跑的子树
    kReactiveSelector = 3, // 每 tick 从高优先级重选,高优分支接管时 Halt 低优正在跑的子树
    kParallel = 4,         // 并行按成功/失败阈值裁决
    kInverter = 5,         // 取反(Running 透传)
    kForceSuccess = 6,     // 失败转成功(Running 透传)
    kRepeat = 7,           // 重复执行最多 N 次(0 表示无限次直到失败)
    kCooldown = 8,         // 成功后进入固定 tick 冷却
    kCondition = 9,        // 瞬时布尔条件叶子
    kAction = 10,          // 动作叶子(支持 Running 与被抢占时的 Halt 回调)
};

template <typename Context>
class BehaviorTreeBuilder;

// 共享只读行为树拓扑定义。
template <typename Context>
class BehaviorTreeDef
{
public:
    using ConditionFn = std::function<bool(const Context&, const Blackboard&)>;
    using ActionTickFn = std::function<BtStatus(Context&, Blackboard&)>;
    using ActionHaltFn = std::function<void(Context&, Blackboard&)>;

    [[nodiscard]] bool Valid() const noexcept
    {
        return root_ != kInvalidBtNode && root_ < nodes_.size();
    }

    [[nodiscard]] BehaviorTreeState CreateState() const
    {
        BehaviorTreeState state;
        state.memory.resize(nodes_.size());
        return state;
    }

    // 推进一帧;调用方只需持有一份共享 BehaviorTreeDef 与实体自己的 BehaviorTreeState。
    BtStatus Tick(Context& ctx, BehaviorTreeState& state) const
    {
        if (!Valid())
        {
            return BtStatus::kFailure;
        }
        if (state.memory.size() != nodes_.size())
        {
            state.memory.assign(nodes_.size(), BehaviorTreeState::NodeMemory{});
        }
        ++state.tickCount;
        return TickNode(root_, ctx, state);
    }

    // 外部强制中止整棵树(如怪物死亡 / 脱战重置 / 被强控)。
    void HaltAll(Context& ctx, BehaviorTreeState& state) const
    {
        if (!Valid() || state.memory.size() != nodes_.size())
        {
            return;
        }
        HaltNode(root_, ctx, state);
    }

private:
    friend class BehaviorTreeBuilder<Context>;

    struct NodeDef
    {
        BtNodeKind kind{BtNodeKind::kCondition};
        uint32_t childBegin{0};
        uint32_t childCount{0};
        uint32_t paramA{0}; // Parallel: successThreshold; Repeat: maxCount; Cooldown: cooldownTicks
        uint32_t paramB{0}; // Parallel: failureThreshold
        ConditionFn condition{};
        ActionTickFn action{};
        ActionHaltFn onHalt{};
    };

    void HaltNode(BtNodeId nodeId, Context& ctx, BehaviorTreeState& state) const
    {
        auto& mem = state.memory[nodeId];
        if (!mem.isRunning)
        {
            return;
        }
        const auto& node = nodes_[nodeId];
        for (uint32_t i = 0; i < node.childCount; ++i)
        {
            HaltNode(childIndices_[node.childBegin + i], ctx, state);
        }
        if (node.kind == BtNodeKind::kAction && node.onHalt)
        {
            node.onHalt(ctx, state.blackboard);
        }
        mem.isRunning = false;
        mem.runningChild = 0;
        mem.repeatCount = 0;
    }

    BtStatus TickNode(BtNodeId nodeId, Context& ctx, BehaviorTreeState& state) const
    {
        const auto& node = nodes_[nodeId];
        auto& mem = state.memory[nodeId];
        BtStatus result = BtStatus::kFailure;

        switch (node.kind)
        {
        case BtNodeKind::kCondition:
            result = (node.condition && node.condition(ctx, state.blackboard))
                         ? BtStatus::kSuccess
                         : BtStatus::kFailure;
            break;

        case BtNodeKind::kAction:
            result = node.action ? node.action(ctx, state.blackboard) : BtStatus::kFailure;
            break;

        case BtNodeKind::kSequence: {
            const uint32_t start = mem.isRunning ? mem.runningChild : 0;
            result = BtStatus::kSuccess;
            for (uint32_t i = start; i < node.childCount; ++i)
            {
                const BtStatus childStatus = TickNode(childIndices_[node.childBegin + i], ctx, state);
                if (childStatus == BtStatus::kRunning)
                {
                    mem.runningChild = i;
                    result = BtStatus::kRunning;
                    break;
                }
                if (childStatus == BtStatus::kFailure)
                {
                    mem.runningChild = 0;
                    result = BtStatus::kFailure;
                    break;
                }
            }
            if (result != BtStatus::kRunning)
            {
                mem.runningChild = 0;
            }
            break;
        }

        case BtNodeKind::kSelector: {
            const uint32_t start = mem.isRunning ? mem.runningChild : 0;
            result = BtStatus::kFailure;
            for (uint32_t i = start; i < node.childCount; ++i)
            {
                const BtStatus childStatus = TickNode(childIndices_[node.childBegin + i], ctx, state);
                if (childStatus == BtStatus::kRunning)
                {
                    mem.runningChild = i;
                    result = BtStatus::kRunning;
                    break;
                }
                if (childStatus == BtStatus::kSuccess)
                {
                    mem.runningChild = 0;
                    result = BtStatus::kSuccess;
                    break;
                }
            }
            if (result != BtStatus::kRunning)
            {
                mem.runningChild = 0;
            }
            break;
        }

        case BtNodeKind::kReactiveSequence: {
            result = BtStatus::kSuccess;
            for (uint32_t i = 0; i < node.childCount; ++i)
            {
                const BtStatus childStatus = TickNode(childIndices_[node.childBegin + i], ctx, state);
                if (childStatus != BtStatus::kSuccess)
                {
                    // 截断后续分支:上一帧还在跑的后续子节点必须全部 Halt。
                    for (uint32_t j = i + 1; j < node.childCount; ++j)
                    {
                        HaltNode(childIndices_[node.childBegin + j], ctx, state);
                    }
                    result = childStatus;
                    break;
                }
            }
            break;
        }

        case BtNodeKind::kReactiveSelector: {
            result = BtStatus::kFailure;
            for (uint32_t i = 0; i < node.childCount; ++i)
            {
                const BtStatus childStatus = TickNode(childIndices_[node.childBegin + i], ctx, state);
                if (childStatus != BtStatus::kFailure)
                {
                    // 高优先级分支接管:低优先级正在运行的分支立刻 Halt。
                    for (uint32_t j = i + 1; j < node.childCount; ++j)
                    {
                        HaltNode(childIndices_[node.childBegin + j], ctx, state);
                    }
                    result = childStatus;
                    break;
                }
            }
            break;
        }

        case BtNodeKind::kParallel: {
            uint32_t successCount = 0;
            uint32_t failureCount = 0;
            const uint32_t successNeed = node.paramA == 0 ? node.childCount : node.paramA;
            const uint32_t failureNeed = node.paramB == 0 ? 1u : node.paramB;
            for (uint32_t i = 0; i < node.childCount; ++i)
            {
                const BtStatus childStatus = TickNode(childIndices_[node.childBegin + i], ctx, state);
                if (childStatus == BtStatus::kSuccess)
                {
                    ++successCount;
                }
                else if (childStatus == BtStatus::kFailure)
                {
                    ++failureCount;
                }
            }
            if (successCount >= successNeed)
            {
                result = BtStatus::kSuccess;
            }
            else if (failureCount >= failureNeed)
            {
                result = BtStatus::kFailure;
            }
            else
            {
                result = BtStatus::kRunning;
            }
            if (result != BtStatus::kRunning)
            {
                for (uint32_t i = 0; i < node.childCount; ++i)
                {
                    HaltNode(childIndices_[node.childBegin + i], ctx, state);
                }
            }
            break;
        }

        case BtNodeKind::kInverter: {
            if (node.childCount == 1)
            {
                const BtStatus childStatus = TickNode(childIndices_[node.childBegin], ctx, state);
                result = (childStatus == BtStatus::kRunning)
                             ? BtStatus::kRunning
                             : (childStatus == BtStatus::kSuccess ? BtStatus::kFailure : BtStatus::kSuccess);
            }
            break;
        }

        case BtNodeKind::kForceSuccess: {
            if (node.childCount == 1)
            {
                const BtStatus childStatus = TickNode(childIndices_[node.childBegin], ctx, state);
                result = (childStatus == BtStatus::kRunning) ? BtStatus::kRunning : BtStatus::kSuccess;
            }
            break;
        }

        case BtNodeKind::kRepeat: {
            if (node.childCount != 1)
            {
                break;
            }
            const BtNodeId child = childIndices_[node.childBegin];
            result = BtStatus::kSuccess;
            while (node.paramA == 0 || mem.repeatCount < node.paramA)
            {
                const BtStatus childStatus = TickNode(child, ctx, state);
                if (childStatus == BtStatus::kRunning)
                {
                    result = BtStatus::kRunning;
                    break;
                }
                if (childStatus == BtStatus::kFailure)
                {
                    mem.repeatCount = 0;
                    result = BtStatus::kFailure;
                    break;
                }
                ++mem.repeatCount;
                // 无限循环模式单帧最多跑一次,防止同一 tick 内死循环卡死主线程。
                if (node.paramA == 0)
                {
                    result = BtStatus::kRunning;
                    break;
                }
            }
            if (result != BtStatus::kRunning)
            {
                mem.repeatCount = 0;
            }
            break;
        }

        case BtNodeKind::kCooldown: {
            if (node.childCount != 1)
            {
                break;
            }
            if (!mem.isRunning && state.tickCount < mem.cooldownReadyTick)
            {
                result = BtStatus::kFailure;
                break;
            }
            result = TickNode(childIndices_[node.childBegin], ctx, state);
            if (result == BtStatus::kSuccess)
            {
                mem.cooldownReadyTick = state.tickCount + node.paramA;
            }
            break;
        }
        }

        mem.isRunning = (result == BtStatus::kRunning);
        return result;
    }

    BtNodeId root_{kInvalidBtNode};
    std::vector<NodeDef> nodes_{};
    std::vector<BtNodeId> childIndices_{};
};

// 构树器:Build() 阶段一次性扁平化子节点表并做环路检测,保证运行期零指针、无死循环。
template <typename Context>
class BehaviorTreeBuilder
{
public:
    using Def = BehaviorTreeDef<Context>;
    using ConditionFn = typename Def::ConditionFn;
    using ActionTickFn = typename Def::ActionTickFn;
    using ActionHaltFn = typename Def::ActionHaltFn;

    BtNodeId Sequence(std::initializer_list<BtNodeId> children)
    {
        return AddComposite(BtNodeKind::kSequence, children);
    }

    BtNodeId Selector(std::initializer_list<BtNodeId> children)
    {
        return AddComposite(BtNodeKind::kSelector, children);
    }

    BtNodeId ReactiveSequence(std::initializer_list<BtNodeId> children)
    {
        return AddComposite(BtNodeKind::kReactiveSequence, children);
    }

    BtNodeId ReactiveSelector(std::initializer_list<BtNodeId> children)
    {
        return AddComposite(BtNodeKind::kReactiveSelector, children);
    }

    BtNodeId Parallel(uint32_t successThreshold, uint32_t failureThreshold,
                      std::initializer_list<BtNodeId> children)
    {
        const BtNodeId id = AddComposite(BtNodeKind::kParallel, children);
        drafts_[id].paramA = successThreshold;
        drafts_[id].paramB = failureThreshold;
        return id;
    }

    BtNodeId Inverter(BtNodeId child)
    {
        return AddDecorator(BtNodeKind::kInverter, child, 0);
    }

    BtNodeId ForceSuccess(BtNodeId child)
    {
        return AddDecorator(BtNodeKind::kForceSuccess, child, 0);
    }

    BtNodeId Repeat(uint32_t maxCount, BtNodeId child)
    {
        return AddDecorator(BtNodeKind::kRepeat, child, maxCount);
    }

    BtNodeId Cooldown(uint32_t cooldownTicks, BtNodeId child)
    {
        return AddDecorator(BtNodeKind::kCooldown, child, cooldownTicks);
    }

    BtNodeId Condition(ConditionFn fn)
    {
        DraftNode node;
        node.kind = BtNodeKind::kCondition;
        node.condition = std::move(fn);
        return PushDraft(std::move(node));
    }

    BtNodeId Action(ActionTickFn tickFn, ActionHaltFn haltFn = {})
    {
        DraftNode node;
        node.kind = BtNodeKind::kAction;
        node.action = std::move(tickFn);
        node.onHalt = std::move(haltFn);
        return PushDraft(std::move(node));
    }

    // 编译为不可变扁平树;若根节点非法或图中有环,返回 std::nullopt(fail-closed)。
    [[nodiscard]] std::optional<Def> Build(BtNodeId root)
    {
        if (root >= drafts_.size())
        {
            return std::nullopt;
        }
        // 三色 DFS 检环与悬空子节点编号。
        std::vector<uint8_t> color(drafts_.size(), 0);
        if (!ValidateAcyclic(root, color))
        {
            return std::nullopt;
        }

        Def def;
        def.root_ = root;
        def.nodes_.reserve(drafts_.size());
        for (auto& draft : drafts_)
        {
            typename Def::NodeDef out;
            out.kind = draft.kind;
            out.childBegin = static_cast<uint32_t>(def.childIndices_.size());
            out.childCount = static_cast<uint32_t>(draft.children.size());
            out.paramA = draft.paramA;
            out.paramB = draft.paramB;
            out.condition = std::move(draft.condition);
            out.action = std::move(draft.action);
            out.onHalt = std::move(draft.onHalt);
            def.childIndices_.insert(def.childIndices_.end(), draft.children.begin(), draft.children.end());
            def.nodes_.push_back(std::move(out));
        }
        return def;
    }

private:
    struct DraftNode
    {
        BtNodeKind kind{BtNodeKind::kCondition};
        uint32_t paramA{0};
        uint32_t paramB{0};
        std::vector<BtNodeId> children{};
        ConditionFn condition{};
        ActionTickFn action{};
        ActionHaltFn onHalt{};
    };

    BtNodeId PushDraft(DraftNode node)
    {
        const auto id = static_cast<BtNodeId>(drafts_.size());
        drafts_.push_back(std::move(node));
        return id;
    }

    BtNodeId AddComposite(BtNodeKind kind, std::initializer_list<BtNodeId> children)
    {
        DraftNode node;
        node.kind = kind;
        node.children.assign(children.begin(), children.end());
        return PushDraft(std::move(node));
    }

    BtNodeId AddDecorator(BtNodeKind kind, BtNodeId child, uint32_t paramA)
    {
        DraftNode node;
        node.kind = kind;
        node.paramA = paramA;
        node.children.push_back(child);
        return PushDraft(std::move(node));
    }

    bool ValidateAcyclic(BtNodeId id, std::vector<uint8_t>& color) const
    {
        if (id >= drafts_.size())
        {
            return false;
        }
        if (color[id] == 1)
        {
            return false; // 检测到环路
        }
        if (color[id] == 2)
        {
            return true;
        }
        color[id] = 1;
        for (const BtNodeId child : drafts_[id].children)
        {
            if (!ValidateAcyclic(child, color))
            {
                return false;
            }
        }
        color[id] = 2;
        return true;
    }

    std::vector<DraftNode> drafts_{};
};

} // namespace combat_ai
