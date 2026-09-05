
#include "player_attribute_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <map>

#include <muduo/base/Logging.h>

#include "player/system/player_attribute.h"
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

void SceneAttributeClientPlayerHandler::GetAttributePanel(entt::entity player,const ::GetAttributePanelRequest* request,
	::GetAttributePanelResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel());
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::AllocateAttributePoints(entt::entity player,const ::AllocateAttributePointsRequest* request,
	::AllocateAttributePointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (request->pool_id() == 0 || request->allocated().empty())
	{
		SetTip(kInvalidParameter);
		return;
	}
	if (const auto err = PlayerAttributeSystem::Allocate(player, request->pool_id(), ToStdMap(request->allocated()));
		err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel());
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::ResetAttributePoints(entt::entity player,const ::ResetAttributePointsRequest* request,
	::ResetAttributePointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAttributeSystem::Reset(player, request->pool_id()); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel());
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::AutoAllocateAttributePoints(entt::entity player,const ::AutoAllocateAttributePointsRequest* request,
	::AutoAllocateAttributePointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	std::map<uint32_t, uint32_t> suggested;
	if (const auto err = PlayerAttributeSystem::AutoAllocate(player, request->pool_id(), suggested); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	response->set_pool_id(request->pool_id());
	for (const auto& [dimensionId, value] : suggested)
	{
		(*response->mutable_suggested())[dimensionId] = value;
	}
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::CreateAttributeScheme(entt::entity player,const ::CreateAttributeSchemeRequest* request,
	::CreateAttributeSchemeResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	uint32_t schemeId = 0;
	if (const auto err = PlayerAttributeSystem::CreateScheme(player, request->name(), schemeId); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	response->set_scheme_id(schemeId);
	PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel());
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::SwitchAttributeScheme(entt::entity player,const ::SwitchAttributeSchemeRequest* request,
	::SwitchAttributeSchemeResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAttributeSystem::SwitchScheme(player, request->scheme_id()); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel());
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::RenameAttributeScheme(entt::entity player,const ::RenameAttributeSchemeRequest* request,
	::RenameAttributeSchemeResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAttributeSystem::RenameScheme(player, request->scheme_id(), request->name()); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel());
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::NotifyAttributePanelChanged(entt::entity player,const ::AttributePanelChangedS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::GmSetPlayerLevel(entt::entity player,const ::GmSetPlayerLevelRequest* request,
	::GmSetPlayerLevelResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 与 GmAddCurrency 同口径:开发期 GM 直连;上线前经 gate GM 鉴权白名单(gate_security.h)收口
	if (const auto err = PlayerAttributeSystem::GmSetLevel(player, request->level()); err != kSuccess)
	{
		SetTip(err);
		return;
	}
	PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel());
///<<< END WRITING YOUR CODE

}
