#pragma once
#include <cstddef>
#include <cstdint>
#include <functional>
#include <span>
#include <entt/src/entt/entity/entity.hpp>

#include "modules/combat_ai/state_mutex_matrix.h"

// actor 行为状态(战斗中 / 跟随 / 骑乘)的互斥、打断与退出收尾。
// 前置条件:actorEntity 是 actorRegistry 中的活实体;失效实体在任何状态副作用之前拒绝。
// ActorStateComp 与运行期位图缓存 ActorStateMaskComp 按需存在,缺失表示没有激活状态。
// AddState 是唯一延迟创建入口;查询不创建,RemoveState 对不存在的状态幂等成功且不触发退出钩子,
// HasState 对缺失或失效实体答 false。
// 线程模型:只在 scene 节点 loop 线程调用。
class ActorActionStateSystem
{
public:
    using ExitReason = combat_ai::StateExitReason;
    using ExitHook = combat_ai::StateExitHookRegistry<entt::entity>::Hook;
    using SelfStateSyncTestHook =
        std::function<void(entt::entity, uint64_t actionStateMask, uint64_t combatStateMask, uint64_t stateSeq)>;

    static uint32_t TryPerformAction(entt::entity actorEntity, uint32_t actorAction, uint32_t successState);

    static uint32_t CanExecuteActionWithoutStateChange(entt::entity actorEntity, uint32_t actorAction);

    static bool HasActionPolicy(uint32_t actorAction);

    static bool HasState(entt::entity actorEntity, uint32_t state);

    // O(1) 读取当前激活的行为状态位图(只读,不创建组件)。
    static uint64_t ActiveMask(entt::entity actorEntity);

    static uint32_t AddState(entt::entity actorEntity, uint32_t actorState);

    static uint32_t RemoveState(entt::entity actorEntity, uint32_t actorState);

    // 注册某状态的退出/打断收尾钩子(例如打断跟随 -> TeamFollowSystem::Detach,打断战斗 -> 中止在途施法)。
    // 钩子在状态从位图与组件中移除之后触发,且仅当该状态原先确实激活时调用一次,防止重入循环与虚假触发。
    static void RegisterExitHook(uint32_t actorState, ExitHook hook);
    static void ClearExitHook(uint32_t actorState);
    static void ClearAllExitHooks();

    // 将规范化后的 ActorActionStateTableManager 快照预编译为 O(1) 位图互斥矩阵,
    // 并校验 state_id 范围、state_mode 合法性与重复规则行;返回非法条目数(0 表示全部合法)。
    static std::size_t RefreshCompiledTable();

    // 以规范化规则列表({action, state, mode, tip})覆盖当前矩阵(供单测或动态策略注入)。
    // 调用 ResetRulesToTable() 可恢复读取 ActorActionStateTableManager。
    static std::size_t LoadRules(std::span<const combat_ai::StateRule> rules);
    static void ResetRulesToTable();

    // 当主角行为状态位图或战斗异常状态位图较上次推送发生变化时,立即单播 SelfActorStateFlagsS2C 给主角本人。
    // TryPerformAction 内部自动合并本轮所有打断与入态为一次推送;仅对拥有有效网关会话且未冻结的玩家发送。
    static void SyncSelfStateFlagsIfChanged(entt::entity actorEntity);

    // 进场 / 重连落地后调用:重置上次推送基准为 0(客户端入场时已复位),若存在非零状态立刻补推一次。
    static void ResendStateFlagsAfterEntry(entt::entity actorEntity);

    // 单测探针:非空时接管原本的 SendToPlayerClient 单播(传空恢复真实网络发送)。
    static void SetSelfStateSyncTestHook(SelfStateSyncTestHook hook);
};
