#include "player_tip.h"

#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "network/client_message_sender.h"
#include "thread_context/ecs_context.h"

void PlayerTipSystem::SendToPlayer(entt::entity player, uint32_t tipId, const StringVector &strParam)
{
	TipInfoMessage message;
	message.set_id(tipId);
	for (const auto &param : strParam)
	{
		*message.mutable_parameters()->Add() = param;
	}

	SendToPlayerClient(SceneClientPlayerCommonSendTipToClientMessageId, message, player);
}

void PlayerTipSystem::SendToPlayer(const Guid playerId, const uint32_t tipId, const StringVector &strParam)
{
	auto playerIt = tlsEcs.playerList.find(playerId);
	if (playerIt == tlsEcs.playerList.end())
	{
		return;
	}

	SendToPlayer(playerIt->second, tipId, strParam);
}
