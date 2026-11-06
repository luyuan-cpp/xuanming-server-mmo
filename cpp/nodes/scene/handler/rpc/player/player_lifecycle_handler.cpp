
#include "player_lifecycle_handler.h"

///<<< BEGIN WRITING YOUR CODE

#include "engine/core/utils/registry/actor_log.h"
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
	LOG_WARN << "GateLoginNotify: deprecated path reached "
	         << ActorLog(player)
	         << ", enter_gs_type=" << request->enter_gs_type();
	///<<< END WRITING YOUR CODE

}

void ScenePlayerHandler::ExitGame(entt::entity player,const ::GameNodeExitGameRequest* request,
	::google::protobuf::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE
	LOG_INFO << "ExitGame: " << ActorLog(player) << " reason=" << ExitGameReason_Name(request->reason());

	// TODO: If player has monthly card subscription, switch to AI control instead of exiting.
	// auto* subscriptionComp = tlsEcs.actorRegistry.try_get<MonthlyCardComp>(player);
	// if (subscriptionComp && subscriptionComp->IsActive()) {
	//     SwitchToAIControl(player);
	//     return;
	// }

	// gate 断线回调与客户端主动退出都走这里(gate client_session_handler.cpp 代发 ExitGame,原因见 ExitGameReason)。
	// 连接意外断开先按断线保留处理:角色留在原地等重连,到期或不能保留时才退出
	// (docs/design/player-connection-lost-hold.md)。主动登出、租约到期、旧版 gate 不带原因,一律立即退出。
	if (request->reason() == EXIT_GAME_REASON_DISCONNECTED)
	{
		PlayerLifecycleSystem::HandleClientConnectionLost(player);
		return;
	}
	PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kClientDisconnect);

	///<<< END WRITING YOUR CODE

}
