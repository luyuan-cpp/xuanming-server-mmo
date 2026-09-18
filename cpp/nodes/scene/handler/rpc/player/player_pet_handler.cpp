
#include "player_pet_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <map>

#include <muduo/base/Logging.h>

#include "player/system/player_pet.h"
#include "player_gm_guard.h" // P0-a:GM 客户端指令的 scene 侧第二道锁
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
	// 失败码统一写全局 tip 实体,由生成层 TRANSFER_ERROR_MESSAGE 搬进响应
	void SetTip(uint32_t err)
	{
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(err);
	}

	std::map<uint32_t, uint32_t> ToStdMap(const google::protobuf::Map<uint32_t, uint32_t>& source)
	{
		std::map<uint32_t, uint32_t> result;
		for (const auto& [k, v] : source)
		{
			result[k] = v;
		}
		return result;
	}
} // namespace
///<<< END WRITING YOUR CODE

void ScenePetClientPlayerHandler::GetPetList(entt::entity player,const ::GetPetListRequest* request,
	::GetPetListResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PetSystem::BuildList(player, *response->mutable_pets());
///<<< END WRITING YOUR CODE

}

void ScenePetClientPlayerHandler::SummonPet(entt::entity player,const ::SummonPetRequest* request,
	::SummonPetResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PetSystem::Summon(player, request->pet_id()); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PetSystem::BuildList(player, *response->mutable_pets());
///<<< END WRITING YOUR CODE

}

void ScenePetClientPlayerHandler::RecallPet(entt::entity player,const ::RecallPetRequest* request,
	::RecallPetResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PetSystem::Recall(player); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PetSystem::BuildList(player, *response->mutable_pets());
///<<< END WRITING YOUR CODE

}

void ScenePetClientPlayerHandler::AllocatePetPoints(entt::entity player,const ::AllocatePetPointsRequest* request,
	::AllocatePetPointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (request->pet_id() == 0 || request->allocated().empty())
	{
		SetTip(kInvalidParameter);
		return;
	}
	if (const auto err = PetSystem::Allocate(player, request->pet_id(), ToStdMap(request->allocated()));
		err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PetSystem::BuildList(player, *response->mutable_pets());
///<<< END WRITING YOUR CODE

}

void ScenePetClientPlayerHandler::ResetPetPoints(entt::entity player,const ::ResetPetPointsRequest* request,
	::ResetPetPointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PetSystem::Reset(player, request->pet_id()); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PetSystem::BuildList(player, *response->mutable_pets());
///<<< END WRITING YOUR CODE

}

void ScenePetClientPlayerHandler::AutoAllocatePetPoints(entt::entity player,const ::AutoAllocatePetPointsRequest* request,
	::AutoAllocatePetPointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	std::map<uint32_t, uint32_t> suggested;
	if (const auto err = PetSystem::AutoAllocate(player, request->pet_id(), suggested); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	response->set_pet_id(request->pet_id());
	for (const auto& [dimensionId, value] : suggested)
	{
		(*response->mutable_suggested())[dimensionId] = value;
	}
///<<< END WRITING YOUR CODE

}

void ScenePetClientPlayerHandler::RenamePet(entt::entity player,const ::RenamePetRequest* request,
	::RenamePetResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PetSystem::Rename(player, request->pet_id(), request->name()); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PetSystem::BuildList(player, *response->mutable_pets());
///<<< END WRITING YOUR CODE

}

void ScenePetClientPlayerHandler::NotifyPetListChanged(entt::entity player,const ::PetListChangedS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void ScenePetClientPlayerHandler::GmGrantPet(entt::entity player,const ::GmGrantPetRequest* request,
	::GmGrantPetResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// P0-a 已收口:gate 按消息号的 GM 白名单(gate_gm_client_messages.h,187 在表内)
	// 在非 dev/test 直接拒绝;这里是防绕开 gate 直连 scene 的第二道锁,判据 SCENE_RUN_MODE。
	// 见 docs/design/player-pet.md §8 与 cpp/nodes/gate/SECURITY.md §3。
	if (scene_gm_guard::RejectGmClientRpc("GmGrantPet"))
	{
		SetTip(kFeatureUnavailable);
		return;
	}
	uint64_t petId = 0;
	if (const auto err = PetSystem::GrantPet(player, request->pet_table_id(), petId); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	response->set_pet_id(petId);
	PetSystem::BuildList(player, *response->mutable_pets());
///<<< END WRITING YOUR CODE

}
