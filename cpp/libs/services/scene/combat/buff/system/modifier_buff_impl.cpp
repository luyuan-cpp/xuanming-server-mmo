#include "modifier_buff_impl.h"

#include "muduo/base/Logging.h"
#include "table/code/buff_table.h"
#include "table/proto/buff_table.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"  // Player:区分玩家缺等级(故障)与 NPC 没有等级(按 1 级)
#include "combat/buff/comp/buff_comp.h"
#include "combat/buff/constants/buff.h"
#include "combat/engagement/system/combat_engagement.h"  // 别人给的回血算一次交手(治疗关系)
#include "player/system/player_level_rules.h"  // CappedLevel:回血公式按生效等级
#include <thread_context/ecs_context.h>

void ModifierBuffImplSystem::OnBuffStart(entt::entity parent, BuffEntry& buff, const BuffTable* buffTable) {
}

void ModifierBuffImplSystem::OnBuffRemove(const entt::entity parent, BuffEntry& buff, const BuffTable* buffTable) {
}

void ModifierBuffImplSystem::OnBuffDestroy(entt::entity parent, BuffEntry& buff, const BuffTable* buffTable) {
}

static bool OnHealthRegenerationBasedOnLostHealth(entt::entity parent, BuffEntry& buffComp, const BuffTable* buffTable)
{
    if (buffTable == nullptr) {
        return false;
    }

    // TODO: Compute max_health on demand
    // 每帧 buff tick 路径:BuffSystem::Update 的 view 只保证 BuffListComp 存在,
    // 属性组件不一定齐(例如以后的 NPC / 怪物),不能无守卫 get。
    auto *baseAttributesPbComponent = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(parent);
    const auto *derivedAttributesPbComponent = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(parent);
    if (baseAttributesPbComponent == nullptr || derivedAttributesPbComponent == nullptr) {
        LOG_WARN << "Health regeneration skipped: missing attribute components, entity="
                 << entt::to_integral(parent) << " buff_table_id=" << buffTable->id();
        return false;
    }
    // LevelComp 只有玩家有(加载时挂上的持久数据),按生效等级取:超上限的存储等级按上限,0 按 1(player_level_rules.h)。
    // 非玩家目标(NPC / 怪物)没有等级组件,按 1 级算,与 skill.cpp 的伤害公式一致。
    // 玩家缺 LevelComp 是加载不变量被破坏:记 ERROR、这一跳不回血,不按 1 级补默认值(组件访问规则)。
    uint32_t level = 1;
    if (const auto *levelComponent = tlsEcs.actorRegistry.try_get<LevelComp>(parent)) {
        level = playerlevel::CappedLevel(levelComponent->level());
    } else if (tlsEcs.actorRegistry.any_of<Player>(parent)) {
        LOG_ERROR << "Health regeneration skipped: player LevelComp missing (load invariant broken), entity="
                  << entt::to_integral(parent) << " buff_table_id=" << buffTable->id();
        return false;
    }

    const auto lostHealth = derivedAttributesPbComponent->max_health() - baseAttributesPbComponent->health();

    BuffTableManager::Instance().SetHealthRegenerationParam(
        { static_cast<double>(level),  static_cast<double>(lostHealth) });

    const auto healingAmount = BuffTableManager::Instance().GetHealthRegeneration(buffTable->id());
    const auto healthBefore = baseAttributesPbComponent->health();
    const auto currentHealth = std::min<uint64_t>(derivedAttributesPbComponent->max_health(),
        static_cast<uint64_t>(static_cast<double>(healthBefore) +
            healingAmount));

    baseAttributesPbComponent->set_health(currentHealth);

    LOG_TRACE << "Healing applied, current health: " << currentHealth;

    // 真回了血才算一次交手(治疗关系):施法者与受治疗者互相关注,远处给你加血的人也看得见。
    // 自己给自己回血、满血空跳都不算;施法者整数是 entt 句柄,已销毁的由 RecordInteraction 忽略。
    if (currentHealth > healthBefore) {
        CombatEngagementSystem::RecordInteraction(static_cast<entt::entity>(buffComp.buffPb.caster()), parent);
    }

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
