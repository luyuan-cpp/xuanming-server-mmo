#pragma once
#include <cstdint>
#include <muduo/net/TcpConnection.h>

#include <entt/src/entt/entity/registry.hpp>

class NodeHandshakeRequest;
class NodeHandshakeResponse;
class RpcController;   // network/rpc_controller.h —— 本项目的 per-call ctx,不是 ::google::protobuf::RpcController

class NodeHandshakeManager {
public:
	void TryRegisterNodeSession(uint32_t nodeType, const muduo::net::TcpConnectionPtr& conn) const;

	void OnHandshakeReplied(const NodeHandshakeResponse& response) const;

	// ctx:本次 RPC 调用的上下文(所在连接等),由派发层按次构造、显式传入;勿存储。
	void OnNodeHandshake(const RpcController& ctx, const NodeHandshakeRequest& request, NodeHandshakeResponse& response) const;

	void TriggerNodeConnectionEvent(entt::registry& registry, const NodeHandshakeResponse& response) const;
};