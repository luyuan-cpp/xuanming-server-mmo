#include "player_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
#include "player/system/player_skill.h"
#include "player/system/player_attribute.h"
#include <muduo/base/Logging.h>
#include "proto/common/component/player_login_comp.pb.h"
///<<< END WRITING YOUR CODE
void PlayerEventHandler::Register()
{
    tlsEcs.dispatcher.sink<RegisterPlayerEvent>().connect<&PlayerEventHandler::RegisterPlayerEventHandler>();
    tlsEcs.dispatcher.sink<PlayerUpgradeEvent>().connect<&PlayerEventHandler::PlayerUpgradeEventHandler>();
    tlsEcs.dispatcher.sink<InitializePlayerCompsEvent>().connect<&PlayerEventHandler::InitializePlayerCompsEventHandler>();
    tlsEcs.dispatcher.sink<PlayerLoginEvent>().connect<&PlayerEventHandler::PlayerLoginEventHandler>();
}

void PlayerEventHandler::UnRegister()
{
    tlsEcs.dispatcher.sink<RegisterPlayerEvent>().disconnect<&PlayerEventHandler::RegisterPlayerEventHandler>();
    tlsEcs.dispatcher.sink<PlayerUpgradeEvent>().disconnect<&PlayerEventHandler::PlayerUpgradeEventHandler>();
    tlsEcs.dispatcher.sink<InitializePlayerCompsEvent>().disconnect<&PlayerEventHandler::InitializePlayerCompsEventHandler>();
    tlsEcs.dispatcher.sink<PlayerLoginEvent>().disconnect<&PlayerEventHandler::PlayerLoginEventHandler>();
}
void PlayerEventHandler::RegisterPlayerEventHandler(const RegisterPlayerEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
	auto player = entt::to_entity(event.actor_entity());

	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "Player Not Found :" << event.actor_entity();
		return;
	}

	PlayerSkillSystem::RegisterPlayer(player);
///<<< END WRITING YOUR CODE
}
void PlayerEventHandler::PlayerUpgradeEventHandler(const PlayerUpgradeEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
	auto player = entt::to_entity(event.actor_entity());
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "Player Not Found :" << event.actor_entity();
		return;
	}
	// 升级发点:总点数按等级换算,不存量;重算二级属性并推面板(客户端整体覆盖)
	PlayerAttributeSystem::Recalculate(player);
	PlayerAttributeSystem::PushPanel(player);
///<<< END WRITING YOUR CODE
}
void PlayerEventHandler::InitializePlayerCompsEventHandler(const InitializePlayerCompsEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
	auto player = entt::to_entity(event.actor_entity());

	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "Player Not Found :" << event.actor_entity();
		return;
	}

///<<< END WRITING YOUR CODE
}
void PlayerEventHandler::PlayerLoginEventHandler(const PlayerLoginEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
	auto player = entt::to_entity(event.actor_entity());

	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "Player Not Found :" << event.actor_entity();
		return;
	}

	const auto enterType = static_cast<EnterGsType>(event.enter_gs_type());
	LOG_INFO << "PlayerLoginEvent: player=" << event.actor_entity()
			 << " enter_gs_type=" << EnterGsType_Name(enterType);

	switch (enterType)
	{
	case LOGIN_FIRST:
		// TODO: full data sync to client, daily reset check, welcome message
		break;
	case LOGIN_RECONNECT:
		// TODO: delta state sync, resume interrupted actions
		break;
	case LOGIN_REPLACE:
		// TODO: cleanup old transient state (trade, matchmaking), re-sync to new client
		break;
	default:
		LOG_WARN << "PlayerLoginEvent: unexpected enter_gs_type=" << event.enter_gs_type();
		break;
	}
///<<< END WRITING YOUR CODE
}
