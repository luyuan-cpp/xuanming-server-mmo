#include "node_util.h"
#include <muduo/base/Logging.h>

#include "proto/common/base/common.pb.h"
#include <network/rpc_client.h>
#include "thread_context/node_context_manager.h"

// Static node-type-to-name map
const std::unordered_map<eNodeType, std::string> nodeTypeNameMap = {
	{eNodeType::SceneNodeService, eNodeType_Name(SceneNodeService)},
	{eNodeType::GateNodeService, eNodeType_Name(GateNodeService)},
	{eNodeType::LoginNodeService, eNodeType_Name(LoginNodeService)},
	{eNodeType::SceneManagerNodeService, eNodeType_Name(SceneManagerNodeService)},
	{eNodeType::DataServiceNodeService, eNodeType_Name(DataServiceNodeService)},
	{eNodeType::FriendNodeService, eNodeType_Name(FriendNodeService)},
	{eNodeType::GuildNodeService, eNodeType_Name(GuildNodeService)},
	// 回合制战斗节点:etcd 前缀 BattleNodeService.rpc;短名/Kafka topic 前缀
	// (battle-{id})由 NodeTypeToShortName 自动派生,无需另配。
	{eNodeType::BattleNodeService, eNodeType_Name(BattleNodeService)},
	// 匹配服务(Go gRPC,无状态):gate 按 NODE_MATCH 路由客户端 JoinQueue/
	// WatchBattle 等消息,缺席则发现侧报 "Unknown service type for prefix:
	// MatchNodeService.rpc/..."(2026-09-01 冒烟补)。
	{eNodeType::MatchNodeService, eNodeType_Name(MatchNodeService)}};

eNodeType NodeUtils::GetServiceTypeFromPrefix(const std::string &prefix)
{
	for (const auto &[type, name] : nodeTypeNameMap)
	{
		if (prefix.find(name) != std::string::npos)
		{
			return type;
		}
	}

	LOG_ERROR << "Unknown service type for prefix: " << prefix;
	return eNodeType(std::numeric_limits<::int32_t>::max());
}

entt::registry &NodeUtils::GetRegistryForNodeType(uint32_t nodeType)
{
	return tlsNodeContextManager.GetRegistry(nodeType);
}

std::optional<entt::entity> NodeUtils::FindNodeEntityByNodeId(uint32_t nodeType, uint32_t nodeId)
{
	auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	for (const auto &[entity, nodeInfo] : registry.view<NodeInfo>().each())
	{
		if (nodeInfo.node_id() == nodeId)
		{
			return entity;
		}
	}

	return std::nullopt;
}

std::optional<entt::entity> NodeUtils::FindNodeEntityByZoneAndNodeId(uint32_t nodeType, uint32_t zoneId, uint32_t nodeId)
{
	auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	for (const auto &[entity, nodeInfo] : registry.view<NodeInfo>().each())
	{
		if (nodeInfo.zone_id() == zoneId && nodeInfo.node_id() == nodeId)
		{
			return entity;
		}
	}

	return std::nullopt;
}

std::optional<entt::entity> NodeUtils::FindNodeEntityByUuid(uint32_t nodeType, const std::string &nodeUuid)
{
	if (nodeUuid.empty())
	{
		return std::nullopt;
	}
	auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	for (const auto &[entity, nodeInfo] : registry.view<NodeInfo>().each())
	{
		if (IsSameNode(nodeInfo.node_uuid(), nodeUuid))
		{
			return entity;
		}
	}

	return std::nullopt;
}

bool NodeUtils::IsGrpcOnlyNodeType(uint32_t nodeType)
{
	// 见头文件注释。目前只有回合制战斗节点:纯 gRPC 服务
	// (turn-based-battle-server.md D2/D6),注册 PROTOCOL_GRPC 后
	// 发现方(gate 等)会走 ConnectToGrpcNode 建 stub,而不是
	// 对一个没有业务语义的 muduo TCP 端口拨号。
	switch (static_cast<eNodeType>(nodeType))
	{
	case eNodeType::BattleNodeService:
		return true;
	default:
		return false;
	}
}

bool NodeUtils::IsZoneScopedNodeType(uint32_t nodeType)
{
	// See header comment for rationale. The list is the set of services that
	// register under etcd keys of the form `<Service>.rpc/zone/<N>/...` AND
	// whose callers only talk to the same zone. If a new service is added
	// that fits that pattern, extend this switch — otherwise cross-zone
	// entries will silently collide on node_id inside the local registry.
	switch (static_cast<eNodeType>(nodeType))
	{
	case eNodeType::GateNodeService:
	case eNodeType::SceneNodeService:
	case eNodeType::LoginNodeService:
	case eNodeType::PlayerLocatorNodeService:
		return true;
	default:
		return false;
	}
}

bool NodeUtils::IsGlobalPoolNodeType(uint32_t nodeType)
{
	// 见头文件注释(设计文档 cross-zone-matchmaking.md D11)。
	switch (static_cast<eNodeType>(nodeType))
	{
	case eNodeType::MatchNodeService:
	case eNodeType::BattleNodeService:
		return true;
	default:
		return false;
	}
}

std::string NodeUtils::GetRegistryName(const entt::registry &registry)
{
	const auto type = GetRegistryType(registry);
	if (type == eNodeType(std::numeric_limits<::int32_t>::max()))
	{
		return "UnknownRegistry";
	}
	return eNodeType_Name(static_cast<uint32_t>(type));
}

eNodeType NodeUtils::GetRegistryType(const entt::registry &registry)
{
	for (uint32_t i = 0; i < tlsNodeContextManager.GetAllRegistries().size(); ++i)
	{
		if (&tlsNodeContextManager.GetRegistry(i) == &registry)
		{
			return eNodeType(i);
		}
	}

	return eNodeType(std::numeric_limits<::int32_t>::max());
}

bool NodeUtils::IsSameNode(const std::string &uuid1, const std::string &uuid2)
{
	return uuid1 == uuid2;
}

bool NodeUtils::IsNodeConnected(uint32_t nodeType, const NodeInfo &info)
{
	switch (info.protocol_type())
	{
	case PROTOCOL_TCP:
	{
		entt::registry &registry = tlsNodeContextManager.GetRegistry(nodeType);
		for (const auto &[entity, client, nodeInfo] : registry.view<RpcClientPtr, NodeInfo>().each())
		{
			if (NodeUtils::IsSameNode(info.node_uuid(), nodeInfo.node_uuid()))
			{
				LOG_INFO << "Node already registered, IP: " << nodeInfo.endpoint().ip()
						 << ", Port: " << nodeInfo.endpoint().port();
				return true;
			}
		}
	}
	break;
	case PROTOCOL_GRPC:
	{
		entt::registry &registry = tlsNodeContextManager.GetRegistry(nodeType);
		for (const auto &[entity, nodeInfo] : registry.view<NodeInfo>().each())
		{
			if (NodeUtils::IsSameNode(info.node_uuid(), nodeInfo.node_uuid()))
			{
				LOG_TRACE << "GRPC node already tracked, uuid=" << nodeInfo.node_uuid();
				return true;
			}
		}
	}
	break;
	default:
		break;
	}

	return false;
}
