// kafka_manager.h

#pragma once

#include <chrono>
#include <string>
#include <functional>
#include <memory>
#include <optional>
#include <mutex>
#include <cstdint>
#include <vector>
#include <future>
#include "proto/common/database/rollback_database_table.pb.h"

namespace muduo { namespace net { class EventLoop; } }

using KafkaMessageCallback = std::function<void(const std::string& topic, const std::string& payload)>;

// 只引策略结构体这一个小头文件,不引 kafka_consumer.h:后者会把 rdkafkacpp.h
// 连同 LIBRDKAFKA_STATICLIB 的定义顺序要求带进每一个 include 了 node.h 的 TU。
#include "messaging/kafka/kafka_partition_assign_policy.h"

class KafkaConfig;
class KafkaConsumer;

class KafkaManager {
public:
	KafkaManager();
	~KafkaManager();

	KafkaManager(const KafkaManager&) = delete;
	KafkaManager& operator=(const KafkaManager&) = delete;

	// 只登记生产端 broker,不订阅任何 topic(消费一律经 Node::RegisterKafkaMessageHandler -> Subscribe)。
	// 必须在之后调用 Publish / KafkaProducer::send 的那个线程上调用:生产者是 thread_local。
	// 返回 false 仅当 Kafka.Brokers 为空;调用方必须检查(Node::InitKafka 会拒绝启动)。
	bool Init(const KafkaConfig& config);
	bool Subscribe(const KafkaConfig& config,
		const std::vector<std::string>& topics,
		const std::string& groupId = {},
		const std::vector<int32_t>& partitions = {},
		KafkaMessageCallback callback = {},
		const KafkaPartitionAssignPolicy& assignPolicy = {},
        const sql_writer_maintenance_receipt::ManualConsumerAssignment& manualIdentity = {});
    using Maintenance=sql_writer_maintenance_receipt;
    bool PrepareOnlyBeforeSubscribe(bool enabled) { if(!consumers_.empty()||consumersStopped_||shutdown_)return false;prepareOnly_=enabled;return true; }
    std::future<bool> BeginManualDrain();
    bool ManualDrainReady() const;
    std::future<std::optional<Maintenance::ConsumerReadiness>> PrepareReadiness(const Maintenance::Plan& plan,
        const std::string& manifest,const std::string& issuer,const std::string& instance);
    bool OpenPrepared(const Maintenance::ConsumerReadiness& expected,const std::function<bool()>& openSource);
    std::future<std::optional<Maintenance::ManualConsumerEvidence>> ManualEvidence(const std::string& operation,
        const std::string& manifest,const std::string& issuer,const std::string& instance);
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
	// 后台 poller join 之后，再等此前投递到所属 loop 的回调全部返回。
	bool ConsumersJoined() const;
	// 仅本机安全析构门禁；UNKNOWN 不等于尚有回调或 native worker。
	bool LocalWorkJoined() const;
	// P3 才封全进程 producer；成功只覆盖 Kafka，不能替代 RPC/Redis 业务终局。
	std::optional<std::string> ClosedPublicationFrontier() const;

	// timeout 为零时不阻塞;Scene 停机 barrier 用它观察所有 DBTask
	// 是否已离开 producer 队列。
	bool FlushProducer(std::chrono::milliseconds timeout);
	std::size_t PendingProducerMessages() const;

	// 停止 consumer 并执行有界 producer flush。调用幂等。
	bool Shutdown(std::chrono::milliseconds producerFlushTimeout = std::chrono::milliseconds(2000));

private:
	struct CallbackState {
		std::mutex mutex;
		std::uint64_t accepted = 0;
		std::uint64_t completed = 0;
		bool joined = false;
		bool unknown = false;
	};
	std::shared_ptr<CallbackState> callbacks_ = std::make_shared<CallbackState>();
    struct PreparedState { std::mutex mutex;std::optional<Maintenance::ConsumerReadiness> aggregate;std::vector<Maintenance::ConsumerReadiness> owners; };
    std::shared_ptr<PreparedState> prepared_=std::make_shared<PreparedState>();
    bool prepareOnly_=false;
	std::vector<std::shared_ptr<KafkaConsumer>> consumers_;
	std::optional<std::reference_wrapper<muduo::net::EventLoop>> dispatchLoop_;
	bool shutdown_ = false;
	bool shutdownResult_ = true;
	bool consumersStopped_ = false;
};
