#include "actor_action_state.h"

#include <ranges>

#include "table/code/actoractionstate_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "actor/action_state/constants/actor_state.h"
#include "engine/core/macros/return_define.h"
#include "engine/core/macros/error_return.h"
#include "engine/core/type_define/type_define.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/event/actor_event.pb.h"

namespace {
    // Check if an action conflicts with the current state; return error code on conflict
    uint32_t CheckForStateConflict(const ActorActionStateTable* actorActionStateTable, uint32_t actorState) {
        if (actorState >= static_cast<uint32_t>(actorActionStateTable->state_size())) {
            return kSuccess;
        }

        if (const auto& state = actorActionStateTable->state(static_cast<int32_t>(actorState));
            state.state_mode() == kActionStateMutualExclusion) {
            return state.state_tip();
        }
        return kSuccess;
    }

    // Interrupt current state and perform the action
    bool InterruptAndPerformAction(const ActorActionStateTable* actorActionStateTable, uint32_t actorState, entt::entity actorEntity) {
        if (actorState >= static_cast<uint32_t>(actorActionStateTable->state_size())) {
            return false;
        }

        if (const auto& state = actorActionStateTable->state(static_cast<int32_t>(actorState));
            state.state_mode() == kActionStateInterrupt) {
            // Trigger interrupt event
            InterruptCurrentStateEvent interruptEvent;
            interruptEvent.set_actor_entity(entt::to_integral(actorEntity));
            interruptEvent.set_actor_state(actorState);
            tlsEcs.dispatcher.trigger(interruptEvent);

            RETURN_FALSE_ON_ERROR(ActorActionStateSystem::RemoveState(actorEntity, actorState));
            
            return true;
        }

        return false;
    }
}

uint32_t ActorActionStateSystem::TryPerformAction(entt::entity actorEntity, uint32_t actorAction, uint32_t successState) {
    LookupActorActionStateOrReturnError(actorAction);

    const auto *actorStatePbComponent = tlsEcs.actorRegistry.try_get<ActorStateComp>(actorEntity);
    if (!actorStatePbComponent)
    {
        RETURN_ON_ERROR(AddState(actorEntity, successState));
        return kSuccess;
    }
    for (const auto &actorState : actorStatePbComponent->state_list() | std::views::keys)
    {
        RETURN_ON_ERROR(CheckForStateConflict(actorActionStateRow, actorState));
    }

    // 打断分支会走 RemoveState -> state_list()->erase(actorState),而 protobuf Map
    // 的 erase 会让指向被删元素的迭代器失效 —— 正好就是 range-for 手里那一个,
    // 下一次 ++it 就是 UB。这条路径挂在每次释放技能的 CheckState 上,必须先把
    // 状态键快照出来再遍历。快照同时也切断了对 actorStatePbComponent 指针的依赖
    // (InterruptCurrentStateEvent 的下游 handler 可以任意增删组件)。
    UInt32Vector currentStates;
    currentStates.reserve(actorStatePbComponent->state_list().size());
    for (const auto &actorState : actorStatePbComponent->state_list() | std::views::keys)
    {
        currentStates.emplace_back(actorState);
    }

    for (const auto actorState : currentStates)
    {
        InterruptAndPerformAction(actorActionStateRow, actorState, actorEntity);
    }

    RETURN_ON_ERROR(AddState(actorEntity, successState));

    return kSuccess;
}


uint32_t ActorActionStateSystem::CanExecuteActionWithoutStateChange(entt::entity actorEntity, uint32_t actorAction) {
    LookupActorActionStateOrReturnError(actorAction);

    ECS_GET_OR_RETURN(actorStatePbComponent, ActorStateComp, actorEntity, kSuccess);
    for (const auto &actorState : actorStatePbComponent->state_list() | std::views::keys)
    {
        RETURN_ON_ERROR(CheckForStateConflict(actorActionStateRow, actorState));
    }

    return kSuccess;
}

bool ActorActionStateSystem::HasState(const entt::entity actorEntity, const uint32_t state) {
    ECS_GET_OR_FALSE(actorStatePbComponent, ActorStateComp, actorEntity);
    if (state >= kActorStateActorStateMax) {
        return false;
    }

    return actorStatePbComponent->state_list().contains(state);
}

uint32_t ActorActionStateSystem::AddState(const entt::entity actorEntity, uint32_t actorState) {
    auto& actorStatePbComponent = tlsEcs.actorRegistry.get_or_emplace<ActorStateComp>(actorEntity);
    if (actorState >= kActorStateActorStateMax){
        return MAKE_ERROR_MSG(kInvalidParameter,
            "entity=" << entt::to_integral(actorEntity)
            << " actorState=" << actorState);
    }

    if (actorStatePbComponent.state_list().contains(actorState)){
        return kSuccess;
    }

    actorStatePbComponent.mutable_state_list()->emplace(std::make_pair(actorState, true));
    return kSuccess;
}

uint32_t ActorActionStateSystem::RemoveState(entt::entity actorEntity, uint32_t actorState) {
    auto *actorStatePbComponent = tlsEcs.actorRegistry.try_get<ActorStateComp>(actorEntity);
    if (!actorStatePbComponent)
    {
        return MAKE_ERROR_MSG(kInvalidParameter,
                              "entity=" << entt::to_integral(actorEntity)
                                        << " actorState=" << actorState
                                        << " reason=no_ActorStateComp");
    }
    if (actorState >= kActorStateActorStateMax ||
        !actorStatePbComponent->state_list().contains(actorState))
    {
        return MAKE_ERROR_MSG(kInvalidParameter,
            "entity=" << entt::to_integral(actorEntity)
            << " actorState=" << actorState);
    }

    actorStatePbComponent->mutable_state_list()->erase(actorState);
    return kSuccess;
}

