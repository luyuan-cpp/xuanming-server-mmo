#pragma once

#include <cstdint>
#include <entt/src/entt/entity/entity.hpp>

class ReleaseSkillRequest;
class SkillTable;
class CooldownTimeListComp;

class SkillSystem
{
public:
	// 冷却列表唯一写入口:有效实体第一次启动冷却时创建;失效实体不创建。
	static void StartCooldown(const entt::entity casterEntity, const SkillTable* skillTable);

	// 冷却列表的另一个创建点:加载玩家时存档里有冷却记录(或更新版本带来的列表级字段)才挂(docs/design/realtime-cooldown-buff-persistence.md §2.2)。
	// 记录原样挂上,到点的不删(按时间判定,同组下次施法覆盖);开始时刻晚于本机当前时刻的(写盘节点时钟超前)
	// 压到当前时刻,剩余最多一个整冷却。只在加载链上调用。
	static void RestoreCooldowns(entt::entity casterEntity, const CooldownTimeListComp& saved);

	static uint32_t ReleaseSkill(entt::entity casterEntity, const ::ReleaseSkillRequest* request);
	// 最先确认实体、持久数据与玩家派生属性;缺失时拒绝且不打断/加状态。
	// 冷却和上下文按需存在,缺失不构成前置失败,写入口负责首次创建。
	static uint32_t CheckSkillPrerequisites(entt::entity casterEntity, const ::ReleaseSkillRequest* request, entt::entity target);

	static bool IsSkillOfType(const uint32_t skillTableId, const uint32_t skillType);

	static void HandleGeneralSkillSpell(const entt::entity casterEntity, uint64_t skillId);
	static void HandleSkillRecovery(const entt::entity casterEntity, uint64_t skillId);
	static void HandleSkillFinish(const entt::entity casterEntity, uint64_t skillId);

	static void HandleChannelSkillSpell(entt::entity casterEntity, uint64_t skillId);
	static void HandleChannelThink(entt::entity casterEntity, uint64_t skillId);
	static void HandleChannelFinish(entt::entity casterEntity, uint64_t skillId);

    // target 由 ReleaseSkill 的 ValidateSkillTargetEntity 校验为同场景、未冻结的玩家/NPC或空。
    static uint32_t ValidateTarget(entt::entity casterEntity, const ::ReleaseSkillRequest* request, entt::entity target);

    // 只读检查冷却;活实体缺列表表示无冷却,不创建组件。
    static uint32_t CheckCooldown(entt::entity casterEntity, const SkillTable* skillTable);

    // Check cast timer
    static uint32_t CheckCasting(entt::entity casterEntity, const SkillTable* skillTable);

	static uint32_t CheckRecovery(const entt::entity casterEntity, const SkillTable* skillTable);

	static uint32_t CheckChannel(const entt::entity casterEntity, const SkillTable* skillTable);

    // Broadcast skill-used message
    static void BroadcastSkillUsedMessage(entt::entity casterEntity, const ::ReleaseSkillRequest* request, entt::entity target);
	
    // Set up casting timer
    static void SetupCastingTimer(entt::entity casterEntity, const SkillTable* skillTable, uint64_t skillId);

    // Send skill-interrupted message
    static void SendSkillInterruptedMessage(entt::entity casterEntity, uint32_t skillTableId);

	static void TriggerSkillEffect(entt::entity casterEntity, uint64_t skillId);

	static bool HandleSkillSpell(const entt::entity casterEntity, const uint64_t skillId);
};
