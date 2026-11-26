
#include "player_spirit_shop_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/time/system/time.h"
#include "services/scene/player/system/player_spirit_shop.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneSpiritShopClientPlayerHandler::GetSpiritShopInfo(entt::entity player,const ::GetSpiritShopInfoRequest* request,
	::GetSpiritShopInfoResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	(void)request;
	if (const auto err = PlayerSpiritShopSystem::GetInfo(player, TimeSystem::NowMillisecondsUTC(), *response->mutable_info());
		err != kSuccess)
	{
		response->clear_info();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneSpiritShopClientPlayerHandler::BuySpiritShopGoods(entt::entity player,const ::BuySpiritShopGoodsRequest* request,
	::BuySpiritShopGoodsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerSpiritShopSystem::Buy(player, *request, TimeSystem::NowMillisecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}
