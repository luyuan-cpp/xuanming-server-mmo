#include "kafka_producer.h"
#include "muduo/base/Logging.h"

#include <algorithm>
#include <cstdint>
#include <limits>
#include <utility>

namespace {
// 重建前 purge 旧实例之后,等它把 purge 回执吐完的上限(毫秒)。
// 取 200 而不是更大的值:purge() 是阻塞式的,它返回时被清掉的消息已经全部变成待取的回执,flush 只需把
// 回调派发完(限流后每条是微秒级),200ms 绰绰有余;而这段等待发生在调用 send() 的线程上 —— 对 scene
// 就是 loop 线程,它停多久,该节点全部玩家的 tick / AOI / RPC 应答就停多久。到点还没派发完的回执不再等,
// 用 outq_len() 一次性计入 DeliveryFailed(见 rebuildAfterFatal),账不丢。
//
// **这个上限只管 flush 这一步,不是整次重建的停顿上界。** 重建发生在调用 send() 的线程上
// (游戏主循环 / EventLoop),另外两步都没有超时:
//   - purge() 要等每个 broker 线程应答(third_party/librdkafka/src/rdkafka.c 的 rd_kafka_purge,
//     RD_POLL_INFINITE);
//   - 销毁旧实例(rd_kafka_destroy)要 join 库的主线程与全部 broker 线程。
// broker 只是连不上(TCP 不通)时这两步都是毫秒级;唯一的长停顿是某个 broker 线程正卡在 DNS 解析
// (getaddrinfo)里,那时要等解析器自己超时(十几秒量级)。它需要「fatal」与「DNS 故障」同时出现,
// 概率很低,目前接受。刻意不加 PURGE_NON_BLOCKING:它只让 purge 这一步不等,随后的销毁仍要 join
// 同一个卡住的线程,最坏停顿不会变短,反而可能让在途消息来不及出失败回执、漏计 DeliveryFailed。
// 真要让停顿有界,得把旧实例交给后台线程去销毁(已核实 rd_kafka_destroy 不回调 dr_cb),
// 并处理进程退出时该线程仍在运行的情况 —— 需要能编译、能实测时再做。
constexpr uint32_t kPurgeDrainTimeoutMs = 200;
} // namespace

void KafkaProducer::setBrokers(const std::string& brokers) {
	if (producer_) return; // already created
	pendingBrokers_ = brokers;
	LOG_INFO << "KafkaProducer: brokers stored for lazy init: " << brokers;
}

bool KafkaProducer::init(const std::string& brokers) {
	if (producer_) return true; // idempotent

	if (!createProducer(brokers)) {
		return false;
	}

	pendingBrokers_.clear();
	LOG_INFO << "KafkaProducer initialized for brokers: " << brokers;
	return true;
}

bool KafkaProducer::createProducer(const std::string& brokers) {
	std::string errstr;

	// 先在局部变量里把 conf / producer 都建好,全部成功才换进成员:
	// 重建路径上半途失败时,不能留下「conf 是新的、producer 是空的」这种半截状态。
	std::unique_ptr<RdKafka::Conf> conf(RdKafka::Conf::create(RdKafka::Conf::CONF_GLOBAL));
	if (!conf) {
		LOG_ERROR << "Kafka conf create failed";
		return false;
	}
	if (conf->set("bootstrap.servers", brokers, errstr) != RdKafka::Conf::CONF_OK) {
		LOG_ERROR << "Kafka conf error: " << errstr;
		return false;
	}

	if (conf->set("dr_cb", this, errstr) != RdKafka::Conf::CONF_OK) {
		LOG_ERROR << "Kafka producer: failed to set dr_cb: " << errstr;
	}
	if (conf->set("enable.sparse.connections", "true", errstr) != RdKafka::Conf::CONF_OK) {
		LOG_ERROR << "Kafka producer: failed to set enable.sparse.connections: " << errstr;
	}
	// 幂等生产者:内部重试时同一分区内保序、不重复(隐含 acks=all、max.in.flight<=5、retries 不封顶,
	// 这三项由 librdkafka 自行调整,这里**不要**再显式设置——设了不相容的值会让 create 直接失败)。
	// 设置失败不当成致命错误:退回旧的非幂等行为并大声报出来,总比所有 C++ 节点一条消息都发不出去强。
	if (conf->set("enable.idempotence", "true", errstr) != RdKafka::Conf::CONF_OK) {
		LOG_ERROR << "Kafka producer: failed to set enable.idempotence, falling back to non-idempotent delivery"
			<< " (per-key ordering is NOT guaranteed across internal retries): " << errstr;
	}

	std::unique_ptr<RdKafka::Producer> producer(RdKafka::Producer::create(conf.get(), errstr));
	if (!producer) {
		LOG_ERROR << "Kafka producer create failed: " << errstr;
		return false;
	}

	conf_ = std::move(conf);
	producer_ = std::move(producer);
	brokers_ = brokers;
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
	// 收尾:本线程再也不会有 send() 来驱动补报了。析构里的 flush() 本来就会打日志,这里再打一行是安全的。
	if (const std::uint64_t suppressed = failureLogThrottle_.TakeSuppressed(); suppressed > 0) {
		logSuppressedSummary(suppressed);
	}
}

RdKafka::ErrorCode KafkaProducer::produceOnce(const std::string& topic, const std::string& message,
	const std::string& key, int32_t partition) {
	if (!producer_) {
		return RdKafka::ERR__STATE;
	}

	return producer_->produce(
		topic,
		partition,
		RdKafka::Producer::RK_MSG_COPY,
		const_cast<char*>(message.c_str()),
		message.size(),
		key.empty() ? nullptr : const_cast<char*>(key.c_str()),
		key.size(),
		0,
		nullptr, nullptr);
}

RdKafka::ErrorCode KafkaProducer::send(const std::string& topic, const std::string& message,
	const std::string& key, int32_t partition) {
	using kafka_producer_policy::ProduceAction;

	// 上一次 poll() 里的投递回执报告了 fatal:先把作废的实例换掉再发,别把这一条也搭进去。
	if (fatalPending_) {
		rebuildAfterFatal("delivery report");
	}

	if (!ensureInitialized()) {
		kafka_producer_stats::IncProduceRejected();
		logFailure("[Kafka] Produce rejected (message NOT queued): producer unavailable"
			" (no brokers configured, or producer creation failed) topic=" + topic + " key=" + key);
		return RdKafka::ERR__STATE;
	}

	RdKafka::ErrorCode resp = produceOnce(topic, message, key, partition);
	switch (kafka_producer_policy::ClassifyProduceResult(resp)) {
	case ProduceAction::kAccepted:
		break;
	case ProduceAction::kRetryAfterPoll:
		// 队列满:非阻塞地取走已有结果的回执腾位置,再原样重试一次(见 kafka_producer_policy.h)。
		producer_->poll(0);
		resp = produceOnce(topic, message, key, partition);
		break;
	case ProduceAction::kRebuildProducer:
		// 这个实例已经作废。重建成功就把当前这条在新实例上重试一次;
		// 处于重建冷却期或重建失败时 resp 保持 ERR__FATAL,由调用方按失败处理。
		if (rebuildAfterFatal("produce")) {
			resp = produceOnce(topic, message, key, partition);
		}
		break;
	case ProduceAction::kReject:
	case ProduceAction::kCount:
		break;
	}

	if (resp != RdKafka::ERR_NO_ERROR) {
		kafka_producer_stats::IncProduceRejected();
		logFailure("[Kafka] Produce rejected (message NOT queued): topic=" + topic + " key=" + key
			+ " bytes=" + std::to_string(message.size()) + " err=" + RdKafka::err2str(resp));
	}
	else {
		// Producer ack per message — fires at steady-state Kafka write rate
		// (stress-1zone-45k saw 200+/s, which would burn ~17M INFO lines/day
		// on dev hosts). Keep the breadcrumb at DEBUG so it's available when
		// LogLevel is dropped but invisible at production WARN/INFO.
		LOG_DEBUG << "[Kafka] Message queued to topic " << topic;
	}

	poll(); // Ensure delivery callbacks are dispatched

	// 限流压掉的条数,在「窗口过期、之后不再失败」时靠这里补报:本线程没有独立的生产端轮询,
	// send() 是唯一的驱动点,回执也只在上面这次 poll() 里派发。放在 poll() 之后,
	// 让本次派发的回执先计完数。先用 PendingSuppressed() 预检,无失败时不必每条消息取一次时钟。
	if (failureLogThrottle_.PendingSuppressed() > 0) {
		const std::uint64_t suppressed =
			failureLogThrottle_.TakeExpiredSuppressed(std::chrono::steady_clock::now());
		if (suppressed > 0) {
			logSuppressedSummary(suppressed);
		}
	}

	return resp;
}

bool KafkaProducer::rebuildAfterFatal(const char* trigger) {
	if (!rebuildGate_.TryAcquire(std::chrono::steady_clock::now())) {
		// 冷却期内不重建:fatal 的成因若还在,新实例会立刻再次 fatal,每次 send() 都销毁、
		// 重建一整套 librdkafka 线程只会雪上加霜。fatalPending_ 保持原值,冷却期过后再来。
		return false;
	}

	std::string reason = "unknown";
	if (producer_) {
		std::string errstr;
		const RdKafka::ErrorCode code = producer_->fatal_error(errstr);
		reason = RdKafka::err2str(code) + ": " + errstr;
	}
	kafka_producer_stats::IncFatalRebuilds();
	LOG_ERROR << "[Kafka] Producer instance entered a fatal state and is being rebuilt (trigger=" << trigger
		<< ", reason=" << reason << "). Messages still queued in the old instance are lost and will be"
		<< " reported as delivery failures.";

	const std::string brokers = brokers_;
	if (producer_) {
		// purge 让旧实例队列里的消息立刻以失败回执收场(计入 DeliveryFailed、带 key 记日志),
		// 而不是陪着析构一起等到超时;rebuilding_ 期间涌出来的失败回执不代表新的 fatal。
		rebuilding_ = true;
		producer_->purge(RdKafka::Producer::PURGE_QUEUE | RdKafka::Producer::PURGE_INFLIGHT);
		producer_->flush(kPurgeDrainTimeoutMs);
		rebuilding_ = false;
		// flush 到点仍未派发完的回执:这些消息同样丢了,只是没机会逐条经过 dr_cb。一次性补记,
		// 让 DeliveryFailed 仍然等于「确定没送到的条数」。
		if (const int abandoned = producer_->outq_len(); abandoned > 0) {
			kafka_producer_stats::AddDeliveryFailed(static_cast<std::uint64_t>(abandoned));
			LOG_ERROR << "[Kafka] " << abandoned << " messages were still queued in the fatal producer instance when the "
				<< kPurgeDrainTimeoutMs << "ms drain budget ran out; counted as delivery failures without per-message logs.";
		}
		// purge 吐出来的失败回执绝大多数会被限流压掉;紧跟着重建日志把条数报出来,
		// 不要让「这次重建丢了多少条」等到十几秒后的下一次 send() 才出现。
		if (const std::uint64_t suppressed = failureLogThrottle_.TakeSuppressed(); suppressed > 0) {
			logSuppressedSummary(suppressed);
		}
		producer_.reset();
	}
	fatalPending_ = false;

	if (!createProducer(brokers)) {
		// 留给下一次 send() 经 ensureInitialized() 惰性重试。
		pendingBrokers_ = brokers;
		LOG_ERROR << "[Kafka] Producer rebuild failed; will retry lazily on the next send()";
		return false;
	}

	LOG_INFO << "[Kafka] Producer rebuilt after fatal error, brokers: " << brokers;
	return true;
}

void KafkaProducer::logFailure(const std::string& line) {
	const auto decision = failureLogThrottle_.OnFailure(std::chrono::steady_clock::now());
	if (decision.suppressedBeforeThis > 0) {
		logSuppressedSummary(decision.suppressedBeforeThis);
	}
	if (decision.logThisOne) {
		LOG_ERROR << line;
	}
}

void KafkaProducer::logSuppressedSummary(std::uint64_t suppressed) {
	const auto totals = kafka_producer_stats::Read();
	LOG_ERROR << "[Kafka] " << suppressed
		<< " producer failure log lines were suppressed by the log throttle; process totals:"
		<< " delivery_failed=" << totals.deliveryFailed
		<< " produce_rejected=" << totals.produceRejected
		<< " fatal_rebuilds=" << totals.fatalRebuilds;
}

void KafkaProducer::dr_cb(RdKafka::Message& message) {
	const RdKafka::ErrorCode err = message.err();
	if (err == RdKafka::ERR_NO_ERROR) {
		// Per-message delivery breadcrumb — same volume as send() success.
		// Demote to DEBUG; failures surface at ERROR below.
		LOG_DEBUG << "[Kafka] Delivered message to topic " << message.topic_name()
			<< " [" << message.partition() << "] at offset "
			<< message.offset();
		return;
	}

	// 走到这里 = 这条消息**确定没有送到 broker**(librdkafka 内部重试已经用尽,或实例作废 / 被 purge)。
	// 不在这里重发旧载荷,理由见 kafka_producer.h 顶部「投递语义」。
	kafka_producer_stats::IncDeliveryFailed();

	// 对未来版本的防御:librdkafka 2.14 不会给投递回执填 ERR__FATAL(fatal 时库自己 purge 队列,
	// 排队中的消息以 ERR__PURGE_QUEUE 收场),实际的识别点是 send() 里 produce() 的返回码。
	// 回执是在 poll() / flush() 的调用栈里回调的,此刻不能销毁实例:只置位,下一次 send() 处理。
	if (kafka_producer_policy::IsFatalDeliveryError(err) && !rebuilding_) {
		fatalPending_ = true;
	}

	// key 要带上:对存档类消息它就是玩家 id,出了事要能知道是谁的存档没落库。
	const std::string* key = message.key();
	logFailure("[Kafka] Delivery failed (message lost): topic=" + message.topic_name()
		+ " partition=" + std::to_string(message.partition())
		+ " key=" + (key != nullptr ? *key : std::string())
		+ " bytes=" + std::to_string(message.len())
		+ " err=" + RdKafka::err2str(err) + " (" + message.errstr() + ")"
		+ (kafka_producer_policy::IsPurgeDeliveryError(err)
			? " [purged: the producer instance went fatal or is being rebuilt]" : ""));
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
