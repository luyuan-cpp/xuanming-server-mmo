#include "client_message_sender.h"

#include <string>
#include <unordered_map>
#include <unordered_set>
#include <vector>

#include "broadcast_target_codec.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "muduo/base/Logging.h"
#include "network_utils.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"

void SendToPlayerClient(uint32_t messageId, const google::protobuf::Message &message, Guid playerId)
{
	SendToPlayerClient(messageId, message, tlsEcs.GetPlayer(playerId));
}

void SendToPlayerClient(uint32_t messageId, const google::protobuf::Message &message, entt::entity playerEntity)
{
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		LOG_WARN << "Invalid player entity.";
		return;
	}

	const auto *sessionSnapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
	if (!sessionSnapshot)
	{
		LOG_WARN << "Player node info not found for player entity " << entt::to_integral(playerEntity);
		return;
	}

	const auto resolvedGateEntity = ResolveLocalZoneGateEntity(sessionSnapshot->gate_session_id());
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
	if (!resolvedGateEntity)
	{
		LOG_ERROR << "Gate session not found for player with session ID " << sessionSnapshot->gate_session_id();
		return;
	}
	const auto gateEntity = *resolvedGateEntity;

	auto *gateRpcSession = gateNodeRegistry.try_get<RpcSession>(gateEntity);
	if (!gateRpcSession)
	{
		LOG_ERROR << "RpcSession not found for gate with session ID " << sessionSnapshot->gate_session_id();
		return;
	}

	// 身份栅栏(R13):玩家实体的 Guid 是权威身份,一并带给 gate 供写 socket 前比对。
	// try_get 而不是 get:极少数内部实体(测试桩 / 未完全构造的玩家)可能没有 Guid,
	// 拿不到时带 0 走兼容位,总比在推送路径上抛异常好。
	const auto *playerGuid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(playerEntity);
	SendToClientSession(messageId, message, *gateRpcSession, sessionSnapshot->gate_session_id(),
	                    playerGuid != nullptr ? playerGuid->value : kInvalidGuid);
}

void SendToClientSession(uint32_t messageId, const google::protobuf::Message &message,
                         RpcSession &gateRpcSession, SessionId clientSessionId, Guid targetPlayerId)
{
	NodeRouteMessageRequest request;
	const size_t serializedMessageSize = message.ByteSizeLong();
	request.mutable_message_content()->mutable_serialized_message()->resize(serializedMessageSize);
	if (!message.SerializePartialToArray(request.mutable_message_content()->mutable_serialized_message()->data(),
	                                     static_cast<int>(serializedMessageSize)))
	{
		LOG_ERROR << "Failed to serialize message.";
		return;
	}
	request.mutable_header()->set_session_id(clientSessionId);
	// kInvalidGuid 与 0 在 gate 侧是同一个意思:"发送方没有玩家身份可带",走兼容位放行。
	// 不能把 kInvalidGuid 原样塞进去 —— 它是 uint64 最大值,会被当成一个真实玩家号去比对,
	// 从而把本该放行的兼容位变成必然失配的丢弃。
	if (targetPlayerId != kInvalidGuid)
	{
		request.mutable_header()->set_target_player_id(targetPlayerId);
	}
	request.mutable_message_content()->set_message_id(messageId);
	gateRpcSession.SendRequest(GateSendMessageToPlayerMessageId, request);
}

namespace
{
// 广播目标:(session_id, player_id) 成对。编解码收在 broadcast_target_codec.h,
// 与 gate 侧共用同一份顺序契约(R13)—— 两边各写一遍必然某天错开一格,
// 而错开一格的表现是"每个人的身份都被比对成别人的"。
using BroadcastTargets = std::vector<broadcast_targets::Target>;

template <typename PlayerEntityContainer>
void BroadcastToPlayerClientsImpl(uint32_t messageId, const google::protobuf::Message &message,
                                  const PlayerEntityContainer &playerEntities)
{
	std::unordered_map<entt::entity, BroadcastTargets> targetsByGateEntity;
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);

	for (const auto playerEntity : playerEntities)
	{
		if (!tlsEcs.actorRegistry.valid(playerEntity))
		{
			LOG_ERROR << "Invalid player entity in playerList";
			continue;
		}

		const auto *sessionSnapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
		if (!sessionSnapshot)
		{
			const auto *playerGuid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(playerEntity);
			LOG_ERROR << "Player node info not found for player entity: " << (playerGuid ? playerGuid->value : 0);
			continue;
		}

		const auto resolvedGateEntity = ResolveLocalZoneGateEntity(sessionSnapshot->gate_session_id());
		if (!resolvedGateEntity)
		{
			LOG_ERROR << "Gate node not found for player session ID: " << sessionSnapshot->gate_session_id();
			continue;
		}
		const auto gateEntity = *resolvedGateEntity;

		// 身份栅栏(R13):带上玩家 Guid,gate 写每一条 socket 前比对。
		// 拿不到 Guid(极少数未完全构造的实体)时填 0 = 兼容位,gate 放行不校验;
		// 绝不能填 kInvalidGuid —— 那是 uint64 最大值,会被当成一个真实玩家号去比对。
		const auto *playerGuid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(playerEntity);
		targetsByGateEntity[gateEntity].push_back(broadcast_targets::Target{
		    sessionSnapshot->gate_session_id(),
		    (playerGuid != nullptr && playerGuid->value != kInvalidGuid) ? playerGuid->value : 0});
	}

	const std::string serializedMessage = message.SerializeAsString();

	for (auto &&[gateEntity, broadcastTargets] : targetsByGateEntity)
	{
		auto *gateRpcSession = gateNodeRegistry.try_get<RpcSession>(gateEntity);
		if (!gateRpcSession)
		{
			LOG_ERROR << "RpcSession not found for gate node";
			continue;
		}

		BroadcastToPlayersRequest request;
		request.mutable_message_content()->set_message_id(messageId);
		request.mutable_message_content()->set_serialized_message(serializedMessage);

		broadcast_targets::Encode(request, broadcastTargets);

		gateRpcSession->SendRequest(GateBroadcastToPlayersMessageId, request);
	}
}

template <typename PlayerEntityContainer>
void BroadcastToSceneClientsImpl(uint32_t messageId, const google::protobuf::Message &message, uint64_t sceneId,
                                 const PlayerEntityContainer &scenePlayerEntities)
{
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
	std::unordered_set<entt::entity> sceneGateEntities;

	for (const auto playerEntity : scenePlayerEntities)
	{
		if (!tlsEcs.actorRegistry.valid(playerEntity))
			continue;
		const auto *sessionSnapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
		if (!sessionSnapshot)
			continue;
		if (const auto resolvedGateEntity = ResolveLocalZoneGateEntity(sessionSnapshot->gate_session_id());
		    resolvedGateEntity)
			sceneGateEntities.insert(*resolvedGateEntity);
	}

	BroadcastToSceneRequest request;
	request.set_scene_id(sceneId);
	request.mutable_message_content()->set_message_id(messageId);
	request.mutable_message_content()->set_serialized_message(message.SerializeAsString());

	for (const auto gateEntity : sceneGateEntities)
	{
		auto *gateRpcSession = gateNodeRegistry.try_get<RpcSession>(gateEntity);
		if (!gateRpcSession)
			continue;
		gateRpcSession->SendRequest(GateBroadcastToSceneMessageId, request);
	}
}

} // namespace

void BroadcastToPlayerClients(uint32_t messageId, const google::protobuf::Message &message,
                              const EntityUnorderedSet &playerEntities)
{
	BroadcastToPlayerClientsImpl(messageId, message, playerEntities);
}

void BroadcastToPlayerClients(uint32_t messageId, const google::protobuf::Message &message,
                              const EntityVector &playerEntities)
{
	BroadcastToPlayerClientsImpl(messageId, message, playerEntities);
}

void BroadcastToSceneClients(uint32_t messageId, const google::protobuf::Message &message, uint64_t sceneId,
                             const EntityUnorderedSet &scenePlayerEntities)
{
	BroadcastToSceneClientsImpl(messageId, message, sceneId, scenePlayerEntities);
}

void BroadcastToSceneClients(uint32_t messageId, const google::protobuf::Message &message, uint64_t sceneId,
                             const EntityVector &scenePlayerEntities)
{
	BroadcastToSceneClientsImpl(messageId, message, sceneId, scenePlayerEntities);
}

void BroadcastToKnownGateClients(uint32_t messageId, const google::protobuf::Message &message)
{
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);

	BroadcastToAllRequest request;
	request.mutable_message_content()->set_message_id(messageId);
	request.mutable_message_content()->set_serialized_message(message.SerializeAsString());

	auto gateSessions = gateNodeRegistry.view<RpcSession>();
	for (const auto gateEntity : gateSessions)
	{
		auto &gateRpcSession = gateSessions.get<RpcSession>(gateEntity);
		gateRpcSession.SendRequest(GateBroadcastToAllMessageId, request);
	}
}
