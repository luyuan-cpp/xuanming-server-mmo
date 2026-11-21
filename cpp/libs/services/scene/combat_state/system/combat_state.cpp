#include "combat_state.h"

#include <cstdint>
#include <unordered_set>

#include "muduo/base/Logging.h"
#include "modules/combat_ai/state_mutex_matrix.h"
#include "table/code/actoractioncombatstate_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "actor/action_state/system/actor_action_state.h"
#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"
#include "actor/attribute/system/actor_attribute_calculator.h"
#include "combat_state/constants/combat_state.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include "thread_context/ecs_context.h"

namespace {
    // 64 位战斗异常状态运行期位图缓存,与 CombatStateCollectionComp 同步维护,
    // 避免施法热路径遍历 protobuf::Map。按需在首次写入时延迟创建,不参与持久化。
    struct CombatStateMaskComp
    {
        uint64_t mask{0};
    };

    struct CompiledCombatStateCache
    {
        std::uintptr_t snapshotAddr{0};
        std::uintptr_t firstRowAddr{0};
        int32_t rowCount{-1};
        combat_ai::StateMutexMatrix matrix{};
    };

    thread_local CompiledCombatStateCache tlsCompiledCombatStateCache{};

    static_assert(static_cast<uint32_t>(kActorMaxCombatStateType) <= combat_ai::kMaxMutexStates,
                  "eActorCombatState exceeds 64-bit StateMutexMatrix capacity");
    static_assert(static_cast<uint32_t>(kCombatStateMutualExclusion) ==
                      static_cast<uint32_t>(combat_ai::StateMode::kBlock),
                  "kCombatStateMutualExclusion must match combat_ai::StateMode::kBlock");
    static_assert(static_cast<uint32_t>(kCombatStatePermitted) ==
                      static_cast<uint32_t>(combat_ai::StateMode::kAllow),
                  "kCombatStatePermitted must match combat_ai::StateMode::kAllow");
    static_assert(static_cast<uint32_t>(kCombatStateInterrupt) ==
                      static_cast<uint32_t>(combat_ai::StateMode::kInterrupt),
                  "kCombatStateInterrupt must match combat_ai::StateMode::kInterrupt");

    uint64_t FoldProtoCombatMask(const CombatStateCollectionComp& comp) {
        uint64_t mask = 0;
        for (const auto& [stateKey, details] : comp.states()) {
            (void)details;
            if (stateKey < kActorMaxCombatStateType) {
                mask |= (uint64_t{1} << stateKey);
            }
        }
        return mask;
    }

    uint64_t ReadCombatMask(entt::entity entityId) {
        if (!tlsEcs.actorRegistry.valid(entityId)) {
            return 0;
        }
        if (const auto* maskComp = tlsEcs.actorRegistry.try_get<CombatStateMaskComp>(entityId)) {
            return maskComp->mask;
        }
        if (const auto* collection = tlsEcs.actorRegistry.try_get<CombatStateCollectionComp>(entityId)) {
            return FoldProtoCombatMask(*collection);
        }
        return 0;
    }

    std::size_t CompileCombatMatrixFromSnapshot(CompiledCombatStateCache& cache) {
        const auto& snap = ActorActionCombatStateTableManager::Instance().GetSnapshot();
        cache.snapshotAddr = reinterpret_cast<std::uintptr_t>(&snap);
        cache.rowCount = snap.data.data_size();
        cache.firstRowAddr =
            cache.rowCount > 0 ? reinterpret_cast<std::uintptr_t>(&snap.data.data(0)) : 0;
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

            if (stateId >= kActorMaxCombatStateType) {
                LOG_ERROR << "ActorActionCombatState invalid state_id: action_id=" << actionId
                          << " state_id=" << stateId
                          << " max_state=" << static_cast<uint32_t>(kActorMaxCombatStateType);
                ++invalidEntries;
                continue;
            }
            if (modeRaw > static_cast<uint32_t>(combat_ai::StateMode::kInterrupt)) {
                LOG_ERROR << "ActorActionCombatState invalid state_mode: action_id=" << actionId
                          << " state_id=" << stateId << " state_mode=" << modeRaw;
                ++invalidEntries;
                continue;
            }

            const uint64_t pairKey = (static_cast<uint64_t>(actionId) << 32) | static_cast<uint64_t>(stateId);
            if (!seenPairs.insert(pairKey).second) {
                LOG_WARN << "ActorActionCombatState duplicate rule overwritten: action_id=" << actionId
                         << " state_id=" << stateId;
            }
            cache.matrix.SetRule(actionId, stateId, static_cast<combat_ai::StateMode>(modeRaw), tip);
        }
        return invalidEntries;
    }

    const combat_ai::StateMutexMatrix& GetCompiledCombatMatrix() {
        auto& cache = tlsCompiledCombatStateCache;
        const auto& snap = ActorActionCombatStateTableManager::Instance().GetSnapshot();
        const auto snapshotAddr = reinterpret_cast<std::uintptr_t>(&snap);
        const int32_t rowCount = snap.data.data_size();
        const auto firstRowAddr =
            rowCount > 0 ? reinterpret_cast<std::uintptr_t>(&snap.data.data(0)) : 0;
        if (cache.snapshotAddr != snapshotAddr || cache.rowCount != rowCount ||
            cache.firstRowAddr != firstRowAddr) {
            CompileCombatMatrixFromSnapshot(cache);
        }
        return cache.matrix;
    }
} // namespace

void CombatStateSystem::AddCombatState(entt::entity entityId, uint64_t sourceBuffId, uint32_t stateType) {
    // 先校验实体、再校验参数,最后才 get_or_emplace。
    // 调用方是 Buff 生命周期钩子,Buff 可能比实体活得久,
    // 对一个已销毁的实体做 get_or_emplace 在 EnTT 里是 UB(Release 下直接写坏内存)。
    // 参数校验也必须前置,否则非法 state_type 会白白给实体挂上一个空组件。
    if (!tlsEcs.actorRegistry.valid(entityId)) {
        return;
    }

    if (stateType >= kActorMaxCombatStateType) {
        return;
    }

    // 此处延迟创建,只有首个合法 Buff 状态实际写入时才挂状态集合与位图缓存。
    auto& combatStateCollection = tlsEcs.actorRegistry.get_or_emplace<CombatStateCollectionComp>(entityId);
    auto* existingMask = tlsEcs.actorRegistry.try_get<CombatStateMaskComp>(entityId);
    auto& maskComp = existingMask != nullptr
                         ? *existingMask
                         : tlsEcs.actorRegistry.emplace<CombatStateMaskComp>(
                               entityId, CombatStateMaskComp{FoldProtoCombatMask(combatStateCollection)});

    auto stateIterator = combatStateCollection.mutable_states()->find(stateType);
    if (stateIterator == combatStateCollection.mutable_states()->end()) {
        const auto [newStateIterator, wasInserted] = combatStateCollection.mutable_states()->emplace(
            stateType, CombatStateDetailsComp{});
        if (!wasInserted) {
            return;
        }
        stateIterator = newStateIterator;
    }

    stateIterator->second.mutable_sources()->emplace(sourceBuffId, false);
    const uint64_t prevMask = maskComp.mask;
    maskComp.mask |= (uint64_t{1} << stateType);

    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(entityId, kCombatState);
    if (maskComp.mask != prevMask) {
        ActorActionStateSystem::SyncSelfStateFlagsIfChanged(entityId);
    }
}

void CombatStateSystem::RemoveCombatState(entt::entity entityId, uint64_t sourceBuffId, uint32_t stateType) {
    // 同 AddCombatState:实体可能已销毁,Buff 清理钩子仍可能抵达。
    // 而且"移除"路径本来就不该 get_or_emplace —— 那会给从来没有过战斗状态的
    // 实体凭空建一个空组件(§7.5 也禁止在这类路径上 get_or_emplace)。
    if (!tlsEcs.actorRegistry.valid(entityId)) {
        return;
    }

    if (stateType >= kActorMaxCombatStateType) {
        return;
    }

    auto* combatStateCollectionPtr = tlsEcs.actorRegistry.try_get<CombatStateCollectionComp>(entityId);
    if (combatStateCollectionPtr == nullptr) {
        return;
    }
    auto& combatStateCollection = *combatStateCollectionPtr;

    auto stateIterator = combatStateCollection.mutable_states()->find(stateType);
    if (stateIterator == combatStateCollection.mutable_states()->end()) {
        return;
    }

    stateIterator->second.mutable_sources()->erase(sourceBuffId);

    bool maskChanged = false;
    if (stateIterator->second.sources().empty()) {
        combatStateCollection.mutable_states()->erase(stateIterator);
        if (auto* maskComp = tlsEcs.actorRegistry.try_get<CombatStateMaskComp>(entityId)) {
            const uint64_t prevMask = maskComp->mask;
            maskComp->mask &= ~(uint64_t{1} << stateType);
            maskChanged = (maskComp->mask != prevMask);
        } else {
            maskChanged = true;
        }
    }

    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(entityId, kCombatState);
    if (maskChanged) {
        ActorActionStateSystem::SyncSelfStateFlagsIfChanged(entityId);
    }
}

uint32_t CombatStateSystem::ValidateSkillUsage(const entt::entity entityId, const uint32_t combatAction) {
    const uint64_t activeMask = ReadCombatMask(entityId);
    if (activeMask == 0) {
        return kSuccess;
    }
    return GetCompiledCombatMatrix().CanPerform(activeMask, combatAction, kSuccess, kInvalidTableId);
}

uint64_t CombatStateSystem::ActiveMask(const entt::entity entityId) {
    return ReadCombatMask(entityId);
}

std::size_t CombatStateSystem::RefreshCompiledTable() {
    return CompileCombatMatrixFromSnapshot(tlsCompiledCombatStateCache);
}
