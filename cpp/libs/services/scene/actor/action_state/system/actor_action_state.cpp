#include "actor_action_state.h"

#include <cstdint>
#include <unordered_set>
#include <utility>

#include "muduo/base/Logging.h"
#include "table/code/actoractionstate_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "actor/action_state/constants/actor_state.h"
#include "engine/core/macros/error_return.h"
#include "engine/core/type_define/type_define.h"
#include "proto/common/component/actor_comp.pb.h"
#include "thread_context/ecs_context.h"

namespace {
    // 64 位运行期状态位图缓存,与 ActorStateComp 同步维护,避免热路径遍历 protobuf::Map。
    // 仅在首次写状态时随 ActorStateComp 一起延迟创建,不参与持久化。
    struct ActorStateMaskComp
    {
        uint64_t mask{0};
    };

    struct CompiledActionStateCache
    {
        std::uintptr_t snapshotAddr{0};
        std::uintptr_t firstRowAddr{0};
        int32_t rowCount{-1};
        bool usingCustomRules{false};
        combat_ai::StateMutexMatrix matrix{};
    };

    thread_local CompiledActionStateCache tlsCompiledActionStateCache{};
    thread_local combat_ai::StateExitHookRegistry<entt::entity> tlsExitHooks{};

    static_assert(static_cast<uint32_t>(kActorStateActorStateMax) <= combat_ai::kMaxMutexStates,
                  "eActorState exceeds 64-bit StateMutexMatrix capacity");
    static_assert(static_cast<uint32_t>(kActionStateMutualExclusion) ==
                      static_cast<uint32_t>(combat_ai::StateMode::kBlock),
                  "StateMode::kBlock must match kActionStateMutualExclusion");
    static_assert(static_cast<uint32_t>(kActionStatePermitted) ==
                      static_cast<uint32_t>(combat_ai::StateMode::kAllow),
                  "StateMode::kAllow must match kActionStatePermitted");
    static_assert(static_cast<uint32_t>(kActionStateInterrupt) ==
                      static_cast<uint32_t>(combat_ai::StateMode::kInterrupt),
                  "StateMode::kInterrupt must match kActionStateInterrupt");

    ActorStateComp* FindActorStateComp(entt::entity actorEntity) {
        if (!tlsEcs.actorRegistry.valid(actorEntity)) {
            return nullptr;
        }
        return tlsEcs.actorRegistry.try_get<ActorStateComp>(actorEntity);
    }

    uint64_t FoldProtoStateMask(const ActorStateComp& comp) {
        uint64_t mask = 0;
        for (const auto& [state, active] : comp.state_list()) {
            (void)active;
            if (state < kActorStateActorStateMax) {
                mask |= (uint64_t{1} << state);
            }
        }
        return mask;
    }

    // 热路径 O(1) 读 ActorStateMaskComp;若单测绕过 AddState 直接 emplace 了 ActorStateComp,
    // 退化折叠一次到栈上位图(仍然零堆分配),不丢失既有状态。
    uint64_t ReadActiveMask(entt::entity actorEntity) {
        if (!tlsEcs.actorRegistry.valid(actorEntity)) {
            return 0;
        }
        if (const auto* maskComp = tlsEcs.actorRegistry.try_get<ActorStateMaskComp>(actorEntity)) {
            return maskComp->mask;
        }
        if (const auto* actorStatePbComponent = tlsEcs.actorRegistry.try_get<ActorStateComp>(actorEntity)) {
            return FoldProtoStateMask(*actorStatePbComponent);
        }
        return 0;
    }

    std::size_t CompileFromTableSnapshot(CompiledActionStateCache& cache) {
        const auto& snap = ActorActionStateTableManager::Instance().GetSnapshot();
        cache.snapshotAddr = reinterpret_cast<std::uintptr_t>(&snap);
        cache.rowCount = snap.data.data_size();
        cache.firstRowAddr =
            cache.rowCount > 0 ? reinterpret_cast<std::uintptr_t>(&snap.data.data(0)) : 0;
        cache.usingCustomRules = false;
        cache.matrix.Clear();

        std::size_t invalidEntries = 0;
        std::unordered_set<uint64_t> seenPairs;
        seenPairs.reserve(static_cast<std::size_t>(snap.data.data_size()));

        for (int32_t i = 0; i < snap.data.data_size(); ++i) {
            const auto& row = snap.data.data(i);
            const uint32_t actionId = row.id();
            const uint32_t stateId = row.state_id();
            const uint32_t modeRaw = row.state_mode();
            const uint32_t tip = row.state_tip();

            cache.matrix.EnsureAction(actionId);

            if (stateId >= kActorStateActorStateMax) {
                LOG_ERROR << "ActorActionState invalid state_id: action_id=" << actionId
                          << " state_id=" << stateId
                          << " max_state=" << static_cast<uint32_t>(kActorStateActorStateMax);
                ++invalidEntries;
                continue;
            }
            if (modeRaw > static_cast<uint32_t>(combat_ai::StateMode::kInterrupt)) {
                LOG_ERROR << "ActorActionState invalid state_mode: action_id=" << actionId
                          << " state_id=" << stateId << " state_mode=" << modeRaw;
                ++invalidEntries;
                continue;
            }

            const uint64_t pairKey = (static_cast<uint64_t>(actionId) << 32) | static_cast<uint64_t>(stateId);
            if (!seenPairs.insert(pairKey).second) {
                LOG_WARN << "ActorActionState duplicate rule overwritten: action_id=" << actionId
                         << " state_id=" << stateId;
            }
            cache.matrix.SetRule(actionId, stateId, static_cast<combat_ai::StateMode>(modeRaw), tip);
        }
        return invalidEntries;
    }

    const combat_ai::StateMutexMatrix& GetCompiledMatrix() {
        auto& cache = tlsCompiledActionStateCache;
        if (cache.usingCustomRules) {
            return cache.matrix;
        }
        const auto& snap = ActorActionStateTableManager::Instance().GetSnapshot();
        const auto snapshotAddr = reinterpret_cast<std::uintptr_t>(&snap);
        const int32_t rowCount = snap.data.data_size();
        const auto firstRowAddr =
            rowCount > 0 ? reinterpret_cast<std::uintptr_t>(&snap.data.data(0)) : 0;
        if (cache.snapshotAddr != snapshotAddr || cache.rowCount != rowCount ||
            cache.firstRowAddr != firstRowAddr) {
            CompileFromTableSnapshot(cache);
        }
        return cache.matrix;
    }

    uint32_t RemoveStateInternal(entt::entity actorEntity, uint32_t actorState,
                                 combat_ai::StateExitReason reason) {
        if (!tlsEcs.actorRegistry.valid(actorEntity)) {
            return MAKE_ERROR_MSG(kThisEntityIsInvalid,
                                  "entity=" << entt::to_integral(actorEntity)
                                            << " actorState=" << actorState);
        }
        if (actorState >= kActorStateActorStateMax) {
            return MAKE_ERROR_MSG(kInvalidParameter,
                                  "entity=" << entt::to_integral(actorEntity)
                                            << " actorState=" << actorState);
        }

        const uint64_t bit = uint64_t{1} << actorState;
        bool wasActive = false;

        if (auto* maskComp = tlsEcs.actorRegistry.try_get<ActorStateMaskComp>(actorEntity)) {
            wasActive = (maskComp->mask & bit) != 0;
            maskComp->mask &= ~bit;
        }
        if (auto* actorStatePbComponent = FindActorStateComp(actorEntity)) {
            if (actorStatePbComponent->mutable_state_list()->erase(actorState) > 0) {
                wasActive = true;
            }
        }

        // 先清除状态再回调子系统收尾,保证钩子内 HasState 为 false 且重入调用 RemoveState 幂等无环。
        if (wasActive) {
            tlsExitHooks.NotifyExited(actorEntity, actorState, reason);
        }
        return kSuccess;
    }
} // namespace

uint32_t ActorActionStateSystem::TryPerformAction(entt::entity actorEntity, uint32_t actorAction,
                                                  uint32_t successState) {
    const auto& matrix = GetCompiledMatrix();
    if (!matrix.HasAction(actorAction)) {
        return kInvalidTableId;
    }
    if (!tlsEcs.actorRegistry.valid(actorEntity)) {
        return MAKE_ERROR_MSG(kThisEntityIsInvalid,
                              "entity=" << entt::to_integral(actorEntity)
                                        << " actorAction=" << actorAction);
    }
    if (successState >= kActorStateActorStateMax) {
        return MAKE_ERROR_MSG(kInvalidParameter,
                              "entity=" << entt::to_integral(actorEntity)
                                        << " successState=" << successState);
    }

    // 两阶段位图执行:先纯只读校验冲突,通过后按位掩码逐个打断(栈上纯整数循环,
    // 零 vector 拷贝、零 protobuf::Map 迭代器失效),并自动派发退出钩子,最后入态。
    const uint64_t activeMask = ReadActiveMask(actorEntity);
    uint32_t enterError = kSuccess;
    const uint32_t result = matrix.TryPerform(
        activeMask,
        actorAction,
        successState,
        kActorStateActorStateMax,
        kSuccess,
        kInvalidTableId,
        kInvalidParameter,
        [actorEntity = actorEntity](uint32_t interruptedState) {
            RemoveStateInternal(actorEntity, interruptedState, combat_ai::StateExitReason::kInterrupted);
        },
        [actorEntity = actorEntity, &enterError](uint32_t stateToEnter) {
            enterError = AddState(actorEntity, stateToEnter);
        });
    if (result != kSuccess) {
        return result;
    }
    return enterError;
}

uint32_t ActorActionStateSystem::CanExecuteActionWithoutStateChange(entt::entity actorEntity,
                                                                    uint32_t actorAction) {
    const auto& matrix = GetCompiledMatrix();
    if (!matrix.HasAction(actorAction)) {
        return kInvalidTableId;
    }
    if (!tlsEcs.actorRegistry.valid(actorEntity)) {
        return MAKE_ERROR_MSG(kThisEntityIsInvalid,
                              "entity=" << entt::to_integral(actorEntity)
                                        << " actorAction=" << actorAction);
    }
    return matrix.CanPerform(ReadActiveMask(actorEntity), actorAction, kSuccess, kInvalidTableId);
}

bool ActorActionStateSystem::HasActionPolicy(uint32_t actorAction) {
    return GetCompiledMatrix().HasAction(actorAction);
}

bool ActorActionStateSystem::HasState(const entt::entity actorEntity, const uint32_t state) {
    if (state >= kActorStateActorStateMax) {
        return false;
    }
    return (ReadActiveMask(actorEntity) & (uint64_t{1} << state)) != 0;
}

uint64_t ActorActionStateSystem::ActiveMask(const entt::entity actorEntity) {
    return ReadActiveMask(actorEntity);
}

uint32_t ActorActionStateSystem::AddState(const entt::entity actorEntity, uint32_t actorState) {
    if (actorState >= kActorStateActorStateMax) {
        return MAKE_ERROR_MSG(kInvalidParameter,
                              "entity=" << entt::to_integral(actorEntity)
                                        << " actorState=" << actorState);
    }
    if (!tlsEcs.actorRegistry.valid(actorEntity)) {
        return MAKE_ERROR_MSG(kThisEntityIsInvalid,
                              "entity=" << entt::to_integral(actorEntity)
                                        << " actorState=" << actorState);
    }

    // 唯一写入口在第一次激活状态时延迟创建 ActorStateComp 与位图缓存,只读校验不创建。
    auto& actorStatePbComponent = tlsEcs.actorRegistry.get_or_emplace<ActorStateComp>(actorEntity);
    auto* existingMask = tlsEcs.actorRegistry.try_get<ActorStateMaskComp>(actorEntity);
    auto& maskComp = existingMask != nullptr
                         ? *existingMask
                         : tlsEcs.actorRegistry.emplace<ActorStateMaskComp>(
                               actorEntity, ActorStateMaskComp{FoldProtoStateMask(actorStatePbComponent)});

    const uint64_t bit = uint64_t{1} << actorState;
    if ((maskComp.mask & bit) != 0) {
        return kSuccess;
    }

    maskComp.mask |= bit;
    actorStatePbComponent.mutable_state_list()->emplace(std::make_pair(actorState, true));
    return kSuccess;
}

uint32_t ActorActionStateSystem::RemoveState(entt::entity actorEntity, uint32_t actorState) {
    return RemoveStateInternal(actorEntity, actorState, combat_ai::StateExitReason::kRemoved);
}

void ActorActionStateSystem::RegisterExitHook(uint32_t actorState, ExitHook hook) {
    if (actorState >= kActorStateActorStateMax) {
        return;
    }
    tlsExitHooks.Register(actorState, std::move(hook));
}

void ActorActionStateSystem::ClearExitHook(uint32_t actorState) {
    if (actorState >= kActorStateActorStateMax) {
        return;
    }
    tlsExitHooks.Unregister(actorState);
}

void ActorActionStateSystem::ClearAllExitHooks() {
    tlsExitHooks.Clear();
}

std::size_t ActorActionStateSystem::RefreshCompiledTable() {
    return CompileFromTableSnapshot(tlsCompiledActionStateCache);
}

std::size_t ActorActionStateSystem::LoadRules(std::span<const combat_ai::StateRule> rules) {
    auto& cache = tlsCompiledActionStateCache;
    cache.usingCustomRules = true;
    cache.matrix.Clear();
    return cache.matrix.LoadRules(rules);
}

void ActorActionStateSystem::ResetRulesToTable() {
    tlsCompiledActionStateCache.usingCustomRules = false;
    tlsCompiledActionStateCache.snapshotAddr = 0;
    tlsCompiledActionStateCache.firstRowAddr = 0;
    tlsCompiledActionStateCache.rowCount = -1;
}
