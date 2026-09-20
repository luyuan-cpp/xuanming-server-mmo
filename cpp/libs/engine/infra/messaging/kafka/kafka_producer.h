#pragma once

// 见 kafka_consumer.h 顶部的长注释:C++ 包装层随工程源码编译,
// 所以包含 rdkafkacpp.h 之前必须先定义 LIBRDKAFKA_STATICLIB。
#ifndef LIBRDKAFKA_STATICLIB
#define LIBRDKAFKA_STATICLIB
#endif

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <string>
#include <memory>
#include <functional>
#include <rdkafkacpp.h>

#include "messaging/kafka/kafka_producer_policy.h"

// 进程级计数器(各线程的 thread_local 生产者共用一份)。写法与 owner_epoch_stats 一致:
// C++ 侧没有 prometheus,先用原子计数 + 日志;将来接指标时从这里读,不要另起一套。
namespace kafka_producer_stats {
	// 投递回执报告失败的消息数 = **确定没送到 broker 的消息数**(含超时、fatal、重建时 purge 掉的)。
	inline std::atomic<std::uint64_t>& DeliveryFailed()
	{
		static std::atomic<std::uint64_t> g_counter{0};
		return g_counter;
	}

	// produce() 同步拒绝、且重试后仍未入队的消息数(队列满、fatal 冷却期、消息过大等)。
	inline std::atomic<std::uint64_t>& ProduceRejected()
	{
		static std::atomic<std::uint64_t> g_counter{0};
		return g_counter;
	}

	// 因幂等生产者进入 fatal 而重建实例的次数。正常应恒为 0。
	inline std::atomic<std::uint64_t>& FatalRebuilds()
	{
		static std::atomic<std::uint64_t> g_counter{0};
		return g_counter;
	}

	inline void IncDeliveryFailed() { DeliveryFailed().fetch_add(1, std::memory_order_relaxed); }
	inline void IncProduceRejected() { ProduceRejected().fetch_add(1, std::memory_order_relaxed); }
	inline void IncFatalRebuilds() { FatalRebuilds().fetch_add(1, std::memory_order_relaxed); }

	struct Snapshot
	{
		std::uint64_t deliveryFailed;
		std::uint64_t produceRejected;
		std::uint64_t fatalRebuilds;

		bool Any() const { return deliveryFailed + produceRejected + fatalRebuilds > 0; }
	};

	inline Snapshot Read()
	{
		return Snapshot{
			DeliveryFailed().load(std::memory_order_relaxed),
			ProduceRejected().load(std::memory_order_relaxed),
			FatalRebuilds().load(std::memory_order_relaxed)};
	}
} // namespace kafka_producer_stats

// 线程模型:Instance() 是 thread_local,每个线程一个生产者。send() / poll() / flush() 与投递回执
// (dr_cb 只在本线程调 poll / flush 时被回调)都发生在同一个线程上,成员无需加锁。
// 刻意**不注册 event_cb**:librdkafka 的日志事件会从它的内部线程直接回调 event_cb,
// 那会把跨线程问题引进来;fatal 状态改从两个都在本线程上的点识别(produce() 的返回码、回执的错误码)。
//
// 投递语义(2026-09 起):
//  - enable.idempotence=true。librdkafka 内部重试时同一分区内**保序、不重复**。仓库不变量
//    (AGENTS §7.3)靠「同一业务实体 key 有序」保证旧存档不会盖掉新存档;不开幂等时
//    max.in.flight 取默认大值,一次内部重试就可能让同 key 的两条消息乱序到达。
//  - 消息**仍然可能丢**:broker 不可用超过 message.timeout.ms(librdkafka 默认 5 分钟)后,
//    队列里的消息以失败回执收场。这里保证的是「丢得明明白白」——逐条 ERROR 日志带 topic / key
//    (限流)+ DeliveryFailed 计数;不在回执里自动重发旧载荷:对存档这类「后写覆盖前写」的消息,
//    把一份过期载荷补发到更新的载荷之后,比丢掉它更糟。补救靠调用方重发**当前**状态(在线玩家的
//    周期存盘天然如此)。
//  - 幂等生产者遇到不可恢复的序列错误会进入 fatal,此后该实例的 produce() 一律返回 ERR__FATAL。
//    send() 识别后重建实例并把当前这条消息在新实例上重试一次;旧实例队列里的消息按丢失计数。
class KafkaProducer : public RdKafka::DeliveryReportCb {
public:
	using DeliveryCallback = std::function<void(const std::string& topic, int32_t partition, int64_t offset, const std::string& message)>;
	~KafkaProducer();

	// Store brokers for deferred creation. Does not create the producer yet.
	void setBrokers(const std::string& brokers);

	// Create the underlying rdkafka producer immediately. Called by setBrokers
	// compatibility path or lazily on first send().
	bool init(const std::string& brokers);

	bool initialized() const { return producer_ != nullptr; }

	static KafkaProducer& Instance() {
		thread_local KafkaProducer instance;
		return instance;
	}

	RdKafka::ErrorCode send(const std::string& topic, const std::string& message, const std::string& key = "", int32_t partition = RdKafka::Topic::PARTITION_UA);

	void poll();
	// 驱动投递回调,直到 producer 队列为空或超时;只有 librdkafka
	// 报告 flush 完成时才返回 true。
	bool flush(std::chrono::milliseconds timeout);
	std::size_t pendingMessageCount() const;

	void dr_cb(RdKafka::Message& message) override;

private:
	bool ensureInitialized();
	// 按当前投递语义建一个新的底层生产者;失败时 producer_ 保持为空。
	bool createProducer(const std::string& brokers);
	RdKafka::ErrorCode produceOnce(const std::string& topic, const std::string& message,
		const std::string& key, int32_t partition);
	// 销毁已进入 fatal 的实例并重建。只能在 librdkafka 的调用栈之外调用(不能在 dr_cb 里)。
	// 返回 true = 新实例可用;false = 处于重建冷却期,或重建失败(留给下次 send() 惰性重试)。
	bool rebuildAfterFatal(const char* trigger);
	void logFailure(const std::string& line);

	std::unique_ptr<RdKafka::Producer> producer_;
	std::unique_ptr<RdKafka::Conf> conf_;
	std::string pendingBrokers_;
	// 当前实例所用的 brokers,重建时沿用。
	std::string brokers_;
	// 投递回执报告了 fatal。回执是在 poll() 的调用栈里回调的,不能就地销毁实例,
	// 只置位,由下一次 send() 在发送之前处理。
	bool fatalPending_ = false;
	// 正在 purge 旧实例:此时涌出来的失败回执不代表新的 fatal。
	bool rebuilding_ = false;
	kafka_producer_policy::RebuildGate rebuildGate_{std::chrono::seconds(5)};
	kafka_producer_policy::FailureLogThrottle failureLogThrottle_{20, std::chrono::seconds(10)};
};
