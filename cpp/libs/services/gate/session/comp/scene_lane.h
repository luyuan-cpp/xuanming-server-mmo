#pragma once

// 会话场景 lane 的纯逻辑核心(docs/design/client-scene-request-handoff-hold.md §4)。
//
// gate 把同一会话发往 scene 的客户端消息按收包顺序发出,并对请求-响应消息单飞:一次只放一条在途,拿到 scene 的应答
// (或失败)之后才发下一条请求。scene 明确答复「确定未执行、稍后重投」(玩家归属正在交接,或还没在目标节点就绪)时,
// 这一条放回队头、按固定间隔重投,直到被受理或等待超过上限。连续换几次节点,队列一直留在 gate 的会话上。
//
// 单向消息(内层响应为 Empty,移动等)没有完成可等:排在它前面的请求都已发出就发。所以连接上的发出顺序与收包顺序
// 相同,scene 按连接顺序执行,执行顺序也就与收包顺序相同(与改造前一致);在途请求再慢也挡不住移动。
// 唯一允许单向消息越过请求的情形是队头那条请求在等重投(见 HoldForRetry)。
//
// 本文件只依赖 STL:不认识连接、RPC、ECS,也不读时钟(时刻由调用方传入),所以单测可以脱离引擎直接编译
// (cpp/nodes/gate/tests/scene_lane_test.cpp)。装配与副作用(发送、回执、在途判丢、计数、日志)在
// cpp/nodes/gate/handler/rpc/client_scene_lane.cpp。
//
// 线程模型:gate 只有一个 EventLoop,会话表与本文件的状态都只在 loop 线程上读写,不加锁。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <optional>
#include <utility>
#include <vector>

namespace gate_scene_lane
{

using SteadyTime = std::chrono::steady_clock::time_point;

// gate 给每次发往 scene 的请求-响应调用编的号:进程内单调,每次发出(含重投)都换新号。0 保留,表示「没有在途」。
using CallSeq = uint64_t;
inline constexpr CallSeq kNoCallSeq = 0;

enum class AdmitResult : uint8_t
{
	kQueued,	   // 已排入队尾
	kRejectedFull, // 排队 + 在途已达上限
};

// 一个会话的场景 lane。Item 由调用方定义,必须有 `bool oneWay` 成员(单向消息:发出即完,不占在途)。
// 不变量:
//   - 同一时刻至多一条在途请求;在途那一条由 lane 保管(重投要用),Complete 交还给调用方;
//   - 队头在等重投(Holding)期间不发出任何请求,排在后面的请求保持收包顺序;
//   - Holding 为真时队列非空,队头就是等重投的那一条,队列里没有单向消息。
template <typename Item>
class SceneLane
{
public:
	// 准入:排到队尾。depthLimit 是排队 + 在途的总上限。
	AdmitResult Admit(Item item, std::size_t depthLimit)
	{
		if (Depth() >= depthLimit)
		{
			return AdmitResult::kRejectedFull;
		}
		queued_.push_back(std::move(item));
		return AdmitResult::kQueued;
	}

	// 刚到的单向消息能不能不排队、马上发出:前面没有还没发出的请求(队列为空;在途那一条已经在连接上排在它前面),
	// 或者队头那条请求在等重投(理由见 HoldForRetry)。
	bool OneWayMaySkipQueue() const { return queued_.empty() || Holding(); }

	// 取出下一条可以发出的消息,没有返回 nullopt:
	//   - 队头是单向消息:可以发(它前面的请求都已发出);
	//   - 队头是请求:没有在途,且不在等重投或重投时刻已到。
	// 取出之后调用方必须处置这一条:请求发出成功就 MarkSent;其余情形(单向消息已发出、已回执)再次调用本函数继续取下一条。
	std::optional<Item> TakeNextIfReady(SteadyTime now)
	{
		if (queued_.empty())
		{
			return std::nullopt;
		}
		if (!queued_.front().oneWay)
		{
			if (inFlight_ != kNoCallSeq)
			{
				return std::nullopt;
			}
			if (retryAt_.has_value() && now < *retryAt_)
			{
				return std::nullopt;
			}
			retryAt_.reset();
		}
		Item item = std::move(queued_.front());
		queued_.pop_front();
		return item;
	}

	// 标记这一条请求已发出,并把它交给 lane 保管。
	// 前置条件:当前没有在途,且 seq 不为 kNoCallSeq。违反前置条件返回 false、不改状态(item 被丢弃)。
	bool MarkSent(Item item, CallSeq seq)
	{
		if (inFlight_ != kNoCallSeq || seq == kNoCallSeq)
		{
			return false;
		}
		inFlight_ = seq;
		inFlightItem_ = std::move(item);
		return true;
	}

	// 认领完成(应答、失败,或装配层判丢)。只有 seq 等于当前在途序号时才交还在途那一条并回到空闲;
	// 其他序号(迟到、重复、串号)返回 nullopt,保证每个请求恰好一个回执。
	std::optional<Item> Complete(CallSeq seq)
	{
		if (seq == kNoCallSeq || seq != inFlight_)
		{
			return std::nullopt;
		}
		inFlight_ = kNoCallSeq;
		return std::exchange(inFlightItem_, std::nullopt);
	}

	// 这一条请求没被受理、确定未执行:放回队头,retryAt 之前不再发出任何请求。前置条件:当前没有在途(item 来自 Complete)。
	//
	// 排在它后面的单向消息交还给调用方马上发出(按收包顺序),之后到达的单向消息也不再等它(OneWayMaySkipQueue)。
	// 理由:能被放回重投的请求只读写玩家自身的状态、与所在场景无关(scene 侧的白名单),它与移动谁先谁后不改变任何结果;
	// 反过来让移动等它,落地之后会把旧场景里攒下的移动一次性放进新场景,或者把落地后最初一段的移动丢掉。
	// 单向消息照旧发到会话此刻的路由上,由 scene 自己决定收不收(交接冻结中、实体没载入时 scene 不处理),与改造前相同。
	// 排在它后面的请求留在队列里,保持收包顺序。
	[[nodiscard]] std::vector<Item> HoldForRetry(Item item, SteadyTime retryAt)
	{
		std::vector<Item> oneWay;
		std::deque<Item> requests;
		for (auto &queued : queued_)
		{
			if (queued.oneWay)
			{
				oneWay.push_back(std::move(queued));
			}
			else
			{
				requests.push_back(std::move(queued));
			}
		}
		queued_ = std::move(requests);
		queued_.push_front(std::move(item));
		retryAt_ = retryAt;
		return oneWay;
	}

	// 交出全部排队项(含等重投的那一条)并结束等重投;在途那一条不动,照常由 Complete 认领。
	// 会话转去别的 gate 时用:这些消息都没有发出(或没有被受理),调用方逐条处置。
	[[nodiscard]] std::deque<Item> TakeAllQueued()
	{
		retryAt_.reset();
		return std::exchange(queued_, std::deque<Item>{});
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
	std::optional<SteadyTime> retryAt_;
};

// 从收包起等待是否已超过上限:超过就不再发出(或不再重投)。请求回带请求 id 的 1003,明确「没有执行」,客户端重试安全;
// 单向消息直接丢(过时的移动没有意义)。
// 不变式 I6:gate→scene 调用超时 + maxWait + 1000 ≤ 客户端超时(client-scene-request-handoff-hold.md §5)。
inline bool WaitExceeded(SteadyTime admittedAt, SteadyTime now, std::chrono::milliseconds maxWait)
{
	return now - admittedAt > maxWait;
}

} // namespace gate_scene_lane
