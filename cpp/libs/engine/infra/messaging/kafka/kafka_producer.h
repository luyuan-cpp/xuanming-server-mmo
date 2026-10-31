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
	// 投递回执失败数；超时、fatal、purge包含UNKNOWN，不能当作确定未送达或排空证据。
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
	// 重建时没来得及逐条派发回执的消息:一次性计入,保证 DeliveryFailed 仍然等于「确定没送到的条数」。
	inline void AddDeliveryFailed(std::uint64_t count) { DeliveryFailed().fetch_add(count, std::memory_order_relaxed); }
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
// 那会把跨线程问题引进来(已对照 vendored 源码核实:src-cpp/HandleImpl.cpp 在设了 event_cb 时
// 会一并注册 log_cb,而日志回调来自内部线程)。fatal 状态从 produce() 的返回码识别 —— 所有 fatal
// 都会写入句柄的 fatal 状态,此后 produce() 一律返回 ERR__FATAL,这一个点就足够。
// 回执里对 ERR__FATAL 的判断只是对未来版本的防御:librdkafka 2.14 不会给投递回执填这个错误码,
// fatal 时库自己会 purge 队列,排队中的消息以 ERR__PURGE_QUEUE 收场。
//
// 投递语义(2026-09 起):
//  - enable.idempotence=true。librdkafka 内部重试时同一分区内**保序、不重复**。仓库不变量
//    (AGENTS §7.3)靠「同一业务实体 key 有序」;不开幂等时
//    max.in.flight 取默认大值,一次内部重试就可能让同 key 的两条消息乱序到达。
//  - 消息**仍然可能丢**:broker 不可用超过 message.timeout.ms(librdkafka 默认 5 分钟)后,
//    队列里的消息以失败回执收场。这里保证的是「丢得明明白白」——逐条 ERROR 日志带 topic / key
//    (限流)+ DeliveryFailed 计数;不在回执里自动重发旧载荷:对「后写覆盖前写」的消息,
//    把一份过期载荷补发到更新的载荷之后,比丢掉它更糟。补救靠调用方重发**当前**状态。
//  - 幂等生产者遇到不可恢复的序列错误会进入 fatal,此后该实例的 produce() 一律返回 ERR__FATAL。
//    send() 识别后重建实例并把当前这条消息在新实例上重试一次;旧实例队列里的消息按丢失计数。
//
// 逐条回执契约(sendTracked + setDeliveryListener,2026-10 起;battle 的结果日志用它确认 acks=all,
// 见 cpp/nodes/battle/logic/battle_room_manager.cpp):
//  - sendTracked 带一个非 0 关联号,经 msg_opaque 随消息进 librdkafka,回执时原样带回。0 等价于 send()。
//  - 返回值不是 ERR_NO_ERROR = 没有入队,**永远不会**有回执;返回 ERR_NO_ERROR = 之后恰好一次 DeliveryReport,
//    来源三选一:librdkafka 的投递回执;实例因 fatal 重建时为旧实例里来不及派发的消息合成的 ERR__PURGE_QUEUE
//    (synthesized = true);调用方自己的超时兜底。
//  - 监听器只在本线程的 poll() / flush() / send()(队列满重试与重建路径)调用栈里**同步**回调。监听器只许记账:
//    不得再调 send / sendTracked / poll / flush,不得在回调里换监听器,也不得改 ECS 组件集合 —— 后续处理由调用方
//    在自己的 loop 顶层做。
//  - ERR_NO_ERROR 表示 acks=all 已确认(幂等生产者隐含 acks=all);非零表示没送到或可能没送到(ERR__MSG_TIMED_OUT
//    属于后者)。调用方的补救只能是重发**当前**状态,不能重发旧载荷(理由见上面「投递语义」)。
//  - 析构时第一件事是注销监听器,之后 flush 派发的回执不再回调 —— 不依赖 thread_local 的析构顺序。
class KafkaProducer : public RdKafka::DeliveryReportCb {
public:
	using DeliveryCallback = std::function<void(const std::string& topic, int32_t partition, int64_t offset, const std::string& message)>;

	// 一条被跟踪消息的投递结局。err == ERR_NO_ERROR = acks=all 已确认。
	// synthesized = 不是 librdkafka 给的回执,而是实例重建时为来不及派发的消息合成的失败。
	struct DeliveryReport {
		std::uint64_t correlationId = 0;
		RdKafka::ErrorCode err = RdKafka::ERR_NO_ERROR;
		bool synthesized = false;
	};
	using DeliveryListener = std::function<void(const DeliveryReport&)>;

	KafkaProducer();
	~KafkaProducer();
	// 仅封 producer 准入；P1 仍需 relay 时不得调用，P3 才汇总所有线程。
	static void CloseProcessAdmission();
	static kafka_producer_policy::PublicationRegistry::Snapshot ProcessPublicationFrontier();
	// 在创建线程 flush 后销毁 native handle，返回后才记 native join。
	bool StopAndJoin(std::chrono::milliseconds timeout);

	// Store brokers for deferred creation. Does not create the producer yet.
	void setBrokers(const std::string& brokers);

	// Create the underlying rdkafka producer immediately. Called by setBrokers
	// compatibility path or lazily on first send().
	bool init(const std::string& brokers);

	bool initialized() const { return producer_ != nullptr; }

	// 已登记 broker(句柄可能尚未惰性创建)。单测与诊断用。
	bool brokersConfigured() const { return producer_ != nullptr || !pendingBrokers_.empty(); }

	static KafkaProducer& Instance() {
		thread_local KafkaProducer instance;
		return instance;
	}

	// 阻塞契约(调用方多在 loop / EventLoop 线程上,例如 scene 的存盘在 Redis 异步回调里):
	//  - 正常路径不阻塞:produce() 只是入本地队列,随后 poll(0) 非阻塞地派发回执;队列满时也只 poll(0) 重试一次。
	//  - **例外**:幂等生产者进入 fatal 后的重建在本线程上同步完成(purge → 有界 flush → 销毁旧实例 → 新建)。
	//    典型耗时几十毫秒,flush 这一步有上限(见 kafka_producer.cpp 的 kPurgeDrainTimeoutMs);两次重建至少间隔 5s,
	//    所以同一次业务操作里连续多次 send() 最多触发一次。fatal 只来自不可恢复的序列错误,**broker 不可用 /
	//    超时不会触发**(那条路径是消息在队列里等到 message.timeout.ms 后以失败回执收场,不重建、不阻塞)。
	//  - 已知债:销毁旧实例要 join librdkafka 线程,broker 线程若正卡在 DNS 解析里则没有上界(需要 fatal 与 DNS
	//    故障同时出现)。彻底有界的做法是把旧实例交给后台线程销毁,前置条件是能编译并实测,见 PROGRESS。
	RdKafka::ErrorCode send(const std::string& topic, const std::string& message, const std::string& key = "", int32_t partition = RdKafka::Topic::PARTITION_UA);

	// 与 send() 相同,另外按类注释「逐条回执契约」为这条消息登记 correlationId(0 = 不跟踪,等价于 send())。
	// 阻塞契约同 send()。correlationId 的唯一性由调用方保证。
	RdKafka::ErrorCode sendTracked(const std::string& topic, const std::string& message, const std::string& key,
		std::uint64_t correlationId, int32_t partition = RdKafka::Topic::PARTITION_UA);

	// 登记(或传 nullptr 注销)逐条回执的监听器。一个线程的生产者只有一个监听器,后登记的覆盖先登记的。
	void setDeliveryListener(DeliveryListener listener);

	// 已入队、还没拿到回执的被跟踪消息条数。诊断与单测用。
	std::size_t trackedInFlight() const;
	// 包含普通send；UNKNOWN永久保留到该boot结束，调用者仍须先封准入和join全部producer线程。
	bool publicationsSettled() const { return publicationRegistry_->OwnerSettled(publicationOwner_); }

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
	// send() / sendTracked() 的共同本体。correlationId = 0 不跟踪。
	RdKafka::ErrorCode sendImpl(const std::string& topic, const std::string& message, const std::string& key,
		int32_t partition, std::uint64_t correlationId);
	// msgOpaque 原样交给 librdkafka,回执里经 Message::msg_opaque() 带回(关联号编码在指针值里,不指向任何对象)。
	RdKafka::ErrorCode produceOnce(const std::string& topic, const std::string& message,
		const std::string& key, int32_t partition, void* msgOpaque);
	// 监听器非空才回调。只在本线程的 poll / flush / send 栈里调用。
	void notifyDelivery(const DeliveryReport& report);
	// 销毁已进入 fatal 的实例并重建。只能在 librdkafka 的调用栈之外调用(不能在 dr_cb 里)。
	// 返回 true = 新实例可用;false = 处于重建冷却期,或重建失败(留给下次 send() 惰性重试)。
	bool rebuildAfterFatal(const char* trigger);
	void logFailure(const std::string& line);
	// 补报「有 n 条失败日志被限流压掉」,并带上进程级累计数。
	void logSuppressedSummary(std::uint64_t suppressed);

	std::unique_ptr<RdKafka::Producer> producer_;
	std::unique_ptr<RdKafka::Conf> conf_;
	std::string pendingBrokers_;
	// 当前实例所用的 brokers,重建时沿用。
	std::string brokers_;
	// 投递回执报告了 fatal(librdkafka 2.14 下不会发生,见类注释;为未来版本保留)。
	// 回执是在 poll() 的调用栈里回调的,不能就地销毁实例,只置位,由下一次 send() 在发送之前处理。
	bool fatalPending_ = false;
	// 正在 purge 旧实例:此时涌出来的失败回执不代表新的 fatal。
	bool rebuilding_ = false;
	kafka_producer_policy::RebuildGate rebuildGate_{std::chrono::seconds(5)};
	kafka_producer_policy::FailureLogThrottle failureLogThrottle_{20, std::chrono::seconds(10)};
	// 已入队、还没拿到回执的被跟踪关联号(只属于当前实例;重建时 TakeAll 合成失败回执后清空)。
	kafka_producer_policy::DeliveryTracker deliveryTracker_;
	std::shared_ptr<kafka_producer_policy::PublicationRegistry> publicationRegistry_;
	kafka_producer_policy::PublicationRegistry::OwnerPtr publicationOwner_;
	bool publicationClosed_ = false;
	DeliveryListener deliveryListener_;

	// 关联号经 msg_opaque(void*)往返:指针宽度必须装得下 64 位关联号,否则高位被截断、回执对不上号。
	static_assert(sizeof(std::uintptr_t) >= sizeof(std::uint64_t),
		"KafkaProducer::sendTracked carries a 64-bit correlation id in msg_opaque");
};
