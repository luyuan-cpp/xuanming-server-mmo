#include "node_util.h"
#include <muduo/base/Logging.h>

#include "proto/common/base/common.pb.h"
#include <network/rpc_client.h>
#include <network/rpc_session.h>
#include "thread_context/node_context_manager.h"
#include <vector>

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
	{eNodeType::MatchNodeService, eNodeType_Name(MatchNodeService)},
	// 客户端 RPC 路由服:etcd 前缀 ClientRpcRouterNodeService.rpc;gate 唯一的 gRPC 目标
	// (docs/design/client-rpc-router.md),Go-Zero 实现,全局池。
	{eNodeType::ClientRpcRouterNodeService, eNodeType_Name(ClientRpcRouterNodeService)}};

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
	case eNodeType::ClientRpcRouterNodeService:
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

std::size_t NodeUtils::RemoveRpcSessionsBoundTo(const muduo::net::TcpConnectionPtr &conn)
{
	if (!conn)
	{
		return 0;
	}
	std::size_t removed = 0;
	for (auto &registry : tlsNodeContextManager.GetAllRegistries())
	{
		// 先收集再删:不在正在迭代的视图上就地 remove,不依赖 EnTT 对"当前元素可删"的保证。
		// 顺带把 weak 已过期(对端早已断开、组件却没摘)的会话一并清掉:它们已经不指向任何连接。
		std::vector<entt::entity> bound;
		for (const auto &[entity, session] : registry.view<RpcSession>().each())
		{
			const auto sessionConn = session.connection.lock();
			if (!sessionConn || sessionConn.get() == conn.get())
			{
				bound.push_back(entity);
			}
		}
		for (const entt::entity entity : bound)
		{
			registry.remove<RpcSession>(entity);
			++removed;
		}
	}
	if (removed > 0)
	{
		// INFO 级:这是节点链路断开时唯一可在默认日志级别看到的"已摘"证据(运行时验收用)。
		LOG_INFO << "Detached " << removed << " RpcSession(s) from closed connection "
				 << conn->peerAddress().toIpPort();
	}
	return removed;
}
