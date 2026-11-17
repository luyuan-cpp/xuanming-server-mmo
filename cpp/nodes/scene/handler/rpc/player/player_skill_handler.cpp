
#include "player_skill_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "proto/common/base/tip.pb.h"
#include "combat/skill/system/skill.h"
#include "macros/return_define.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "player/system/player_skill.h"
#include "player/system/player_pet.h"
#include "table/code/skill_table.h"
#include <thread_context/ecs_context.h>

///<<< END WRITING YOUR CODE

void SceneSkillClientPlayerHandler::ReleaseSkill(entt::entity player,const ::ReleaseSkillRequest* request,
	::ReleaseSkillResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
const auto skillTableId = request->skill_table_id();
if (skillTableId == 0 || !SkillTableManager::Instance().Exists(skillTableId))
{
	LOG_WARN << "Reject ReleaseSkill: unknown skill_table_id=" << skillTableId
			 << " entity=" << entt::to_integral(player);
	response->mutable_error_message()->set_id(kInvalidTableId);
	return;
}

// 没有这个技能 / 不能用回 kInvalidTableId;缺技能列表、职业或等级是加载故障,回 kServiceUnavailable(已记 ERROR)
if (const auto usable = PlayerSkillSystem::CheckSkillUsable(player, skillTableId); usable != kSuccess)
{
	LOG_WARN << "Reject ReleaseSkill: entity=" << entt::to_integral(player)
			 << " cannot use skill_table_id=" << skillTableId << " tip=" << usable;
	response->mutable_error_message()->set_id(usable);
	return;
}

// 回合模型只能走 Battle.SubmitAction，不能交给旧即时制按秒运行同一份效果。
// 非主动条目也没有手动施放动作。
if (const auto *skillRow = SkillTableManager::Instance().FindByIdSilent(skillTableId).first;
	skillRow != nullptr && (skillRow->battle_model() == 1 || PlayerSkillSystem::IsPassiveSkill(*skillRow)))
{
	response->mutable_error_message()->set_id(kSkillCannotBeCastInCurrentState);
	return;
}

	CHECK_PLAYER_REQUEST(request, SkillSystem::ReleaseSkill);
///<<< END WRITING YOUR CODE

}

void SceneSkillClientPlayerHandler::NotifySkillUsed(entt::entity player,const ::SkillUsedS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneSkillClientPlayerHandler::NotifySkillInterrupted(entt::entity player,const ::SkillInterruptedS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneSkillClientPlayerHandler::ListSkills(entt::entity player,const ::ListSkillsRequest* request,
	::ListSkillsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 按人物/宝宝身份返回已学技能、可学习目录、等级与银两。
	// BuildSkillBook 成功时 skill_list 一定在场(哪怕是空表):robot 的
	// SceneSkillClientPlayerListSkillsHandler 把缺字段读成"技能被清空"(SetOwnedSkillIDs(nil)),
	// 显式的空列表才能让首登的就绪信号照常触发。
	const auto err = request->pet_id() != 0 ? PetSystem::BuildSkillBook(player, request->pet_id(), *response) :
		PlayerSkillSystem::BuildSkillBook(player, *response);
	if (err != kSuccess)
	{
		response->Clear();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneSkillClientPlayerHandler::UpgradeSkill(entt::entity player,const ::UpgradeSkillRequest* request,
	::UpgradeSkillResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 修习者只认会话绑定的 player 实体,请求里没有、也不接受角色身份
	if (request->pet_id() != 0) PetSystem::UpgradeSkill(player, *request, *response);
	else PlayerSkillSystem::UpgradeSkill(player, *request, *response);
///<<< END WRITING YOUR CODE

}
