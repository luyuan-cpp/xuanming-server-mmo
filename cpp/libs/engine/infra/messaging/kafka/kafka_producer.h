#pragma once

// 见 kafka_consumer.h 顶部的长注释:C++ 包装层随工程源码编译,
// 所以包含 rdkafkacpp.h 之前必须先定义 LIBRDKAFKA_STATICLIB。
#ifndef LIBRDKAFKA_STATICLIB
#define LIBRDKAFKA_STATICLIB
#endif

#include <chrono>
#include <cstddef>
#include <string>
#include <memory>
#include <functional>
#include <rdkafkacpp.h>

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

	std::unique_ptr<RdKafka::Producer> producer_;
	std::unique_ptr<RdKafka::Conf> conf_;
	std::string pendingBrokers_;
};
