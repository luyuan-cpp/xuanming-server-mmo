#pragma once

// battle 对外发布的可分配状态(R6,docs/design/global-match-hierarchical-pools.md §5.1):
// 把「此刻接不接新房间」写进 etcd 里本节点的 NodeInfo.admission(proto/common/base/common.proto 字段 14),
// match 选点时排除 CLOSED 的节点(go/match/internal/battleplace)。这只是提示:建房准入闸(battle_admission_gate.h)
// 与 Agones 分配许可仍是唯一的准入判定;提示过时最多让 match 白拨一次(battle_not_allocatable,无副作用,match 熔断该实例)。
//
// 「怎么可靠地写进 etcd」(只在变化时写、按回读对账、丢写自愈)与 scene 共用,在 core 的
// node/system/node/node_admission_advertise.h(决策)与 node_admission_publisher.h(写 etcd 的胶水);
// 这里只留 battle 自己的判定:什么时候算接新房间。名字照旧从 battle_admission 命名空间可见,调用方与单测不用改。
//
// 与 battle_admission_gate.h 同一条纪律:不依赖 muduo / protobuf / 引擎,不打日志,单测独立编译
// (tests/battle_admission_gate_test.cpp)。线程模型:不加锁,只在 loop 线程调用(main.cpp 的对账定时器与关闸当场)。

#include "battle_admission_gate.h"
#include "node/system/node/node_admission_advertise.h"

namespace battle_admission
{

using node_admission::Advertised;
using node_admission::AdvertiseAction;
using node_admission::AdvertisePolicy;
using node_admission::ToString;

// 本节点此刻是否接新房间:建房准入闸开着,且 Agones 会放行分配许可。
// lifecycleGrantsPermits 由调用方按 GameServerLifecycle 判定(BattleNodeImpl::LifecycleGrantsPermits),本头不依赖 Agones。
constexpr bool AcceptsNewRooms(const AdmissionPhase phase, const bool lifecycleGrantsPermits)
{
	return phase == AdmissionPhase::kOpen && lifecycleGrantsPermits;
}

// 期望发布的状态。只会是 kOpen / kClosed。
constexpr Advertised DesiredAdvertised(const AdmissionPhase phase, const bool lifecycleGrantsPermits)
{
	return AcceptsNewRooms(phase, lifecycleGrantsPermits) ? Advertised::kOpen : Advertised::kClosed;
}

} // namespace battle_admission
