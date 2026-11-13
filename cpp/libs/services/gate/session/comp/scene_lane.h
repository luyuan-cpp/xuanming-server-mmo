#pragma once

// 会话场景 lane 的纯逻辑核心(docs/design/client-scene-request-handoff-hold.md §4)。
//
// gate 把同一会话发往 scene 的客户端消息按收包顺序单飞:请求-响应类一次只放一条在途,拿到 scene 的应答(或失败)之后才发
// 下一条;单向消息(内层响应为 Empty,例如移动)轮到时直接发出、不等完成。scene 明确答复「确定未执行、稍后重投」
// (玩家归属正在交接,或还没在目标节点就绪)时,这一条放回队头、按固定间隔重投,直到被受理或等待超过上限。
// 这样玩家换场景节点的窗口里,客户端请求不丢、不乱序,也不需要客户端自己重发;连续换几次节点时队列一直留在 gate 的会话上。
//
// 本文件只依赖 STL:不认识连接、RPC、ECS,也不读时钟(时刻由调用方传入),所以单测可以脱离引擎直接编译
// (cpp/nodes/gate/tests/scene_lane_test.cpp)。装配与副作用(发送、回执、计数、日志)在
// cpp/nodes/gate/handler/rpc/client_scene_lane.cpp。
//
// 线程模型:gate 只有一个 EventLoop,会话表与本文件的状态都只在 loop 线程上读写,不加锁。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <optional>
#include <utility>

namespace gate_scene_lane
{

using SteadyTime = std::chrono::steady_clock::time_point;

// gate 给每次发往 scene 的请求-响应调用编的号:进程内单调,每次发出(含重投)都换新号。0 保留,表示「没有在途」。
using CallSeq = uint64_t;
inline constexpr CallSeq kNoCallSeq = 0;

enum class AdmitResult : uint8_t
{
	kQueued,			  // 已排入队尾
	kRejectedFull,		  // 排队 + 在途已达上限
	kDroppedOneWayOnHold, // 队头在等重投(玩家归属交接中):单向消息有时效,不排队,直接丢
};

// 一个会话的场景 lane。Item 由调用方定义,必须有 `bool oneWay` 成员(单向消息:发出即算完成,不占在途)。
// 不变量:
//   - 同一时刻至多一条在途;在途那一条由 lane 保管(重投要用),Complete / ExpireInFlightIfLost 交还给调用方;
//   - 队头在等重投(Holding)期间不发出任何消息,排在后面的请求保持收包顺序;
//   - Holding 为真时队列非空,队头就是等重投的那一条。
template <typename Item>
class SceneLane
{
public:
	// 准入。depthLimit 是排队 + 在途的总上限。队头在等重投时,单向消息不入队(kDroppedOneWayOnHold)。
	AdmitResult Admit(Item item, std::size_t depthLimit)
	{
		if (item.oneWay && Holding())
		{
			return AdmitResult::kDroppedOneWayOnHold;
		}
		if (Depth() >= depthLimit)
		{
			return AdmitResult::kRejectedFull;
		}
		queued_.push_back(std::move(item));
		return AdmitResult::kQueued;
	}

	// 可以发出下一条时取出队头:没有在途、队列非空,且不在等重投(或重投时刻已到)。否则返回 nullopt。
	// 取出之后调用方必须处置这一条:请求-响应类发出成功就 MarkSent;要等重投就 HoldForRetry;
	// 其余情形(单向消息已发出、已回执、已丢弃)再次调用本函数继续取下一条。
	std::optional<Item> TakeNextIfReady(SteadyTime now)
	{
		if (inFlight_ != kNoCallSeq || queued_.empty())
		{
			return std::nullopt;
		}
		if (retryAt_.has_value() && now < *retryAt_)
		{
			return std::nullopt;
		}
		retryAt_.reset();
		Item item = std::move(queued_.front());
		queued_.pop_front();
		return item;
	}

	// 标记一条请求-响应消息已发出,并把它交给 lane 保管。lostAfter 是「到这个时刻还没有完成回调就判丢」的本地截止。
	// 前置条件:当前没有在途,且 seq 不为 kNoCallSeq。违反前置条件返回 false、不改状态(item 被丢弃)。
	bool MarkSent(Item item, CallSeq seq, SteadyTime lostAfter)
	{
		if (inFlight_ != kNoCallSeq || seq == kNoCallSeq)
		{
			return false;
		}
		inFlight_ = seq;
		inFlightItem_ = std::move(item);
		inFlightLostAfter_ = lostAfter;
		return true;
	}

	// 认领完成。只有 seq 等于当前在途序号时才交还在途那一条并回到空闲;其他序号(迟到、重复、串号)返回 nullopt。
	std::optional<Item> Complete(CallSeq seq)
	{
		if (seq == kNoCallSeq || seq != inFlight_)
		{
			return std::nullopt;
		}
		inFlight_ = kNoCallSeq;
		return std::exchange(inFlightItem_, std::nullopt);
	}

	// 在途那一条过了本地截止仍没有完成回调:判丢,交还给调用方回执。没有在途或还没到截止返回 nullopt。
	// 之后迟到的完成回调因序号对不上被 Complete 忽略,保证每个请求恰好一个回执。
	std::optional<Item> ExpireInFlightIfLost(SteadyTime now)
	{
		if (inFlight_ == kNoCallSeq || now < inFlightLostAfter_)
		{
			return std::nullopt;
		}
		inFlight_ = kNoCallSeq;
		return std::exchange(inFlightItem_, std::nullopt);
	}

	// 这一条没被受理、确定未执行:放回队头,retryAt 之前不再发出任何消息。排队中的单向消息有时效,一并清掉,
	// 返回清掉的条数。前置条件:当前没有在途(item 来自 Complete 或 TakeNextIfReady)。
	std::size_t HoldForRetry(Item item, SteadyTime retryAt)
	{
		const std::size_t purged = std::erase_if(queued_, [](const Item &queued) { return queued.oneWay; });
		queued_.push_front(std::move(item));
		retryAt_ = retryAt;
		return purged;
	}

	// 队头是否在等重投。
	bool Holding() const { return retryAt_.has_value(); }
	// 没有在途、没有排队(因而也不在等重投)。
	bool Empty() const { return inFlight_ == kNoCallSeq && queued_.empty(); }
	CallSeq InFlightSeq() const { return inFlight_; }
	std::size_t QueuedCount() const { return queued_.size(); }
	std::size_t Depth() const { return queued_.size() + (inFlight_ == kNoCallSeq ? 0 : 1); }

private:
	std::deque<Item> queued_;
	CallSeq inFlight_{kNoCallSeq};
	std::optional<Item> inFlightItem_;
	SteadyTime inFlightLostAfter_{};
	std::optional<SteadyTime> retryAt_;
};

// 从收包起等待是否已超过上限:超过就不再发出(或不再重投),回带请求 id 的 1003,明确「没有执行」,客户端重试安全。
// 不变式:gate→scene 调用超时 + maxWait + 1000 ≤ 客户端超时(client-scene-request-handoff-hold.md §5)。
inline bool WaitExceeded(SteadyTime admittedAt, SteadyTime now, std::chrono::milliseconds maxWait)
{
	return now - admittedAt > maxWait;
}

} // namespace gate_scene_lane
