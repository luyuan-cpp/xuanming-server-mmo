#include "combat_state.h"

#include <ranges>

#include "table/code/actoractioncombatstate_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"
#include "actor/attribute/system/actor_attribute_calculator.h"
#include "combat_state/constants/combat_state.h"
#include "macros/return_define.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include "proto/common/event/actor_combat_state_event.pb.h"

void CombatStateSystem::AddCombatState(const CombatStateAddedEvent& addEvent) {
    const auto entityId = entt::to_entity(addEvent.actor_entity());
    // 先校验实体、再校验参数,最后才 get_or_emplace。
    // 事件里的 entity 是外部数据(buff 生命周期投递,buff 可能比实体活得久),
    // 对一个已销毁的实体做 get_or_emplace 在 EnTT 里是 UB(Release 下直接写坏内存)。
    // 参数校验也必须前置,否则非法 state_type 会白白给实体挂上一个空组件。
    if (!tlsEcs.actorRegistry.valid(entityId)) {
        return;
    }

    if (addEvent.state_type() >= kActorMaxCombatStateType) {
        return;
    }

    auto& combatStateCollection = tlsEcs.actorRegistry.get_or_emplace<CombatStateCollectionComp>(entityId);

    auto stateIterator = combatStateCollection.mutable_states()->find(addEvent.state_type());
    if (stateIterator == combatStateCollection.mutable_states()->end()) {
        const auto [newStateIterator, wasInserted] = combatStateCollection.mutable_states()->emplace(
            addEvent.state_type(), CombatStateDetailsComp{});
        if (!wasInserted) {
            return; 
        }
        stateIterator = newStateIterator;
    }

    stateIterator->second.mutable_sources()->emplace(addEvent.source_buff_id(), false);

    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(entityId, kCombatState);
}

void CombatStateSystem::RemoveCombatState(const CombatStateRemovedEvent& removeEvent) {
    const auto entityId = entt::to_entity(removeEvent.actor_entity());
    // 同 AddCombatState:实体可能已销毁(buff 销毁事件常常晚于实体)。
    // 而且"移除"路径本来就不该 get_or_emplace —— 那会给从来没有过战斗状态的
    // 实体凭空建一个空组件(§7.5 也禁止在这类路径上 get_or_emplace)。
    if (!tlsEcs.actorRegistry.valid(entityId)) {
        return;
    }

    if (removeEvent.state_type() >= kActorMaxCombatStateType) {
        return;
    }

    auto* combatStateCollectionPtr = tlsEcs.actorRegistry.try_get<CombatStateCollectionComp>(entityId);
    if (combatStateCollectionPtr == nullptr) {
        return;
    }
    auto& combatStateCollection = *combatStateCollectionPtr;

    auto stateIterator = combatStateCollection.mutable_states()->find(removeEvent.state_type());
    if (stateIterator == combatStateCollection.mutable_states()->end()) {
        return;
    }

    stateIterator->second.mutable_sources()->erase(removeEvent.source_buff_id());

    if (stateIterator->second.sources().empty()) {
        combatStateCollection.mutable_states()->erase(stateIterator);
    }

    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(entityId, kCombatState);
}

uint32_t CombatStateSystem::ValidateSkillUsage(const entt::entity entityId, const uint32_t combatAction)
{
    ECS_GET_OR_RETURN(combatStateCollection, CombatStateCollectionComp, entityId, kSuccess);

    if (combatStateCollection->states().empty())
    {
        return kSuccess;
    }

    LookupActorActionCombatStateOrReturnError(combatAction);

    for (const auto &stateKey : combatStateCollection->states() | std::views::keys)
    {
        // 表里 state 这一列的长度是配置决定的,和 kActorMaxCombatStateType 没有
        // 任何强制关系;repeated 字段的 Get(index) 越界在 Release 下是野读。
        // 同一套判定在 actor_action_state.cpp 的 CheckForStateConflict 里是有
        // 边界检查的,这里漏了,补齐口径。
        if (stateKey >= static_cast<uint32_t>(actorActionCombatStateRow->state_size())) {
            continue;
        }

        const auto& combatState = actorActionCombatStateRow->state(static_cast<int32_t>(stateKey));

        if (combatState.state_mode() == kCombatStateMutualExclusion) {
            return combatState.state_tip();
        }
    }

    return kSuccess;
}

