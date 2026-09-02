#include "node_connector.h"
#include "proto/common/base/common.pb.h"
#include "node_util.h"
#include <muduo/base/Logging.h>
#include <network/rpc_client.h>
#include <network/node_utils.h>
#include <grpc_client/grpc_init_client.h>
#include <thread_context/redis_manager.h>
#include "node.h"
#include "network/network_utils.h"
#include "thread_context/node_context_manager.h"
#include "node/system/grpc_channel_cache.h"

void NodeConnector::ConnectToNode(const NodeInfo &nodeInfo)
{
	if (gNode->IsCurrentNode(nodeInfo))
	{
		LOG_TRACE << "Skipping connection to self node, uuid=" << nodeInfo.node_uuid();
		return;
	}
	switch (nodeInfo.protocol_type())
	{
	case PROTOCOL_GRPC:
		ConnectToGrpcNode(nodeInfo);
		break;
	case PROTOCOL_TCP:
		ConnectToTcpNode(nodeInfo);
		break;
	case PROTOCOL_HTTP:
		ConnectToHttpNode(nodeInfo);
		break;
	default:
		LOG_ERROR << "Unsupported protocol: " << nodeInfo.protocol_type()
				  << " node: " << nodeInfo.DebugString();
		break;
	}
}

void NodeConnector::ConnectToGrpcNode(const NodeInfo &nodeInfo)
{
	auto &targetRegistry = NodeUtils::GetRegistryForNodeType(nodeInfo.node_type());

	// GRPC node_id is only unique within a zone, not globally across zones.
	// Use uuid-based dedup and auto-generated entity IDs to avoid collisions
	// when the gate discovers GRPC nodes from multiple zones (e.g. login nodes
	// with node_id 1..3 in each of 10 zones sharing the same etcd).
	for (const auto &[entity, existing] : targetRegistry.view<NodeInfo>().each())
	{
		if (NodeUtils::IsSameNode(nodeInfo.node_uuid(), existing.node_uuid()))
		{
			LOG_INFO << "GRPC node already registered, IP: " << nodeInfo.grpc_endpoint().ip()
					 << ", Port: " << nodeInfo.grpc_endpoint().port();
			return;
		}
	}

	auto createdId = targetRegistry.create();

	// gRPC connections MUST use grpc_endpoint. Falling back to endpoint (raw TCP)
	// would send HTTP/2 preface to a protobuf codec, causing InvalidLength errors.
	const auto &ep = nodeInfo.grpc_endpoint();
	if (ep.ip().empty() || ep.port() == 0)
	{
		LOG_ERROR << "Cannot connect to GRPC node: grpc_endpoint is empty, ID: " << nodeInfo.node_id()
				  << ", NodeType: " << nodeInfo.node_type()
				  << ". The target node may not have registered a gRPC server.";
		targetRegistry.destroy(createdId);
		return;
	}
	const auto target = ::FormatIpAndPort(ep.ip(), ep.port());
	auto cachedChannel = gNode->GetGrpcChannelCache().GetOrCreateChannel(target);
	const auto &grpcChannel = targetRegistry.emplace<std::shared_ptr<grpc::Channel>>(createdId, std::move(cachedChannel));

	InitGrpcNode(grpcChannel, targetRegistry, createdId);
	targetRegistry.emplace<NodeInfo>(createdId, nodeInfo);

	LOG_INFO << "Connecting to GRPC node, ID: " << nodeInfo.node_id()
			 << ", IP: " << ep.ip()
			 << ", Port: " << ep.port()
			 << ", NodeType: " << nodeInfo.node_type()
			 << ", Entity: " << entt::to_integral(createdId);
}

void NodeConnector::ConnectToTcpNode(const NodeInfo &nodeInfo)
{
	auto &targetRegistry = tlsNodeContextManager.GetRegistry(nodeInfo.node_type());

	// TCP nodes used to key entt::entity by node_id, which only works as long
	// as a registry holds at most one zone's worth of nodes (node_id is unique
	// within a zone, not across zones). To make multi-zone deployments safe by
	// construction we now treat uuid as the primary key and let entt assign
	// entity ids automatically — the same scheme ConnectToGrpcNode has always
	// used. Callers that previously did `entt::entity{node_id}` should resolve
	// the entity via NodeUtils::FindNodeEntityByZoneAndNodeId or by uuid.
	for (const auto &[existingEntity, existingNodeInfo] : targetRegistry.view<NodeInfo>().each())
	{
		if (!NodeUtils::IsSameNode(existingNodeInfo.node_uuid(), nodeInfo.node_uuid()))
		{
			continue;
		}

		LOG_INFO << "TCP node already registered, IP: " << nodeInfo.endpoint().ip()
				 << ", Port: " << nodeInfo.endpoint().port()
				 << ", uuid: " << nodeInfo.node_uuid();

		// Same uuid resurfacing while the previous client is still tracked is
		// effectively a re-registration after a transient drop; surrender the
		// old client to the disconnect list so its lifecycle is wound down,
		// then fall through to create a fresh entity.
		if (auto *existingClient = targetRegistry.try_get<RpcClientPtr>(existingEntity))
		{
			gNode->GetDisconnectedClientList().push_back(*existingClient);
		}
		targetRegistry.destroy(existingEntity);
		break;
	}

	const auto createdId = targetRegistry.create();

	muduo::net::InetAddress remoteEndpoint(nodeInfo.endpoint().ip(), nodeInfo.endpoint().port());
	auto &client = targetRegistry.emplace<RpcClientPtr>(
		createdId,
		std::make_shared<RpcClientPtr::element_type>(gNode->GetLoop(), remoteEndpoint));

	targetRegistry.emplace<NodeInfo>(createdId, nodeInfo);

	auto *replyService = gNode->GetNodeReplyService();
	if (replyService != nullptr)
	{
		client->registerService(replyService);
	}
	else
	{
		LOG_WARN << "Node reply service is null, skip client registerService. node_type=" << gNode->GetNodeType();
	}
	client->connect();

	LOG_INFO << "Connecting to TCP node, Uuid: " << nodeInfo.node_uuid()
			 << ", IP: " << nodeInfo.endpoint().ip()
			 << ", Port: " << nodeInfo.endpoint().port()
			 << ", Entity: " << entt::to_integral(createdId);
}

void NodeConnector::ConnectToHttpNode(const NodeInfo &)
{
	// HTTP connection — extensible
}

void NodeConnector::ConnectAllNodes()
{
	auto &serviceNodesByType = tlsEcs.nodeGlobalRegistry.get_or_emplace<ServiceNodeList>(tlsEcs.GrpcNodeEntity());

	const auto &whitelist = gNode->GetTargetNodeTypeWhitelist();
	for (uint32_t nodeType = 0; nodeType < eNodeType_ARRAYSIZE; ++nodeType)
	{
		if (!whitelist.contains(nodeType))
			continue;

		for (const auto &serviceNode : serviceNodesByType[nodeType].node_list())
		{
			if (NodeUtils::IsNodeConnected(nodeType, serviceNode))
				continue;

			ConnectToNode(serviceNode);
			LOG_TRACE << "Connected to node from ConnectAllNodes, type: " << nodeType
					  << ", node_id: " << serviceNode.node_id()
					  << ", uuid: " << serviceNode.node_uuid();
		}
	}
}
