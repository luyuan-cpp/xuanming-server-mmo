#include "kafka_producer.h"
#include "muduo/base/Logging.h"

#include <algorithm>
#include <cstdint>
#include <limits>

void KafkaProducer::setBrokers(const std::string& brokers) {
	if (producer_) return; // already created
	pendingBrokers_ = brokers;
	LOG_INFO << "KafkaProducer: brokers stored for lazy init: " << brokers;
}

bool KafkaProducer::init(const std::string& brokers) {
	if (producer_) return true; // idempotent

	std::string errstr;

	conf_.reset(RdKafka::Conf::create(RdKafka::Conf::CONF_GLOBAL));
	if (conf_->set("bootstrap.servers", brokers, errstr) != RdKafka::Conf::CONF_OK) {
		LOG_ERROR << "Kafka conf error: " << errstr;
		producer_.reset();
		return false;
	}

	if (conf_->set("dr_cb", this, errstr) != RdKafka::Conf::CONF_OK) {
		LOG_ERROR << "Kafka producer: failed to set dr_cb: " << errstr;
	}
	if (conf_->set("enable.sparse.connections", "true", errstr) != RdKafka::Conf::CONF_OK) {
		LOG_ERROR << "Kafka producer: failed to set enable.sparse.connections: " << errstr;
	}

	producer_.reset(RdKafka::Producer::create(conf_.get(), errstr));
	if (!producer_) {
		LOG_ERROR << "Kafka producer create failed: " << errstr;
		return false;
	}

	pendingBrokers_.clear();
	LOG_INFO << "KafkaProducer initialized for brokers: " << brokers;
	return true;
}

bool KafkaProducer::ensureInitialized() {
	if (producer_) return true;
	if (pendingBrokers_.empty()) return false;
	return init(pendingBrokers_);
}

KafkaProducer::~KafkaProducer() {
	// broker 不可用时不允许进程析构无限挂起;正常停机的
	// KafkaManager 也复用同一条有界 flush 路径。
	flush(std::chrono::seconds(5));
}

RdKafka::ErrorCode KafkaProducer::send(const std::string& topic, const std::string& message,
	const std::string& key, int32_t partition) {
	if (!ensureInitialized()) {
		LOG_ERROR << "KafkaProducer send requested but no brokers configured.";
		return RdKafka::ERR__STATE;
	}

	RdKafka::ErrorCode resp = producer_->produce(
		topic,
		partition,
		RdKafka::Producer::RK_MSG_COPY,
		const_cast<char*>(message.c_str()),
		message.size(),
		key.empty() ? nullptr : const_cast<char*>(key.c_str()),
		key.size(),
		0,
		nullptr, nullptr);

	if (resp != RdKafka::ERR_NO_ERROR) {
		LOG_ERROR << "Produce failed: " << RdKafka::err2str(resp);
	}
	else {
		// Producer ack per message — fires at steady-state Kafka write rate
		// (stress-1zone-45k saw 200+/s, which would burn ~17M INFO lines/day
		// on dev hosts). Keep the breadcrumb at DEBUG so it's available when
		// LogLevel is dropped but invisible at production WARN/INFO.
		LOG_DEBUG << "[Kafka] Message queued to topic " << topic;
	}

	poll(); // Ensure delivery callbacks are dispatched

	return resp;
}


void KafkaProducer::dr_cb(RdKafka::Message& message) {
	if (message.err()) {
		LOG_ERROR << "[Kafka] Delivery failed: " << message.errstr();
	}
	else {
		// Per-message delivery breadcrumb — same volume as send() success.
		// Demote to DEBUG; failures still surface at ERROR above.
		LOG_DEBUG << "[Kafka] Delivered message to topic " << message.topic_name()
			<< " [" << message.partition() << "] at offset "
			<< message.offset();
	}
}

void KafkaProducer::poll() {
	if (!producer_) {
		return;
	}

	producer_->poll(0); // 0 = non-blocking
}

bool KafkaProducer::flush(std::chrono::milliseconds timeout) {
	if (!producer_) {
		return true;
	}

	const auto timeoutCount = std::clamp<std::int64_t>(
		static_cast<std::int64_t>(timeout.count()), 0, std::numeric_limits<int>::max());
	const RdKafka::ErrorCode error = producer_->flush(static_cast<int>(timeoutCount));
	const std::size_t pending = pendingMessageCount();
	const bool complete = error == RdKafka::ERR_NO_ERROR && pending == 0;

	// 零超时是停机 barrier 每 100ms 调用的非阻塞轮询;中间状态不刷屏,
	// 最终的有界 flush 再记录一次失败。
	if (!complete && timeoutCount > 0) {
		LOG_ERROR << "Kafka producer flush timed out or failed: timeout_ms=" << timeoutCount
			<< " pending=" << pending << " error=" << RdKafka::err2str(error);
	}
	return complete;
}

std::size_t KafkaProducer::pendingMessageCount() const {
	if (!producer_) {
		return 0;
	}
	const int pending = producer_->outq_len();
	return pending > 0 ? static_cast<std::size_t>(pending) : 0;
}
