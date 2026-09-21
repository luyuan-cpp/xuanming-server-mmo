
#include "player_lifecycle_handler.h"

///<<< BEGIN WRITING YOUR CODE

#include "muduo/base/Logging.h"
#include "player/system/player_lifecycle.h"
///<<< END WRITING YOUR CODE

void ScenePlayerHandler::GateLoginNotify(entt::entity player,const ::GateLoginNotifyRequest* request,
	::google::protobuf::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE
	// Deprecated: Gate now sends PlayerEnterGameNode RPC directly (carries session_id,
	// enter_gs_type, scene_id). This handler is kept for backward compatibility but
	// should not be reached in normal operation.
	LOG_WARN << "GateLoginNotify: deprecated path reached for player "
	         << entt::to_integral(player)
	         << ", enter_gs_type=" << request->enter_gs_type();
	///<<< END WRITING YOUR CODE

}

void ScenePlayerHandler::ExitGame(entt::entity player,const ::GameNodeExitGameRequest* request,
	::google::protobuf::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE
	LOG_INFO << "ExitGame: player=" << entt::to_integral(player);

	// TODO: If player has monthly card subscription, switch to AI control instead of exiting.
	// auto* subscriptionComp = tlsEcs.actorRegistry.try_get<MonthlyCardComp>(player);
	// if (subscriptionComp && subscriptionComp->IsActive()) {
	//     SwitchToAIControl(player);
	//     return;
	// }

	// gate 断线回调与客户端主动退出都走这里(gate client_message_processor.cpp 断线时代发 ExitGame)。
	PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kClientDisconnect);

	///<<< END WRITING YOUR CODE

}
