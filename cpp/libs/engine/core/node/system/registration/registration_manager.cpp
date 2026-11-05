#include "registration_manager.h"
#include <chrono>
#include <type_traits>
#include <table/proto/tip/common_error_tip.pb.h>
#include "node/system/node/node_util.h"
#include <rpc/service_metadata/scene_service_metadata.h>
#include <rpc/service_metadata/gate_service_service_metadata.h>
#include <network/rpc_client.h>
#include <network/rpc_session.h>
#include <network/rpc_status.h>
#include "node/system/node/node.h"
#include <network/node_utils.h>
#include "thread_context/node_context_manager.h"
#include "proto/common/base/message.pb.h"
#include <network/rpc_controller.h>
#include <muduo/net/EventLoop.h>

namespace
{
static_assert(std::is_same_v<SceneNodeHandshakeRpc::Request, GateNodeHandshakeRpc::Request> &&
                  std::is_same_v<SceneNodeHandshakeRpc::Response, GateNodeHandshakeRpc::Response>,
              "Scene / Gate NodeHandshake must share request and response types");

// 仅选择通信协议入口，不按对端类型安装业务恢复逻辑。Gate 项同时供入站白名单使用。
template <typename Fn>
constexpr bool VisitHandshakeRpc(uint32_t nodeType, Fn&& fn)
{
    switch (static_cast<eNodeType>(nodeType))
    {
    case eNodeType::SceneNodeService:
        fn(SceneNodeHandshakeRpc{});
        return true;
    case eNodeType::GateNodeService:
        fn(GateNodeHandshakeRpc{});
        return true;
    default:
        return false;
    }
}

constexpr uint32_t HandshakeMessageIdOf(uint32_t nodeType)
{
    uint32_t messageId = 0;
    VisitHandshakeRpc(nodeType, [&messageId]<typename Method>(Method) { messageId = Method::kMessageId; });
    return messageId;
}
static_assert(HandshakeMessageIdOf(static_cast<uint32_t>(eNodeType::SceneNodeService)) == SceneNodeHandshakeMessageId);
static_assert(HandshakeMessageIdOf(static_cast<uint32_t>(eNodeType::GateNodeService)) == GateNodeHandshakeMessageId);

constexpr std::chrono::milliseconds kNodeHandshakeCallTimeout{3000};
constexpr double kFailedHandshakeCloseDelaySeconds = 0.5;

bool IsHandshakeNodeType(uint32_t nodeType)
{
    return VisitHandshakeRpc(nodeType, [](auto) {});
}

bool IsCurrentConnection(const RpcClientPtr& client, const TcpConnectionPtr& conn)
{
    return client && conn && client->connected() && client->channel()->IsBoundTo(conn);
}

void CloseFailedHandshake(const RpcClientPtr& client, const TcpConnectionPtr& conn)
{
    // 每条连接只有一次握手。失败后关闭，让 TcpClient 负责通信重连；不排业务恢复或 RPC 重放。
    // 延迟关闭抑制快速拒绝的忙循环，只持 weak，且旧定时器不能关掉替代连接。
    conn->getLoop()->runAfter(kFailedHandshakeCloseDelaySeconds,
        [weakClient = std::weak_ptr<RpcClient>(client), weakConn = std::weak_ptr<muduo::net::TcpConnection>(conn)] {
            const auto liveClient = weakClient.lock();
            const auto liveConn = weakConn.lock();
            if (IsCurrentConnection(liveClient, liveConn) && !liveClient->IsReadyOn(liveConn))
            {
                liveConn->forceClose();
            }
        });
}
} // namespace

void NodeHandshakeManager::BeginHandshake(const RpcClientPtr& client, const NodeInfo& selfNode,
                                         const NodeInfo& expectedPeer, const TcpConnectionPtr& conn)
{
    if (!IsCurrentConnection(client, conn) || client->IsReadyOn(conn)) return;
    if (expectedPeer.node_uuid().empty() || !IsHandshakeNodeType(expectedPeer.node_type()))
    {
        LOG_ERROR << "Node handshake rejected invalid expected peer: node_type=" << expectedPeer.node_type();
        CloseFailedHandshake(client, conn);
        return;
    }

    NodeHandshakeRequest request;
    *request.mutable_self_node() = selfNode;
    VisitHandshakeRpc(expectedPeer.node_type(), [&]<typename Method>(Method) {
        // 握手经通信 channel 发起；业务走 RpcClient 的 Ready 门禁，不能抢在身份确认之前发送。
        client->channel()->CallMethod<Method>(request,
            [expectedPeer, weakClient = std::weak_ptr<RpcClient>(client),
             weakConn = std::weak_ptr<muduo::net::TcpConnection>(conn)](
                const RpcStatus& status, const std::shared_ptr<NodeHandshakeResponse>& response) {
                const auto liveClient = weakClient.lock();
                const auto liveConn = weakConn.lock();
                if (!IsCurrentConnection(liveClient, liveConn)) return;
                if (!status.ok() || response->error_message().id() != kCommon_errorOK)
                {
                    LOG_WARN << "Node handshake failed: status=" << status.name()
                             << ", peer=" << liveConn->peerAddress().toIpPort();
                    CloseFailedHandshake(liveClient, liveConn);
                    return;
                }
                const auto& peer = response->peer_node();
                if (peer.node_type() != expectedPeer.node_type() ||
                    peer.node_uuid() != expectedPeer.node_uuid() ||
                    peer.node_id() != expectedPeer.node_id() || peer.zone_id() != expectedPeer.zone_id())
                {
                    LOG_ERROR << "Node handshake reply identity mismatch: expected type=" << expectedPeer.node_type()
                              << " uuid=" << expectedPeer.node_uuid() << ", got type=" << peer.node_type()
                              << " uuid=" << peer.node_uuid();
                    CloseFailedHandshake(liveClient, liveConn);
                    return;
                }
                if (liveClient->MarkReady(liveConn))
                {
                    LOG_INFO << "Node link ready: type=" << peer.node_type() << ", uuid=" << peer.node_uuid();
                }
            }, RpcCallOptions{kNodeHandshakeCallTimeout});
    });
}

void NodeHandshakeManager::OnNodeHandshake(
	const RpcController& ctx,
	const NodeHandshakeRequest& request,
	NodeHandshakeResponse& response
) const {
	// 连接随 ctx 显式传入(Go 的 ctx 语义),不再从 thread_local 取。
	const TcpConnectionPtr& rpcConn = ctx.conn();
	if (!rpcConn)
	{
		response.mutable_error_message()->set_id(kFailedToRegisterTheNode);
		LOG_ERROR << "No connection on the RPC context.";
		return;
	}

	auto& peerNode = request.self_node();
	// 生成的 IsTcpNodeType 包含可注册的全部服务，不等于拥有 RPC0 握手入口。
	// 先拒绝纯 gRPC 服务，避免把只有 NodeInfo/RpcSession 的实体混进其出站通道池。
	if (!IsHandshakeNodeType(peerNode.node_type()))
	{
		response.mutable_error_message()->set_id(kFailedToRegisterTheNode);
		LOG_ERROR << "Invalid node type for registration: " << peerNode.node_type();
		return;
	}
	response.mutable_peer_node()->CopyFrom(gNode->GetNodeInfo());
	LOG_TRACE << "Node registration request: " << request.DebugString();

	auto tryRegister = [&, this](const TcpConnectionPtr& conn, uint32_t nodeType) -> bool {
		entt::registry& registry = tlsNodeContextManager.GetRegistry(nodeType);
		const auto& nodesOfType = gNode->GetServiceDiscoveryManager().ServiceNodes().NodesOfType(nodeType);
		const NodeInfo* nodeToRegister = &peerNode;
		for (const auto& serverNode : nodesOfType) {
			if (!NodeUtils::IsSameNode(serverNode.node_uuid(), peerNode.node_uuid())) {
				continue;
			}
			nodeToRegister = &serverNode;
			break;
		}

		if (NodeUtils::IsZoneScopedNodeType(nodeType) &&
			nodeToRegister->zone_id() != gNode->GetNodeInfo().zone_id()) {
			LOG_WARN << "Reject node registration from foreign zone. type=" << nodeType
				<< ", node_zone=" << nodeToRegister->zone_id()
				<< ", self_zone=" << gNode->GetNodeInfo().zone_id()
				<< ", uuid=" << nodeToRegister->node_uuid();
			return false;
		}

		// uuid is the only globally-unique handle for a remote node, so we
		// look up (or freshly allocate) the entity by uuid. Previously the
		// entity slot was keyed by node_id, which silently aliased nodes
		// from different zones onto the same slot.
		entt::entity nodeEntity = entt::null;
		if (const auto found = NodeUtils::FindNodeEntityByUuid(nodeType, nodeToRegister->node_uuid()); found) {
			nodeEntity = *found;
			// Reset session-bound components on re-registration so any
			// stale RpcSession from a previous connection is cleared.
			registry.remove<RpcSession>(nodeEntity);
		} else {
			nodeEntity = registry.create();
			LOG_TRACE << "Allocated new entity " << entt::to_integral(nodeEntity)
				<< " for handshaking peer uuid=" << nodeToRegister->node_uuid();
		}

		registry.emplace_or_replace<NodeInfo>(nodeEntity, *nodeToRegister);
		registry.emplace<RpcSession>(nodeEntity, RpcSession{ conn , nodeToRegister->node_uuid()});
		LOG_INFO << "Node registered, id: " << nodeToRegister->node_id()
			<< " uuid: " << nodeToRegister->node_uuid()
			<< " entity: " << entt::to_integral(nodeEntity)
			<< " in " << NodeUtils::GetRegistryName(registry);
		return true;
		};

	if (tryRegister(rpcConn, peerNode.node_type())) {
		response.mutable_error_message()->set_id(kCommon_errorOK);
		LOG_INFO << "Node registration succeeded: " << peerNode.DebugString();
		return;
	}
	response.mutable_error_message()->set_id(kFailedToRegisterTheNode);
}
