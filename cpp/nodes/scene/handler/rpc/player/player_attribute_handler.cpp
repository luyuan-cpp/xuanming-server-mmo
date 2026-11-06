
#include "player_attribute_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <map>
#include <string>
#include <utility>

#include "player/system/client_request_ledger.h"
#include "player/system/player_attribute.h"
#include "player_gm_guard.h" // P0-a:GM 客户端指令的 scene 侧第二道锁
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
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
	response->clear_panel();
	if (const auto err = PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel()); err != kSuccess)
	{
		response->clear_panel();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::AllocateAttributePoints(entt::entity player,const ::AllocateAttributePointsRequest* request,
	::AllocateAttributePointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (request->pool_id() == 0 || request->allocated().empty())
	{
		response->mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	if (const auto err = PlayerAttributeSystem::Allocate(player, request->pool_id(), ToStdMap(request->allocated()));
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	if (const auto err = PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel()); err != kSuccess)
	{
		response->clear_panel();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::ResetAttributePoints(entt::entity player,const ::ResetAttributePointsRequest* request,
	::ResetAttributePointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAttributeSystem::Reset(player, request->pool_id()); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	if (const auto err = PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel()); err != kSuccess)
	{
		response->clear_panel();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::AutoAllocateAttributePoints(entt::entity player,const ::AutoAllocateAttributePointsRequest* request,
	::AutoAllocateAttributePointsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	std::map<uint32_t, uint32_t> suggested;
	if (const auto err = PlayerAttributeSystem::AutoAllocate(player, request->pool_id(), suggested); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
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
	// 客户端幂等键(docs/design/client-request-idempotency.md):建方案要扣金币,没有去重时连点或重发会多建一个、多扣一次。
	// 查账排在 CreateScheme 的一切校验之前:首次建成之后方案数可能已到上限、余额可能不够了,重放再判会被拒掉。
	const uint64_t clientRequestId = request->client_request_id();
	std::string fingerprint;
	ClientRequestResult first;
	auto verdict = ClientRequestVerdict::kNew;
	if (clientRequestId != 0)
	{
		fingerprint = ClientRequestFingerprint(*request);
		verdict = client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kCreateAttributeScheme,
											  fingerprint, first);
	}
	if (verdict == ClientRequestVerdict::kMismatch)
	{
		// 同一个键换了 name:客户端把旧键用在了新操作上,拒绝且不执行。
		response->mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	if (verdict == ClientRequestVerdict::kUnavailable)
	{
		// 这名玩家的幂等键账本缺失或已判坏:分不清是不是重发,不执行(fail-closed)。
		response->mutable_error_message()->set_id(kServiceUnavailable);
		return;
	}
	uint32_t schemeId = 0;
	if (verdict == ClientRequestVerdict::kReplay)
	{
		// 首次已经建好:不再建、不再扣,回首次的方案号与当前面板。
		schemeId = first.createdSchemeId;
		response->set_replayed(true);
	}
	else
	{
		if (const auto err = PlayerAttributeSystem::CreateScheme(player, request->name(), schemeId); err != kSuccess)
		{
			response->mutable_error_message()->set_id(err);
			return;
		}
		// 建成了才记键。下面面板组不出来也照样算建成:方案已经建了、钱已经扣了。
		ClientRequestResult result;
		result.createdSchemeId = schemeId;
		client_request_dedup::Remember(player, clientRequestId, ClientRequestKind::kCreateAttributeScheme,
									   std::move(fingerprint), result);
	}
	response->set_scheme_id(schemeId);
	if (const auto err = PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel()); err != kSuccess)
	{
		response->clear_panel();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::SwitchAttributeScheme(entt::entity player,const ::SwitchAttributeSchemeRequest* request,
	::SwitchAttributeSchemeResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAttributeSystem::SwitchScheme(player, request->scheme_id()); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	if (const auto err = PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel()); err != kSuccess)
	{
		response->clear_panel();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAttributeClientPlayerHandler::RenameAttributeScheme(entt::entity player,const ::RenameAttributeSchemeRequest* request,
	::RenameAttributeSchemeResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAttributeSystem::RenameScheme(player, request->scheme_id(), request->name()); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	if (const auto err = PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel()); err != kSuccess)
	{
		response->clear_panel();
		response->mutable_error_message()->set_id(err);
	}
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
	// P0-a:GM 指令只在 dev/test 开放。第一道锁在 gate(message_id 175 的白名单),
	// 这里是防绕开 gate 直连 scene RPC 端口的第二道锁,判据 SCENE_RUN_MODE。
	if (scene_gm_guard::RejectGmClientRpc("GmSetPlayerLevel"))
	{
		response->mutable_error_message()->set_id(kFeatureUnavailable);
		return;
	}
	if (const auto err = PlayerAttributeSystem::GmSetLevel(player, request->level()); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	if (const auto err = PlayerAttributeSystem::BuildPanel(player, *response->mutable_panel()); err != kSuccess)
	{
		response->clear_panel();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}
