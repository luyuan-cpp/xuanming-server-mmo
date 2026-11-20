#include "network_utils.h"

#include <boost/asio.hpp>
#include <iostream>
#include <sstream>
#include "muduo/base/Logging.h"
#include "network/node_utils.h"
#include "node/system/node/node_util.h"
#include "thread_context/node_context_manager.h"
#include "engine/core/type_define/player_login_identity_comp.h"
#include "thread_context/ecs_context.h"

namespace
{
	// 身份组件记的会话就是 sessionId 时它记下的 gate 实例,否则空串(未知)。返回的引用只在当次调用里用。
	const std::string &RecordedGateInstanceId(entt::entity player, SessionId sessionId)
	{
		static const std::string kUnknown;
		const auto *identity = tlsEcs.actorRegistry.try_get<PlayerLoginIdentityComp>(player);
		return (identity != nullptr && identity->sessionId == sessionId) ? identity->gateInstanceId : kUnknown;
	}
} // namespace

NodeId GetGateNodeId(SessionId session_id)
{
	return static_cast<NodeId>(session_id >> SessionIdGenerator::sequence_bit_count());
}

std::optional<entt::entity> ResolveLocalZoneGateEntity(SessionId sessionId)
{
	const auto zoneId = GetZoneId();
	const auto gateNodeId = GetGateNodeId(sessionId);
	if (auto entity = NodeUtils::FindNodeEntityByZoneAndNodeId(
		eNodeType::GateNodeService,
		zoneId,
		gateNodeId))
	{
		return entity;
	}

	// Defensive fallback for the brief window where this node's zone
	// (GetZoneId(), read from Node::localNodeInfo_, filled in Node::InitRpcServer) is not yet installed — e.g. early in
	// startup before NodeContext is fully populated, or in dev/single-zone
	// launches where zone propagation hasn't happened yet. The local-thread
	// gate registry is already filtered down to same-zone gates by
	// registration_manager.cpp's IsZoneScopedNodeType check, so a unique
	// node_id match here is safe. Cross-zone routing is NOT this path's
	// responsibility — true cross-zone targets are handled by the gate
	// redirect flow upstream.
	auto &registry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
	std::optional<entt::entity> uniqueMatch;
	size_t matches = 0;
	for (const auto &[entity, nodeInfo] : registry.view<NodeInfo>().each())
	{
		if (nodeInfo.node_id() == gateNodeId)
		{
			uniqueMatch = entity;
			++matches;
		}
	}
	if (matches == 1)
	{
		LOG_WARN << "ResolveLocalZoneGateEntity zone-context-lag fallback, session_id="
				 << sessionId << ", gate_node_id=" << gateNodeId
				 << ", zone_id=" << zoneId;
		return uniqueMatch;
	}
	return std::nullopt;
}

std::optional<entt::entity> ResolveSessionGateEntity(SessionId sessionId, const std::string &gateInstanceId)
{
	if (gateInstanceId.empty())
	{
		return ResolveLocalZoneGateEntity(sessionId);
	}
	return NodeUtils::FindNodeEntityByUuid(eNodeType::GateNodeService, gateInstanceId);
}

std::optional<entt::entity> ResolvePlayerGateEntity(entt::entity player, SessionId sessionId)
{
	return ResolveSessionGateEntity(sessionId, RecordedGateInstanceId(player, sessionId));
}

std::string BoundGateInstanceId(entt::entity player, SessionId sessionId)
{
	if (const auto &recorded = RecordedGateInstanceId(player, sessionId); !recorded.empty())
	{
		return recorded;
	}
	const auto gateEntity = ResolveLocalZoneGateEntity(sessionId);
	if (!gateEntity)
	{
		return {};
	}
	const auto *gateNodeInfo = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService).try_get<NodeInfo>(*gateEntity);
	return gateNodeInfo != nullptr ? gateNodeInfo->node_uuid() : std::string{};
}

void ParseIpPort(const std::string &input, std::string &ip, uint16_t &port)
{
	std::istringstream ss(input);
	std::string host;

	std::getline(ss, host, ':');
	ip = host;

	std::getline(ss, host);
	port = std::stoi(host);
}

std::string FormatIpAndPort(const std::string &ip, uint32_t port)
{
	return ip + ":" + std::to_string(port);
}
