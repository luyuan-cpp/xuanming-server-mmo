#pragma once

// 把本节点的可分配状态写进 etcd 里的 NodeInfo.admission,并按 watch 回读对账。决策规则与「为什么只在变化时写」见
// node_admission_advertise.h;battle(main.cpp ReconcileAdvertisedAdmission)与 scene(scene_planned_drain.cpp)共用。
// 只在 loop 线程调用(MutableNodeInfo 断言 loop 线程)。

#include <chrono>
#include <optional>

#include "node/system/node/node.h"
#include "node/system/node/node_admission_advertise.h"
#include "proto/common/base/common.pb.h"

namespace node_admission
{

inline NodeAdmission ToProto(const Advertised advertised)
{
	switch (advertised)
	{
	case Advertised::kOpen:
		return NODE_ADMISSION_OPEN;
	case Advertised::kClosed:
		return NODE_ADMISSION_CLOSED;
	case Advertised::kUnspecified:
		return NODE_ADMISSION_UNSPECIFIED;
	}
	return NODE_ADMISSION_UNSPECIFIED;
}

inline Advertised FromProto(const NodeAdmission admission)
{
	switch (admission)
	{
	case NODE_ADMISSION_OPEN:
		return Advertised::kOpen;
	case NODE_ADMISSION_CLOSED:
		return Advertised::kClosed;
	default:
		// UNSPECIFIED,以及更新版本才有的取值:都按「没有上报」处理,与期望值不一致即重写。
		return Advertised::kUnspecified;
	}
}

// etcd 回读:发现快照里本节点(同 uuid)的登记。节点要 watch 自己那一类的发现前缀才看得到自己(battle 为劫持检测与
// 注册自检 watch 自己的键,scene 为同类自检与选号 watch 本 zone 的 scene)。回读里还看不到自己的键(首发未回、已失租)
// 返回 nullopt。
inline std::optional<Advertised> Observed(Node &node)
{
	for (const auto &registered : node.GetServiceDiscoveryManager().ServiceNodes().NodesOfType(node.GetNodeType()))
	{
		if (node.IsCurrentNode(registered))
		{
			return FromProto(registered.admission());
		}
	}
	return std::nullopt;
}

// 一拍对账的结果:本拍做了什么、回读到的值。日志由调用方按自己的上下文打。
struct ReconcileResult
{
	AdvertiseAction action{AdvertiseAction::kNone};
	std::optional<Advertised> observed;
};

// 一拍对账。内存里的 NodeInfo 恒为期望值:首发与重注册整份发布它,带上的就是最新状态;etcd 里的值按回读对账,
// 只在不一致且注册链空闲时写(UpdateNodeInfo:带租约的整份 Put)。
inline ReconcileResult Reconcile(Node &node, const Advertised desired, AdvertisePolicy &policy)
{
	node.MutableNodeInfo().set_admission(ToProto(desired));

	auto &etcd = node.GetServiceDiscoveryManager().etcdService;
	// 传输为空 = etcd 服务还没起或已经停了(停机收尾 CloseTransportOwnersInLoop 之后),写了也只会被丢弃。
	const bool registrationIdle = etcd.Transport() != nullptr && etcd.IsIdentityConfirmedFresh() &&
								  !etcd.RegistrationInFlight();
	ReconcileResult result;
	result.observed = Observed(node);
	result.action = policy.Decide(desired, result.observed, registrationIdle, std::chrono::steady_clock::now());
	if (result.action != AdvertiseAction::kNone)
	{
		node.GetEtcdManager().UpdateNodeInfo();
	}
	return result;
}

} // namespace node_admission
