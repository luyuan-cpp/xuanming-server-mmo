#pragma once
#include <cstdint>
#include <memory>
#include <string>
#include <muduo/net/TcpConnection.h>

class NodeInfo;
class NodeHandshakeRequest;
class NodeHandshakeResponse;
class RpcClient;
class RpcController;   // network/rpc_controller.h —— 本项目的 per-call ctx,不是 ::google::protobuf::RpcController

// 节点间 RPC0 链路的握手:出站侧(gate → scene)在 TCP 连上后发 NodeHandshake,入站侧收到后为对端挂 RpcSession。
// 双向核对实例身份:请求点名要连的对端实例(expected_peer_uuid),入站侧不是它就拒绝、不碰注册表;
// 应答带回入站侧自己的身份,出站侧与发现记录逐项比对后才把这条连接置 Ready。
// 仅恢复通信绑定与 Ready，不发布业务恢复事件。只在所属 EventLoop 线程使用。
class NodeHandshakeManager {
public:
	// 节点客户端使用 kAfterHandshake；每次 UP 调用一次，立即发起有截止时间的握手。
	// 仅捕获身份快照与 weak 连接/client，不依赖全局 Node、ECS 或业务完成钩子。
	// 失败(超时、被拒、身份不符)时按 FailedHandshakeCloseDelaySeconds 退避后关闭这条连接,由 TcpClient 重连。
	static void BeginHandshake(const std::shared_ptr<RpcClient>& client, const NodeInfo& selfNode,
	                           const NodeInfo& expectedPeer, const muduo::net::TcpConnectionPtr& conn);

	// 握手失败后关闭失败连接前等待的秒数。consecutiveFailures 含本次、从 1 计(0 按 1 处理):
	// 首次 0.5s,每多失败一次翻倍,8s 封顶。成功握手由 RpcClient 清零计数。
	static double FailedHandshakeCloseDelaySeconds(uint32_t consecutiveFailures);

	// 入站握手是否点名本进程实例:expected_peer_uuid 非空且等于 selfUuid。纯函数,不读全局状态。
	static bool IsAddressedTo(const NodeHandshakeRequest& request, const std::string& selfUuid);

	// ctx:本次 RPC 调用的上下文(所在连接等),由派发层按次构造、显式传入;勿存储。
	void OnNodeHandshake(const RpcController& ctx, const NodeHandshakeRequest& request, NodeHandshakeResponse& response) const;

};
