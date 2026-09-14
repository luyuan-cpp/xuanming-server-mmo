#include "muduo/base/Logging.h"
#include "muduo/net/TcpConnection.h"

#include "gate_codec.h"
#include "network/codec/dispatcher.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "thread_context/redis_manager.h"
#include "core/utils/defer/defer.h"
#include "session/manager/session_manager.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "proto/common/base/message.pb.h"

using namespace muduo;
using namespace muduo::net;

extern ProtobufDispatcher gRpcResponseDispatcher;

void OnServiceRouteNodeStringMsgReply(const TcpConnectionPtr& conn, const std::shared_ptr<::RouteMessageResponse>& replied, Timestamp timestamp)
{
	///<<< BEGIN WRITING YOUR CODE
	// 响应路径没有 CallMethod,也就没有 per-call RpcController;本函数已显式收到 conn,
	// 会话 id 直接取 replied->session_id()。原先写进 thread_local tlsRpc 的路由/会话字段
	// 全仓无读者,随 tlsRpc 一并移除(见 docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §7.2)。
	if (replied->route_nodes_size() <= 0)
	{
		LOG_ERROR << "msg list empty:" << replied->DebugString();
		return;
	}
	
	auto& route_data = replied->route_nodes(replied->route_nodes_size() - 1);
	if (route_data.message_id() >= gRpcMethodRegistry.size())
	{
		LOG_ERROR << "message_id not found " << route_data.message_id();
		return;
	}
	// Gate differs from other nodes: send response directly to client
	const auto it = tlsSessionManager.sessions().find(replied->session_id());
	if (it == tlsSessionManager.sessions().end())
	{
		LOG_ERROR << "conn id not found, session id: " << replied->session_id();
		return;
	}

	const auto clientConn = it->second.conn.lock();
	if (!clientConn)
	{
		LOG_DEBUG << "client connection already released, session id: " << replied->session_id();
		return;
	}
	MessageContent message;
	message.set_serialized_message(replied->body());
	message.set_message_id(route_data.message_id());
	GetGateCodec().send(clientConn, message);
	///<<< END WRITING YOUR CODE
}

void OnRoutePlayerStringMsgReply(const TcpConnectionPtr& conn, const std::shared_ptr<::RoutePlayerMessageResponse>& replied, Timestamp timestamp)
{
	///<<< BEGIN WRITING YOUR CODE
	///<<< END WRITING YOUR CODE
}

