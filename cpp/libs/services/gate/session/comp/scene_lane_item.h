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

// 会话绑定的场景路由:场景实例(scene_id)与 scene 节点实体。两项任一变了,就是换过场景或换过节点。
// 节点实体存成整数值(entt::to_integral),只在本进程内、同一会话前后比较,不跨进程。
struct SceneRouteStamp
{
	uint64_t sceneId{0};
	uint64_t sceneNode{0};

	bool operator==(const SceneRouteStamp &) const = default;
};

struct SceneLaneItem
{
	// 客户端原包。codec 每帧 New 一个新对象,持有它是安全的;它不是连接对象(AGENTS.md §11.7)。重投时原样再发一次。
	std::shared_ptr<const ClientRequest> request;
	// 收包时刻的 trace(docs/design/distributed-tracing.md §5.5):真正发出可能在上一条请求的完成回调或定时重投里,
	// 那时线程上是别的上下文。发出与回执时装回。
	tracing::TraceContext trace;
	// 收包时刻。排队与等重投的总等待从这里起算(WaitExceeded)。
	SteadyTime admittedAt{};
	// 收包时会话绑的场景路由。发出时与此刻的路由不同,就在转发信封上标 received_on_previous_route(设计 S13):
	// scene 据此不在新场景里执行含义绑在旧场景上的消息。
	SceneRouteStamp receiptRoute;
	// 回执用:不解引用 request 也能回带请求 id 的信封。
	uint64_t clientRequestId{0};
	uint32_t messageId{0};
	// 被放回队头等重投的次数。只用于计数:第一次放回计一条 held。
	uint32_t holds{0};
	// 内层方法的响应是 Empty(移动等):发出即完,scene 不回包,不占在途。
	bool oneWay{false};
};

using SessionSceneLane = SceneLane<SceneLaneItem>;

} // namespace gate_scene_lane
