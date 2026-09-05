#include "skill.h"

#include <muduo/base/Logging.h>

#include "proto/scene/player_skill.pb.h"
#include "table/proto/tip/entity_error_tip.pb.h"
#include "table/code/skillpermission_table.h"
#include "table/code/skill_table.h"
#include "actor/action_state/constants/actor_state.h"
#include "actor/action_state/system/actor_action_state.h"
#include "combat_state/system/combat_state.h"
#include "combat/buff/system/buff.h"
#include "combat/skill/comp/skill_comp.h"
#include "combat/skill/constants/skill.h"
#include "player/comp/player_frozen_comp.h"  // cross-zone-readiness-audit.md §11.3 / §11.4
#include "spatial/system/view.h"
#include "proto/common/event/combat_event.pb.h"
#include "proto/common/event/skill_event.pb.h"
#include "macros/return_define.h"
#include "macros/error_return.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "proto/common/component/buff_comp.pb.h"
#include "proto/common/component/npc_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "rpc/service_metadata/player_skill_service_metadata.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"  // DerivedAttributesComp(属性加点二级属性)

#include "time/comp/timer_task_comp.h"
#include "time/system/time_cooldown.h"
#include "time/system/time.h"
#include <algorithm>
#include <core/system/id_generator.h>
#include <utils/random/random.h>

uint64_t GenerateUniqueSkillId(const SkillContextCompMap& casterSkillContexts, const SkillContextCompMap& targetSkillContexts) {
	uint64_t newSkillId;
	do {
		newSkillId = tlsIdGeneratorManager.skillIdGenerator.Generate();
	} while (casterSkillContexts.contains(newSkillId) || targetSkillContexts.contains(newSkillId));
	return newSkillId;
}

// Reject casts from a player mid cross-zone migration: the source-side cast
// would never reach the destination. cross-zone-readiness-audit.md §11.3.
bool IsCasterFrozenForMigration(entt::entity casterEntity, uint64_t skillId, const char* where) {
	if (!tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(casterEntity)) {
		return false;
	}
	LOG_WARN << where << " rejected: caster frozen for cross-zone migration. "
		<< "skill_id=" << skillId << " caster=" << entt::to_integral(casterEntity);
	return true;
}

// Mirror a skill context onto the target's context map (if the target has one).
void AddTargetSkillContext(entt::entity target, const SkillContextPtrComp& context) {
	if (auto* targetSkillContextMap = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(target)) {
		targetSkillContextMap->emplace(context->skillid(), context);
	}
}

// Drop a skill context from the target's context map (if the target has one).
void RemoveTargetSkillContext(entt::entity target, uint64_t skillId) {
	if (auto* targetSkillContextMap = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(target)) {
		targetSkillContextMap->erase(skillId);
	}
}

// Look up a skill context on the caster's context map; nullptr if absent.
SkillContextPtrComp FindCasterSkillContext(entt::entity caster, uint64_t skillId) {
	auto* casterSkillContextMap = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
	if (!casterSkillContextMap) {
		return nullptr;
	}
	const auto it = casterSkillContextMap->find(skillId);
	return it != casterSkillContextMap->end() ? it->second : nullptr;
}

void SkillSystem::StartCooldown(entt::entity caster, const SkillTable* skillTable) {
	ECS_GET_OR_VOID(coolDownComp, CooldownTimeListComp, caster);
	CooldownTimeComp comp;
	comp.set_start(TimeSystem::NowMilliseconds());
	comp.set_cooldown_table_id(skillTable->cooldown_id());

	const auto coolDownList = coolDownComp->mutable_cooldown_list();
	(*coolDownList)[skillTable->cooldown_id()] = comp;
}

void LookAtTargetPosition(entt::entity caster, const ReleaseSkillRequest* request) {
	if (request->has_position()) {
		ViewSystem::LookAtPosition(caster, request->position());
	} else if (request->target_id() > 0) {
		const entt::entity target{ request->target_id() };
		const auto *transform = tlsEcs.actorRegistry.try_get<Transform>(target);
		if (transform)
		{
			ViewSystem::LookAtPosition(caster, transform->location());
		}
	}
}

std::shared_ptr<SkillContextComp> CreateSkillContext(entt::entity caster, const ReleaseSkillRequest* request) {
	auto context = std::make_shared<SkillContextComp>();
	context->set_caster(entt::to_integral(caster));
	context->set_skilltableid(request->skill_table_id());
	context->set_target(request->target_id());
	context->set_casttime(TimeSystem::NowMilliseconds());

	static const SkillContextCompMap kEmptyContexts;
	const auto* casterContexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
	context->set_skillid(GenerateUniqueSkillId(casterContexts ? *casterContexts : kEmptyContexts, kEmptyContexts));
	return context;
}

void AddSkillContext(entt::entity caster, const ReleaseSkillRequest* request, std::shared_ptr<SkillContextComp> context) {
	ECS_GET_OR_VOID(casterSkillContextMap, SkillContextCompMap, caster);
	casterSkillContextMap->emplace(context->skillid(), context);

	entt::entity target{ request->target_id() };
	if (tlsEcs.actorRegistry.valid(target)) {
		AddTargetSkillContext(target, context);
	}
}

void ConsumeItems(entt::entity caster, const SkillTable* skillTable) {
	// TODO: Implement item consumption logic
}

void ConsumeResources(entt::entity caster, const SkillTable* skillTable) {
	// TODO: Implement resource consumption logic
}

void ApplySkillHitEffectIfValid(const entt::entity casterEntity, const uint64_t targetId) {
	const entt::entity targetEntity{targetId};
	if (!tlsEcs.actorRegistry.valid(targetEntity)) {
		return;
	}
	BuffSystem::OnSkillHit(casterEntity, targetEntity);
}

uint32_t SkillSystem::ReleaseSkill(const entt::entity casterEntity, const ReleaseSkillRequest* request) {
	LookupSkillOrReturnError(request->skill_table_id());

	RETURN_ON_ERROR(CheckSkillPrerequisites(casterEntity, request));
	LookAtTargetPosition(casterEntity, request);
	BroadcastSkillUsedMessage(casterEntity, request);
    
	const auto context = CreateSkillContext(casterEntity, request);
	AddSkillContext(casterEntity, request, context);
    
	ConsumeItems(casterEntity, skillRow);
	ConsumeResources(casterEntity, skillRow);
	StartCooldown(casterEntity, skillRow);
	SetupCastingTimer(casterEntity, skillRow, context->skillid());

	ApplySkillHitEffectIfValid(casterEntity, request->target_id());

	return kSuccess;
}

uint32_t CheckPlayerLevel(const entt::entity casterEntity, const SkillTable* skillTable) {
	// TODO: Implement level requirement validation for non-NPC casters
	return kSuccess;
}

uint32_t CanUseSkillInCurrentState(const uint32_t state, const uint32_t skill) {
	LookupSkillPermissionOrReturnError(state);

	// skill 是 SkillTable.skill_type 里的原始序号(eSkillType 的位号 0..5),
	// 而 SkillPermission.skill_type 这一列正是按序号平铺的(整表 6 格 = 6 种技能类型)。
	// 旧写法拿 (1 << skill) 这个位掩码当下标:序号 0/1/2 读到 1/2/4 号错位格子,
	// 序号 ≥3 一律越界、被下面的守卫兜成 kInvalidTableData —— 切换/激活/普攻
	// 这三类技能在任何战斗状态下都恒定报"表数据错误"。
	const auto skillTypeIndex = static_cast<int32_t>(skill);
	if (skillTypeIndex >= skillPermissionRow->skill_type_size())
	{
		return MAKE_ERROR_MSG(kInvalidTableData,
			"state=" << state << " skill=" << skill
			<< " skillTypeIndex=" << skillTypeIndex
			<< " size=" << skillPermissionRow->skill_type_size());
	}
	
	return skillPermissionRow->skill_type(skillTypeIndex);
}


uint32_t CheckBuff(const entt::entity casterEntity, const SkillTable* skillTable) {

	ECS_GET_OR_RETURN(combatStateCollection, CombatStateCollectionComp, casterEntity, kSuccess);

	// Every active combat state must permit every skill type this skill carries.
	for (const auto& [currentState, buffList] : combatStateCollection->states())
	{
		for (const auto& skillType : skillTable->skill_type()) {
			RETURN_ON_ERROR(CanUseSkillInCurrentState(currentState, static_cast<eSkillType>(skillType)));
		}
	}

	return kSuccess;
}


uint32_t CheckState(const entt::entity casterEntity, const SkillTable* skillTable) {
	RETURN_ON_ERROR(ActorActionStateSystem::TryPerformAction(casterEntity, kActorActionUseSkill, kActorStateCombat));
	RETURN_ON_ERROR(CombatStateSystem::ValidateSkillUsage(casterEntity, kActorActionUseSkill));
	return kSuccess;
}

uint32_t CheckItemUse(const entt::entity casterEntity, const SkillTable* skillTable) {
	// TODO: Validate item requirements before skill use
	return kSuccess;
}

uint32_t SkillSystem::CheckSkillPrerequisites(const entt::entity casterEntity, const ::ReleaseSkillRequest* request) {
	LookupSkillOrReturnError(request->skill_table_id());

	RETURN_ON_ERROR(ValidateTarget(request));
	RETURN_ON_ERROR(CheckCooldown(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckCasting(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckRecovery(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckChannel(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckPlayerLevel(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckBuff(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckState(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckItemUse(casterEntity, skillRow));
	return kSuccess;
}

bool SkillSystem::IsSkillOfType(const uint32_t skillTableId, const uint32_t skillType) {
	LookupSkillOrReturnFalse(skillTableId);

	for (auto& tabSkillType : skillRow->skill_type()) {
		if ((1 << tabSkillType) == skillType) {
			return true;
		}
	}

	return false;
}

void SkillSystem::HandleGeneralSkillSpell(const entt::entity casterEntity, const uint64_t skillId) {
    if (!tlsEcs.actorRegistry.valid(casterEntity))
    {
        return;
    }

    // Frozen caster: skill cast attempted while the player is mid cross-
    // zone migration. The skill would resolve on the source side and never
    // reach the destination - treat as no-op. cross-zone-readiness-audit.md §11.3.
    if (IsCasterFrozenForMigration(casterEntity, skillId, "SkillSystem::HandleGeneralSkillSpell"))
    {
        return;
    }

	HandleSkillSpell(casterEntity, skillId);

	LOG_INFO << "Handling general skill spell. Caster: " << entt::to_integral(casterEntity)
		<< ", Skill ID: " << skillId;

	TriggerSkillEffect(casterEntity, skillId);
	HandleSkillRecovery(casterEntity, skillId);
}

// Set up a timer for skill recovery after casting
void SkillSystem::HandleSkillRecovery(const entt::entity casterEntity, uint64_t skillId) {
	const auto skillContext = FindCasterSkillContext(casterEntity, skillId);
	if (!skillContext)
	{
		return;
	}

	LookupSkillOrReturnVoid(skillContext->skilltableid());

	auto& recoveryTimerComp = tlsEcs.actorRegistry.get_or_emplace<RecoveryTimerComp>(casterEntity);
	recoveryTimerComp.skillId = skillId;
	recoveryTimerComp.timer.RunAfter(skillRow->recovery_time(), [casterEntity, skillId] {
		return HandleSkillFinish(casterEntity, skillId);
		});
}

void SkillSystem::HandleSkillFinish(const entt::entity casterEntity, uint64_t skillId) {
    if (!tlsEcs.actorRegistry.valid(casterEntity))
    {
        return;
    }

	// TODO: Handle offline player
	ECS_GET_OR_VOID(casterSkillContextMap, SkillContextCompMap, casterEntity);
	auto skillContentIt = casterSkillContextMap->find(skillId);
	if (skillContentIt != casterSkillContextMap->end())
	{
		entt::entity target = entt::to_entity(skillContentIt->second->target());
		if (tlsEcs.actorRegistry.valid(target)) {
			RemoveTargetSkillContext(target, skillId);
		}
		casterSkillContextMap->erase(skillContentIt);
	}
}

void SkillSystem::HandleChannelSkillSpell(entt::entity casterEntity, uint64_t skillId) {
    if (!tlsEcs.actorRegistry.valid(casterEntity))
    {
        return;
    }

    // Frozen caster (channel variant). Same rationale as HandleGeneralSkillSpell -
    // any in-flight migration means the source-side cast cannot reach the
    // destination. cross-zone-readiness-audit.md §11.3.
    if (IsCasterFrozenForMigration(casterEntity, skillId, "SkillSystem::HandleChannelSkillSpell"))
    {
        return;
    }

	// skillId 是 tlsIdGeneratorManager 发的技能实例 id,不是技能表 id。
	// 旧写法直接拿它查 SkillTable,雪花 id 永远查不到行 —— 于是这里必然
	// 早退,吟唱类技能从来没真正跑起来过。要先用实例 id 找上下文,
	// 再用上下文里的 skilltableid 查表(与 HandleSkillRecovery 一致)。
	const auto skillContext = FindCasterSkillContext(casterEntity, skillId);
	if (!skillContext)
	{
		LOG_ERROR << "Channel skill context not found. caster=" << entt::to_integral(casterEntity)
			<< " skill_id=" << skillId;
		return;
	}

	LookupSkillOrReturnVoid(skillContext->skilltableid());

	LOG_INFO << "Handling channel skill spell. Caster: " << entt::to_integral(casterEntity)
		<< ", Skill ID: " << skillId;

	HandleSkillSpell(casterEntity, skillId);

	auto& channelFinishTimerComp = tlsEcs.actorRegistry.get_or_emplace<ChannelFinishTimerComp>(casterEntity);
	channelFinishTimerComp.skillId = skillId;
	channelFinishTimerComp.timer.RunAfter(skillRow->channel_finish(), [casterEntity, skillId] {
		return HandleChannelFinish(casterEntity, skillId);
		});

	auto& channelIntervalTimer = tlsEcs.actorRegistry.get_or_emplace<ChannelIntervalTimerComp>(casterEntity).timer;
	channelIntervalTimer.RunEvery(skillRow->channel_think(), [casterEntity, skillId] {
		return HandleChannelThink(casterEntity, skillId);
		});
}

void SkillSystem::HandleChannelThink(entt::entity casterEntity, uint64_t skillId) {
	// TODO: Implement channel think logic
}

void SkillSystem::HandleChannelFinish(const entt::entity casterEntity, const uint64_t skillId) {
    if (!tlsEcs.actorRegistry.valid(casterEntity))
    {
        return;
    }

	tlsEcs.actorRegistry.remove<ChannelIntervalTimerComp>(casterEntity);
	HandleSkillRecovery(casterEntity, skillId);
}

uint32_t SkillSystem::ValidateTarget(const ::ReleaseSkillRequest* request) {
	LookupSkillOrReturnError(request->skill_table_id());

	// Validate target ID
	if (!skillRow->targeting_mode().empty() && request->target_id() <= 0) {
		return MAKE_ERROR_MSG(kSkillInvalidTargetId,
			"target_id=" << request->target_id()
			<< " skill_table_id=" << request->skill_table_id());
	}

	for (auto& tabSkillType : skillRow->targeting_mode()) {
		const auto targetingMode = (1 << tabSkillType);

		// No target / AOE skills don't need a specific target entity.
		if (targetingMode == kNoTargetRequired || targetingMode == kAreaOfEffect) {
			return kSuccess;
		}

		if (targetingMode != kTargetedSkill) {
			continue;
		}

		const entt::entity target{ request->target_id() };
		if (!tlsEcs.actorRegistry.valid(target)) {
			return MAKE_ERROR_MSG(kSkillInvalidTargetId,
				"target_id=" << request->target_id()
				<< " skill_table_id=" << request->skill_table_id()
				<< " reason=entity_invalid");
		}

		if (!tlsEcs.actorRegistry.any_of<Player>(target) && !tlsEcs.actorRegistry.any_of<Npc>(target)) {
			return MAKE_ERROR_MSG(kSkillInvalidTargetId,
				"target_id=" << request->target_id()
				<< " skill_table_id=" << request->skill_table_id()
				<< " reason=invalid_entity_type");
		}

		return kSuccess;
	}

	return kSuccess;
}

// Common interrupt-or-reject check for casting/recovery/channel timers
template <typename TimerComp>
uint32_t CheckTimerPhase(const entt::entity casterEntity, const SkillTable* skillTable) {
	ECS_GET_OR_RETURN(timerComp, TimerComp, casterEntity, kSuccess);
	if (timerComp->timer.IsActive())
	{
		if (skillTable->immediate()) {
			LOG_INFO << "Immediate skill: " << skillTable->id()
				<< " is currently in phase. Sending interrupt message.";
			SkillSystem::SendSkillInterruptedMessage(casterEntity, skillTable->id());
			// 被打断的那一次施法,它的 SkillContext 是在 ReleaseSkill 里建的,
			// 只有 HandleSkillFinish 会删。这里把定时器组件摘掉之后回调再也不会
			// 触发,上下文就永久留在 caster / target 的 SkillContextCompMap 里 ——
			// 玩家每打断一次泄漏一条,无上界。先收口再摘定时器。
			SkillSystem::HandleSkillFinish(casterEntity, timerComp->skillId);
			tlsEcs.actorRegistry.remove<TimerComp>(casterEntity);
			return kSuccess;
		}
		return MAKE_ERROR_MSG(kSkillUnInterruptible,
			"skill_id=" << skillTable->id()
			<< " caster=" << entt::to_integral(casterEntity));
	}
	tlsEcs.actorRegistry.remove<TimerComp>(casterEntity);
	return kSuccess;
}

uint32_t SkillSystem::CheckCooldown(const entt::entity casterEntity, const SkillTable* skillTable) {
	ECS_GET_OR_RETURN(coolDownTimeListComp, CooldownTimeListComp, casterEntity, kSuccess);
	if (const auto it = coolDownTimeListComp->cooldown_list().find(skillTable->cooldown_id());
		it != coolDownTimeListComp->cooldown_list().end() &&
		CoolDownTimeMillisecondSystem::IsInCooldown(it->second))
	{
		return MAKE_ERROR_MSG(kSkillCooldownNotReady,
			"skill_id=" << skillTable->id()
			<< " caster=" << entt::to_integral(casterEntity)
			<< " cooldown_id=" << skillTable->cooldown_id()
			<< " remaining_ms=" << CoolDownTimeMillisecondSystem::Remaining(it->second));
	}

	return kSuccess;
}

uint32_t SkillSystem::CheckCasting(const entt::entity casterEntity, const SkillTable* skillTable) {
	return CheckTimerPhase<CastingTimerComp>(casterEntity, skillTable);
}

uint32_t SkillSystem::CheckRecovery(const entt::entity casterEntity, const SkillTable* skillTable) {
	return CheckTimerPhase<RecoveryTimerComp>(casterEntity, skillTable);
}

uint32_t SkillSystem::CheckChannel(const entt::entity casterEntity, const SkillTable* skillTable) {
	return CheckTimerPhase<ChannelFinishTimerComp>(casterEntity, skillTable);
}

void SkillSystem::BroadcastSkillUsedMessage(const entt::entity casterEntity, const ::ReleaseSkillRequest* request) {
	SkillUsedS2C skillUsedS2C;
	skillUsedS2C.set_entity(entt::to_integral(casterEntity));
	skillUsedS2C.add_target_entity(request->target_id());
	skillUsedS2C.set_skill_table_id(request->skill_table_id());
	skillUsedS2C.mutable_position()->CopyFrom(request->position());

	ViewSystem::BroadcastMessageToVisiblePlayers(
		casterEntity,
		SceneSkillClientPlayerNotifySkillUsedMessageId,
		skillUsedS2C
	);
}

void SkillSystem::SetupCastingTimer(entt::entity casterEntity, const SkillTable* skillTable, uint64_t skillId) {
	auto& castingTimerComp = tlsEcs.actorRegistry.get_or_emplace<CastingTimerComp>(casterEntity);
	castingTimerComp.skillId = skillId;
	auto& castingTimer = castingTimerComp.timer;
	if (IsSkillOfType(skillTable->id(), kGeneralSkill)) {
		castingTimer.RunAfter(skillTable->cast_point(), [casterEntity, skillId] {
			return HandleGeneralSkillSpell(casterEntity, skillId);
			});
	}
	else if (IsSkillOfType(skillTable->id(), kChannelSkill)) {
		castingTimer.RunAfter(skillTable->cast_point(), [casterEntity, skillId] {
			return HandleChannelSkillSpell(casterEntity, skillId);
			});
	}
}

void SkillSystem::SendSkillInterruptedMessage(const entt::entity casterEntity, const uint32_t skillTableId) {
	SkillInterruptedS2C skillInterruptedS2C;
	skillInterruptedS2C.set_entity(entt::to_integral(casterEntity));
	skillInterruptedS2C.set_skill_table_id(skillTableId);

	ViewSystem::BroadcastMessageToVisiblePlayers(
		casterEntity,
		SceneSkillClientPlayerNotifySkillInterruptedMessageId,
		skillInterruptedS2C
	);
}

void SkillSystem::TriggerSkillEffect(const entt::entity casterEntity, const uint64_t skillId) {
	const auto skillContext = FindCasterSkillContext(casterEntity, skillId);
	if (!skillContext)
	{
		return;
	}

	LookupSkillOrReturnVoid(skillContext->skilltableid());

	LOG_INFO << "Triggering skill effect. Caster: " << entt::to_integral(casterEntity) << ", Skill ID: " << skillId;

	for (const auto& effect : skillRow->effect()) {
		BuffSystem::AddOrUpdateBuff(entt::to_entity(skillContext->target()), effect, skillContext);
	}
}

bool IsTargetDead(entt::entity targetEntity) {
	ECS_GET_OR_FALSE(targetBaseAttributes, BaseAttributesComp, targetEntity);
	return targetBaseAttributes->health() <= 0;
}


double CalculateFinalDamage(const entt::entity casterEntity, const entt::entity target, double baseDamage) {
	const auto *casterAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(casterEntity);
	const auto *targetAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(target);
	if (!casterAttributes || !targetAttributes)
		return 0.0;

	// critchance 是 uint64(见 CLAUDE.md §4),装不下 0~1 的小数,口径只能是
	// 整数百分比。旧写法把它直接当概率跟 [0,1) 的均匀随机数比:今天全系统
	// 没有任何一处写 critchance,所以恒为 0 = 永不暴击;一旦有人按字面填 5(5%),
	// 就会变成 5.0 > 随机数恒成立 = 100% 暴击。这里显式按百分比换算并夹到 [0,1]。
	const double critChance = std::clamp(static_cast<double>(casterAttributes->critchance()) / 100.0, 0.0, 1.0);
	double strength = casterAttributes->strength();
	double armor = targetAttributes->armor();
	double resistance = targetAttributes->resistance();
	// 属性加点二级属性(与回合引擎 CalculateFinalDamage 同口径):实时技能吃法伤,守方防御加法减伤
	double attackBonus = 0.0;
	double defense = 0.0;
	if (const auto* casterDerived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(casterEntity))
	{
		attackBonus = static_cast<double>(casterDerived->magic_attack());
	}
	if (const auto* targetDerived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(target))
	{
		defense = static_cast<double>(targetDerived->defense());
	}

	// Apply crit and penetration modifiers
    double finalDamage = baseDamage * (1 + strength * 0.1) + attackBonus;
    finalDamage = finalDamage - armor - defense;
    finalDamage *= (1 - resistance * 0.01);

    // rand() 是全局非线程安全且未播种的 C 运行时状态,战斗判定统一走 tlsRandom。
    if (critChance > 0.0 && tlsRandom.RandReal<double>(0.0, 1.0) < critChance) {
        finalDamage *= 2;
    }

    return std::max(finalDamage, 0.0);
}


void CalculateSkillDamage(const entt::entity casterEntity, DamageEventComp& damageEvent) {
	const auto skillContext = FindCasterSkillContext(casterEntity, damageEvent.skill_id());
	if (!skillContext)
	{
		LOG_ERROR << "Skill context not found for skill ID: " << damageEvent.skill_id();
        return;
	}

	LookupSkillOrReturnVoid(skillContext->skilltableid());

    auto targetEntity = entt::to_entity(damageEvent.target());

	if (!tlsEcs.actorRegistry.valid(targetEntity))
	{
		return;
	}

    // Frozen target: skill landed on a player who is mid cross-zone
    // migration. Source side no longer represents the player's authoritative
    // state — applying damage here desynchronizes from the destination, which
    // is about to spawn the player from the marshalled snapshot (full HP,
    // stable buffs). Default policy per cross-zone-readiness-audit.md §11.4
    // is "damage drop" — silently skip; caster's hit goes to lost.
    if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(targetEntity))
    {
        LOG_INFO << "[CrossZone] Damage on frozen target dropped. caster="
                 << entt::to_integral(casterEntity) << " target=" << entt::to_integral(targetEntity)
                 << " skill_id=" << damageEvent.skill_id();
        return;
    }

    if (IsTargetDead(targetEntity)) {
        LOG_INFO << "Target is already dead, skipping damage calculation.";
        return;
    }

	ECS_GET_OR_VOID(levelComponent, LevelComp, casterEntity);
	SkillTableManager::Instance().SetDamageParam({static_cast<double>(levelComponent->level())});

	damageEvent.set_attacker_id(entt::to_integral(casterEntity));

    double baseDamage = SkillTableManager::Instance().GetDamage(skillContext->skilltableid());
    double finalDamage = CalculateFinalDamage(casterEntity, targetEntity, baseDamage);
    damageEvent.set_damage(finalDamage);
}


void TriggerBeforeDamageEvents(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent) {
    BuffSystem::OnBeforeGiveDamage(casterEntity, targetEntity, damageEvent);
    BuffSystem::OnBeforeTakeDamage(casterEntity, targetEntity, damageEvent);
}

void ApplyDamage(BaseAttributesComp& baseAttributesPBComponent, const DamageEventComp& damageEvent) {
    const auto damage = static_cast<uint64_t>(std::ceil(damageEvent.damage()));

    if (baseAttributesPBComponent.health() > damage) {
        baseAttributesPBComponent.set_health(baseAttributesPBComponent.health() - damage);
    }
    else {
        baseAttributesPBComponent.set_health(0);
    }
}

void TriggerBeKillEvent(const entt::entity casterEntity, const entt::entity target) {
	BeKillEvent beKillEvent;
	beKillEvent.set_caster(entt::to_integral(casterEntity));
	beKillEvent.set_target(entt::to_integral(target));

	tlsEcs.dispatcher.trigger(beKillEvent);
}

void TriggerAfterDamageEvents(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent) {
	BuffSystem::OnAfterGiveDamage(casterEntity, targetEntity, damageEvent);
	BuffSystem::OnAfterTakeDamage(casterEntity, targetEntity, damageEvent);
}

void HandleTargetDeath(const entt::entity casterEntity, const entt::entity target, const DamageEventComp& damageEvent) {
    BuffSystem::OnBeforeDead(target); 
    BuffSystem::OnAfterDead(target);

    // Trigger kill event if not self-damage
    if (casterEntity != target) {
        BuffSystem::OnKill(casterEntity);
    }

    TriggerBeKillEvent(casterEntity, target);
}

void DealDamage(DamageEventComp& damageEvent, const entt::entity caster, const entt::entity target) {
	ECS_GET_OR_VOID(baseAttributesPBComponent, BaseAttributesComp, target);

	if (IsTargetDead(target)) {
		return;
	}

	damageEvent.set_target(entt::to_integral(target)); 
	TriggerBeforeDamageEvents(caster, target, damageEvent);
	ApplyDamage(*baseAttributesPBComponent, damageEvent);

	if (IsTargetDead(target)) {
		HandleTargetDeath(caster, target, damageEvent);
	}

	TriggerAfterDamageEvents(caster, target, damageEvent);
}

void SkillSystem::HandleSkillSpell(const entt::entity casterEntity, const uint64_t skillId) {
	const auto skillContext = FindCasterSkillContext(casterEntity, skillId);
	if (!skillContext)
	{
		return;
	}

	const entt::entity targetEntity = entt::to_entity(skillContext->target());

	if (!tlsEcs.actorRegistry.valid(targetEntity))
	{
		return;
	}
    
	DamageEventComp damageEvent;
	damageEvent.set_skill_id(skillId);
	damageEvent.set_target(skillContext->target());
	CalculateSkillDamage(casterEntity, damageEvent);
	DealDamage(damageEvent, casterEntity, targetEntity);

	SkillExecutedEvent skillExecutedEvent;
	skillExecutedEvent.set_caster(entt::to_integral(casterEntity));
	skillExecutedEvent.set_target(skillContext->target());
	BuffSystem::OnSkillExecuted(skillExecutedEvent);
}


