#include "node_connector.h"
#include "engine/config/player_store_path.h"
#include "proto/common/base/common.pb.h"
#include "proto/common/base/node.pb.h"
#include "node_util.h"
#include <muduo/base/Logging.h>
#include <network/rpc_client.h>
#include <network/node_utils.h>
#include <thread_context/redis_manager.h>
#include "node.h"
#include "network/network_utils.h"
#include "thread_context/node_context_manager.h"
#include "muduo_grpc/channel.h"
#include "node/system/node/muduo_grpc_channel_slot.h"

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

	// node_id 是业务寻址编号,不是带版本位的 EnTT 实体句柄。
	// 实体由 registry.create() 自动分配,按 uuid 判断是否为同一进程;
	// 查找实体应使用 uuid 或 FindNodeEntityByZoneAndNodeId。
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
	// 所有出站 gRPC 走 muduo 驱动的传输(docs/design/grpc-muduo-transport.md §14):一个节点一条 muduo_grpc::Channel,
	// 挂成 MuduoGrpcChannelPtr 组件,生成的 Send<Svc><Method> 从这里取。通道只接受 IPv4 字面量(§8);
	// 业务服务通道不开周期 keepalive(对端是 grpc-go 默认强制策略,§4.4),连接参数取 ChannelOptions 默认值。
	// 只有玩家存盘例外:认领响应是一个玩家的全部行,可以超过默认的接收上限。
	muduo_grpc::ChannelOptions channelOptions;
	if (nodeInfo.node_type() == common::base::PlayerStoreNodeService)
	{
		channelOptions.maxReceiveMessageBytes = player_store_path::kMaxClaimResponseBytes;
	}
	auto channel = ep.port() <= 0xFFFF
					   ? muduo_grpc::Channel::Create(*gNode->GetLoop(), ep.ip(), static_cast<uint16_t>(ep.port()), std::move(channelOptions))
					   : nullptr;
	if (!channel)
	{
		LOG_ERROR << "Cannot create gRPC channel to node, ID: " << nodeInfo.node_id()
				  << ", NodeType: " << nodeInfo.node_type()
				  << ", IP: " << ep.ip() << ", Port: " << ep.port()
				  << " (the grpc_endpoint must be an IPv4 literal with a valid port)";
		targetRegistry.destroy(createdId);
		return;
	}
	// 发现即预连:第一次调用不必再等一次建连握手(与旧官方通道的 GetState(true) 选点行为一致)。
	channel->GetState(true);
	targetRegistry.emplace<MuduoGrpcChannelPtr>(createdId, std::move(channel));
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
    entt::entity createdId = entt::null;
    for (const auto &[entity, existing] : targetRegistry.view<NodeInfo>().each())
    {
        if (!NodeUtils::IsSameNode(existing.node_uuid(), nodeInfo.node_uuid())) continue;
        // 同实例的实体和 client 是稳定句柄，通信断开由原 TcpClient 重连，不重建业务引用。
        if (existing.node_id() != nodeInfo.node_id() || existing.zone_id() != nodeInfo.zone_id() ||
            existing.endpoint().ip() != nodeInfo.endpoint().ip() || existing.endpoint().port() != nodeInfo.endpoint().port())
        {
            LOG_ERROR << "Reject changed TCP identity or endpoint for existing uuid=" << nodeInfo.node_uuid();
            return;
        }
        if (const auto* existingClient = targetRegistry.try_get<RpcClientPtr>(entity); existingClient && *existingClient)
        {
            return;
        }
        // 同 UUID 已有入站目录实体时，只补出站通信能力，保留原实体和其他状态。
        targetRegistry.remove<RpcClientPtr>(entity);
        createdId = entity;
        break;
    }
    if (createdId == entt::null)
    {
        createdId = targetRegistry.create();
        targetRegistry.emplace<NodeInfo>(createdId, nodeInfo);
    }

	muduo::net::InetAddress remoteEndpoint(nodeInfo.endpoint().ip(), nodeInfo.endpoint().port());
	auto &client = targetRegistry.emplace<RpcClientPtr>(
		createdId,
		std::make_shared<RpcClient>(gNode->GetLoop(), remoteEndpoint, RpcClient::Readiness::kAfterHandshake));

	auto *replyService = gNode->GetNodeReplyService();
	if (replyService != nullptr)
	{
		client->registerService(replyService);
	}
	else
	{
		LOG_WARN << "Node reply service is null, skip client registerService. node_type=" << gNode->GetNodeType();
	}
	client->setConnectionObserver([weakClient = std::weak_ptr<RpcClient>(client),
                                   selfNode = gNode->GetNodeInfo(), expectedPeer = nodeInfo](const muduo::net::TcpConnectionPtr& conn)
    {
        if (conn->connected())
        {
            NodeHandshakeManager::BeginHandshake(weakClient.lock(), selfNode, expectedPeer, conn);
        }
    });
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
	const auto &serviceNodes = gNode->GetServiceDiscoveryManager().ServiceNodes();

	const auto &whitelist = gNode->GetTargetNodeTypeWhitelist();
	for (uint32_t nodeType = 0; nodeType < eNodeType_ARRAYSIZE; ++nodeType)
	{
		if (!whitelist.contains(nodeType))
			continue;

		for (const auto &serviceNode : serviceNodes.NodesOfType(nodeType))
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
