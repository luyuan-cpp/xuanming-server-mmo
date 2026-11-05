#pragma once
#include <memory>
#include <muduo/net/TcpConnection.h>

class NodeInfo;
class NodeHandshakeRequest;
class NodeHandshakeResponse;
class RpcClient;
class RpcController;   // network/rpc_controller.h —— 本项目的 per-call ctx,不是 ::google::protobuf::RpcController

// 节点间 RPC0 链路的握手:出站侧(gate → scene)在 TCP 连上后发 NodeHandshake,入站侧收到后为对端挂 RpcSession。
// 仅恢复通信绑定与 Ready，不发布业务恢复事件。只在所属 EventLoop 线程使用。
class NodeHandshakeManager {
public:
	// 节点客户端使用 kAfterHandshake；每次 UP 调用一次，立即发起有截止时间的握手。
	// 仅捕获身份快照与 weak 连接/client，不依赖全局 Node、ECS 或业务完成钩子。
	static void BeginHandshake(const std::shared_ptr<RpcClient>& client, const NodeInfo& selfNode,
	                           const NodeInfo& expectedPeer, const muduo::net::TcpConnectionPtr& conn);

	// ctx:本次 RPC 调用的上下文(所在连接等),由派发层按次构造、显式传入;勿存储。
	void OnNodeHandshake(const RpcController& ctx, const NodeHandshakeRequest& request, NodeHandshakeResponse& response) const;

};
