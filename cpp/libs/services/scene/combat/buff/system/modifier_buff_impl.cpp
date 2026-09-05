#include "modifier_buff_impl.h"

#include "muduo/base/Logging.h"
#include "table/code/buff_table.h"
#include "table/proto/buff_table.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"
#include "actor/attribute/system/actor_attribute_calculator.h"
#include "combat/buff/comp/buff_comp.h"
#include "combat/buff/constants/buff.h"
#include <thread_context/ecs_context.h>

void ModifierBuffImplSystem::OnBuffStart(entt::entity parent, BuffEntry& buff, const BuffTable* buffTable) {
    if (buffTable && IsMovementSpeedBuff(buffTable)) {
        ActorAttributeCalculatorSystem::MarkAttributeForUpdate(parent, kMoveSpeed);
    }
}

void ModifierBuffImplSystem::OnBuffRefresh(entt::entity parent, uint32_t buffTableId,
    const SkillContextPtrComp& abilityContext, BuffEntry& buffComp) {
}

void ModifierBuffImplSystem::OnBuffRemove(const entt::entity parent, BuffEntry& buff, const BuffTable* buffTable) {
    if (buffTable && IsMovementSpeedBuff(buffTable)) {
        ActorAttributeCalculatorSystem::MarkAttributeForUpdate(parent, kMoveSpeed);
    }
}

void ModifierBuffImplSystem::OnBuffDestroy(entt::entity parent, BuffEntry& buff, const BuffTable* buffTable) {
}

static bool OnHealthRegenerationBasedOnLostHealth(entt::entity parent, BuffEntry& buffComp, const BuffTable* buffTable)
{
    if (buffTable == nullptr) {
        return false;
    }

    // TODO: Compute max_health on demand
    auto &baseAttributesPbComponent = tlsEcs.actorRegistry.get<BaseAttributesComp>(parent);
    const auto &derivedAttributesPbComponent = tlsEcs.actorRegistry.get<DerivedAttributesComp>(parent);
    const auto &levelComponent = tlsEcs.actorRegistry.get<LevelComp>(parent);

    const auto lostHealth = derivedAttributesPbComponent.max_health() - baseAttributesPbComponent.health();

    BuffTableManager::Instance().SetHealthRegenerationParam(
        { static_cast<double>(levelComponent.level()),  static_cast<double>(lostHealth) });

    const auto healingAmount = BuffTableManager::Instance().GetHealthRegeneration(buffTable->id());
    const auto currentHealth = std::min<uint64_t>(derivedAttributesPbComponent.max_health(),
        static_cast<uint64_t>(static_cast<double>(baseAttributesPbComponent.health()) +
            healingAmount));

    baseAttributesPbComponent.set_health(currentHealth);

    LOG_TRACE << "Healing applied, current health: " << currentHealth;

    return true;
}

void ModifierBuffImplSystem::OnIntervalThink(entt::entity parent, BuffEntry& buffComp, const BuffTable* buffTable)
{
    if (buffTable && buffTable->buff_type() == kBuffTypeHealthRegenerationBasedOnLostHealth) {
        OnHealthRegenerationBasedOnLostHealth(parent, buffComp, buffTable);
    }
}

void ModifierBuffImplSystem::OnSkillHit(entt::entity caster, entt::entity target)
{
}

bool ModifierBuffImplSystem::IsMovementSpeedBuff(const BuffTable* buffTable) {
    return buffTable->buff_type() == kBuffTypeMovementSpeedReduction || 
           buffTable->buff_type() == kBuffTypeMovementSpeedBoost;
}

