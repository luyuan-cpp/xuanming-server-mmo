#include "skill.h"
#include "actor/actor_net_id.h"

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
#include "player/system/player_level_rules.h"  // CappedLevel:等级公式按生效等级
#include "proto/common/component/battle_comp.pb.h"  // InBattleComp:回合制战斗局中闸(§5.3 冻结清单)
#include "spatial/system/view.h"
#include "macros/return_define.h"
#include "macros/error_return.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "proto/common/component/buff_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/npc_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "rpc/service_metadata/player_skill_service_metadata.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"  // DerivedAttributesComp(属性加点二级属性)
#include "services/battle/system/combat_damage_rules.h"  // 与回合引擎共用的伤害公式

#include "modules/scene/comp/scene_comp.h"
#include "time/comp/timer_task_comp.h"
#include "time/system/time_cooldown.h"
#include "time/system/time.h"
#include <algorithm>
#include <core/system/id_generator.h>
#include <utils/random/random.h>

uint64_t GenerateUniqueSkillId(const SkillContextCompMap& casterSkillContexts) {
	uint64_t newSkillId;
	do {
		newSkillId = tlsIdGeneratorManager.skillIdGenerator.Generate();
	} while (casterSkillContexts.contains(newSkillId));
	return newSkillId;
}

// 归属交接在途(PlayerFrozenComp:跨 zone 传送 / 同 zone 跨节点换图)的玩家不得施法:
// 冻结那一次存盘之后源端内存态不得再变,这次施法的结果到不了目标节点(它从盘上加载)。
// 函数名里的 Migration 是历史叫法,player_migrate 搬数据链已删。
// cross-zone-readiness-audit.md §11.3(该文只有 §11 仍有效)。
bool IsCasterFrozenForMigration(entt::entity casterEntity, uint64_t skillId, const char* where) {
	if (!tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(casterEntity)) {
		return false;
	}
	LOG_WARN << where << " rejected: caster frozen for cross-zone migration. "
		<< "skill_id=" << skillId << " caster=" << entt::to_integral(casterEntity);
	return true;
}

// Look up a skill context on the caster's context map; nullptr if absent.
SkillContextPtrComp FindCasterSkillContext(entt::entity caster, uint64_t skillId) {
	auto* casterSkillContextMap = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
	if (!casterSkillContextMap) {
		// 按需表不存在就是没有在途上下文,查询不创建。
		return nullptr;
	}
	const auto it = casterSkillContextMap->find(skillId);
	return it != casterSkillContextMap->end() ? it->second : nullptr;
}

namespace {

bool HasLiveScene(entt::entity actor) {
	const auto* scene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(actor);
	return scene != nullptr && tlsEcs.sceneRegistry.valid(scene->sceneEntity);
}

bool IsInSameScene(entt::entity caster, entt::entity target) {
	const auto* casterScene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(caster);
	const auto* targetScene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(target);
	// 双方可能共同保留同一个已销毁句柄，相等并不代表场景仍然存在。
	return casterScene != nullptr && targetScene != nullptr &&
		casterScene->sceneEntity == targetScene->sceneEntity &&
		tlsEcs.sceneRegistry.valid(casterScene->sceneEntity);
}

// 校验已解析的本地目标。客户端不透明actor号先经ActorNetIds反查,技能上下文仍保存本地完整句柄。
// registry由本节点所有场景共用,有效句柄仍须确认目标是玩家/NPC、同场景且未冻结。
// InBattleComp 不在这里判断,由调用方按各自语义处理(施法期拒绝 / 落点期整体丢弃)。
entt::entity ValidateSkillTargetEntity(entt::entity caster, entt::entity target) {
	const auto& registry = tlsEcs.actorRegistry;
	if (!registry.valid(target) || !registry.any_of<Player, Npc>(target)) {
		return entt::null;
	}
	if (registry.any_of<PlayerFrozenComp>(target) || !IsInSameScene(caster, target)) {
		return entt::null;
	}
	return target;
}

// 持久数据和玩家派生属性不能按空表放行;冷却、上下文与相位是按需状态,不在此列。
uint32_t CheckRequiredSkillComps(entt::entity caster, uint32_t skillTableId) {
	const auto& registry = tlsEcs.actorRegistry;
	if (!registry.valid(caster)) {
		return MAKE_ERROR_MSG(kThisEntityIsInvalid,
			"invalid skill caster=" << entt::to_integral(caster) << " skill_table_id=" << skillTableId);
	}
	if (registry.all_of<BaseAttributesComp, LevelComp>(caster) &&
        (!registry.any_of<Player>(caster) || registry.all_of<DerivedAttributesComp>(caster)) &&
		actor_net_id::Of(registry, caster) != 0) {
		return kSuccess;
	}
	return MAKE_ERROR_MSG(kServiceUnavailable,
		"missing required skill comps, caster=" << entt::to_integral(caster)
		<< " skill_table_id=" << skillTableId
		<< " has_base_attributes=" << registry.all_of<BaseAttributesComp>(caster)
		<< " has_level=" << registry.all_of<LevelComp>(caster)
		<< " has_derived_attributes=" << registry.all_of<DerivedAttributesComp>(caster)
		<< " has_actor_network_id=" << (actor_net_id::Of(registry, caster) != 0));
}

// 打断 = 这次施法结束:它的全部相位定时器一起摘掉(摘组件即取消定时器)。
// 同一施法者同一时刻至多一次施法在途 —— 相位进行中,非立即技能被前置校验拒绝,
// 立即技能先打断它 —— 所以全摘不会误伤别的施法。引导的周期 think 定时器也在其中,
// 以前只摘被检查的那一个组件,引导被打断后 RunEvery 会一直跑下去。
void EndAllSkillPhases(entt::entity caster) {
	tlsEcs.actorRegistry.remove<CastingTimerComp, RecoveryTimerComp, ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster);
}

// 延迟落点失败只结束指定技能,避免旧回调清掉后来施法的相位。
void AbortSkillCast(entt::entity caster, uint64_t skillId) {
	SkillSystem::HandleSkillFinish(caster, skillId);
	if (!tlsEcs.actorRegistry.valid(caster)) return;
	if (const auto* phase = tlsEcs.actorRegistry.try_get<CastingTimerComp>(caster);
		phase != nullptr && phase->skillId == skillId) {
		tlsEcs.actorRegistry.remove<CastingTimerComp>(caster);
	}
	if (const auto* phase = tlsEcs.actorRegistry.try_get<RecoveryTimerComp>(caster);
		phase != nullptr && phase->skillId == skillId) {
		tlsEcs.actorRegistry.remove<RecoveryTimerComp>(caster);
	}
	if (const auto* phase = tlsEcs.actorRegistry.try_get<ChannelFinishTimerComp>(caster);
		phase != nullptr && phase->skillId == skillId) {
		tlsEcs.actorRegistry.remove<ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster);
	}
}

// 前置检查与落点之间允许延迟，只在定时器回调入口复核。
bool CanContinueSkillCast(entt::entity caster, uint64_t skillId) {
	const auto context = FindCasterSkillContext(caster, skillId);
	if (context == nullptr) return false;
	if (CheckRequiredSkillComps(caster, context->skilltableid()) != kSuccess ||
		!HasLiveScene(caster) ||
		IsCasterFrozenForMigration(caster, skillId, "SkillSystem::CanContinueSkillCast") ||
		tlsEcs.actorRegistry.any_of<InBattleComp>(caster)) {
		AbortSkillCast(caster, skillId);
		return false;
	}
	return true;
}

bool HasRequiredTargetComps(entt::entity target) {
	const auto& registry = tlsEcs.actorRegistry;
	if (registry.all_of<BaseAttributesComp>(target) &&
		(!registry.any_of<Player>(target) || registry.all_of<LevelComp, DerivedAttributesComp>(target))) {
		return true;
	}
	LOG_ERROR << "Skill target missing persisted or derived component, target=" << entt::to_integral(target);
	return false;
}

}  // namespace

void SkillSystem::StartCooldown(entt::entity caster, const SkillTable* skillTable) {
	if (!tlsEcs.actorRegistry.valid(caster) || skillTable == nullptr) {
		LOG_ERROR << "StartCooldown: invalid caster or skill table, caster=" << entt::to_integral(caster);
		return;
	}
	if (skillTable->cooldown_id() == 0) {
		return;  // 0 = 无冷却组，Cooldown 表 id 从 1 起。
	}
	// 冷却表的唯一写入口,第一次启动冷却时延迟创建。
	auto& coolDownComp = tlsEcs.actorRegistry.get_or_emplace<CooldownTimeListComp>(caster);
	CooldownTimeComp comp;
	comp.set_start(TimeSystem::NowMilliseconds());
	comp.set_cooldown_table_id(skillTable->cooldown_id());

	const auto coolDownList = coolDownComp.mutable_cooldown_list();
	(*coolDownList)[skillTable->cooldown_id()] = comp;
}

void LookAtTargetPosition(entt::entity caster, const ReleaseSkillRequest* request, entt::entity target) {
	if (request->has_position()) {
		ViewSystem::LookAtPosition(caster, request->position());
	} else if (target != entt::null) {
		const auto *transform = tlsEcs.actorRegistry.try_get<Transform>(target);
		if (transform)
		{
			ViewSystem::LookAtPosition(caster, transform->location());
		}
	}
}

std::shared_ptr<SkillContextComp> CreateSkillContext(entt::entity caster, const ReleaseSkillRequest* request, entt::entity target) {
	auto context = std::make_shared<SkillContextComp>();
	context->set_caster(entt::to_integral(caster));
	context->set_skilltableid(request->skill_table_id());
	// 只记录解析通过的目标:后续落点(HandleSkillSpell / TriggerSkillEffect)按这个整数作用到目标上。
	context->set_target(target == entt::null ? 0 : entt::to_integral(target));
	context->set_casttime(TimeSystem::NowMilliseconds());

	static const SkillContextCompMap kEmptyContexts;
	const auto* casterContexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
	context->set_skillid(GenerateUniqueSkillId(casterContexts ? *casterContexts : kEmptyContexts));
	return context;
}

uint32_t AddSkillContext(entt::entity caster, const SkillContextPtrComp& context) {
	// 在途上下文只归施法者所有;目标不保存只写不读的副本,施法者销毁即自然回收。
	// 目标有效性由施法预检与落点复核负责,登记上下文不读写目标的组件。
	// SkillContextCompMap 的唯一写入口:首次写入时延迟创建,失效实体不创建。
	if (!tlsEcs.actorRegistry.valid(caster)) {
		return MAKE_ERROR_MSG(kThisEntityIsInvalid,
			"cannot store skill context, entity=" << entt::to_integral(caster));
	}
	tlsEcs.actorRegistry.get_or_emplace<SkillContextCompMap>(caster).emplace(context->skillid(), context);
	return kSuccess;
}

void ConsumeItems(entt::entity caster, const SkillTable* skillTable) {
	// TODO: Implement item consumption logic
}

void ConsumeResources(entt::entity caster, const SkillTable* skillTable) {
	// TODO: Implement resource consumption logic
}

void ApplySkillHitEffectIfValid(const entt::entity casterEntity, const entt::entity targetEntity) {
	// 回合制战斗中的目标不受实时技能影响(与 ValidateTarget / HandleSkillSpell 口径一致)。
	if (targetEntity == entt::null || tlsEcs.actorRegistry.any_of<InBattleComp>(targetEntity)) {
		return;
	}
	BuffSystem::OnSkillHit(casterEntity, targetEntity);
}

uint32_t SkillSystem::ReleaseSkill(const entt::entity casterEntity, const ReleaseSkillRequest* request) {
	LookupSkillOrReturnError(request->skill_table_id());

	const entt::entity target = ValidateSkillTargetEntity(
		casterEntity, actor_net_id::Resolve(tlsEcs.actorRegistry, request->target_id()));
	RETURN_ON_ERROR(CheckSkillPrerequisites(casterEntity, request, target));
	LookAtTargetPosition(casterEntity, request, target);
	BroadcastSkillUsedMessage(casterEntity, request, target);

	const bool hasCastPhase = IsSkillOfType(skillRow->id(), kGeneralSkill) ||
		IsSkillOfType(skillRow->id(), kChannelSkill);
	SkillContextPtrComp context;
	if (hasCastPhase) {
		context = CreateSkillContext(casterEntity, request, target);
		RETURN_ON_ERROR(AddSkillContext(casterEntity, context));
	}

	ConsumeItems(casterEntity, skillRow);
	ConsumeResources(casterEntity, skillRow);
	StartCooldown(casterEntity, skillRow);
	if (hasCastPhase) {
		SetupCastingTimer(casterEntity, skillRow, context->skillid());
	}

	ApplySkillHitEffectIfValid(casterEntity, target);

	return kSuccess;
}

uint32_t CheckPlayerLevel(const entt::entity casterEntity, const SkillTable* skillTable) {
	// TODO: Implement level requirement validation for non-NPC casters
	// 落地时读 playerlevel::CappedLevel(LevelComp.level()),不直读存储等级(player_level_rules.h)
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
	// CombatStateCollectionComp 是按需状态(CombatStateSystem::AddCombatState 第一次加状态时创建),
	// 不存在 = 身上没有任何战斗状态 = 不受限制,这里放行是正常分支,不是缺必需组件。
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
	return kSuccess;
}

uint32_t CheckItemUse(const entt::entity casterEntity, const SkillTable* skillTable) {
	// TODO: Validate item requirements before skill use
	return kSuccess;
}

uint32_t SkillSystem::CheckSkillPrerequisites(const entt::entity casterEntity, const ::ReleaseSkillRequest* request, const entt::entity target) {
	LookupSkillOrReturnError(request->skill_table_id());

	// 只验证持久和派生组件,不能要求尚未使用过的冷却/上下文先存在。
	RETURN_ON_ERROR(CheckRequiredSkillComps(casterEntity, request->skill_table_id()));
	// 无目标和范围技能也不能在已销毁场景中施放，且须在打断旧相位等副作用之前拒绝。
	if (!HasLiveScene(casterEntity)) {
		return kSkillCannotBeCastInCurrentState;
	}
	if (IsCasterFrozenForMigration(casterEntity, 0, "SkillSystem::CheckSkillPrerequisites")) {
		return kSkillCannotBeCastInCurrentState;
	}

	// 回合制战斗在途:实时技能一律拒绝(moba-battle-target-architecture.md 红线 2
	// "局中挡背包/交易/换装",turn-based-battle-server.md §5.3 冻结清单)。
	// 玩家的战斗数值已经被快照带进 battle 节点,此刻在场景里放技能既改不了局内状态,
	// 又会在结算回写 HP 时被整体覆盖 —— 打了等于白打,还会误伤旁人。
	// 不走 MAKE_ERROR_MSG:这是玩家高频可触发的业务拒绝,刷栈没有意义。
	if (tlsEcs.actorRegistry.any_of<InBattleComp>(casterEntity)) {
		return kSkillCannotBeCastInCurrentState;
	}

	RETURN_ON_ERROR(ValidateTarget(casterEntity, request, target));
	RETURN_ON_ERROR(CheckCooldown(casterEntity, skillRow));
	// 相位检查可能打断旧施法。可以提前完成的纯校验必须先过,拒绝新技能时保留旧上下文和相位。
	RETURN_ON_ERROR(CheckPlayerLevel(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckBuff(casterEntity, skillRow));
	RETURN_ON_ERROR(CombatStateSystem::ValidateSkillUsage(casterEntity, kActorActionUseSkill));
	RETURN_ON_ERROR(ActorActionStateSystem::CanExecuteActionWithoutStateChange(casterEntity, kActorActionUseSkill));
	RETURN_ON_ERROR(CheckItemUse(casterEntity, skillRow));

	RETURN_ON_ERROR(CheckCasting(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckRecovery(casterEntity, skillRow));
	RETURN_ON_ERROR(CheckChannel(casterEntity, skillRow));
	// 提交阶段由动作入口复核并激活状态。
	RETURN_ON_ERROR(CheckState(casterEntity, skillRow));
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
	if (!CanContinueSkillCast(casterEntity, skillId)) return;
	if (!HandleSkillSpell(casterEntity, skillId)) return;
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

	// 后摇相位开始(按需状态,生命周期见 skill_comp.h)
	auto& recoveryTimerComp = tlsEcs.actorRegistry.emplace_or_replace<RecoveryTimerComp>(casterEntity);
	recoveryTimerComp.skillId = skillId;
	recoveryTimerComp.timer.RunAfter(skillRow->recovery_time(), [casterEntity = casterEntity, skillId = skillId] {
		// 后摇结束:先摘相位组件再收尾。在自身定时器回调里销毁组件是安全的 ——
		// TimerTaskComp::OnTimer 调完回调就返回,不再碰组件。
		tlsEcs.actorRegistry.remove<RecoveryTimerComp>(casterEntity);
		HandleSkillFinish(casterEntity, skillId);
		});
}

void SkillSystem::HandleSkillFinish(const entt::entity casterEntity, uint64_t skillId) {
    if (!tlsEcs.actorRegistry.valid(casterEntity))
    {
        return;
    }

	// 没有按需表表示没有在途上下文,重复收尾不创建表。
	auto* casterSkillContextMap = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(casterEntity);
	if (casterSkillContextMap == nullptr)
	{
		return;
	}
	const auto skillContentIt = casterSkillContextMap->find(skillId);
	if (skillContentIt == casterSkillContextMap->end())
	{
		return;
	}

	// 只有施法者拥有这条上下文,不再维护目标镜像。
	casterSkillContextMap->erase(skillContentIt);
}

void SkillSystem::HandleChannelSkillSpell(entt::entity casterEntity, uint64_t skillId) {
	if (!CanContinueSkillCast(casterEntity, skillId)) return;

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

	if (!HandleSkillSpell(casterEntity, skillId)) return;

	// 引导相位开始(按需状态,生命周期见 skill_comp.h):结束定时器 + 周期 think 定时器
	auto& channelFinishTimerComp = tlsEcs.actorRegistry.emplace_or_replace<ChannelFinishTimerComp>(casterEntity);
	channelFinishTimerComp.skillId = skillId;
	channelFinishTimerComp.timer.RunAfter(skillRow->channel_finish(), [casterEntity = casterEntity, skillId = skillId] {
		return HandleChannelFinish(casterEntity, skillId);
		});

	auto& channelIntervalTimer = tlsEcs.actorRegistry.emplace_or_replace<ChannelIntervalTimerComp>(casterEntity).timer;
	channelIntervalTimer.RunEvery(skillRow->channel_think(), [casterEntity = casterEntity, skillId = skillId] {
		return HandleChannelThink(casterEntity, skillId);
		});
}

void SkillSystem::HandleChannelThink(entt::entity casterEntity, uint64_t skillId) {
	if (!CanContinueSkillCast(casterEntity, skillId)) return;
	// TODO: Implement channel think logic
}

void SkillSystem::HandleChannelFinish(const entt::entity casterEntity, const uint64_t skillId) {
    if (!tlsEcs.actorRegistry.valid(casterEntity))
    {
        return;
    }

	// 引导相位结束:结束定时器与周期 think 定时器一起摘掉,再进入后摇。
	// 本函数就是 ChannelFinishTimerComp 自己的定时器回调,在回调里销毁它是安全的
	// (TimerTaskComp::OnTimer 调完回调就返回,不再碰组件)。
	tlsEcs.actorRegistry.remove<ChannelFinishTimerComp, ChannelIntervalTimerComp>(casterEntity);
	if (!CanContinueSkillCast(casterEntity, skillId)) return;
	HandleSkillRecovery(casterEntity, skillId);
}

uint32_t SkillSystem::ValidateTarget(const entt::entity casterEntity, const ::ReleaseSkillRequest* request, const entt::entity target) {
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

		// 目标类型、同场景与冻结条件已由 ValidateSkillTargetEntity 统一校验。
		if (target == entt::null) {
			return MAKE_ERROR_MSG(kSkillInvalidTargetId,
				"target_id=" << request->target_id()
				<< " skill_table_id=" << request->skill_table_id()
				<< " reason=unresolved_or_not_in_caster_scene_or_frozen");
		}

		// 目标在回合制战斗里:不受实时技能影响。它的 HP 权威在 battle 节点的快照副本上,
		// 结算会用终值整体覆盖,此刻在场景里扣的血只会被吞掉(或在覆盖前误导表现)。
		if (tlsEcs.actorRegistry.any_of<InBattleComp>(target)) {
			return kSkillInvalidTarget;
		}

		return kSuccess;
	}

	return kSuccess;
}

// Common interrupt-or-reject check for casting/recovery/channel timers
// 相位组件是按需状态(见 skill_comp.h):不存在 = 没在这一相位,正常放行,不是缺必需组件;
// 存在 = 相位进行中,立即技能打断它,其余技能拒绝。这里不再顺手摘「已经跑完」的组件 ——
// 相位结束时由结束动作自己摘(F051);这里只处理打断,以及定时器已不在跑的异常残留。
template <typename PhaseComp>
uint32_t CheckTimerPhase(const entt::entity casterEntity, const SkillTable* skillTable) {
	auto* phase = tlsEcs.actorRegistry.try_get<PhaseComp>(casterEntity);
	if (phase == nullptr)
	{
		return kSuccess;
	}

	if (!phase->timer.IsActive())
	{
		// 组件在、定时器却没在跑:某条路径开始了相位却没挂上定时器,或结束时漏摘。
		// 它的结束回调永远不会来了,不能因此把施法者永久卡住:记错误,然后按「这次施法结束」
		// 收尾 —— 与打断分支同样处理。只摘这一个组件不够:残留的是引导结束定时器时,
		// 周期 think 定时器还在跑;这次施法的上下文也没人收,会一直留在上下文表里。
		LOG_ERROR << "Stale skill phase component without active timer. caster=" << entt::to_integral(casterEntity)
			<< " skill_id=" << phase->skillId << " skill_table_id=" << skillTable->id();
		const auto staleSkillId = phase->skillId;
		SkillSystem::HandleSkillFinish(casterEntity, staleSkillId);
		EndAllSkillPhases(casterEntity);
		return kSuccess;
	}

	if (!skillTable->immediate())
	{
		return MAKE_ERROR_MSG(kSkillUnInterruptible,
			"skill_id=" << skillTable->id()
			<< " caster=" << entt::to_integral(casterEntity));
	}

	LOG_INFO << "Immediate skill: " << skillTable->id()
		<< " is currently in phase. Sending interrupt message.";
	SkillSystem::SendSkillInterruptedMessage(casterEntity, skillTable->id());
	// 被打断的那一次施法,它的 SkillContext 是在 ReleaseSkill 里建的,
	// 只有 HandleSkillFinish 会删。相位组件摘掉之后回调再也不会触发,
	// 上下文就永久留在施法者的 SkillContextCompMap 里 ——
	// 玩家每打断一次泄漏一条,无上界。先收口上下文,再结束这次施法的全部相位。
	const auto interruptedSkillId = phase->skillId;
	SkillSystem::HandleSkillFinish(casterEntity, interruptedSkillId);
	EndAllSkillPhases(casterEntity);
	return kSuccess;
}

uint32_t SkillSystem::CheckCooldown(const entt::entity casterEntity, const SkillTable* skillTable) {
	if (!tlsEcs.actorRegistry.valid(casterEntity)) {
		return MAKE_ERROR_MSG(kThisEntityIsInvalid,
			"invalid cooldown caster=" << entt::to_integral(casterEntity));
	}
	// 按需表不存在表示没有启动过冷却;查询保持组件集合不变。
	auto* coolDownTimeListComp = tlsEcs.actorRegistry.try_get<CooldownTimeListComp>(casterEntity);
	if (coolDownTimeListComp == nullptr)
	{
		return kSuccess;
	}
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

void SkillSystem::BroadcastSkillUsedMessage(const entt::entity casterEntity, const ::ReleaseSkillRequest* request, const entt::entity target) {
	SkillUsedS2C skillUsedS2C;
	const auto casterNetId = actor_net_id::Of(tlsEcs.actorRegistry, casterEntity);
	if (casterNetId == 0) {
		LOG_ERROR << "BroadcastSkillUsedMessage: caster network id missing";
		return;
	}
	skillUsedS2C.set_entity(casterNetId);
	if (target != entt::null) {
		skillUsedS2C.add_target_entity(actor_net_id::Of(tlsEcs.actorRegistry, target));
	}
	skillUsedS2C.set_skill_table_id(request->skill_table_id());
	skillUsedS2C.mutable_position()->CopyFrom(request->position());

	ViewSystem::BroadcastMessageToVisiblePlayers(
		casterEntity,
		SceneSkillClientPlayerNotifySkillUsedMessageId,
		skillUsedS2C
	);
}

void SkillSystem::SetupCastingTimer(entt::entity casterEntity, const SkillTable* skillTable, uint64_t skillId) {
	if (!tlsEcs.actorRegistry.valid(casterEntity) || skillTable == nullptr) {
		LOG_ERROR << "SetupCastingTimer: invalid caster or skill table, caster=" << entt::to_integral(casterEntity);
		return;
	}
	// 前摇到点后进入哪种施放:普通技能直接结算,引导技能进入引导相位。
	// 其它类型没有前摇相位,不挂组件 —— 以前先懒建一个空的 CastingTimerComp,
	// 留给下一次施法的 CheckCasting 去摘(F051)。
	void (*onCastPoint)(entt::entity, uint64_t) = nullptr;
	if (IsSkillOfType(skillTable->id(), kGeneralSkill)) {
		onCastPoint = &SkillSystem::HandleGeneralSkillSpell;
	}
	else if (IsSkillOfType(skillTable->id(), kChannelSkill)) {
		onCastPoint = &SkillSystem::HandleChannelSkillSpell;
	}
	if (onCastPoint == nullptr) {
		return;
	}

	// 前摇相位开始(按需状态,生命周期见 skill_comp.h)
	auto& castingTimerComp = tlsEcs.actorRegistry.emplace_or_replace<CastingTimerComp>(casterEntity);
	castingTimerComp.skillId = skillId;
	castingTimerComp.timer.RunAfter(skillTable->cast_point(),
		[casterEntity = casterEntity, skillId = skillId, onCastPoint] {
			// 前摇结束:先摘相位组件再进入施放。在自身定时器回调里销毁组件是安全的 ——
			// TimerTaskComp::OnTimer 调完回调就返回,不再碰组件。
			tlsEcs.actorRegistry.remove<CastingTimerComp>(casterEntity);
			onCastPoint(casterEntity, skillId);
		});
}

void SkillSystem::SendSkillInterruptedMessage(const entt::entity casterEntity, const uint32_t skillTableId) {
	SkillInterruptedS2C skillInterruptedS2C;
	const auto casterNetId = actor_net_id::Of(tlsEcs.actorRegistry, casterEntity);
	if (casterNetId == 0) {
		LOG_ERROR << "SendSkillInterruptedMessage: caster network id missing";
		return;
	}
	skillInterruptedS2C.set_entity(casterNetId);
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

	// 落点时重新解析:从施法到 cast_point 之间目标可能已经换场景、下线或进入冻结 / 回合制战斗。
	// 没有目标(0)时直接收手,不把 entity{0} 之类的无效句柄交给 AddOrUpdateBuff。
	const auto targetEntity = ValidateSkillTargetEntity(casterEntity, entt::entity{skillContext->target()});
	if (targetEntity == entt::null || tlsEcs.actorRegistry.any_of<InBattleComp>(targetEntity))
	{
		return;
	}
	LOG_INFO << "Triggering skill effect. Caster: " << entt::to_integral(casterEntity) << ", Skill ID: " << skillId;

	for (const auto& effect : skillRow->effect()) {
		BuffSystem::AddOrUpdateBuff(targetEntity, effect, skillContext->caster());
	}
}

bool IsTargetDead(entt::entity targetEntity) {
	ECS_GET_OR_FALSE(targetBaseAttributes, BaseAttributesComp, targetEntity);
	return targetBaseAttributes->health() <= 0;
}


// 实时技能伤害:公式与回合引擎共用 combat_damage_rules.h(比例减伤,常驻减伤封顶 60%)。
// 技能按 SkillTable.damage_type 选物伤 / 法伤;实时侧没有 PVP 对局概念,不乘 PVP 伤害系数。
double CalculateFinalDamage(const entt::entity casterEntity, const entt::entity target, double baseDamage,
							const SkillTable& skillRow) {
	// 施法者的 BaseAttributesComp 是持久数据,玩家加载时挂上(PlayerDatabaseMessageFieldsUnmarshal):
	// 调用方已经验证两端必需组件,且到这里没有回调;缺失仍保守拒绝计算,绝不补建。
	const auto *casterAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(casterEntity);
	if (casterAttributes == nullptr)
	{
		LOG_ERROR << "CalculateFinalDamage: missing caster BaseAttributesComp, caster=" << entt::to_integral(casterEntity)
			<< " skill_table_id=" << skillRow.id();
		return 0.0;
	}
	const auto *targetAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(target);
	if (targetAttributes == nullptr)
		return 0.0;

	// critchance 是 uint64(见 CLAUDE.md §4),装不下 0~1 的小数,口径只能是
	// 整数百分比。旧写法把它直接当概率跟 [0,1) 的均匀随机数比:今天全系统
	// 没有任何一处写 critchance,所以恒为 0 = 永不暴击;一旦有人按字面填 5(5%),
	// 就会变成 5.0 > 随机数恒成立 = 100% 暴击。这里显式按百分比换算并夹到 [0,1]。
	const double critChance = std::clamp(static_cast<double>(casterAttributes->critchance()) / 100.0, 0.0, 1.0);
	uint64_t physicalAttack = 0;
	uint64_t magicAttack = 0;
	if (const auto* casterDerived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(casterEntity))
	{
		physicalAttack = casterDerived->physical_attack();
		magicAttack = casterDerived->magic_attack();
	}
	uint64_t defense = 0;
	if (const auto* targetDerived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(target))
	{
		defense = targetDerived->defense();
	}
	// 没有等级组件的目标(NPC / 怪物)按 1 级算。LevelComp 只有玩家有(加载时挂上),
	// 按生效等级取:超上限的存储等级按上限(player_level_rules.h)
	uint32_t targetLevel = 1;
	if (const auto* targetLevelComp = tlsEcs.actorRegistry.try_get<LevelComp>(target))
	{
		targetLevel = playerlevel::CappedLevel(targetLevelComp->level());
	}

	double finalDamage = combatdamage::DamageBeforeCritical(
		baseDamage, casterAttributes->strength(),
		combatdamage::SelectAttack(skillRow.damage_type(), physicalAttack, magicAttack),
		skillRow.attack_multiplier(),
		targetAttributes->armor(), defense, targetAttributes->resistance(), targetLevel);

	// rand() 是全局非线程安全且未播种的 C 运行时状态,战斗判定统一走 tlsRandom。
	if (critChance > 0.0 && tlsRandom.RandReal<double>(0.0, 1.0) < critChance) {
		finalDamage *= 2;
	}

	return std::max(finalDamage, 0.0);
}


bool CalculateSkillDamage(const entt::entity casterEntity, DamageEventComp& damageEvent) {
	const auto skillContext = FindCasterSkillContext(casterEntity, damageEvent.skill_id());
	if (!skillContext)
	{
		LOG_ERROR << "Skill context not found for skill ID: " << damageEvent.skill_id();
		return false;
	}

	LookupSkillOrReturn(skillContext->skilltableid(), false);

    const auto targetEntity = entt::entity{damageEvent.target()};

	// 施法者的 LevelComp 在玩家加载时挂上(PlayerDatabaseMessageFieldsUnmarshal),是必需组件;
	// 缺了是不变量被破坏,返回失败让整条落点链路停止,不能让零伤害继续触发Buff。
	const auto* levelComponent = tlsEcs.actorRegistry.try_get<LevelComp>(casterEntity);
	if (levelComponent == nullptr)
	{
		LOG_ERROR << "CalculateSkillDamage: missing caster LevelComp, caster=" << entt::to_integral(casterEntity)
			<< " skill_id=" << damageEvent.skill_id();
		return false;
	}
	// 伤害表达式的 level 参数按生效等级(超上限的存储等级按上限,player_level_rules.h)
	SkillTableManager::Instance().SetDamageParam(
		{static_cast<double>(playerlevel::CappedLevel(levelComponent->level()))});

	damageEvent.set_attacker_id(entt::to_integral(casterEntity));

    double baseDamage = SkillTableManager::Instance().GetDamage(skillContext->skilltableid());
    double finalDamage = CalculateFinalDamage(casterEntity, targetEntity, baseDamage, *skillRow);
    damageEvent.set_damage(finalDamage);
	return true;
}


void TriggerBeforeDamageEvents(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent) {
    BuffSystem::OnBeforeGiveDamage(casterEntity, targetEntity, damageEvent);
    BuffSystem::OnBeforeTakeDamage(casterEntity, targetEntity, damageEvent);
}

void ApplyDamage(BaseAttributesComp& baseAttributesPBComponent, const DamageEventComp& damageEvent) {
    // 与回合引擎共用落血规则:向上取整、不超过当前气血;buff 改写后的伤害若为 NaN / 无穷,不进整数转换
    const uint64_t health = baseAttributesPBComponent.health();
    baseAttributesPBComponent.set_health(health - combatdamage::DamageToHealth(damageEvent.damage(), health));
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

bool SkillSystem::HandleSkillSpell(const entt::entity casterEntity, const uint64_t skillId) {
	const auto skillContext = FindCasterSkillContext(casterEntity, skillId);
	if (!skillContext)
	{
		return false;
	}

	// 落点时重新解析:施法期间目标可能已经换场景、下线或进入归属交接冻结。
	// 冻结目标也要在这里整体拦下,理由同下面 InBattleComp 的注释。
	const entt::entity targetEntity = ValidateSkillTargetEntity(casterEntity, entt::entity{skillContext->target()});

	if (targetEntity == entt::null)
	{
		return true;
	}

	// 施法瞬间目标还在场景里、cast_point 定时器触发时它已经进了回合制战斗:整体早退。
	// 目标冻结和回合制战斗均在伤害与 Buff 副作用之前整体拦下。
	if (tlsEcs.actorRegistry.any_of<InBattleComp>(targetEntity))
	{
		LOG_INFO << "[TurnBattle] Target entered turn-based battle before impact;entire skill discarded. caster="
				 << entt::to_integral(casterEntity) << " target=" << entt::to_integral(targetEntity)
				 << " skill_id=" << skillId;
		return true;
	}
	if (!HasRequiredTargetComps(targetEntity)) {
		AbortSkillCast(casterEntity, skillId);
		return false;
	}

	DamageEventComp damageEvent;
	damageEvent.set_skill_id(skillId);
	damageEvent.set_target(skillContext->target());
	if (!CalculateSkillDamage(casterEntity, damageEvent)) {
		AbortSkillCast(casterEntity, skillId);
		return false;
	}
	DealDamage(damageEvent, casterEntity, targetEntity);
	return true;
}


