#pragma once

// 会话路由 lane 的排队项(docs/design/client-request-ordering.md §4.2)。
//
// 单列一个头文件,是为了让 session_info_comp.h 只依赖 STL 与前置声明,不把 protobuf 头带进
// 所有包含会话结构的编译单元。装配与副作用在 cpp/nodes/gate/handler/rpc/client_request_lane.cpp。

#include <cstdint>
#include <memory>
#include <string>

#include "core/tracing/trace_context.h"
#include "router_lane.h"

class ClientRequest;

namespace gate_lane
{

struct RouterLaneItem
{
	// 客户端原包。codec 每帧 New 一个新对象,持有它是安全的;它不是连接对象(AGENTS.md §11.7)。
	std::shared_ptr<const ClientRequest> request;
	// 入队时刻的会话身份快照:序列化好的 SessionDetails,已写入 session_incarnation 与 gate_call_seq,
	// 发出时原样作为 x-session-detail-bin。取入队时刻而不是发出时刻:排队期间换了角色,
	// 请求也不会被算到新角色头上(与 Java 版 ClientSession.BackendPending 同一口径)。
	std::string sessionDetailsBin;
	// 入队时刻的 trace(docs/design/distributed-tracing.md §5.5),同样取入队时刻:真正发出可能在上一条请求的完成回调
	// 或过期扫描里,那时线程上是别的上下文。发出时装回,出站 gRPC 据此写 traceparent。头文件只依赖 STL。
	tracing::TraceContext trace;
	CallSeq seq{kNoCallSeq};
	SteadyTime enqueuedAt{};
	// 回执用:不解引用 request 也能回带请求 id 的信封。
	uint64_t clientRequestId{0};
	uint32_t messageId{0};
};

using SessionRouterLane = RouterLane<RouterLaneItem>;

} // namespace gate_lane
