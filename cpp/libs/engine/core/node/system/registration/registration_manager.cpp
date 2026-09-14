#include "registration_manager.h"
#include <table/proto/tip/common_error_tip.pb.h>
#include <time/comp/timer_task_comp.h>
#include "node/system/node/node_util.h"
#include <rpc/service_metadata/scene_service_metadata.h>
#include <rpc/service_metadata/gate_service_service_metadata.h>
#include <network/rpc_client.h>
#include <network/rpc_session.h>
#include "node/system/node/node.h"
#include <network/node_utils.h>
#include "proto/common/event/server_event.pb.h"
#include <proto_helpers/proto_util.h>
#include "thread_context/node_context_manager.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/event/node_event.pb.h"
#include <network/rpc_controller.h>

static uint32_t kNodeTypeToMessageId[eNodeType_ARRAYSIZE] = {
	0,
	0,
	0,
	SceneNodeHandshakeMessageId,
	GateNodeHandshakeMessageId
};

void NodeHandshakeManager::TryRegisterNodeSession(uint32_t nodeType, const muduo::net::TcpConnectionPtr& conn) const {
	if (nodeType >= eNodeType_ARRAYSIZE || kNodeTypeToMessageId[nodeType] == 0) {
		return;
	}
	entt::registry& registry = tlsNodeContextManager.GetRegistry(nodeType);
	for (const auto& [entity, client, nodeInfo] : registry.view<RpcClientPtr, NodeInfo>().each()) {
		if (client->GetConnection() == nullptr || client->GetConnection().get() != conn.get()) continue;
		LOG_INFO << "Peer address match in " << NodeUtils::GetRegistryName(registry)
			<< ": " << conn->peerAddress().toIpPort();
		// 弱捕获,不能按值捕获 TcpConnectionPtr / RpcClientPtr。这个闭包会在
		// EventLoop 的定时器队列里活最长 0.5s;若节点恰在这个窗口内被摘除
		// (etcd DELETE → DestroyEntity → ~RpcClient → ~TcpClient),闭包里的那份
		// 强引用会把 muduo TcpClient::~TcpClient 的
		//   unique = connection_.use_count() == 1
		// 带成 false,forceClose() 被跳过,连接在 kConnected 态一直悬到闭包销毁,
		// 最后在 TcpConnection.cc:71 的 assert(state_ == kDisconnected) 上炸 ——
		// 与已修掉的 tlsRpc.conn 是同一形状的缺陷,只是持有者从 thread_local 换成
		// 了定时器闭包(docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §7)。
		// weak_ptr 不参与 use_count;触发时 lock() 失败就说明节点已经走了,放弃重试即可。
		std::weak_ptr<muduo::net::TcpConnection> weakConn = conn;
		std::weak_ptr<RpcClient> weakClient = client;
		// One-shot retry. The retry chain naturally terminates when the
		// peer replies success: OnHandshakeReplied calls
		// registry.remove<TimerTaskComp>(entity), so this timer (and any
		// future scheduling on the same entity) is cancelled. If the reply
		// fails, OnHandshakeReplied schedules another RunAfter(0.5),
		// keeping retries paced at ~2/s instead of the busy 500ms loop the
		// previous RunEvery would spin even after success.
		registry.get_or_emplace<TimerTaskComp>(entity).RunAfter(0.5, [weakConn, nodeType, weakClient]() {
			// 在回调内部才升成强引用,且只活到本次回调返回,不会外泄。
			auto conn = weakConn.lock();
			auto clientCopy = weakClient.lock();
			if (!conn || !conn->connected() || !clientCopy || !clientCopy->connected()) {
				return;
			}
			NodeHandshakeRequest req;
			req.mutable_self_node()->CopyFrom(GetNodeInfo());
			clientCopy->CallRemoteMethod(kNodeTypeToMessageId[nodeType], req);
			});
		return;
	}
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
	response.mutable_peer_node()->CopyFrom(gNode->GetNodeInfo());
	LOG_TRACE << "Node registration request: " << request.DebugString();

	auto tryRegister = [&, this](const TcpConnectionPtr& conn, uint32_t nodeType) -> bool {
		entt::registry& registry = tlsNodeContextManager.GetRegistry(nodeType);
		const auto& nodeList = tlsEcs.nodeGlobalRegistry.get_or_emplace<ServiceNodeList>(tlsEcs.GrpcNodeEntity());
		const NodeInfo* nodeToRegister = &peerNode;
		for (auto& serverNode : nodeList[nodeType].node_list()) {
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

	if (!IsTcpNodeType(peerNode.node_type()))
	{
		response.mutable_error_message()->set_id(kFailedToRegisterTheNode);
		LOG_ERROR << "Invalid node type for registration: " << peerNode.node_type();
		return;
	}
	if (tryRegister(rpcConn, peerNode.node_type())) {
		response.mutable_error_message()->set_id(kCommon_errorOK);
		LOG_INFO << "Node registration succeeded: " << peerNode.DebugString();
		return;
	}
	response.mutable_error_message()->set_id(kFailedToRegisterTheNode);
}

void NodeHandshakeManager::OnHandshakeReplied(const NodeHandshakeResponse& response) const {
	LOG_TRACE << "Node registration response: " << response.DebugString();
	uint32_t nodeType = response.peer_node().node_type();
	entt::registry& registry = tlsNodeContextManager.GetRegistry(nodeType);
	if (response.error_message().id() != kCommon_errorOK) {
		LOG_TRACE << "Registration failed: " << response.DebugString();
		for (const auto& [entity, client, nodeInfo] : registry.view<RpcClientPtr, NodeInfo>().each()) {
			if (!NodeUtils::IsSameNode(nodeInfo.node_uuid(), response.peer_node().node_uuid())) continue;
			// 与 TryRegisterNodeSession 里那一处同属一条重试链,同样弱捕获。
			// 这里只捕获 RpcClientPtr、没有 TcpConnectionPtr:只要别处不再持有这条
			// 出站连接,~RpcClient 会先释放 channel_ 再释放 client_,~TcpClient 看到的
			// 连接 use_count 仍为 1,forceClose 照常执行 —— 所以运行期它并不会引发本事故。
			// 改弱是为了让整条重试链都不延长 RpcClient 的寿命、与上面写明的规则一致,
			// 而不是修一个已经触发的缺陷。
			std::weak_ptr<RpcClient> weakClient = client;
			// One-shot retry on handshake failure. The retry chain stops
			// the moment the peer replies success — OnHandshakeReplied
			// removes TimerTaskComp from this entity. RunEvery here would
			// keep firing every 500ms even after the success reply
			// arrives, double-handshaking the peer indefinitely.
			registry.get_or_emplace<TimerTaskComp>(entity).RunAfter(0.5, [weakClient, nodeType]() {
				auto clientCopy = weakClient.lock();
				if (!clientCopy || !clientCopy->connected()) {
					return;
				}
				NodeHandshakeRequest req;
				*req.mutable_self_node() = GetNodeInfo();
				clientCopy->CallRemoteMethod(kNodeTypeToMessageId[nodeType], req);
				});
			return;
		}
		return;
	}

	// uuid is the canonical handle for the remote peer; node_id alone is
	// not unique across zones and would mis-resolve the entity slot.
	const auto peerEntityOpt = NodeUtils::FindNodeEntityByUuid(nodeType, response.peer_node().node_uuid());
	if (!peerEntityOpt) {
		LOG_WARN << "OnHandshakeReplied: peer entity not found for uuid="
			<< response.peer_node().node_uuid()
			<< " in " << NodeUtils::GetRegistryName(registry);
		return;
	}
	registry.remove<TimerTaskComp>(*peerEntityOpt);
	TriggerNodeConnectionEvent(registry, response);
	LOG_INFO << "Node registration success.";
}


void NodeHandshakeManager::TriggerNodeConnectionEvent(entt::registry& registry, const NodeHandshakeResponse& response) const {
	for (const auto& [entity, client, nodeInfo] : registry.view<RpcClientPtr, NodeInfo>().each()) {
		if (client->peer_addr().toIp() != response.peer_node().endpoint().ip() ||
			client->peer_addr().port() != response.peer_node().endpoint().port()) {
			continue;
		}
		ConnectToNodeEvent connectEvent;
		connectEvent.set_entity(entt::to_integral(entity));
		connectEvent.set_node_type(nodeInfo.node_type());
		tlsEcs.dispatcher.trigger(connectEvent);
		registry.remove<TimerTaskComp>(entity);
		break;
	}
}
