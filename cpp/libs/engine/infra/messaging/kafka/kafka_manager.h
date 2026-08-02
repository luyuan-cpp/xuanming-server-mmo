// kafka_manager.h

#pragma once

#include <chrono>
#include <cstddef>
#include <string>
#include <functional>
#include <memory>
#include <optional>
#include <vector>

namespace muduo { namespace net { class EventLoop; } }

using KafkaMessageCallback = std::function<void(const std::string& topic, const std::string& payload)>;

class KafkaConfig;
class KafkaConsumer;

class KafkaManager {
public:
	KafkaManager();
	~KafkaManager();

	KafkaManager(const KafkaManager&) = delete;
	KafkaManager& operator=(const KafkaManager&) = delete;

	bool Init(const KafkaConfig& config);
	bool Subscribe(const KafkaConfig& config,
		const std::vector<std::string>& topics,
		const std::string& groupId = {},
		const std::vector<int32_t>& partitions = {},
		KafkaMessageCallback callback = {});
	bool Publish(const std::string& topic, const std::string& msg);

	// Cooperative poll from the muduo EventLoop. Becomes a near-no-op once
	// StartBackgroundPolling has been called.
	void Poll();

	// Move record consumption off the EventLoop onto a dedicated thread. The
	// `dispatchLoop` is where decoded message callbacks are queued so they
	// keep running on the same thread as the rest of the node state. Must be
	// called after Subscribe() succeeds; safe to call multiple times.
	void StartBackgroundPolling(muduo::net::EventLoop* dispatchLoop);

	// 先停消费入口并 join 后台 poller,但保留 producer 给停机存盘发送 DBTask。
	// 幂等,普通 Shutdown() 会再次调用。
	void StopConsumers();

	// timeout 为零时不阻塞;Scene 停机 barrier 用它观察所有 DBTask
	// 是否已离开 producer 队列。
	bool FlushProducer(std::chrono::milliseconds timeout);
	std::size_t PendingProducerMessages() const;

	// 停止 consumer 并执行有界 producer flush。调用幂等。
	bool Shutdown(std::chrono::milliseconds producerFlushTimeout = std::chrono::milliseconds(2000));

private:
	std::vector<std::unique_ptr<KafkaConsumer>> consumers_;
	std::optional<std::reference_wrapper<muduo::net::EventLoop>> dispatchLoop_;
	bool shutdown_ = false;
	bool shutdownResult_ = true;
	bool consumersStopped_ = false;
};
