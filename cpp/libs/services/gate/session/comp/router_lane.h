#pragma once

// 会话路由 lane 的纯逻辑核心(docs/design/client-request-ordering.md §4)。
//
// gate 对同一会话经路由服转发的 gRPC 类请求按收包顺序单飞:上一条观察到完成(成功回包 / 失败回调 /
// 本地截止到期;路由服节点摘除经 ~Channel 的 UNAVAILABLE 走失败回调)之后才发下一条。不同会话之间互不影响;
// 路由服与业务服保持无状态、随机分配。
//
// 本文件只依赖 STL(含同样只依赖 STL 的 engine/core utils/data_structures/inflight_index.h):不认识连接、gRPC、ECS,也不读时钟(时刻由调用方传入),所以单测可以脱离引擎直接编译
// (cpp/nodes/gate/tests/router_lane_test.cpp)。装配与副作用(发送、回执、日志)在
// cpp/nodes/gate/handler/rpc/client_request_lane.cpp。
//
// 线程模型:gate 只有一个 EventLoop,会话表与本文件的状态都只在 loop 线程上读写,不加锁。

#include <algorithm>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <functional>
#include <optional>
#include <queue>
#include <type_traits>
#include <unordered_map>
#include <utility>
#include <vector>

#include "utils/data_structures/inflight_index.h"

namespace gate_lane
{

using SteadyTime = std::chrono::steady_clock::time_point;

// gate 自己发的调用序号:进程内单调,准入时按收包顺序分配。0 保留,表示「不受 lane 管理的调用」
// (例如 gate 合成的断线通知),认领时一律不当作 lane 的完成。
using CallSeq = uint64_t;
static_assert(std::is_same_v<CallSeq, InflightCallId>);
static_assert(std::is_same_v<SteadyTime, InflightSteadyTime>);
inline constexpr CallSeq kNoCallSeq = 0;

enum class AdmitResult : uint8_t
{
	kQueued,			 // 已排入队尾
	kRejectedFull,		 // 排队 + 在途已达上限:回带请求 id 的 1008,不计非法包
	kRejectedRedirected, // 会话已被重定向到别的 gate:回带请求 id 的 1003
};

// 一个会话的路由 lane。Item 由调用方定义(请求原包、身份快照、序号、入队时刻)。
// 不变量:同一时刻至多一条在途;在途期间不出队;Complete 只认当前在途的那个序号。
template <typename Item>
class RouterLane
{
public:
	// 准入。depthLimit 是排队 + 在途的总上限。
	AdmitResult Admit(Item item, std::size_t depthLimit)
	{
		if (redirected_)
		{
			return AdmitResult::kRejectedRedirected;
		}
		if (Depth() >= depthLimit)
		{
			return AdmitResult::kRejectedFull;
		}
		queued_.push_back(std::move(item));
		return AdmitResult::kQueued;
	}

	// 空闲时取出队头交给调用方处理(发出,或因排队超时 / 无路由服而直接回执);不空闲或队列为空返回 nullopt。
	// 取出之后调用方必须二选一:发出成功就 MarkSent;没发出就再次调用本函数继续取下一条。
	std::optional<Item> TakeNextIfIdle()
	{
		if (inFlight_ != kNoCallSeq || queued_.empty())
		{
			return std::nullopt;
		}
		Item item = std::move(queued_.front());
		queued_.pop_front();
		return item;
	}

	// 标记已发出。前置条件:当前空闲,且 seq 不为 kNoCallSeq。违反前置条件返回 false、不改状态。
	bool MarkSent(CallSeq seq)
	{
		if (inFlight_ != kNoCallSeq || seq == kNoCallSeq)
		{
			return false;
		}
		inFlight_ = seq;
		return true;
	}

	// 认领完成。只有 seq 等于当前在途序号时才返回 true 并回到空闲;其他序号(迟到、重复、串号)一律忽略。
	bool Complete(CallSeq seq)
	{
		if (seq == kNoCallSeq || seq != inFlight_)
		{
			return false;
		}
		inFlight_ = kNoCallSeq;
		return true;
	}

	// 会话转去别的 gate:之后的准入一律拒绝;全部排队项按收包顺序取出交给调用方。它们从未发出、确定未执行,
	// 调用方要逐条回执(O4「每个请求恰好一个回执」、O9:回带请求 id 的 1003,客户端到新 gate 上重试安全)。
	// 在途那一条照常认领。再次调用返回空。
	[[nodiscard]] std::deque<Item> MarkRedirected()
	{
		redirected_ = true;
		return std::exchange(queued_, std::deque<Item>{});
	}

	// 排队项(不含在途那一条)里有没有满足 pred 的。至多扫一遍队列(≤ 深度上限)。
	template <typename Pred>
	bool AnyQueued(Pred pred) const
	{
		return std::any_of(queued_.begin(), queued_.end(), pred);
	}

	// 丢弃全部排队项(断线时用:连接已没了,回执无处可发),返回丢弃的条数。在途序号不变,由调用方从在途索引里注销。
	std::size_t DropQueued()
	{
		const std::size_t dropped = queued_.size();
		queued_.clear();
		return dropped;
	}

	CallSeq InFlightSeq() const { return inFlight_; }
	bool Idle() const { return inFlight_ == kNoCallSeq; }
	bool Redirected() const { return redirected_; }
	std::size_t QueuedCount() const { return queued_.size(); }
	std::size_t Depth() const { return queued_.size() + (inFlight_ == kNoCallSeq ? 0 : 1); }

private:
	std::deque<Item> queued_;
	CallSeq inFlight_{kNoCallSeq};
	bool redirected_{false};
};

// 进程级在途索引:按 gate 序号认领,保证每个调用恰好被处理一次;另带一个按截止时刻排序的过期堆。
// Call 由调用方定义,必须有 `SteadyTime expiresAt` 成员。
//
// 为什么要过期堆:最初是为了官方 gRPC 下的 F29(client-access-band-routing.md)—— 路由服节点摘除时实体上的
// 完成队列随之析构,在途调用既不触发成功回调也不触发失败回调。第 3 批起传输换成 muduo_grpc
// (docs/design/grpc-muduo-transport.md §14):节点摘除时 ~Channel 以 UNAVAILABLE 投递在途调用的失败回调,
// 截止到期由通道自己报 DEADLINE_EXCEEDED,完成回调恰好一次,F29 这条理由已不成立。
// 现在它兜的是「调用的完成没被 lane 认领」:一是传输层「恰好一次」契约出缺陷、回调丢了;二是回调到了但认领不了 ——
// 路由服的成功回包缺 x-session-detail-bin 回显、回显解析失败或不带 gate_call_seq(版本不符,见 router_muduo_forward.cpp
// 的 LogMissingSessionEchoSampled)。两种情况下队头都会卡住,而客户端在等回包、不会再发新请求,纯惰性清理永远触发
// 不到(client-request-ordering.md §4.5),主动扫描是会话能继续往下走的唯一兜底。正常运行时扫描不应摘出任何调用;
// router_lane_expired 增长时先对照「no x-session-detail-bin echo」那条 WARN。
//
// 实现已上移到 engine/core(utils/data_structures/inflight_index.h),与节点间 RPC0 的 RpcChannel::outstandings_
// 共用同一份(docs/design/muduo-rpc-channel.md §4.4)。语义不变:Insert 拒收 0 与重复;Extract 恰好一次;
// 过期堆惰性删除。SteadyTime / CallSeq 与引擎的 InflightSteadyTime / InflightCallId 是同一类型。
// (按路由服实体批量摘除的 ExtractIf 已删:节点摘除改由 ~Channel 的 UNAVAILABLE 失败回调逐条认领,见 client_request_lane.h。)
template <typename Call>
using InflightIndex = ::InflightIndex<Call>;

// 排队等待是否已超过上限:超过就不再发出,直接回带请求 id 的 1003(明确「没有执行」,客户端重试安全)。
// 不变式 I4:gate deadline(路由服)+ maxWait + 1000 ≤ 客户端超时(client-request-ordering.md §5.2)。
inline bool QueueWaitExceeded(SteadyTime enqueuedAt, SteadyTime now, std::chrono::milliseconds maxWait)
{
	return now - enqueuedAt > maxWait;
}

} // namespace gate_lane
