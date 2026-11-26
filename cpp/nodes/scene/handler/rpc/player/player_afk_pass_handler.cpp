
#include "player_afk_pass_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/time/system/time.h"
#include "player_gm_guard.h" // GM 客户端指令的 scene 侧第二道锁
#include "services/scene/player/system/afk_pass.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneAfkPassClientPlayerHandler::GetAfkPass(entt::entity player,const ::GetAfkPassRequest* request,
	::GetAfkPassResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 纯读取:到期、服务器时间、售价与余额。规则全在 AfkPassSystem,这里只取墙钟(docs/design/afk-monthly-card.md §6)。
	AfkPassSystem::FillInfo(player, TimeSystem::NowSecondsUTC(), *response->mutable_info());
///<<< END WRITING YOUR CODE

}

void SceneAfkPassClientPlayerHandler::BuyAfkPass(entt::entity player,const ::BuyAfkPassRequest* request,
	::BuyAfkPassResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 幂等键、报价、冻结、叠加上限、余额与扣钱 / 加天数都在 AfkPassSystem::Buy(设计 §4.1);成功时它已经把开通之后的
	// 状态写进 response,失败时 response 里只有这里写的错误码。
	if (const auto err = AfkPassSystem::Buy(player, *request, TimeSystem::NowSecondsUTC(), *response); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAfkPassClientPlayerHandler::GmSetAfkPass(entt::entity player,const ::GmSetAfkPassRequest* request,
	::GmSetAfkPassResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 这是"prod 关掉"而不是鉴权:gate 按消息号的 GM 清单(gate_gm_client_messages.h)在非 dev/test 直接拒绝;
	// 这里是防绕开 gate 直连 scene 的第二道锁,判据 SCENE_RUN_MODE(默认 prod = 拒绝)。
	if (scene_gm_guard::RejectGmClientRpc("GmSetAfkPass"))
	{
		response->mutable_error_message()->set_id(kFeatureUnavailable);
		return;
	}
	if (const auto err = AfkPassSystem::GmSet(player, request->remaining_seconds(), TimeSystem::NowSecondsUTC(),
											  *response->mutable_info());
		err != kSuccess)
	{
		response->clear_info();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAfkPassClientPlayerHandler::SetAfkPassSettings(entt::entity player,const ::SetAfkPassSettingsRequest* request,
	::SetAfkPassSettingsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = AfkPassSystem::SetSettings(player, request->auto_return_city_on_team_end(),
													TimeSystem::NowSecondsUTC(), *response->mutable_info());
		err != kSuccess)
	{
		response->clear_info();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAfkPassClientPlayerHandler::AckHostedRecap(entt::entity player,const ::AckHostedRecapRequest* request,
	::AckHostedRecapResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = AfkPassSystem::AckHostedRecap(player, request->started_at(), TimeSystem::NowSecondsUTC(),
													   *response->mutable_info());
		err != kSuccess)
	{
		response->clear_info();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}
