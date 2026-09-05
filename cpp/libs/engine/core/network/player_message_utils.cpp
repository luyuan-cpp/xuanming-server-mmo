#include "player_message_utils.h"

#include "proto/common/component/player_network_comp.pb.h"
#include "thread_context/redis_manager.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "muduo/base/Logging.h"
#include "network_utils.h"
#include "network/rpc_session.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "network/node_utils.h"
#include "node/system/node/node_util.h"
#include <rpc/service_metadata/scene_service_metadata.h>
#include "thread_context/node_context_manager.h"
#include "utils/random/random.h"
#include <thread_context/ecs_context.h>

void SendMessageToClientViaGate(uint32_t messageId, const google::protobuf::Message &message, Guid playerId)
{
	SendMessageToClientViaGate(messageId, message, tlsEcs.GetPlayer(playerId));
}

void SendMessageToClientViaGate(uint32_t messageId, const google::protobuf::Message &message, entt::entity playerEntity)
{
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		LOG_WARN << "Invalid player entity.";
		return;
	}

	const auto *playerSessionSnapshotPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
	if (!playerSessionSnapshotPB)
	{
		LOG_WARN << "Player node info not found for player entity " << entt::to_integral(playerEntity);
		return;
	}

	auto gateEntityOpt = ResolveLocalZoneGateEntity(playerSessionSnapshotPB->gate_session_id());
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
	if (!gateEntityOpt)
	{
		LOG_ERROR << "Gate session not found for player with session ID " << playerSessionSnapshotPB->gate_session_id();
		return;
	}
	entt::entity gateSessionId = *gateEntityOpt;

	const auto gateSessionPtr = gateNodeRegistry.try_get<RpcSession>(gateSessionId);
	if (!gateSessionPtr)
	{
		LOG_ERROR << "RpcSession not found for gate with session ID " << playerSessionSnapshotPB->gate_session_id();
		return;
	}

	SendMessageToClientViaGate(messageId, message, *gateSessionPtr, playerSessionSnapshotPB->gate_session_id());
}

void SendMessageToClientViaGate(uint32_t messageId, const google::protobuf::Message &message, RpcSession &gate, SessionId sessionId)
{
	NodeRouteMessageRequest request;
	const size_t byteSize = message.ByteSizeLong();
	request.mutable_message_content()->mutable_serialized_message()->resize(byteSize);
	if (!message.SerializePartialToArray(request.mutable_message_content()->mutable_serialized_message()->data(), static_cast<int>(byteSize)))
	{
		LOG_ERROR << "Failed to serialize message.";
		return;
	}
	request.mutable_header()->set_session_id(sessionId);
	request.mutable_message_content()->set_message_id(messageId);
	gate.SendRequest(GateSendMessageToPlayerMessageId, request);
}

void SendMessageToGateById(uint32_t messageId, const google::protobuf::Message &message, NodeId gateNodeId)
{
	// Post uuid-refactor entt::entity != node_id, so resolve the entity via
	// (self_zone, node_id). This path is only used by same-zone callers.
	auto gateEntityOpt = NodeUtils::FindNodeEntityByZoneAndNodeId(
		eNodeType::GateNodeService, GetZoneId(), gateNodeId);
	if (!gateEntityOpt)
	{
		LOG_ERROR << "Gate not found for NodeId -> " << gateNodeId;
		return;
	}
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
	const auto gateSessionPtr = gateNodeRegistry.try_get<RpcSession>(*gateEntityOpt);
	if (!gateSessionPtr)
	{
		LOG_ERROR << "RpcSession not found for Gate NodeId -> " << gateNodeId;
		return;
	}

	gateSessionPtr->SendRequest(messageId, message);
}

using BroadCastSessionIdList = std::unordered_set<SessionId>;

// Choose bitmap or list encoding based on session count and density.
// Bitmap: base(uint32) + N/8 bytes.  List: ~3 bytes per varint (session IDs > 16384).
// Bitmap wins when density > ~4% and count >= threshold.
static void EncodeBroadcastSessionList(BroadcastToPlayersRequest &request,
									   const BroadCastSessionIdList &sessionIdList)
{
	constexpr size_t kBitmapThreshold = 32;

	if (sessionIdList.size() >= kBitmapThreshold)
	{
		uint32_t minId = UINT32_MAX;
		uint32_t maxId = 0;
		for (const auto sessionId : sessionIdList)
		{
			if (sessionId < minId)
				minId = sessionId;
			if (sessionId > maxId)
				maxId = sessionId;
		}

		const uint32_t span = maxId - minId + 1;
		const size_t bitmapBytes = (span + 7) / 8;

		// Use bitmap when it is smaller than varint list
		if (bitmapBytes + 6 < sessionIdList.size() * 3)
		{
			request.set_session_bitmap_base(minId);
			std::string bitmap(bitmapBytes, '\0');
			for (const auto sessionId : sessionIdList)
			{
				const uint32_t offset = sessionId - minId;
				bitmap[offset / 8] |= static_cast<char>(1 << (offset % 8));
			}
			request.set_session_bitmap(std::move(bitmap));
			return;
		}
	}

	for (const auto sessionId : sessionIdList)
	{
		request.mutable_session_list()->Add(sessionId);
	}
}

template <typename PlayerContainer>
void InternalBroadcast(uint32_t messageId, const google::protobuf::Message &message, const PlayerContainer &playerList)
{
	std::unordered_map<entt::entity, BroadCastSessionIdList> gateList;
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);

	for (auto &player : playerList)
	{
		if (!tlsEcs.actorRegistry.valid(player))
		{
			LOG_ERROR << "Invalid player entity in playerList";
			continue;
		}

		const auto *playerSessionSnapshotPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
		if (!playerSessionSnapshotPB)
		{
			const auto* playerGuid = tlsEcs.actorRegistry.try_get<Guid>(player);
			LOG_ERROR << "Player node info not found for player entity: " << (playerGuid ? *playerGuid : 0);
			continue;
		}

		auto gateEntityOpt = ResolveLocalZoneGateEntity(playerSessionSnapshotPB->gate_session_id());
		if (!gateEntityOpt)
		{
			LOG_ERROR << "Gate node not found for player session ID: " << playerSessionSnapshotPB->gate_session_id();
			continue;
		}
		entt::entity gateNodeId = *gateEntityOpt;

		gateList[gateNodeId].emplace(playerSessionSnapshotPB->gate_session_id());
	}

	const std::string serializedMessage = message.SerializeAsString();

	for (auto &&[gateNodeId, sessionIdList] : gateList)
	{
		const auto gateNodeSession = gateNodeRegistry.try_get<RpcSession>(gateNodeId);
		if (!gateNodeSession)
		{
			LOG_ERROR << "RpcSession not found for gate node";
			continue;
		}

		BroadcastToPlayersRequest request;
		request.mutable_message_content()->set_message_id(messageId);
		request.mutable_message_content()->set_serialized_message(serializedMessage);

		EncodeBroadcastSessionList(request, sessionIdList);

		gateNodeSession->SendRequest(GateBroadcastToPlayersMessageId, request);
	}
}

void BroadcastMessageToPlayers(uint32_t messageId, const google::protobuf::Message &message, const EntityUnorderedSet &playerList)
{
	InternalBroadcast(messageId, message, playerList);
}

void BroadcastMessageToPlayers(uint32_t messageId, const google::protobuf::Message &message, const EntityVector &playerList)
{
	InternalBroadcast(messageId, message, playerList);
}

template <typename PlayerContainer>
void InternalBroadcastToScene(uint32_t messageId, const google::protobuf::Message &message,
							  uint64_t sceneId, const PlayerContainer &scenePlayerList)
{
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
	std::unordered_set<entt::entity> gateNodes;

	for (auto &player : scenePlayerList)
	{
		if (!tlsEcs.actorRegistry.valid(player))
			continue;
		const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
		if (!snapshot)
			continue;
		if (auto gateEntityOpt = ResolveLocalZoneGateEntity(snapshot->gate_session_id()); gateEntityOpt)
			gateNodes.insert(*gateEntityOpt);
	}

	BroadcastToSceneRequest request;
	request.set_scene_id(sceneId);
	request.mutable_message_content()->set_message_id(messageId);
	request.mutable_message_content()->set_serialized_message(message.SerializeAsString());

	for (auto gateNodeId : gateNodes)
	{
		const auto gateSession = gateNodeRegistry.try_get<RpcSession>(gateNodeId);
		if (!gateSession)
			continue;
		gateSession->SendRequest(GateBroadcastToSceneMessageId, request);
	}
}

void BroadcastMessageToScene(uint32_t messageId, const google::protobuf::Message &message,
							 uint64_t sceneId, const EntityUnorderedSet &scenePlayerList)
{
	InternalBroadcastToScene(messageId, message, sceneId, scenePlayerList);
}

void BroadcastMessageToScene(uint32_t messageId, const google::protobuf::Message &message,
							 uint64_t sceneId, const EntityVector &scenePlayerList)
{
	InternalBroadcastToScene(messageId, message, sceneId, scenePlayerList);
}

void BroadcastMessageToAll(uint32_t messageId, const google::protobuf::Message &message)
{
	auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);

	BroadcastToAllRequest request;
	request.mutable_message_content()->set_message_id(messageId);
	request.mutable_message_content()->set_serialized_message(message.SerializeAsString());

	auto view = gateNodeRegistry.view<RpcSession>();
	for (auto entity : view)
	{
		auto &gateSession = view.get<RpcSession>(entity);
		gateSession.SendRequest(GateBroadcastToAllMessageId, request);
	}
}

void SendMessageToPlayerOnGrpcNode(uint32_t messageId, const google::protobuf::Message &message, Guid playerId)
{
	SendMessageToPlayerOnGrpcNode(messageId, message, tlsEcs.GetPlayer(playerId));
}

inline entt::entity PickRandomNodeEntity(uint32_t nodeType)
{
	auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	auto view = registry.view<NodeInfo>();
	std::vector<entt::entity> candidates;
	const auto zoneId = GetNodeInfo().zone_id();
	// 全局池类型(match / battle)不比对 zone(设计文档 cross-zone-matchmaking.md D11)。
	const bool globalPool = NodeUtils::IsGlobalPoolNodeType(nodeType);
	for (auto entity : view)
	{
		const auto &node = view.get<NodeInfo>(entity);
		if (globalPool || node.zone_id() == zoneId)
		{
			candidates.push_back(entity);
		}
	}
	if (candidates.empty())
	{
		LOG_ERROR << "No available node for type: " << nodeType;
		return entt::null;
	}
	return candidates[tlsRandom.Rand<size_t>(0, candidates.size() - 1)];
}

void SendMessageToPlayerOnGrpcNode(uint32_t messageId, const google::protobuf::Message &message, entt::entity playerEntity)
{
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		LOG_ERROR << "Player entity is not valid";
		return;
	}

	if (messageId >= gRpcMethodRegistry.size())
	{
		LOG_WARN << "Message dropped, messageId out of range: " << messageId
				 << " (max: " << gRpcMethodRegistry.size() << ")";
		return;
	}

	auto &rpcHandlerMeta = gRpcMethodRegistry[messageId];

	const auto *playerSessionSnapshotPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
	if (!playerSessionSnapshotPB)
	{
		LOG_ERROR << "Player node info not found for player entity";
		return;
	}

	SessionDetails sessionDetails;
	sessionDetails.set_session_id(playerSessionSnapshotPB->gate_session_id());
	sessionDetails.set_player_id(tlsEcs.actorRegistry.get<Guid>(playerEntity));

	if (!rpcHandlerMeta.sender)
	{
		LOG_ERROR << "Message sender not found for message ID: " << messageId;
		return;
	}

	auto node = PickRandomNodeEntity(rpcHandlerMeta.targetNodeType);
	if (node == entt::null || !tlsNodeContextManager.GetRegistry(rpcHandlerMeta.targetNodeType).valid(node))
	{
		LOG_ERROR << "Node not found for type: " << rpcHandlerMeta.targetNodeType;
		return;
	}
	rpcHandlerMeta.sender(tlsNodeContextManager.GetRegistry(rpcHandlerMeta.targetNodeType),
						  node,
						  *rpcHandlerMeta.requestProto,
						  {kSessionBinMetaKey},
						  SerializeSessionDetails(sessionDetails));
}

void SendMessageToPlayerOnSceneNode(uint32_t messageId,
									const google::protobuf::Message &message,
									entt::entity playerEntity)
{
	SendMessageToPlayerOnNode(
		/* wrappedMessageId */ SceneSendMessageToPlayerMessageId,
		/* nodeType */ eNodeType::SceneNodeService,
		/* messageId */ messageId,
		message,
		playerEntity);
}

void SendMessageToPlayerOnSceneNode(uint32_t messageId,
									const google::protobuf::Message &message,
									Guid playerId)
{
	SendMessageToPlayerOnSceneNode(messageId, message, tlsEcs.GetPlayer(playerId));
}

void SendMessageToPlayerOnNode(uint32_t wrappedMessageId,
							   uint32_t nodeType,
							   uint32_t messageId,
							   const google::protobuf::Message &message,
							   Guid playerId)
{
	SendMessageToPlayerOnNode(wrappedMessageId, nodeType, messageId, message, tlsEcs.GetPlayer(playerId));
}

namespace {

// Validate player and return its session snapshot. Logs and returns null on failure.
const PlayerSessionSnapshotComp* ResolvePlayerSessionSnapshot(entt::entity playerEntity, const char* context)
{
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		LOG_ERROR << context << ": invalid player entity -> " << entt::to_integral(playerEntity);
		return nullptr;
	}
	const auto* snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
	if (!snapshot)
	{
		LOG_ERROR << context << ": player session snapshot not found -> " << entt::to_integral(playerEntity);
	}
	return snapshot;
}

// Resolve the RpcSession of the node a player is connected to for the given node type.
// Logs and returns null on any failure.
RpcSession* ResolvePlayerNodeSession(const PlayerSessionSnapshotComp& snapshot, uint32_t nodeType,
	entt::entity playerEntity, const char* context)
{
	const auto& nodeIdMap = snapshot.node_id();
	const auto it = nodeIdMap.find(nodeType);
	if (it == nodeIdMap.end())
	{
		LOG_ERROR << context << ": node type not found in player session snapshot: " << nodeType
			<< ", player entity: " << entt::to_integral(playerEntity);
		return nullptr;
	}

	auto& registry = tlsNodeContextManager.GetRegistry(nodeType);
	const entt::entity nodeEntity{ it->second };
	if (!registry.valid(nodeEntity))
	{
		LOG_ERROR << context << ": node entity invalid for player -> " << entt::to_integral(playerEntity)
			<< ", node id: " << it->second;
		return nullptr;
	}

	auto* session = registry.try_get<RpcSession>(nodeEntity);
	if (!session)
	{
		LOG_ERROR << context << ": RpcSession not found for node -> " << it->second;
	}
	return session;
}

}  // namespace

void SendMessageToPlayerOnNode(uint32_t wrappedMessageId,
							   uint32_t nodeType,
							   uint32_t messageId,
							   const google::protobuf::Message &message,
							   entt::entity playerEntity)
{
	const auto *playerSessionSnapshotPB = ResolvePlayerSessionSnapshot(playerEntity, "SendMessageToPlayerOnNode");
	if (!playerSessionSnapshotPB)
	{
		return;
	}

	auto *session = ResolvePlayerNodeSession(*playerSessionSnapshotPB, nodeType, playerEntity, "SendMessageToPlayerOnNode");
	if (!session)
	{
		return;
	}

	NodeRouteMessageRequest request;
	request.mutable_message_content()->set_message_id(messageId);
	if (!message.SerializeToString(request.mutable_message_content()->mutable_serialized_message()))
	{
		LOG_ERROR << "Failed to serialize message.";
		return;
	}
	request.mutable_header()->set_session_id(playerSessionSnapshotPB->gate_session_id());

	session->SendRequest(wrappedMessageId, request);
}

void CallMethodOnPlayerNode(
	uint32_t remoteMethodId,
	uint32_t nodeType,
	uint32_t messageId,
	const google::protobuf::Message &message,
	Guid playerId)
{
	CallMethodOnPlayerNode(remoteMethodId, nodeType, messageId, message, tlsEcs.GetPlayer(playerId));
}

void CallMethodOnPlayerNode(
	uint32_t remoteMethodId,
	uint32_t nodeType,
	uint32_t messageId,
	const google::protobuf::Message &message,
	entt::entity player)
{
	const auto *playerSessionSnapshotPB = ResolvePlayerSessionSnapshot(player, "CallMethodOnPlayerNode");
	if (!playerSessionSnapshotPB)
	{
		return;
	}

	auto *session = ResolvePlayerNodeSession(*playerSessionSnapshotPB, nodeType, player, "CallMethodOnPlayerNode");
	if (!session)
	{
		return;
	}

	NodeRouteMessageRequest request;
	const size_t byteSize = message.ByteSizeLong();
	request.mutable_message_content()->mutable_serialized_message()->resize(byteSize);

	if (!message.SerializePartialToArray(request.mutable_message_content()->mutable_serialized_message()->data(), static_cast<int>(byteSize)))
	{
		LOG_ERROR << "Failed to serialize message.";
		return;
	}

	request.mutable_message_content()->set_message_id(messageId);
	request.mutable_header()->set_session_id(playerSessionSnapshotPB->gate_session_id());

	session->CallRemoteMethod(remoteMethodId, request);
}
