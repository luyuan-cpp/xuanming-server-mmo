
#include "player_pet_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <map>

#include <muduo/base/Logging.h>

#include "player/system/player_pet.h"
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
	// ⚠ 今天这条 RPC **没有任何鉴权**:客户端直接发就能给自己发宝宝。
	// 与既有的 GmSetPlayerLevel / GmAddCurrency 同状态 —— gate 侧目前不存在
	// 「按消息号的 GM 白名单」(gate_security.h 管的是带签名的 GM admin RPC,不覆盖这条)。
	// 开发期可用,上线前必须与那两条一起统一收口,见 docs/design/player-pet.md §8。
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
