#include "actor_attribute_calculator.h"

#include <algorithm>
#include <array>
#include <ranges>

#include "actor/attribute/comp/actor_attribute_comp.h"
#include "macros/return_define.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"

#include "table/code/buff_table.h"
#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"
#include "combat/buff/comp/buff_comp.h"
#include "player/comp/player_frozen_comp.h"  // cross-zone-readiness-audit.md §11.2
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include <generated/attribute/actorbaseattributess2c_attribute_sync.h>
#include <thread_context/ecs_context.h>  // tlsEcs + entt::exclude (see movement.cpp pattern)

// 重算移速**属性**(标量)。
//
// 旧实现把 buff 的加减速标量灌进 Velocity 的 x/y/z 三轴 —— 而 MovementSystem
// 把 Velocity 当运动学矢量每 tick 积分进 Transform,于是挂移速 buff 的角色
// 沿 (1,1,1) 方向匀速漂移(减速则为负、钻入地下),几秒内被 AOI 划出所有
// 观察者视野,漂移后的位置还会随存盘落库。属性写进独立的 MoveSpeedComp,
// Velocity 留给真实的运动矢量(由未来的移动输入/AI 写入)。
// 移速属性目前无 S2C 通道(ActorBaseAttributesS2C.velocity 语义是运动矢量,
// 不能复用),接通道时再补脏位。
void UpdateMoveSpeed(entt::entity entity) {
    auto& moveSpeedComp = tlsEcs.actorRegistry.get_or_emplace<MoveSpeedComp>(entity);
    double moveSpeed = 0.0;

    ECS_GET_OR_VOID(buffListPtr, BuffListComp, entity);
    for (const auto &buffCompPb : *buffListPtr | std::views::values)
    {
        LookupBuffOrContinue(buffCompPb.buffPb.buff_table_id());

        moveSpeed += buffRow->movement_speed_boost();
        moveSpeed -= buffRow->movement_speed_reduction();
    }

    // 减速叠满也不能变成倒着走。
    moveSpeedComp.moveSpeed = std::max(moveSpeed, 0.0);
}

void UpdateHealth(entt::entity actorEntity) {
    // TODO: Implement health recalculation from base stats + buff modifiers
}

void UpdateEnergy(entt::entity actorEntity) {
    // TODO: Implement energy recalculation from base stats + buff modifiers
}

// 把 CombatStateCollectionComp 的当前状态投影成客户端同步用的
// CombatStateFlagsComp,并置同步脏位。
//
// 旧实现三层皆错,任何一层都足以让眩晕/沉默永远同步不到客户端:
//   1) 写进 get_or_emplace<ActorBaseAttributesS2C>(实体上的一个死组件,
//      全仓零读者)—— 而生成序列化器读的是 try_get<CombatStateFlagsComp>;
//   2) 从不置 kCombatStateFlagsFieldNumber 脏位,序列化分支根本不执行;
//   3) 值写 false —— 而 CombatStateCollectionComp 键存在即代表状态激活
//      (RemoveCombatState 在 sources 清空时删键),语义是反的。
void ResetCombatStateFlags(entt::entity actorEntity) {
    const auto *combatStates = tlsEcs.actorRegistry.try_get<CombatStateCollectionComp>(actorEntity);
    auto& stateFlagsComp = tlsEcs.actorRegistry.get_or_emplace<CombatStateFlagsComp>(actorEntity);
    auto* stateFlags = stateFlagsComp.mutable_state_flags();

    stateFlags->clear();

    if (combatStates)
    {
        for (const auto &stateKey : combatStates->states() | std::views::keys)
        {
            stateFlags->emplace(stateKey, true);
        }
    }

    SetActorBaseAttributesS2CAttrDirtyBit(actorEntity, static_cast<std::size_t>(ActorBaseAttributesS2C::kCombatStateFlagsFieldNumber));
    // entity_id 是这条消息里唯一的归属标识(信封层不带主体 actor):
    // 不一并置位的话,观察者会收到一条不知道是谁的状态更新。
    // 序列化器从 Guid(uint64_t)组件取值,客户端在 ActorCreateS2C.guid
    // 建立过 guid → actor 的映射,能够归属。
    SetActorBaseAttributesS2CAttrDirtyBit(actorEntity, static_cast<std::size_t>(ActorBaseAttributesS2C::kEntityIdFieldNumber));
}

std::array<AttributeCalculatorConfig, kAttributeCalculatorMax> kAttributeConfigs = { {
    {kMoveSpeed, UpdateMoveSpeed},
    {kHealth, UpdateHealth},
    {kEnergy, UpdateEnergy},
    {kCombatState, ResetCombatStateFlags}
} };

void ActorAttributeCalculatorSystem::MarkAttributeForUpdate(const entt::entity actorEntity, const uint32_t attributeBit) {
    auto& attributeBits = tlsEcs.actorRegistry.get_or_emplace<AttributeDirtyFlagsComp>(actorEntity).attributeBits;
    attributeBits.set(attributeBit);
}

void ActorAttributeCalculatorSystem::Update()
{
    // PlayerFrozenComp exclude: a frozen player's attributes were
    // marshalled into PlayerAllData at HandleCrossZoneTransfer time.
    // Recomputing them on the source side now is wasted work — and
    // worse, the values would diverge from what the destination zone
    // reconstructs from the snapshot. Skip frozen entities until the
    // ACK / reaper-declared-failure clears the marker.
    // cross-zone-readiness-audit.md §11.2.
    for (auto&& [entity, dirtyFlags] : tlsEcs.actorRegistry.view<AttributeDirtyFlagsComp>(
            entt::exclude<PlayerFrozenComp>).each())
    {
        auto& attributeBits = dirtyFlags.attributeBits;
        for (const auto& [attributeIndex, updateFunction] : kAttributeConfigs) {
            if (updateFunction && attributeBits.test(attributeIndex)) {
                updateFunction(entity);
                attributeBits.reset(attributeIndex);
            }
        }
    }
}
