#pragma once

// battle 建房准入闸:进程自己的"现在接不接新房间"(集群外入口 D82 的进程级补充)。
//
// Agones 分配许可(GameServerLifecycle::AcquireAllocationPermitBlocking)只回答"Agones 眼里
// 本实例还能不能再接单元",管不到进程自己的两段窗口:
//   * 启动窗口:etcd 发布(Node::StartGrpcServer 内)早于 SetAfterStart 启动 lifecycle,其间
//     lifecycle 还是默认的 Disabled,许可不经 allocate 直接放行。此时建出的房间会让 /ready 之后的
//     GameServer 在 Agones 眼里是 Ready,Fleet 缩容 / 滚动时可能把正在打的局直接删掉;
//   * 停机窗口:SetBeforeShutdown 作废全部房间之后,gRPC drain 仍会让已排进 loop 的 CreateBattle
//     跑完。Disabled 形态许可恒放行;Agones 形态在回 Ready 生效之前许可也可能放行 ——
//     那间房会在进程退出时无声丢失,玩家要等 scene reaper 按 deadline 解冻。
//
// 闸只朝一个方向走:kNotStarted → kOpen → kClosed。kClosed 是终态:停机已开始的节点
// 不能再被 Open 打开。被闸拒绝与许可被拒是同一个对外信号(UNAVAILABLE battle_not_allocatable):
// 都发生在任何副作用之前,match 据此换节点重试,不发 DestroyBattle。
//
// 线程模型:无锁,任何线程可读。Open / Close 都在 loop 线程调用(SetAfterStart / SetBeforeShutdown),
// 所以 loop 内的复核读(CreateBattle 投递进 loop 的任务)与 Close 天然有先后。
// release / acquire:gRPC 线程读到 kOpen 时,Open 之前 lifecycle 启动写下的状态对它可见。
//
// 与 battle_push_policy.h 同一条纪律:不依赖 muduo / protobuf / 引擎,不打日志,
// 单测独立编译(tests/battle_admission_gate_test.cpp)。

#include <atomic>
#include <cstdint>

namespace battle_admission
{

enum class AdmissionPhase : uint8_t
{
	// 节点可能已在 etcd 可见,但 Agones lifecycle 还没启动:拒绝建房。
	kNotStarted,
	// 正常接房间(仍须拿到 Agones 分配许可)。
	kOpen,
	// 停机已开始,在打的房间已全部作废:拒绝建房(终态)。
	kClosed,
};

constexpr const char *ToString(const AdmissionPhase phase)
{
	switch (phase)
	{
	case AdmissionPhase::kNotStarted:
		return "not_started";
	case AdmissionPhase::kOpen:
		return "open";
	case AdmissionPhase::kClosed:
		return "closed";
	}
	return "unknown";
}

class AdmissionGate
{
public:
	// kNotStarted → kOpen。返回是否真的打开了:已打开(重复调用)或已关闭(停机先于启动完成)时
	// 返回 false 且不改变阶段。
	bool Open()
	{
		auto expected = AdmissionPhase::kNotStarted;
		return phase_.compare_exchange_strong(expected, AdmissionPhase::kOpen, std::memory_order_acq_rel,
											  std::memory_order_acquire);
	}

	// 任意阶段 → kClosed(终态,幂等)。
	void Close() { phase_.store(AdmissionPhase::kClosed, std::memory_order_release); }

	AdmissionPhase Phase() const { return phase_.load(std::memory_order_acquire); }

private:
	std::atomic<AdmissionPhase> phase_{AdmissionPhase::kNotStarted};
};

} // namespace battle_admission
