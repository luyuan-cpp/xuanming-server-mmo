#include "node_message_utils.h"
#include "network/rpc_session.h"
#include <network/rpc_client.h>
#include <optional>
#include "thread_context/redis_manager.h"
#include "node/system/node/node_util.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/base/message.pb.h"
#include "thread_context/node_context_manager.h"

namespace {

// node_id 是业务节点号,不是 entt 实体整数:uuid 主键重构之后
// (node_connector.cpp 改用 registry.create() 并按 uuid 建索引)两者不再相等,
// 裸 `entt::entity{nodeId}` 要么撞不上有效槽位,要么撞上一个无关的节点实体并把消息
// 送错地方。全仓统一走 FindNodeEntityByNodeId
// (docs/design/routing-identity-audit-20260908.md R03 的同类残留)。
std::optional<entt::entity> ResolveNodeEntity(NodeId nodeId, uint32_t nodeType, const char* context) {
	auto entityOpt = NodeUtils::FindNodeEntityByNodeId(nodeType, nodeId);
	if (!entityOpt) {
		LOG_ERROR << context << ": node not found: " << nodeId << " of type: " << static_cast<int>(nodeType);
	}
	return entityOpt;
}

// Resolve a node's RpcSession in the per-type registry. Logs and returns null on failure.
RpcSession* ResolveNodeSession(NodeId nodeId, uint32_t nodeType, const char* context) {
	const auto entityOpt = ResolveNodeEntity(nodeId, nodeType, context);
	if (!entityOpt) {
		return nullptr;
	}
	auto& registry = tlsNodeContextManager.GetRegistry(nodeType);
	auto* session = registry.try_get<RpcSession>(*entityOpt);
	if (!session) {
		LOG_ERROR << context << ": RpcSession not found for node: " << nodeId;
	}
	return session;
}

// Resolve a node's RpcClient in the per-type registry. Logs and returns null on failure.
RpcClient* ResolveNodeClient(NodeId nodeId, uint32_t nodeType, const char* context) {
	const auto entityOpt = ResolveNodeEntity(nodeId, nodeType, context);
	if (!entityOpt) {
		return nullptr;
	}
	auto& registry = tlsNodeContextManager.GetRegistry(nodeType);
	auto* clientPtr = registry.try_get<RpcClientPtr>(*entityOpt);
	if (!clientPtr) {
		LOG_ERROR << context << ": RpcClientPtr not found for node: " << nodeId;
		return nullptr;
	}
	return clientPtr->get();
}

// Look up the player's session snapshot. Logs and returns null on failure.
const PlayerSessionSnapshotComp* ResolvePlayerSession(entt::entity playerEntity, const char* context) {
	if (!tlsEcs.actorRegistry.valid(playerEntity)) {
		LOG_ERROR << context << ": invalid player entity";
		return nullptr;
	}
	const auto* sessionPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
	if (!sessionPB) {
		LOG_ERROR << context << ": player session snapshot not found";
	}
	return sessionPB;
}

// Find the node id the player is connected to for a given node type.
// Logs and returns false if the player has no node of that type.
bool FindPlayerNodeId(const PlayerSessionSnapshotComp& sessionPB, uint32_t nodeType,
	entt::entity playerEntity, NodeId& outNodeId) {
	const auto& nodeIdMap = sessionPB.node_id();
	const auto it = nodeIdMap.find(nodeType);
	if (it == nodeIdMap.end()) {
		LOG_ERROR << "Node type not found in player session snapshot: " << nodeType
			<< ", player entity: " << entt::to_integral(playerEntity);
		return false;
	}
	outNodeId = it->second;
	return true;
}

// Wrap an inner message into a gate-routed request. Logs and returns false on serialize failure.
bool BuildRoutedRequest(uint32_t messageId, const google::protobuf::Message& message,
	uint32_t gateSessionId, NodeRouteMessageRequest& request) {
	auto* content = request.mutable_message_content();
	content->set_message_id(messageId);
	if (!message.SerializeToString(content->mutable_serialized_message())) {
		LOG_ERROR << "Failed to serialize routed message";
		return false;
	}
	request.mutable_header()->set_session_id(gateSessionId);
	return true;
}

}  // namespace


void SendMessageToSessionNode(uint32_t messageId, const google::protobuf::Message& message, NodeId nodeId, uint32_t nodeType) {
	if (auto* session = ResolveNodeSession(nodeId, nodeType, "SendMessageToSessionNode")) {
		session->SendRequest(messageId, message);
	}
}


void SendMessageToClientNode(uint32_t messageId, const google::protobuf::Message& message, NodeId nodeId, uint32_t nodeType) {
	if (auto* client = ResolveNodeClient(nodeId, nodeType, "SendMessageToClientNode")) {
		client->SendRequest(messageId, message);
	}
}

void CallRemoteMethodOnSession(uint32_t messageId, const google::protobuf::Message& message,
	NodeId nodeId, uint32_t nodeType)
{
	if (auto* session = ResolveNodeSession(nodeId, nodeType, "CallRemoteMethodOnSession")) {
		session->CallRemoteMethod(messageId, message);
	}
}

void CallRemoteMethodOnClient(uint32_t messageId, const google::protobuf::Message& message,
	NodeId nodeId, uint32_t nodeType)
{
	if (auto* client = ResolveNodeClient(nodeId, nodeType, "CallRemoteMethodOnClient")) {
		client->CallRemoteMethod(messageId, message);
	}
}


void BroadcastToNodes(uint32_t messageId, const google::protobuf::Message& message,
	uint32_t nodeType)
{
	auto& registry = tlsNodeContextManager.GetRegistry(nodeType);

	const auto byte_size = message.ByteSizeLong();
	std::string serialized;
	serialized.reserve(byte_size);
	if (!message.SerializeToString(&serialized)) {
		LOG_ERROR << "Failed to serialize broadcast message";
		return;
	}

	NodeRouteMessageRequest request;
	request.mutable_message_content()->set_message_id(messageId);
	request.mutable_message_content()->mutable_serialized_message()->swap(serialized);

	for (auto&& [_, node] : registry.view<RpcClientPtr>().each())
	{
		// Reuse request where possible; SendRequest may copy internally for async handling
		node->SendRequest(messageId, request);
	}
}

void SendMessageToPlayerViaClientNode(uint32_t wrappedMessageId, uint32_t nodeType, uint32_t messageId, const google::protobuf::Message& message, Guid playerId)
{
	SendMessageToPlayerViaClientNode(wrappedMessageId, nodeType, messageId, message, tlsEcs.GetPlayer(playerId));
}


void SendMessageToPlayerViaClientNode(uint32_t wrappedMessageId,
	uint32_t nodeType,
	uint32_t messageId,
	const google::protobuf::Message& message,
	entt::entity playerEntity)
{
	const auto* sessionPB = ResolvePlayerSession(playerEntity, "SendMessageToPlayerViaClientNode");
	if (!sessionPB) {
		return;
	}

	NodeId nodeId{};
	if (!FindPlayerNodeId(*sessionPB, nodeType, playerEntity, nodeId)) {
		return;
	}

	auto* client = ResolveNodeClient(nodeId, nodeType, "SendMessageToPlayerViaClientNode");
	if (!client) {
		return;
	}

	NodeRouteMessageRequest request;
	if (!BuildRoutedRequest(messageId, message, sessionPB->gate_session_id(), request)) {
		return;
	}

	client->SendRequest(wrappedMessageId, request);
}


void SendMessageToPlayerViaSessionNode(uint32_t wrappedMessageId,
	uint32_t nodeType,
	uint32_t messageId,
	const google::protobuf::Message& message,
	Guid playerId)
{
	SendMessageToPlayerViaSessionNode(wrappedMessageId, nodeType, messageId, message, tlsEcs.GetPlayer(playerId));
}


void SendMessageToPlayerViaSessionNode(uint32_t wrappedMessageId,
	uint32_t nodeType,
	uint32_t messageId,
	const google::protobuf::Message& message,
	entt::entity playerEntity)
{
	const auto* sessionPB = ResolvePlayerSession(playerEntity, "SendMessageToPlayerViaSessionNode");
	if (!sessionPB) {
		return;
	}

	NodeId nodeId{};
	if (!FindPlayerNodeId(*sessionPB, nodeType, playerEntity, nodeId)) {
		return;
	}

	auto* session = ResolveNodeSession(nodeId, nodeType, "SendMessageToPlayerViaSessionNode");
	if (!session) {
		return;
	}

	NodeRouteMessageRequest request;
	if (!BuildRoutedRequest(messageId, message, sessionPB->gate_session_id(), request)) {
		return;
	}

	session->SendRequest(wrappedMessageId, request);
}

