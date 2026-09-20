#pragma once

// KafkaProducer 的纯决策逻辑:produce() 的返回码该怎么处置、重建生产者的最小间隔、
// 失败日志的限流。刻意做成 header-only、只依赖 RdKafka::ErrorCode 这一个枚举、不调用
// librdkafka 的任何函数,也不读墙钟(时间点一律由调用方传入)——这样不连 broker、
// 不链接 C++ 包装层就能单测,与 node_command_route.h 那一组是同一种组织方式。
// 用例在 cpp/tests/kafka_command_test/kafka_producer_policy_test.cpp。

// 见 kafka_consumer.h 顶部的长注释:包含 rdkafkacpp.h 之前必须先定义 LIBRDKAFKA_STATICLIB。
#ifndef LIBRDKAFKA_STATICLIB
#define LIBRDKAFKA_STATICLIB
#endif

#include <chrono>
#include <cstdint>
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

	explicit RebuildGate(Clock::duration minInterval) : minInterval_(minInterval) {}

	// 现在允许重建吗?允许则把 now 记为本次重建时刻。
	bool TryAcquire(Clock::time_point now) {
		if (hasLast_ && now - last_ < minInterval_) {
			return false;
		}
		last_ = now;
		hasLast_ = true;
		return true;
	}

private:
	Clock::duration minInterval_;
	Clock::time_point last_{};
	bool hasLast_ = false;
};

// 失败日志限流。broker 长时间不可用时,队列里成千上万条消息会在 message.timeout.ms 到期的
// 同一刻一起失败;逐条打 ERROR 会把日志盘打满,也会把真正要看的那几行淹掉。
// 规则:每个窗口内前 burst 条逐条记录,其余只计数;下一个窗口的第一条失败到来时,
// 把上个窗口被压掉的条数一并报出来。准确的总数永远以 kafka_producer_stats 的计数器为准。
class FailureLogThrottle {
public:
	using Clock = std::chrono::steady_clock;

	struct Decision {
		bool logThisOne = false;                   // 这一条要不要逐条记录
		std::uint64_t suppressedBeforeThis = 0;    // 非 0 = 先补报一行「上个窗口压掉了 N 条」
	};

	FailureLogThrottle(std::uint32_t burst, Clock::duration window) : burst_(burst), window_(window) {}

	Decision OnFailure(Clock::time_point now) {
		Decision decision;
		if (!windowOpen_ || now - windowStart_ >= window_) {
			decision.suppressedBeforeThis = suppressed_;
			windowStart_ = now;
			windowOpen_ = true;
			loggedInWindow_ = 0;
			suppressed_ = 0;
		}
		if (loggedInWindow_ < burst_) {
			++loggedInWindow_;
			decision.logThisOne = true;
		} else {
			++suppressed_;
		}
		return decision;
	}

	// 还没来得及补报的条数(进程退出 / 重建时用来收尾)。
	std::uint64_t PendingSuppressed() const { return suppressed_; }

private:
	std::uint32_t burst_;
	Clock::duration window_;
	Clock::time_point windowStart_{};
	bool windowOpen_ = false;
	std::uint32_t loggedInWindow_ = 0;
	std::uint64_t suppressed_ = 0;
};

} // namespace kafka_producer_policy
