#include "kafka_manager.h"
#include "proto/common/base/config.pb.h"
#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <boost/algorithm/string/join.hpp>
#include "thread_context/redis_manager.h"
#include "messaging/kafka/kafka_consumer.h"
#include "messaging/kafka/kafka_producer.h"

#include <node_config_manager.h>

namespace {
std::string JoinBrokers(const KafkaConfig& config) {
	std::vector<std::string> brokersVec;
	for (const auto& broker : config.brokers()) {
		brokersVec.push_back(broker);
	}

	return boost::algorithm::join(brokersVec, ",");
}

std::vector<std::string> CollectTopics(const KafkaConfig& config) {
	std::vector<std::string> topicsVec;
	for (const auto& topic : config.topics()) {
		topicsVec.push_back(topic);
	}

	return topicsVec;
}
}

KafkaManager::KafkaManager() = default;

KafkaManager::~KafkaManager() {
	Shutdown();
}

bool KafkaManager::Init(const KafkaConfig& config) {
	// The deploy yaml intentionally leaves Kafka.GroupID empty so each node
	// derives a per-node-id group later (see node_kafka_command_handler.h).
	// Skip the eager top-level subscribe in that case instead of letting
	// KafkaConsumer::init bail with a misleading librdkafka error: the real
	// subscribe happens via Node::RegisterKafkaMessageHandler when
	// SetKafkaHandlers fires, which always supplies a non-empty group id.
	if (config.group_id().empty()) {
		LOG_INFO << "KafkaManager::Init skipped: empty default group_id. "
			<< "Per-node Kafka consumer will be created via "
			<< "RegisterKafkaCommandHandler.";
		// Producer brokers still need to be wired up for outbound Publish().
		const std::string brokers = JoinBrokers(config);
		if (!brokers.empty()) {
			KafkaProducer::Instance().setBrokers(brokers);
		}
		return true;
	}

	auto zoneId = tlsNodeConfigManager.GetGameConfig().zone_id();
	return Subscribe(
		config,
		CollectTopics(config),
		config.group_id(),
		{ static_cast<int32_t>(zoneId) }
	);
}

bool KafkaManager::Subscribe(const KafkaConfig& config,
	const std::vector<std::string>& topics,
	const std::string& groupId,
	const std::vector<int32_t>& partitions,
	KafkaMessageCallback callback) {
	if (consumersStopped_ || shutdown_) {
		LOG_WARN << "KafkaManager: shutdown has started; rejecting new consumer subscription.";
		return false;
	}
	if (!callback) {
		LOG_DEBUG << "KafkaManager: No message callback provided, skipping subscribe.";
		return false;
	}

	if (topics.empty()) {
		LOG_ERROR << "KafkaManager: Cannot subscribe without any topics configured.";
		return false;
	}

	const std::string brokers = JoinBrokers(config);
	const std::string effectiveGroupId = groupId.empty() ? config.group_id() : groupId;

	// Defer producer creation until first Publish() call to save rdkafka threads.
	KafkaProducer::Instance().setBrokers(brokers);

	auto consumer = std::make_unique<KafkaConsumer>();
	if (!consumer->init(
		brokers,
		effectiveGroupId,
		topics,
		partitions,
		callback
	)) {
		LOG_ERROR << "KafkaManager: Failed to initialize Kafka consumer for topics: "
			<< boost::algorithm::join(topics, ",");
		return false;
	}

	if (!consumer->start()) {
		LOG_ERROR << "KafkaManager: Failed to start Kafka consumer for topics: "
			<< boost::algorithm::join(topics, ",");
		return false;
	}

	if (dispatchLoop_) {
		consumer->startBackgroundPolling(&dispatchLoop_->get());
	}
	consumers_.push_back(std::move(consumer));

	LOG_INFO << "KafkaManager initialized successfully. Brokers: " << brokers
		<< " Topics: " << boost::algorithm::join(topics, ",");
	return true;
}

bool KafkaManager::Publish(const std::string& topic, const std::string& msg) {

	auto err = KafkaProducer::Instance().send(topic, msg);
	if (err != RdKafka::ERR_NO_ERROR) {
		LOG_ERROR << "KafkaManager: Failed to send message. Error: " << RdKafka::err2str(err);
		return false;
	}

	return true;
}

void KafkaManager::Poll() {
	for (auto& consumer : consumers_) {
		if (consumer) {
			consumer->poll();
		}
	}
}

void KafkaManager::StartBackgroundPolling(muduo::net::EventLoop* dispatchLoop) {
	if (consumersStopped_ || shutdown_) {
		LOG_WARN << "KafkaManager::StartBackgroundPolling ignored after shutdown started.";
		return;
	}
	if (!dispatchLoop) {
		LOG_ERROR << "KafkaManager::StartBackgroundPolling requires a non-null dispatch loop.";
		return;
	}
	dispatchLoop_ = std::ref(*dispatchLoop);
	for (auto& consumer : consumers_) {
		if (consumer) {
			consumer->startBackgroundPolling(dispatchLoop);
		}
	}
}

bool KafkaManager::FlushProducer(std::chrono::milliseconds timeout) {
	return KafkaProducer::Instance().flush(timeout);
}

std::size_t KafkaManager::PendingProducerMessages() const {
	return KafkaProducer::Instance().pendingMessageCount();
}

void KafkaManager::StopConsumers() {
	if (consumersStopped_) {
		return;
	}
	consumersStopped_ = true;
	for (auto& consumer : consumers_) {
		if (consumer) {
			consumer->stop();
		}
	}
	consumers_.clear();
}

bool KafkaManager::Shutdown(std::chrono::milliseconds producerFlushTimeout) {
	if (shutdown_) {
		return shutdownResult_;
	}
	shutdown_ = true;

	StopConsumers();

	shutdownResult_ = FlushProducer(producerFlushTimeout);

	if (shutdownResult_) {
		LOG_INFO << "KafkaManager has been shut down; producer queue drained.";
	}
	else {
		LOG_ERROR << "KafkaManager shut down with producer messages still in flight. pending="
			<< PendingProducerMessages();
	}
	return shutdownResult_;
}
