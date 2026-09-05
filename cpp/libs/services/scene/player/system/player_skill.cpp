#include "player_skill.h"

#include <unordered_set>

#include "muduo/base/Logging.h"
#include "table/code/class_table.h"
#include "table/code/skill_table.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include <thread_context/ecs_context.h>

namespace
{
	void AddSkillIfMissing(PlayerSkillListComp &skillList,
						   std::unordered_set<uint32_t> &seenSkillIds,
						   const uint32_t skillId)
	{
		if (skillId == 0)
		{
			return;
		}

		if (!SkillTableManager::Instance().Exists(skillId))
		{
			LOG_WARN << "Skipping unknown class skill_table_id=" << skillId;
			return;
		}

		if (!seenSkillIds.emplace(skillId).second)
		{
			return;
		}

		auto *pbSkill = skillList.add_skill_list();
		pbSkill->set_skill_table_id(skillId);
	}
}

void PlayerSkillSystem::RegisterPlayer(entt::entity player)
{
	SanitizeSkillList(player);

	auto &skillList = tlsEcs.actorRegistry.get_or_emplace<PlayerSkillListComp>(player);
	std::unordered_set<uint32_t> seenSkillIds;
	for (const auto &skill : skillList.skill_list())
	{
		if (skill.skill_table_id() != 0)
		{
			seenSkillIds.emplace(skill.skill_table_id());
		}
	}

	for (const auto &tableClass : FindAllClassTable().data())
	{
		for (const auto skillId : tableClass.skill())
		{
			AddSkillIfMissing(skillList, seenSkillIds, skillId);
		}
	}
}

bool PlayerSkillSystem::HasSkill(entt::entity player, const uint32_t skillTableId)
{
	if (skillTableId == 0 || !tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}

	const auto *skillList = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	if (skillList == nullptr)
	{
		return false;
	}

	for (const auto &skill : skillList->skill_list())
	{
		if (skill.skill_table_id() == skillTableId)
		{
			return true;
		}
	}

	return false;
}

void PlayerSkillSystem::SanitizeSkillList(entt::entity player)
{
	auto *skillList = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	if (skillList == nullptr)
	{
		return;
	}

	PlayerSkillListComp sanitizedSkillList;
	std::unordered_set<uint32_t> seenSkillIds;
	bool changed = false;

	for (const auto &skill : skillList->skill_list())
	{
		const auto skillId = skill.skill_table_id();
		if (skillId == 0 || !SkillTableManager::Instance().Exists(skillId))
		{
			LOG_WARN << "Removing unknown player skill_table_id=" << skillId
					 << " entity=" << entt::to_integral(player);
			changed = true;
			continue;
		}

		if (!seenSkillIds.emplace(skillId).second)
		{
			LOG_WARN << "Removing duplicate player skill_table_id=" << skillId
					 << " entity=" << entt::to_integral(player);
			changed = true;
			continue;
		}

		sanitizedSkillList.add_skill_list()->CopyFrom(skill);
	}

	if (changed)
	{
		skillList->Swap(&sanitizedSkillList);
	}
}
