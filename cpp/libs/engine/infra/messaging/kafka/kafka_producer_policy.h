#pragma once
#include <exception>

// KafkaProducer 的纯决策逻辑:produce() 的返回码该怎么处置、重建生产者的最小间隔、
// 失败日志的限流、发送账本。刻意做成 header-only、只依赖 RdKafka::ErrorCode 这一个枚举、不调用
// librdkafka 的任何函数,也不读墙钟(时间点一律由调用方传入)——这样不连 broker、
// 不链接 C++ 包装层就能单测,与 node_command_route.h 那一组是同一种组织方式。
// 用例在 cpp/tests/kafka_command_test/kafka_producer_policy_test.cpp。

// 见 kafka_consumer.h 顶部的长注释:包含 rdkafkacpp.h 之前必须先定义 LIBRDKAFKA_STATICLIB。
#ifndef LIBRDKAFKA_STATICLIB
#define LIBRDKAFKA_STATICLIB
#endif

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <memory>
#include <mutex>
#include <utility>
#include <vector>
#include <rdkafkacpp.h>

namespace kafka_producer_policy {

// produce() 返回之后,send() 该做什么。
enum class ProduceAction : std::uint8_t {
	// 已进入 librdkafka 的发送队列;最终成败由投递回执(dr_cb)给出。
	kAccepted,
	// 本地队列满(ERR__QUEUE_FULL)。队列里往往压着已经有结果、只是还没被 poll 取走的回执,
	// 先 poll(0) 把它们取走腾出位置,再原样重试**一次**。不阻塞等待:调用方是游戏主循环,
	// broker 不可用时每条消息都阻塞几十毫秒会把整个 tick 拖垮。
	kRetryAfterPoll,
	// 幂等生产者进入 fatal(ERR__FATAL):这个实例此后所有 produce() 都会失败,只能重建。
	kRebuildProducer,
	// 其它错误(消息过大、未知 topic/分区等):重试没有意义,原样交还调用方。
	kReject,
	kCount
};

constexpr ProduceAction ClassifyProduceResult(RdKafka::ErrorCode code) {
	switch (code) {
	case RdKafka::ERR_NO_ERROR:
		return ProduceAction::kAccepted;
	case RdKafka::ERR__QUEUE_FULL:
		return ProduceAction::kRetryAfterPoll;
	case RdKafka::ERR__FATAL:
		return ProduceAction::kRebuildProducer;
	default:
		return ProduceAction::kReject;
	}
}

// 投递回执里的这个错误,是否说明「生产者实例本身已经作废」(而不只是这一条消息没送到)。
constexpr bool IsFatalDeliveryError(RdKafka::ErrorCode code) {
	return code == RdKafka::ERR__FATAL;
}

// 重建生产者之前 purge 旧实例队列产生的回执。它们同样是「消息丢了」,要计数、要记日志,
// 但**不能**再被当成「实例需要重建」的信号——否则一次重建会把自己再触发一遍。
constexpr bool IsPurgeDeliveryError(RdKafka::ErrorCode code) {
	return code == RdKafka::ERR__PURGE_QUEUE || code == RdKafka::ERR__PURGE_INFLIGHT;
}

// 重建生产者的最小间隔。fatal 的成因若还在(例如 broker 侧日志被截断后尚未恢复),新实例可能
// 很快再次 fatal;不设间隔的话每次 send() 都会销毁、重建一整套 librdkafka 线程。
class RebuildGate {
public:
	using Clock = std::chrono::steady_clock;

	explicit RebuildGate(Clock::duration minInterval) : minInterval(minInterval) {}

	// 现在允许重建吗?允许则把 now 记为本次重建时刻。
	bool TryAcquire(Clock::time_point now) {
		if (hasLast && now - last < minInterval) {
			return false;
		}
		last = now;
		hasLast = true;
		return true;
	}

private:
	Clock::duration minInterval;
	Clock::time_point last{};
	bool hasLast = false;
};

// 失败日志限流。broker 长时间不可用时,队列里成千上万条消息会在 message.timeout.ms 到期的
// 同一刻一起失败;逐条打 ERROR 会把日志盘打满,也会把真正要看的那几行淹掉。
// 规则:每个窗口内前 burst 条逐条记录,其余只计数;被压掉的条数有三个出口,缺一不可:
//   1) 下一个窗口的第一条失败到来时,随 OnFailure 的返回值补报;
//   2) 窗口已过期、但之后**不再有失败**(这恰恰是故障恢复后的正常情形):由调用方在自己的
//      驱动点上调 TakeExpiredSuppressed 取走补报。少了这一条,一次突发里除前 burst 条之外的
//      全部丢失都不会出现在日志里;
//   3) 实例重建 / 析构:TakeSuppressed 无条件取走收尾。
// 准确的总数永远以 kafka_producer_stats 的计数器为准。
class FailureLogThrottle {
public:
	using Clock = std::chrono::steady_clock;

	struct Decision {
		bool logThisOne = false;                   // 这一条要不要逐条记录
		std::uint64_t suppressedBeforeThis = 0;    // 非 0 = 先补报一行「上个窗口压掉了 N 条」
	};

	FailureLogThrottle(std::uint32_t burst, Clock::duration window) : burst(burst), window(window) {}

	Decision OnFailure(Clock::time_point now) {
		Decision decision;
		if (!windowOpen || now - windowStart >= window) {
			decision.suppressedBeforeThis = suppressed;
			windowStart = now;
			windowOpen = true;
			loggedInWindow = 0;
			suppressed = 0;
		}
		if (loggedInWindow < burst) {
			++loggedInWindow;
			decision.logThisOne = true;
		} else {
			++suppressed;
		}
		return decision;
	}

	// 还没补报的条数。只读,给调用方做廉价预检(为 0 就不必取时钟),不改变任何状态。
	std::uint64_t PendingSuppressed() const { return suppressed; }

	// 窗口已过期且有压掉的条数 → 取走并清零,同时关窗(下一条失败开新窗口,不会重复补报);
	// 否则返回 0 且不改变任何状态。
	std::uint64_t TakeExpiredSuppressed(Clock::time_point now) {
		if (!windowOpen || suppressed == 0 || now - windowStart < window) {
			return 0;
		}
		const std::uint64_t taken = suppressed;
		suppressed = 0;
		windowOpen = false;
		return taken;
	}

	// 无条件取走压掉的条数并清零(实例重建 / 析构时收尾)。窗口本身不动:
	// 同一窗口内后续的失败照旧按 burst 规则处理。
	std::uint64_t TakeSuppressed() {
		const std::uint64_t taken = suppressed;
		suppressed = 0;
		return taken;
	}

private:
	std::uint32_t burst;
	Clock::duration window;
	Clock::time_point windowStart{};
	bool windowOpen = false;
	std::uint32_t loggedInWindow = 0;
	std::uint64_t suppressed = 0;
};

// 每个producer线程的全部发送账本。只读快照不解除准入。
// 失败/超时/purge一律保留UNKNOWN；重建生产者不清账，不能由outq_len==0推断永久闭合。
class PublicationFrontier {
public:
	void Accepted() {
		if (accepted == std::numeric_limits<std::uint64_t>::max()) { unknown = true; return; }
		++accepted;
	}
	void Delivered(bool confirmed) {
		if (!confirmed || this->confirmed == accepted) { unknown = true; return; }
		++this->confirmed;
	}
	void MarkUnknown() { unknown = true; }
	bool Settled() const { return !unknown && accepted == confirmed; }
	std::uint64_t AcceptedCount() const { return accepted; }
	std::uint64_t ConfirmedCount() const { return confirmed; }
	bool HasUnknown() const { return unknown; }
private:
	std::uint64_t accepted = 0;
	std::uint64_t confirmed = 0;
	bool unknown = false;
};

// 跨线程只共享账本，不跨线程调用 librdkafka。句柄仍由创建线程 flush/reset/join。
// CloseAdmission 与 BeginSend 由同一锁线性化；任何线程未 join、失败回执或溢出均不能签闭合。
class PublicationRegistry {
public:
    struct Owner {
        PublicationFrontier frontier;
        std::uint64_t activeSends = 0, activeCallbacks = 0;
        bool handleOpen = false;
        bool closed = false;
    };
    struct Snapshot {
        std::uint64_t accepted = 0;
        std::uint64_t confirmed = 0;
        std::uint64_t activeSends = 0, activeCallbacks = 0;
        std::uint64_t openHandles = 0;
        bool closed = false;
        bool unknown = false;
        bool Settled() const { return closed && !unknown && activeSends == 0 && activeCallbacks == 0 && openHandles == 0 && accepted == confirmed; }
    };
    using OwnerPtr = std::shared_ptr<Owner>;

    OwnerPtr Register() {
        std::lock_guard<std::mutex> lock(mutex);
        auto owner = std::make_shared<Owner>();
        owner->closed = closed;
        // 有限进程线程目录；超界永久 HOLD，不在排空时静默漏掉新 owner。
        if (owners.size() >= 4096) { unknown = true; owner->closed = true; }
        else { owners.push_back(owner); }
        return owner;
    }
    bool BeginSend(const OwnerPtr& owner) {
        std::lock_guard<std::mutex> lock(mutex);
        if (closed || owner->closed) { return false; }
        if (owner->activeSends == std::numeric_limits<std::uint64_t>::max()) { unknown = true; return false; }
        ++owner->activeSends;
        return true;
    }
    void EndSend(const OwnerPtr& owner) {
        std::lock_guard<std::mutex> lock(mutex);
        if (owner->activeSends == 0) { unknown = true; return; }
        --owner->activeSends;
    }
    bool BeginCallback(const OwnerPtr& owner) {std::lock_guard<std::mutex> lock(mutex);if(owner->activeCallbacks==std::numeric_limits<std::uint64_t>::max()){unknown=true;return false;}++owner->activeCallbacks;return true;}
    void EndCallback(const OwnerPtr& owner,bool known) {std::lock_guard<std::mutex> lock(mutex);if(!known)owner->frontier.MarkUnknown();if(owner->activeCallbacks==0){unknown=true;return;}--owner->activeCallbacks;}
    bool OwnerCallbacksActive(const OwnerPtr& owner)const{std::lock_guard<std::mutex> lock(mutex);return owner->activeCallbacks!=0;}
    void Accepted(const OwnerPtr& owner) { std::lock_guard<std::mutex> lock(mutex); owner->frontier.Accepted(); }
    void Delivered(const OwnerPtr& owner, bool confirmed) { std::lock_guard<std::mutex> lock(mutex); owner->frontier.Delivered(confirmed); }
    bool OwnerSettled(const OwnerPtr& owner) const { std::lock_guard<std::mutex> lock(mutex); return owner->activeCallbacks==0 && owner->frontier.Settled(); }
    void MarkUnknown(const OwnerPtr& owner) { std::lock_guard<std::mutex> lock(mutex); owner->frontier.MarkUnknown(); }
    void HandleOpened(const OwnerPtr& owner) { std::lock_guard<std::mutex> lock(mutex); owner->handleOpen = true; }
    // 只有原线程的 native producer 析构真正返回后才调用；注销监听器或 outq_len==0 不等于 join。
    void HandleJoined(const OwnerPtr& owner) { std::lock_guard<std::mutex> lock(mutex); owner->handleOpen = false; }
    void CloseOwner(const OwnerPtr& owner) { std::lock_guard<std::mutex> lock(mutex); owner->closed = true; }
    void CloseAdmission() { std::lock_guard<std::mutex> lock(mutex); closed = true; }
    bool Closed() const { std::lock_guard<std::mutex> lock(mutex); return closed; }
    Snapshot Read() const {
        std::lock_guard<std::mutex> lock(mutex);
        Snapshot out; out.closed = closed; out.unknown = unknown;
        const auto add = [&out](std::uint64_t& target, std::uint64_t value) {
            if (value > std::numeric_limits<std::uint64_t>::max() - target) { out.unknown = true; return; }
            target += value;
        };
        for (const auto& owner : owners) {
            add(out.accepted, owner->frontier.AcceptedCount());
            add(out.confirmed, owner->frontier.ConfirmedCount());
            add(out.activeSends, owner->activeSends);
            add(out.activeCallbacks, owner->activeCallbacks);
            if (owner->handleOpen) { ++out.openHandles; }
            out.unknown = out.unknown || owner->frontier.HasUnknown();
        }
        return out;
    }
private:
    mutable std::mutex mutex;
    std::vector<OwnerPtr> owners;
    bool closed = false;
    bool unknown = false;
};


// 声明在用户callback副本之前；副本target析构及其派生调用也必须先于EndCallback。
class PublicationCallbackScope {
public:
    PublicationCallbackScope(std::shared_ptr<PublicationRegistry> registry,PublicationRegistry::OwnerPtr owner)
        :registry(std::move(registry)),owner(std::move(owner)),exceptions(std::uncaught_exceptions()),active(this->registry->BeginCallback(this->owner)){}
    ~PublicationCallbackScope(){if(active)registry->EndCallback(owner,std::uncaught_exceptions()==exceptions);}
    PublicationCallbackScope(const PublicationCallbackScope&)=delete;
    PublicationCallbackScope& operator=(const PublicationCallbackScope&)=delete;
private:
    std::shared_ptr<PublicationRegistry> registry;PublicationRegistry::OwnerPtr owner;int exceptions;bool active;
};

} // namespace kafka_producer_policy
