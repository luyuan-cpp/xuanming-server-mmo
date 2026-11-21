#pragma once
#include <cstddef>
#include <cstdint>
#include <entt/src/entt/entity/entity.hpp>

class CombatStateSystem
{
public:
    static void AddCombatState(entt::entity actor, uint64_t sourceBuffId, uint32_t stateType);

    static void RemoveCombatState(entt::entity actor, uint64_t sourceBuffId, uint32_t stateType);

    static uint32_t ValidateSkillUsage(entt::entity entityId, uint32_t combatAction);

    // O(1) 读取当前激活的战斗异常状态位图(只读,不创建组件)。
    static uint64_t ActiveMask(entt::entity entityId);

    // 将 ActorActionCombatStateTableManager 当前快照预编译为 O(1) 位图互斥矩阵并校验合法性。
    static std::size_t RefreshCompiledTable();
};
