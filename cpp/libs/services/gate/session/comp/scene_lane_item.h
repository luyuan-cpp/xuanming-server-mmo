#pragma once

// 会话场景 lane 的排队项(docs/design/client-scene-request-handoff-hold.md §4.2)。
//
// 单列一个头文件,理由同 router_lane_item.h:让 session_info_comp.h 只依赖 STL 与前置声明,不把 protobuf 头带进
// 所有包含会话结构的编译单元。装配与副作用在 cpp/nodes/gate/handler/rpc/client_scene_lane.cpp。

#include <cstdint>
#include <memory>

#include "core/tracing/trace_context.h"
#include "scene_lane.h"

class ClientRequest;

namespace gate_scene_lane
{

struct SceneLaneItem
{
	// 客户端原包。codec 每帧 New 一个新对象,持有它是安全的;它不是连接对象(AGENTS.md §11.7)。重投时原样再发一次。
	std::shared_ptr<const ClientRequest> request;
	// 收包时刻的 trace(docs/design/distributed-tracing.md §5.5):真正发出可能在上一条请求的完成回调或定时重投里,
	// 那时线程上是别的上下文。发出与回执时装回。
	tracing::TraceContext trace;
	// 收包时刻。排队与等重投的总等待从这里起算(WaitExceeded)。
	SteadyTime admittedAt{};
	// 回执用:不解引用 request 也能回带请求 id 的信封。
	uint64_t clientRequestId{0};
	uint32_t messageId{0};
	// 被放回队头等重投的次数。只用于计数:第一次放回计一条 held。
	uint32_t holds{0};
	// 内层方法的响应是 Empty(移动等):发出即算完成,没有回包,不占在途。
	bool oneWay{false};
};

using SessionSceneLane = SceneLane<SceneLaneItem>;

} // namespace gate_scene_lane
