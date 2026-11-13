#pragma once

#include <chrono>
#include <cstdint>

// 未验签连接的占位(docs/design/gate-connection-admission-control.md P1-4 的 gate_unauthenticated_connections)。
//
// 一条客户端连接从建连到验签成功之前占一个位:构造时计数加一,析构时减一。会话把它放在 shared_ptr 里
// (SessionInfo::preAuthSlot):验签成功时 reset;会话无论从哪条路径销毁(断开回调、无连接残留会话的就地摘除、
// 进程退出清表),占位都随之归还。计数不靠各个出口手写加减,所以漏不掉、不会漂移。
// 放在 shared_ptr 里而不是让本类可移动、直接作成员:那样会把 SessionInfo 变成只能移动,改掉它原有的拷贝语义。
// 会话真被拷贝时各份共享同一个占位,最后一份销毁才归还,不会减两次。
//
// 计数器由创建方给(gate 是 nodes/gate/handler/rpc/connection_admission.cpp 里 loop 线程的一个 thread_local 整数),
// 必须比所有占位活得久。本类不加锁:只在创建它的那个线程上构造与析构。
class PreAuthSlot
{
public:
	using SteadyTime = std::chrono::steady_clock::time_point;

	PreAuthSlot(uint64_t &heldCount, SteadyTime openedAt) : heldCount_(heldCount), openedAt_(openedAt) { ++heldCount_; }
	~PreAuthSlot() { --heldCount_; }
	PreAuthSlot(const PreAuthSlot &) = delete;
	PreAuthSlot &operator=(const PreAuthSlot &) = delete;

	// 建连时刻(单调时钟):验签成功时用它算握手耗时。
	SteadyTime OpenedAt() const { return openedAt_; }

private:
	uint64_t &heldCount_;
	SteadyTime openedAt_;
};
