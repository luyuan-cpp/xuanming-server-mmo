#pragma once

// gate 只连、只挑本 zone 子集里的路由服(docs/design/connection-fanout-subsetting-20261106.md §3.2)。
//
// zone z 的路由服集合 = 发现快照里的路由服按轨道分组、各取 HRW 前 k 台(router_subset_policy.h),k 由
// GATE_ROUTER_SUBSET_SIZE 给,缺省 3。gate 到路由服的连接从 G×R 降到 G×k;同 zone 的 gate 只把请求送给这 k 台,
// 路由服因此只会拨分给它的那些 zone 的 zone-scoped 目标。
//
// 只在路由模式下安装。所有入口只在 gate 的 loop 线程调用。

#include <memory>
#include <optional>

#include "entt/src/entt/entt.hpp"

class Node;

namespace gate_router_subset
{
	class Subsetter;

	// 读 GATE_ROUTER_SUBSET_SIZE(非法即 LOG_FATAL 拒启),在 node 上给 ClientRpcRouterNodeService 装建连准入与发现观察者。
	// 在 configure 阶段(StartRpcServer 之前)调一次。返回的句柄由 GateRuntimeContext 持有,它比 Node 活得久;
	// Node 上的回调只弱持有它。
	std::shared_ptr<Subsetter> Install(Node &node);

	// 给路由 lane、断线通知、排空迁移挑一台路由服:已建连(挂着未关闭的通道)∩ 本 zone 子集 ∩ 轨道可用
	// (release_track::Select),再按通道状态 READY 优先、IDLE / CONNECTING 其次。没有可挑的(或没安装)返回 nullopt,
	// 调用方回 kServiceUnavailable。
	std::optional<entt::entity> PickRouter();
} // namespace gate_router_subset
