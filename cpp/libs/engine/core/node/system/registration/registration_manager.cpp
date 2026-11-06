#include "registration_manager.h"
#include <algorithm>
#include <chrono>
#include <string>
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
// 握手失败后关闭连接前的退避:首次 0.5s,连续失败翻倍,8s 封顶(NodeHandshakeManager::FailedHandshakeCloseDelaySeconds)。
constexpr double kFailedHandshakeInitialCloseDelaySeconds = 0.5;
constexpr double kFailedHandshakeMaxCloseDelaySeconds = 8.0;

bool IsHandshakeNodeType(uint32_t nodeType)
{
    return VisitHandshakeRpc(nodeType, [](auto) {});
}

bool IsCurrentConnection(const RpcClientPtr& client, const TcpConnectionPtr& conn)
{
    return client && conn && client->connected() && client->channel()->IsBoundTo(conn);
}

// 失败日志里的实例身份:四项都参与比对,日志也四项都打,否则只差 node_id / zone 的不符看不出原因。
std::string DescribeIdentity(const NodeInfo& node)
{
    return "type=" + std::to_string(node.node_type()) + " uuid=" + node.node_uuid() +
           " node_id=" + std::to_string(node.node_id()) + " zone=" + std::to_string(node.zone_id());
}

// 每条连接只有一次握手。失败后关闭，让 TcpClient 负责通信重连；不排业务恢复或 RPC 重放。
// 退避只能放在关闭之前:已建立的连接关闭后 muduo 当场重连(TcpClient::removeConnection → Connector::restart,
// 它自己的建连退避同时被重置)。连续失败按次数拉长等待,长期失败(配置错误、同一地址上已换成别的进程)每条链路
// 最多每 8s 一轮。重试持续多久由权威发现决定:对端实例从 etcd 摘除时 RpcClient 随节点实体销毁,定时器 lock 失败即止。
// 不加抖动:每轮只有一次轻量握手,打到同一 scene 的链路数就是 gate 数,同步重试不构成负载。
// 延迟闭包只持 weak,不延长 client / 连接寿命;旧定时器不能关掉替代连接。
void CloseFailedHandshake(const RpcClientPtr& client, const TcpConnectionPtr& conn, const std::string& reason)
{
    const uint32_t failures = client->RecordHandshakeFailure();
    const double delaySeconds = NodeHandshakeManager::FailedHandshakeCloseDelaySeconds(failures);
    LOG_WARN << "Node handshake failed: " << reason << ", peer=" << conn->peerAddress().toIpPort()
             << ", consecutive_failures=" << failures << ", close_in_seconds=" << delaySeconds;
    conn->getLoop()->runAfter(delaySeconds,
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

double NodeHandshakeManager::FailedHandshakeCloseDelaySeconds(uint32_t consecutiveFailures)
{
    double delaySeconds = kFailedHandshakeInitialCloseDelaySeconds;
    for (uint32_t failure = 1; failure < consecutiveFailures && delaySeconds < kFailedHandshakeMaxCloseDelaySeconds; ++failure)
    {
        delaySeconds *= 2;
    }
    return std::min(delaySeconds, kFailedHandshakeMaxCloseDelaySeconds);
}

bool NodeHandshakeManager::IsAddressedTo(const NodeHandshakeRequest& request, const std::string& selfUuid)
{
    return !request.expected_peer_uuid().empty() && request.expected_peer_uuid() == selfUuid;
}

void NodeHandshakeManager::BeginHandshake(const RpcClientPtr& client, const NodeInfo& selfNode,
                                         const NodeInfo& expectedPeer, const TcpConnectionPtr& conn)
{
    if (!IsCurrentConnection(client, conn) || client->IsReadyOn(conn)) return;
    if (expectedPeer.node_uuid().empty() || !IsHandshakeNodeType(expectedPeer.node_type()))
    {
        CloseFailedHandshake(client, conn, "invalid expected peer " + DescribeIdentity(expectedPeer));
        return;
    }

    NodeHandshakeRequest request;
    *request.mutable_self_node() = selfNode;
    // 点名要连的实例:同一地址上换了进程时,对端据此拒绝,不让这条指向旧实例的连接顶替真正链路的回推会话。
    request.set_expected_peer_uuid(expectedPeer.node_uuid());
    VisitHandshakeRpc(expectedPeer.node_type(), [&]<typename Method>(Method) {
        // 握手经通信 channel 发起；业务走 RpcClient 的 Ready 门禁，不能抢在身份确认之前发送。
        client->channel()->CallMethod<Method>(request,
            [expectedPeer, weakClient = std::weak_ptr<RpcClient>(client),
             weakConn = std::weak_ptr<muduo::net::TcpConnection>(conn)](
                const RpcStatus& status, const std::shared_ptr<NodeHandshakeResponse>& response) {
                const auto liveClient = weakClient.lock();
                const auto liveConn = weakConn.lock();
                if (!IsCurrentConnection(liveClient, liveConn)) return;
                if (!status.ok())
                {
                    CloseFailedHandshake(liveClient, liveConn, std::string("status=") + status.name());
                    return;
                }
                if (response->error_message().id() != kCommon_errorOK)
                {
                    CloseFailedHandshake(liveClient, liveConn,
                                         "rejected by peer error_id=" + std::to_string(response->error_message().id()));
                    return;
                }
                const auto& peer = response->peer_node();
                if (peer.node_type() != expectedPeer.node_type() ||
                    peer.node_uuid() != expectedPeer.node_uuid() ||
                    peer.node_id() != expectedPeer.node_id() || peer.zone_id() != expectedPeer.zone_id())
                {
                    CloseFailedHandshake(liveClient, liveConn, "identity mismatch expected " + DescribeIdentity(expectedPeer) +
                                                                   ", got " + DescribeIdentity(peer));
                    return;
                }
                if (liveClient->MarkReady(liveConn))
                {
                    LOG_INFO << "Node link ready: " << DescribeIdentity(peer)
                             << ", peer=" << liveConn->peerAddress().toIpPort();
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

	// 先确认这次握手是发给本进程实例的,再碰注册表。同一地址上换了进程(崩溃后原地重启,旧实例的 etcd 键要等
	// 租约过期才消失)时,发起方指向旧实例的 RpcClient 会反复连到本进程;若照常登记,它每次都会把真正链路上的
	// RpcSession 换成自己的,随后被发起方以身份不符关闭时,DOWN 连同会话一起摘掉 —— 本端回推全部丢失,
	// 而真正的链路已经 Ready、不会再握手补回来。拒绝时不改任何状态。
	if (!IsAddressedTo(request, gNode->GetNodeInfo().node_uuid()))
	{
		response.mutable_error_message()->set_id(kFailedToRegisterTheNode);
		LOG_WARN << "Reject node handshake addressed to another instance: peer_type=" << peerNode.node_type()
			<< ", peer_uuid=" << peerNode.node_uuid()
			<< ", expected_peer_uuid=" << request.expected_peer_uuid()
			<< ", self_uuid=" << gNode->GetNodeInfo().node_uuid()
			<< ", conn=" << rpcConn->peerAddress().toIpPort();
		return;
	}

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
