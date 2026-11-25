#include "kafka_consumer.h"
#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

#include <atomic>
#include <chrono>
#include <string_view>
#include <thread>
#include <unordered_map>
#include <map>
#include <algorithm>
#include <set>
#include <limits>
#include <stdexcept>
#include <librdkafka/src/rdkafka.h>
#include "google/protobuf/util/message_differencer.h"
#include "messaging/kafka/kafka_trace_header.h"
#include "thread_context/trace_context_tls.h"

namespace
{
	constexpr int kMaxMessagesPerPoll = 128;
	// Suppress repeated "unknown topic" warnings during startup race between
	// consumer subscribe() and broker topic auto-creation. Log WARN at most once
	// per topic per kUnknownTopicWarnIntervalSec; otherwise log at DEBUG.
	constexpr int kUnknownTopicWarnIntervalSec = 30;
	// How long consume() blocks per call in background mode. Big enough that
	// idle traffic doesn't spin the CPU, small enough that stop() returns
	// promptly when the consumer is being shut down.
	constexpr int kBackgroundConsumeTimeoutMs = 200;

	// 分区契约核对的时间预算(断线重连 R19)。核对跑在节点主 EventLoop 上(KafkaManager::Subscribe ← RegisterKafkaHandlers),
	// 这段时间里 etcd 续约、Redis 存活探测、节点链路心跳都停着,而链路 15s 没有帧就会被对端判死,所以每个消费者的核对
	// 都要远短于它。刚起的 broker 在选 controller 期间会先返回 "topic 存在但 0 分区" 或直接查不到,预算内重试几次再判,
	// 别把正常的冷启动竞态当成契约违反;broker 整个不可达时预算用完照常按契约 assign(见 init)。
	// 原来是 5 次 × (5s 查询 + 1s 等待),一个消费者最多卡 30s,gate 一次起 4 个就是两分钟。
	constexpr int kPartitionContractBudgetMs = 3000;
	constexpr int kPartitionContractQueryTimeoutMs = 1000;
	constexpr int kPartitionContractRetryDelayMs = 500;
	// 本进程上一个消费者用完预算也没查到分区数时,后面的消费者只查一次、不再重试:broker 多半整个不可达,
	// 再逐个等满预算只会把主循环卡上 N 倍。查到一次就清掉。init 在主 EventLoop 上串行调用,原子只为不留数据竞争。
	std::atomic<bool> gPartitionContractUnsettled{false};
}

int32_t KafkaConsumer::queryTopicPartitionCount(const std::string &topic)
{
	if (!consumer)
	{
		return -1;
	}

	std::string errstr;
	std::unique_ptr<RdKafka::Topic> topicHandle{
		RdKafka::Topic::create(consumer.get(), topic, nullptr, errstr)};
	if (!topicHandle)
	{
		LOG_WARN << "KafkaConsumer: cannot create topic handle for metadata query. topic="
				 << topic << ", err=" << errstr;
		return -1;
	}

	RdKafka::Metadata *rawMetadata = nullptr;
	const auto err = consumer->metadata(/*all_topics=*/false, topicHandle.get(),
										 &rawMetadata, kPartitionContractQueryTimeoutMs);
	std::unique_ptr<RdKafka::Metadata> metadata{rawMetadata};
	if (err != RdKafka::ERR_NO_ERROR || !metadata)
	{
		LOG_WARN << "KafkaConsumer: metadata query failed. topic=" << topic
				 << ", err=" << RdKafka::err2str(err);
		return -1;
	}

	for (auto it = metadata->topics()->begin(); it != metadata->topics()->end(); ++it)
	{
		if ((*it)->topic() != topic)
		{
			continue;
		}
		if ((*it)->err() != RdKafka::ERR_NO_ERROR)
		{
			LOG_WARN << "KafkaConsumer: metadata reports topic error. topic=" << topic
					 << ", err=" << RdKafka::err2str((*it)->err());
			return -1;
		}
		return static_cast<int32_t>((*it)->partitions()->size());
	}

	return -1;
}

bool KafkaConsumer::init(const std::string &brokers, const std::string &groupId,
						 const std::vector<std::string> &topics,
						 const std::vector<int32_t> &partitions,
						 const MessageCallback &callback,
						 const KafkaPartitionAssignPolicy &assignPolicy,
                         const Maintenance::ManualConsumerAssignment& manualIdentity, bool prepareClosed)
{
    std::lock_guard<std::mutex> ownerLock(ownerMutex);
    this->groupId=groupId; this->topics=topics; manual=!partitions.empty(); this->prepareClosed=prepareClosed;
    this->manualIdentity=manualIdentity; readinessLost=false; successfulPoll=false; assignmentRevision=0;
    prepared.reset(); stoppedManual.reset(); manualStartOffsets.clear(); manualDrainEnds.clear(); manualDrainStarted=false;
	msgCallback = callback;

	std::string errstr;

	// librdkafka 对空 group.id 直接拒绝(返回 CONF_INVALID,报
	// "Invalid value for configuration property 'group.id'")。
	// 2026-10 起 KafkaManager::Init 不再订阅任何 topic,所以走到这里的空 group.id 只可能来自
	// 直接调用 KafkaManager::Subscribe:调用方没给组名,Kafka.GroupID 也为空。旧路径按节点
	// 拼组名的回落在 node_kafka_command_handler.h 的 RegisterKafkaCommandHandler 里。
	// 这是调用方的真失败,没有「稍后再建」的路径:这里显式拦下并打一行 ERROR,
	// 免得失败表现为一条误导人的 librdkafka 报错。
	if (groupId.empty())
	{
		LOG_ERROR << "KafkaConsumer::init failed: empty group.id "
				  << "(Subscribe called without a group and Kafka.GroupID is empty).";
		return false;
	}

	conf.reset(RdKafka::Conf::create(RdKafka::Conf::CONF_GLOBAL));
    if(conf->set("rebalance_cb", static_cast<RdKafka::RebalanceCb*>(this), errstr)!=RdKafka::Conf::CONF_OK) return false;
	if (conf->set("bootstrap.servers", brokers, errstr) != RdKafka::Conf::CONF_OK)
	{
		LOG_ERROR << "KafkaConsumer: failed to set bootstrap.servers: " << errstr;
		return false;
	}
	if (conf->set("group.id", groupId, errstr) != RdKafka::Conf::CONF_OK)
	{
		LOG_ERROR << "KafkaConsumer: failed to set group.id: " << errstr;
		return false;
	}
	// ownOffsets 的理由见 kafka_consumer.h::KafkaPartitionAssignPolicy:
	// 共享分区时 offset 是没有主人的,提交只会互相覆盖。
	const char *const autoCommit = assignPolicy.ownOffsets ? "false" : "true";
	const char *const offsetReset = assignPolicy.ownOffsets ? "latest" : "earliest";
	if (conf->set("enable.auto.commit", autoCommit, errstr) != RdKafka::Conf::CONF_OK)
	{
		LOG_ERROR << "KafkaConsumer: failed to set enable.auto.commit: " << errstr;
	}
	if (conf->set("auto.offset.reset", offsetReset, errstr) != RdKafka::Conf::CONF_OK)
	{
		LOG_ERROR << "KafkaConsumer: failed to set auto.offset.reset: " << errstr;
	}
	if (conf->set("enable.sparse.connections", "true", errstr) != RdKafka::Conf::CONF_OK)
	{
		LOG_ERROR << "KafkaConsumer: failed to set enable.sparse.connections: " << errstr;
	}
	if (conf->set("allow.auto.create.topics", "true", errstr) != RdKafka::Conf::CONF_OK)
	{
		LOG_ERROR << "KafkaConsumer: failed to set allow.auto.create.topics: " << errstr;
	}

	// Tune liveness for the muduo single-thread polling model. The consumer's
	// poll() runs on a 100ms timer on the same EventLoop that handles RPC,
	// client TCP, codec dispatch, and scene-response forwarding. Under 45k
	// concurrent connects (stress-1zone-45k-2026-05-30) the loop can stall
	// for many seconds, at which point the broker stops seeing the consumer:
	//
	//   * librdkafka maintains heartbeats from a background thread, so a
	//     short stall does NOT immediately drop the member. But once the
	//     loop misses poll() for longer than max.poll.interval.ms (default
	//     5 minutes), librdkafka voluntarily leaves the group and the
	//     coordinator marks "no active members" — that is exactly the
	//     symptom seen in the 45k run, with gate-group-1 lag spiking to
	//     17k while gate-2 was still accepting TCP.
	//   * Raising max.poll.interval.ms tolerates longer poll-skips, and
	//     setting session.timeout.ms + heartbeat.interval.ms explicitly
	//     locks in the heartbeat cadence so it doesn't drift across
	//     librdkafka versions.
	//   * cooperative-sticky keeps un-rebalanced partitions assigned during
	//     incremental rebalances; with the default eager strategy every
	//     rebalance revokes everything, which during a login storm
	//     compounds the lag by forcing a full re-assign each tick.
	struct LibrdkafkaTuning
	{
		std::string_view name;
		std::string_view value;
	};
	static constexpr LibrdkafkaTuning kTuning[] = {
		{"session.timeout.ms", "45000"},
		{"max.poll.interval.ms", "900000"},
		{"heartbeat.interval.ms", "3000"},
		{"partition.assignment.strategy", "cooperative-sticky"},
		// Coalesce broker fetches a bit so a tick that DOES get poll() time
		// drains more messages without raising the per-poll batch ceiling.
		{"fetch.min.bytes", "1"},
		{"fetch.wait.max.ms", "50"},
	};
	for (const auto &t : kTuning)
	{
		const std::string name(t.name);
		const std::string value(t.value);
		if (conf->set(name, value, errstr) != RdKafka::Conf::CONF_OK)
		{
			LOG_WARN << "KafkaConsumer: failed to set " << name
					 << "=" << value << ": " << errstr;
		}
	}

	consumer.reset(RdKafka::KafkaConsumer::create(conf.get(), errstr));
	if (!consumer)
	{
		LOG_ERROR << "Failed to create KafkaConsumer: " << errstr;
		return false;
	}

	// Use assign() for specific partitions
	if (!partitions.empty())
	{
		if (topics.empty())
		{
			LOG_ERROR << "KafkaConsumer: topics is empty, cannot assign partitions";
			consumer->close();
			consumer.reset();
			return false;
		}

		// 分区契约门禁。放在 assign 之前:assign 一个不存在的分区不会报错,
		// 只会安静地永远收不到消息。
		if (assignPolicy.expectedTopicPartitions > 0)
		{
			int32_t actual = -1;
			const bool singleAttempt = gPartitionContractUnsettled.load(std::memory_order_relaxed);
			const auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(kPartitionContractBudgetMs);
			for (int attempt = 1;; ++attempt)
			{
				actual = queryTopicPartitionCount(topics[0]);
				if (actual > 0)
				{
					break;
				}
				// 下一轮要等一次再查一次:预算里放不下就停,不在最后一次失败之后再白等。
				const auto nextAttemptEnds = std::chrono::steady_clock::now() +
											 std::chrono::milliseconds(kPartitionContractRetryDelayMs + kPartitionContractQueryTimeoutMs);
				if (singleAttempt || nextAttemptEnds > deadline)
				{
					break;
				}
				LOG_WARN << "KafkaConsumer: partition contract query not settled yet. topic="
						 << topics[0] << ", attempt=" << attempt;
				std::this_thread::sleep_for(std::chrono::milliseconds(kPartitionContractRetryDelayMs));
			}
			gPartitionContractUnsettled.store(actual <= 0, std::memory_order_relaxed);

			if (actual > 0 && actual != assignPolicy.expectedTopicPartitions)
			{
				// broker 明确答了一个不同的数字 —— 这是真正的契约违反,fail-closed。
				// 最常见的成因:topic 被生产者抢在预建之前 auto-create 成 1 分区。
				LOG_ERROR << "KafkaConsumer: Kafka partition contract mismatch. topic=" << topics[0]
						  << ", broker=" << actual
						  << ", contract=" << assignPolicy.expectedTopicPartitions
						  << ". The partition count is part of the addressing protocol(partition = node_id % P) and cannot be resized in place:"
						  << "if a producer auto-created the topic with 1 partition, delete it and rerun topic provisioning"
						  << "(deploy/docker-compose.yml kafka-topic-init / k8s_deploy.ps1 "
						  << "-Command infra-kafka-topics);if the partition count change is intentional,"
						  << "increment Kafka.CommandTopicGeneration to use a new set of topics.";
				consumer->close();
				consumer.reset();
				return false;
			}

			if (actual <= 0)
			{
				// 压根查不到(broker 不可达 / 元数据还没广播开)。这里**不**拒绝启动:
				// 改造之前 subscribe() 在 broker 不可达时也照样成功、由后台重连兜底,
				// 把它变成启动致命会让"Kafka 晚起一会儿"直接打死 gate/scene。
				// 真正危险的那一种(broker 答了 1 分区)已经在上面挡掉了。
				LOG_ERROR << "KafkaConsumer: cannot verify partition contract, broker metadata unavailable. topic="
						  << topics[0] << ", contract=" << assignPolicy.expectedTopicPartitions
						  << ". Continuing to assign according to the contract;if this topic has a different partition count when the broker starts,"
						  << "this process will receive no commands -- confirm that topic provisioning has been run.";
			}
			else
			{
				LOG_INFO << "KafkaConsumer: partition contract verified. topic=" << topics[0]
						 << ", partitions=" << actual;
			}
		}

		std::vector<RdKafka::TopicPartition *> assignedPartitions;
		for (int32_t partition : partitions)
		{
			// startAtEnd:每次进程启动都从分区末尾开始,不重放别人的历史。
			// 理由见 kafka_consumer.h::KafkaPartitionAssignPolicy。
            std::int64_t offset=RdKafka::Topic::OFFSET_STORED;
            if(assignPolicy.startAtEnd && !this->manualIdentity.consumer_id().empty()) {
                std::int64_t low=-1,high=-1;
                if(consumer->query_watermark_offsets(topics[0],partition,&low,&high,3000)!=RdKafka::ERR_NO_ERROR || low<0 || high<low) {
                    RdKafka::TopicPartition::destroy(assignedPartitions); consumer->close(); consumer.reset(); return false;
                }
                offset=high; manualStartOffsets[partition]=offset;
            } else if(assignPolicy.startAtEnd) offset=RdKafka::Topic::OFFSET_END;
            assignedPartitions.push_back(RdKafka::TopicPartition::create(topics[0],partition,offset));
		}
		const auto err = consumer->assign(assignedPartitions);
        ++assignmentRevision;
        if(this->prepareClosed){
            if(consumer->pause(assignedPartitions)!=RdKafka::ERR_NO_ERROR) readinessLost=true;
            for(const auto* p:assignedPartitions)if(consumer->seek(*p,3000)!=RdKafka::ERR_NO_ERROR)readinessLost=true;
        }
		for (auto *assignedPartition : assignedPartitions)
		{
			delete assignedPartition;
		}
		if (err || readinessLost)
		{
			LOG_ERROR << "Failed to assign Kafka partitions: " << RdKafka::err2str(err);
			consumer->close();
			consumer.reset();
			return false;
		}
		LOG_INFO << "Assigned to specific partitions. topic=" << topics[0]
				 << ", partition_count=" << partitions.size()
				 << ", start_at_end=" << assignPolicy.startAtEnd
				 << ", own_offsets=" << assignPolicy.ownOffsets;
	}
	else
	{
		// No specific partitions — use group-managed subscription
		RdKafka::ErrorCode err = consumer->subscribe(topics);
		if (err)
		{
			LOG_ERROR << "Failed to subscribe to topics: " << RdKafka::err2str(err);
			consumer->close();
			consumer.reset();
			return false;
		}
	}

	LOG_INFO << "KafkaConsumer initialized, ready to consume messages.";
	return true;
}

KafkaConsumer::~KafkaConsumer()
{
	stop();
}

bool KafkaConsumer::start()
{
	if (!consumer)
	{
		LOG_ERROR << "KafkaConsumer start requested before successful initialization.";
		running = false;
		return false;
	}

	running = true;
	return true;
}

void KafkaConsumer::stop()
{
	const bool wasRunning = running.exchange(false);

	// Tear down background poll thread first. The thread checks running_
	// every loop iteration and bails out within kBackgroundConsumeTimeoutMs,
	// so this join is short even mid-fetch.
	if (pollThreadRunning.exchange(false))
	{
		if (pollThread.joinable())
		{
			pollThread.join();
		}
	}

    std::lock_guard<std::mutex> ownerLock(ownerMutex);
    if (consumer) {
        if(wasRunning && manual && !manualIdentity.consumer_id().empty()) {
            try { stoppedManual=ReadManualLocked("","",manualIdentity.issuer_id(),manualIdentity.instance_id()); }
            catch (...) { readinessLost=true; }
        }
        consumer->close(); consumer.reset();
    }
}

void KafkaConsumer::startBackgroundPolling(muduo::net::EventLoop* dispatchLoop)
{
	if (!consumer || !running)
	{
		LOG_ERROR << "KafkaConsumer::startBackgroundPolling called before init/start.";
		return;
	}
	if (!dispatchLoop)
	{
		LOG_ERROR << "KafkaConsumer::startBackgroundPolling requires a non-null dispatch loop.";
		return;
	}
	if (pollThreadRunning.exchange(true))
	{
		// Already running — refuse to double-spawn. The thread-local singleton
		// pattern guarantees there's only one consumer per worker thread, so a
		// re-init() + restart sequence calls stop() first which clears the flag.
		LOG_INFO << "KafkaConsumer background polling already active; ignoring duplicate start.";
		return;
	}

	this->dispatchLoop = std::ref(*dispatchLoop);
	pollThread = std::thread([this]() {
		backgroundPollLoop();
	});
	LOG_INFO << "KafkaConsumer background polling thread started.";
}

void KafkaConsumer::backgroundPollLoop()
{
	// Blocking consume on a dedicated thread. We hold no muduo locks here;
	// decoded payloads are handed back to the EventLoop via queueInLoop so the
	// existing callback (which touches ECS state) keeps running on the main
	// thread. This is the structural fix for the 45k stress finding: the old
	// 100ms EventLoop timer driver missed poll() under client-connect storms,
	// blowing past max.poll.interval.ms and getting the consumer kicked out
	// of the group.
	while (pollThreadRunning.load(std::memory_order_acquire) && running.load(std::memory_order_acquire))
	{
		consumeOnce(/*blocking=*/true);
	}
}

void KafkaConsumer::dispatch(const std::string& topic, std::string payload, const tracing::TraceContext& trace)
{
    if(!msgCallback) return;
    const auto ledger=dispatchLedger;
    { std::lock_guard<std::mutex> lock(ledger->mutex); if(ledger->accepted==std::numeric_limits<std::uint64_t>::max()){ledger->unknown=true;return;} ++ledger->accepted; }
    struct Ticket { std::shared_ptr<DispatchLedger> ledger; bool returned=false; ~Ticket(){std::lock_guard<std::mutex> lock(ledger->mutex);++ledger->settled;if(!returned)ledger->unknown=true;} };
    // 回调在消息头带来的 trace 下执行:合法就建子 span,没有或不合法就开新根(docs/design/distributed-tracing.md §5.4)。
    // 不论在 poll 线程上就地执行还是投递到 loop 线程,都不接执行线程上碰巧在场的上下文。
    // 这个子 span 就是本条消息的 consumer span(§10.3),名字是 topic,覆盖回调的执行,父是生产端的 producer span。
    struct Work { std::shared_ptr<Ticket> ticket; MessageCallback callback; std::string topic,payload; tracing::TraceContext trace;
        void Run(){ tracing::ScopedIncoming incoming(trace, tracing::SpanKind::kConsumer, tracing::SpanSystem::kKafka, tracing::SpanName{topic}); callback(topic,payload); callback={}; ticket->returned=true; } };
    const auto work=std::make_shared<Work>(); work->ticket=std::make_shared<Ticket>();work->ticket->ledger=ledger;
    work->callback=msgCallback;work->topic=topic;work->payload=std::move(payload);work->trace=trace;
    if(dispatchLoop) dispatchLoop->get().queueInLoop([work]{work->Run();}); else work->Run();
}

void KafkaConsumer::consumeOnce(bool blocking)
{
    std::unique_lock<std::mutex> ownerLock(ownerMutex);
    if(!consumer || !running) return;
	std::unique_ptr<RdKafka::Message> msg{
		consumer->consume(blocking ? kBackgroundConsumeTimeoutMs : 0)};
	if (!msg)
	{
		return;
	}

	switch (msg->err())
	{
	case RdKafka::ERR_NO_ERROR:
        successfulPoll=true;
        if(prepareClosed) {
            std::unique_ptr<RdKafka::TopicPartition> rewind(RdKafka::TopicPartition::create(msg->topic_name(),msg->partition(),msg->offset()));
            if(consumer->seek(*rewind,1000)!=RdKafka::ERR_NO_ERROR) readinessLost=true;
            std::vector<RdKafka::TopicPartition*> one{rewind.get()};
            if(consumer->pause(one)!=RdKafka::ERR_NO_ERROR) readinessLost=true;
            return; // pause之前已排入native队列的记录回退到原offset，绝不丢弃。
        }
        if(manualDrainStarted) {
            const auto target=manualDrainEnds.find(msg->partition());
            if(target==manualDrainEnds.end()){readinessLost=true;return;}
            if(msg->offset()>=target->second){
                std::unique_ptr<RdKafka::TopicPartition> rewind(RdKafka::TopicPartition::create(msg->topic_name(),msg->partition(),target->second));
                std::vector<RdKafka::TopicPartition*> one{rewind.get()};
                if(consumer->seek(*rewind,1000)!=RdKafka::ERR_NO_ERROR||consumer->pause(one)!=RdKafka::ERR_NO_ERROR)readinessLost=true;
                return; // 尾部之后的记录保留在原offset，不把未知新增流量计成已处理。
            }
            if(msg->offset()+1==target->second){
                std::unique_ptr<RdKafka::TopicPartition> part(RdKafka::TopicPartition::create(msg->topic_name(),msg->partition()));
                std::vector<RdKafka::TopicPartition*> one{part.get()};if(consumer->pause(one)!=RdKafka::ERR_NO_ERROR)readinessLost=true;
            }
        }
        ownerLock.unlock();
		// 消息头归 msg 所有,只在这里读:解析结果按值交给派发。
		dispatch(msg->topic_name(),
			std::string(static_cast<const char *>(msg->payload()), msg->len()),
			kafka_trace::TraceFromHeaders(msg->headers()));
		return;

	case RdKafka::ERR__TIMED_OUT:
        successfulPoll=true;
		return;

	case RdKafka::ERR__UNKNOWN_TOPIC:
	case RdKafka::ERR_UNKNOWN_TOPIC_OR_PART:
	{
		static thread_local std::unordered_map<std::string, std::chrono::steady_clock::time_point> sLastWarnByTopic;
		const std::string topic = msg->topic_name();
		const auto now = std::chrono::steady_clock::now();
		auto it = sLastWarnByTopic.find(topic);
		const bool shouldWarn = (it == sLastWarnByTopic.end()) ||
								(std::chrono::duration_cast<std::chrono::seconds>(now - it->second).count() >= kUnknownTopicWarnIntervalSec);
		if (shouldWarn)
		{
			sLastWarnByTopic[topic] = now;
			LOG_WARN << "[KafkaConsumer] Subscribed topic not available yet: " << msg->errstr();
		}
		else
		{
			LOG_DEBUG << "[KafkaConsumer] Subscribed topic not available yet: " << msg->errstr();
		}
		return;
	}

	default:
		LOG_ERROR << "[KafkaConsumer] Error: " << msg->errstr();
		break;
	}
}

void KafkaConsumer::poll()
{
	// Foreground driver. Once startBackgroundPolling() takes over, the timer
	// caller is harmless — we still drain whatever the background thread may
	// have left queued, but consume() is no-op-cheap when the rdkafka queue
	// is empty.
	if (!running || !consumer)
		return;

	// If background polling owns the consumer, skip — the thread is already
	// draining records, and a second concurrent consume() across threads on
	// the same RdKafka::KafkaConsumer is unsafe.
	if (pollThreadRunning.load(std::memory_order_acquire))
	{
		return;
	}

	for (int i = 0; i < kMaxMessagesPerPoll; ++i)
	{
		consumeOnce(/*blocking=*/false);
		// consumeOnce hides err/timeout state inside; the worst case is we
		// iterate kMaxMessagesPerPoll times with no work, which the rdkafka
		// queue check makes cheap.
	}
}

namespace {
using Maintenance=sql_writer_maintenance_receipt;
using PartKey=std::pair<std::string,std::int32_t>;
struct NativeParts { std::vector<RdKafka::TopicPartition*> values; ~NativeParts(){RdKafka::TopicPartition::destroy(values);} };
using Deadline=std::chrono::steady_clock::time_point;
int Remaining(Deadline deadline){const auto n=std::chrono::duration_cast<std::chrono::milliseconds>(deadline-std::chrono::steady_clock::now()).count();if(n<=0)throw std::runtime_error("consumer readiness timeout");return static_cast<int>((std::min)(n,std::int64_t(3000)));}
std::uint64_t ObservedMs(){const auto n=std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::system_clock::now().time_since_epoch()).count();if(n<=0)throw std::runtime_error("consumer clock invalid");return static_cast<std::uint64_t>(n);}
std::string TopicUuid(const rd_kafka_Uuid_t* uuid){if(!uuid)throw std::runtime_error("topic identity missing");std::string raw(16,'\0');const std::uint64_t hi=rd_kafka_Uuid_most_significant_bits(uuid),lo=rd_kafka_Uuid_least_significant_bits(uuid);for(int i=0;i<8;++i){raw[i]=static_cast<char>(hi>>(56-i*8));raw[i+8]=static_cast<char>(lo>>(56-i*8));}if(raw==std::string(16,'\0'))throw std::runtime_error("topic identity zero");return raw;}
// 所有身份来自同native句柄的broker metadata；没有group/UUID配置常量替身。
std::map<PartKey,Maintenance::KafkaPartitionClosure> ReadPhysicalCatalog(RdKafka::KafkaConsumer& consumer,const std::vector<std::string>& topics,Deadline deadline){
    auto* native=consumer.c_ptr();
    char* clusterRaw=rd_kafka_clusterid(native,Remaining(deadline));if(!clusterRaw)throw std::runtime_error("cluster identity unavailable");
    const std::string cluster(clusterRaw);rd_kafka_mem_free(native,clusterRaw);if(cluster.empty())throw std::runtime_error("cluster identity empty");
    std::set<std::string> requested(topics.begin(),topics.end());if(requested.empty()||requested.size()!=topics.size()||topics.size()>128)throw std::runtime_error("topic catalog invalid");
    std::vector<const char*> names;for(const auto& topic:topics)names.push_back(topic.c_str());
    const auto collection=std::unique_ptr<rd_kafka_TopicCollection_t,decltype(&rd_kafka_TopicCollection_destroy)>(rd_kafka_TopicCollection_of_topic_names(names.data(),names.size()),&rd_kafka_TopicCollection_destroy);
    const auto queue=std::unique_ptr<rd_kafka_queue_t,decltype(&rd_kafka_queue_destroy)>(rd_kafka_queue_new(native),&rd_kafka_queue_destroy);
    const auto options=std::unique_ptr<rd_kafka_AdminOptions_t,decltype(&rd_kafka_AdminOptions_destroy)>(rd_kafka_AdminOptions_new(native,RD_KAFKA_ADMIN_OP_DESCRIBETOPICS),&rd_kafka_AdminOptions_destroy);
    char error[256]{};if(!collection||!queue||!options||rd_kafka_AdminOptions_set_request_timeout(options.get(),Remaining(deadline),error,sizeof(error))!=RD_KAFKA_RESP_ERR_NO_ERROR)throw std::runtime_error("topic query unavailable");
    rd_kafka_DescribeTopics(native,collection.get(),options.get(),queue.get());
    const auto event=std::unique_ptr<rd_kafka_event_t,decltype(&rd_kafka_event_destroy)>(rd_kafka_queue_poll(queue.get(),Remaining(deadline)),&rd_kafka_event_destroy);
    if(!event||rd_kafka_event_error(event.get())!=RD_KAFKA_RESP_ERR_NO_ERROR)throw std::runtime_error("topic query failed");
    const auto* result=rd_kafka_event_DescribeTopics_result(event.get());if(!result)throw std::runtime_error("topic result invalid");
    std::size_t count=0;const auto descriptions=rd_kafka_DescribeTopics_result_topics(result,&count);if(count!=topics.size())throw std::runtime_error("topic result incomplete");
    std::map<PartKey,Maintenance::KafkaPartitionClosure> catalog;
    for(std::size_t i=0;i<count;++i){const auto* d=descriptions[i];const auto* error=rd_kafka_TopicDescription_error(d);if(error&&rd_kafka_error_code(error)!=RD_KAFKA_RESP_ERR_NO_ERROR)throw std::runtime_error("topic rejected");
        const char* topicRaw=rd_kafka_TopicDescription_name(d);if(!topicRaw||!requested.erase(topicRaw))throw std::runtime_error("topic mismatch");const std::string topic(topicRaw),id=TopicUuid(rd_kafka_TopicDescription_topic_id(d));
        std::size_t partitions=0;const auto rows=rd_kafka_TopicDescription_partitions(d,&partitions);if(!rows||partitions==0||partitions>4096)throw std::runtime_error("partition catalog invalid");
        for(std::size_t j=0;j<partitions;++j){const int p=rd_kafka_TopicPartitionInfo_partition(rows[j]);if(p<0||p>=static_cast<int>(partitions))throw std::runtime_error("partition number invalid");Maintenance::KafkaPartitionClosure row;row.set_cluster_id(cluster);row.set_topic_id(id);row.set_topic_name(topic);row.set_partition(p);if(!catalog.emplace(PartKey{topic,p},std::move(row)).second)throw std::runtime_error("duplicate partition");}
    }
    if(!requested.empty())throw std::runtime_error("topic catalog missing");return catalog;
}
void Watermarks(RdKafka::KafkaConsumer& consumer,const PartKey& key,Deadline deadline,std::int64_t& low,std::int64_t& high){if(consumer.query_watermark_offsets(key.first,key.second,&low,&high,Remaining(deadline))!=RdKafka::ERR_NO_ERROR||low<0||high<low)throw std::runtime_error("leader offsets unavailable");}
std::pair<std::int32_t,std::string> GroupIdentity(RdKafka::KafkaConsumer& consumer){
    const auto metadata=std::unique_ptr<rd_kafka_consumer_group_metadata_t,decltype(&rd_kafka_consumer_group_metadata_destroy)>(rd_kafka_consumer_group_metadata(consumer.c_ptr()),&rd_kafka_consumer_group_metadata_destroy);
    if(!metadata)throw std::runtime_error("group identity missing");const auto generation=rd_kafka_consumer_group_metadata_generation_id(metadata.get());const char* member=rd_kafka_consumer_group_metadata_member_id(metadata.get());
    if(generation<=0||!member||!*member||consumer.assignment_lost())throw std::runtime_error("group generation unavailable");return {generation,std::string(member)};
}
}

void KafkaConsumer::rebalance_cb(RdKafka::KafkaConsumer* consumer,RdKafka::ErrorCode error,std::vector<RdKafka::TopicPartition*>& partitions){
    // 回调只由持ownerMutex_的consume/close同步派发；不可另锁同一mutex。
    if(assignmentRevision==std::numeric_limits<std::uint64_t>::max())readinessLost=true;else ++assignmentRevision;
    prepared.reset();successfulPoll=false;
    const bool cooperative=consumer->rebalance_protocol()=="COOPERATIVE";
    if(error==RdKafka::ERR__ASSIGN_PARTITIONS){
        if(cooperative){std::unique_ptr<RdKafka::Error> e(consumer->incremental_assign(partitions));if(e)readinessLost=true;}
        else if(consumer->assign(partitions)!=RdKafka::ERR_NO_ERROR)readinessLost=true;
        if(prepareClosed&&consumer->pause(partitions)!=RdKafka::ERR_NO_ERROR)readinessLost=true;
    }else if(error==RdKafka::ERR__REVOKE_PARTITIONS){
        if(cooperative){std::unique_ptr<RdKafka::Error> e(consumer->incremental_unassign(partitions));if(e)readinessLost=true;}
        else if(consumer->unassign()!=RdKafka::ERR_NO_ERROR)readinessLost=true;
    }else{readinessLost=true;consumer->unassign();}
}

KafkaConsumer::Maintenance::ManualConsumerEvidence KafkaConsumer::ReadManualLocked(const std::string& operation,const std::string& manifest,const std::string& issuer,const std::string& instance){
    if(!manual||!consumer||manualIdentity.consumer_id().empty()||instance.size()!=16||manualIdentity.instance_id()!=instance||manualIdentity.target_instance_id()!=instance||manualIdentity.target_node_id()==0||topics.size()!=1||readinessLost)throw std::runtime_error("manual owner unavailable");
    const auto deadline=std::chrono::steady_clock::now()+std::chrono::seconds(3);const auto catalog=ReadPhysicalCatalog(*consumer,topics,deadline);
    const std::uint32_t count=static_cast<std::uint32_t>(catalog.size());if(count!=manualIdentity.partition_count())throw std::runtime_error("manual partition count changed");
    NativeParts assigned;if(consumer->assignment(assigned.values)!=RdKafka::ERR_NO_ERROR||assigned.values.empty()||consumer->position(assigned.values)!=RdKafka::ERR_NO_ERROR)throw std::runtime_error("manual assignment unavailable");
    Maintenance::ManualConsumerEvidence report;report.set_operation_id(operation);report.set_manifest_digest(manifest);
    for(const auto* p:assigned.values){const PartKey key{p->topic(),p->partition()};const auto physical=catalog.find(key);const auto start=manualStartOffsets.find(p->partition());
        if(physical==catalog.end()||p->err()!=RdKafka::ERR_NO_ERROR||p->offset()<0||start==manualStartOffsets.end()||p->partition()!=static_cast<std::int32_t>(manualIdentity.target_node_id()%count))throw std::runtime_error("manual assignment mismatch");
        std::int64_t low=-1,high=-1;Watermarks(*consumer,key,deadline,low,high);if(p->offset()<low||p->offset()>high||start->second<0)throw std::runtime_error("manual position outside leader range");
        auto* row=report.add_assignments();*row=manualIdentity;row->set_issuer_id(issuer);row->set_instance_id(instance);row->set_cluster_id(physical->second.cluster_id());row->set_topic_id(physical->second.topic_id());row->set_topic_name(p->topic());row->set_partition_count(count);row->set_partition(p->partition());row->set_assignment_revision(assignmentRevision);row->set_start_offset(start->second);row->set_position(p->offset());row->set_earliest(low);row->set_exclusive_end(high);row->set_observed_ms(ObservedMs());
    }
    return report;
}

bool KafkaConsumer::BeginManualDrain(){
    std::lock_guard<std::mutex> lock(ownerMutex);if(!manual)return true;
    if(manualDrainStarted)return !readinessLost;
    if(!consumer||!running||prepareClosed||readinessLost)return false;
    try{
        const auto snapshot=ReadManualLocked("","",manualIdentity.issuer_id(),manualIdentity.instance_id());
        NativeParts pause;
        for(const auto& row:snapshot.assignments()){
            if(!manualDrainEnds.emplace(row.partition(),row.exclusive_end()).second)return false;
            if(row.position()==row.exclusive_end())pause.values.push_back(RdKafka::TopicPartition::create(row.topic_name(),row.partition()));
        }
        if(!pause.values.empty()&&consumer->pause(pause.values)!=RdKafka::ERR_NO_ERROR){readinessLost=true;return false;}
        manualDrainStarted=true;return true;
    }catch(...){readinessLost=true;return false;}
}
bool KafkaConsumer::ManualDrainReady(){
    std::lock_guard<std::mutex> lock(ownerMutex);if(!manual)return true;
    if(!manualDrainStarted||readinessLost||!consumer)return false;
    NativeParts parts;if(consumer->assignment(parts.values)!=RdKafka::ERR_NO_ERROR||consumer->position(parts.values)!=RdKafka::ERR_NO_ERROR||parts.values.size()!=manualDrainEnds.size())return false;
    for(const auto* part:parts.values){const auto target=manualDrainEnds.find(part->partition());if(target==manualDrainEnds.end()||part->err()!=RdKafka::ERR_NO_ERROR||part->offset()!=target->second)return false;}
    return true;
}

std::optional<KafkaConsumer::Maintenance::ManualConsumerEvidence> KafkaConsumer::ManualEvidence(const std::string& operation,const std::string& manifest,const std::string& issuer,const std::string& instance){
    std::lock_guard<std::mutex> lock(ownerMutex);if(!manual)return std::nullopt;
    try{auto report=stoppedManual?*stoppedManual:ReadManualLocked(operation,manifest,issuer,instance);
        report.set_operation_id(operation);report.set_manifest_digest(manifest);for(auto& a:*report.mutable_assignments()){a.set_issuer_id(issuer);if(a.instance_id()!=instance)throw std::runtime_error("manual boot changed");}
        const bool closed=!running&&!consumer;
        std::lock_guard<std::mutex> callbackLock(dispatchLedger->mutex);report.set_accepting_closed(closed||prepareClosed);report.set_callbacks_joined(dispatchLedger->accepted==dispatchLedger->settled);
        report.set_accepted(dispatchLedger->accepted);report.set_settled(dispatchLedger->settled);report.set_unresolved_publications((readinessLost||dispatchLedger->unknown)?1:0);
        if(closed&&!stoppedManual)return std::nullopt;
        if(closed){if(!manualDrainStarted)return std::nullopt;for(const auto& row:report.assignments()){const auto end=manualDrainEnds.find(row.partition());if(end==manualDrainEnds.end()||row.position()!=end->second||row.exclusive_end()!=end->second)return std::nullopt;}}
        return report;
    }catch(...){return std::nullopt;}
}

std::optional<KafkaConsumer::Maintenance::ConsumerReadiness> KafkaConsumer::PrepareReadiness(const Maintenance::Plan& plan,const std::string& manifest,const std::string& issuer,const std::string& instance){
    std::lock_guard<std::mutex> lock(ownerMutex);
    if(!consumer||!running||!prepareClosed||!successfulPoll||readinessLost||plan.run_id().size()!=16||manifest.size()!=32||issuer.size()!=16||instance.size()!=16)return std::nullopt;
    {std::lock_guard<std::mutex> cb(dispatchLedger->mutex);if(dispatchLedger->accepted!=0||dispatchLedger->settled!=0||dispatchLedger->unknown)return std::nullopt;}
    try{
        Maintenance::ConsumerReadiness readiness;
        if(manual){auto report=ReadManualLocked(plan.run_id(),manifest,issuer,instance);report.set_accepting_closed(true);report.set_callbacks_joined(true);
            for(const auto& row:report.assignments()){
                if(row.start_offset()!=row.position()||row.earliest()>row.position()||row.position()>row.exclusive_end())return std::nullopt;
                bool fixed=false;for(const auto& old:plan.manual_assignments())if(old.issuer_id()==issuer&&old.topic_name()==row.topic_name()&&old.topic_id()==row.topic_id()&&old.cluster_id()==row.cluster_id()&&old.partition_count()==row.partition_count())fixed=true;
                if(!fixed)return std::nullopt;
            }
            *readiness.add_manual_assignments()=std::move(report);
        }else{
            const auto deadline=std::chrono::steady_clock::now()+std::chrono::seconds(3);const auto generation=GroupIdentity(*consumer);const auto revision=assignmentRevision;
            const auto catalog=ReadPhysicalCatalog(*consumer,topics,deadline);std::map<PartKey,Maintenance::KafkaPartitionClosure> expected;
            for(const auto& seed:plan.kafka_partitions())if(seed.group()==groupId&&std::find(topics.begin(),topics.end(),seed.topic_name())!=topics.end()){
                const PartKey key{seed.topic_name(),static_cast<std::int32_t>(seed.partition())};const auto actual=catalog.find(key);
                if(actual==catalog.end()||actual->second.cluster_id()!=seed.cluster_id()||actual->second.topic_id()!=seed.topic_id()||!expected.emplace(key,seed).second)return std::nullopt;
            }
            if(expected.size()!=catalog.size())return std::nullopt;
            NativeParts assigned;if(consumer->assignment(assigned.values)!=RdKafka::ERR_NO_ERROR||consumer->committed(assigned.values,Remaining(deadline))!=RdKafka::ERR_NO_ERROR)return std::nullopt;
            auto* group=readiness.add_groups();group->set_operation_id(plan.run_id());group->set_manifest_digest(manifest);group->set_issuer_id(issuer);group->set_instance_id(instance);group->set_group(groupId);group->set_generation_id(generation.first);group->set_member_id(generation.second);
            for(const auto& pair:expected)*group->add_expected_partitions()=pair.second;
            std::set<PartKey> seen;for(auto* p:assigned.values){const PartKey key{p->topic(),p->partition()};const auto seed=expected.find(key);if(seed==expected.end()||!seen.insert(key).second||p->err()!=RdKafka::ERR_NO_ERROR||p->offset()<0)return std::nullopt;
                std::int64_t low=-1,high=-1;Watermarks(*consumer,key,deadline,low,high);if(p->offset()!=high||low>p->offset()||p->offset()<seed->second.protected_committed_floor())return std::nullopt;
                auto* row=group->add_assigned_partitions();*row=seed->second;row->set_final_committed(p->offset());row->set_final_earliest(low);row->set_final_exclusive_end(high);
                // 暂停准备期未派发业务；把尚未解析的position固定到真实已提交next offset，P9不得再seek。
                if(consumer->seek(*p,Remaining(deadline))!=RdKafka::ERR_NO_ERROR)return std::nullopt;
            }
            if(generation!=GroupIdentity(*consumer)||revision!=assignmentRevision)return std::nullopt;
        }
        prepared=readiness;return readiness;
    }catch(...){return std::nullopt;}
}

bool KafkaConsumer::CurrentPreparedMatches(const Maintenance::ConsumerReadiness& expected){
    if(!consumer||!running||!prepareClosed||readinessLost||!prepared||!google::protobuf::util::MessageDifferencer::Equals(*prepared,expected))return false;
    {std::lock_guard<std::mutex> cb(dispatchLedger->mutex);if(dispatchLedger->accepted!=dispatchLedger->settled||dispatchLedger->unknown)return false;}
    try{
        const auto deadline=std::chrono::steady_clock::now()+std::chrono::seconds(3);const auto catalog=ReadPhysicalCatalog(*consumer,topics,deadline);NativeParts assigned;
        if(consumer->assignment(assigned.values)!=RdKafka::ERR_NO_ERROR||consumer->position(assigned.values)!=RdKafka::ERR_NO_ERROR)return false;
        std::map<PartKey,std::int64_t> wanted;
        if(manual){if(expected.manual_assignments_size()!=1||expected.groups_size()!=0)return false;
            for(const auto& row:expected.manual_assignments(0).assignments()){
                const PartKey key{row.topic_name(),static_cast<std::int32_t>(row.partition())};const auto physical=catalog.find(key);
                if(catalog.size()!=row.partition_count()||row.assignment_revision()!=assignmentRevision||row.target_instance_id()!=manualIdentity.target_instance_id()||row.target_node_id()!=manualIdentity.target_node_id()||physical==catalog.end()||physical->second.topic_id()!=row.topic_id()||physical->second.cluster_id()!=row.cluster_id()||!wanted.emplace(key,row.start_offset()).second)return false;
            }
        }else{if(expected.groups_size()!=1||expected.manual_assignments_size()!=0)return false;const auto& group=expected.groups(0);
            if(GroupIdentity(*consumer)!=std::make_pair(group.generation_id(),group.member_id())||static_cast<std::size_t>(group.expected_partitions_size())!=catalog.size())return false;
            NativeParts committed;for(const auto* current:assigned.values)committed.values.push_back(RdKafka::TopicPartition::create(current->topic(),current->partition()));
            if(consumer->committed(committed.values,Remaining(deadline))!=RdKafka::ERR_NO_ERROR)return false;
            for(const auto* current:committed.values){int found=0;for(const auto& row:group.assigned_partitions())if(row.topic_name()==current->topic()&&row.partition()==static_cast<std::uint32_t>(current->partition())&&row.final_committed()==current->offset()&&current->err()==RdKafka::ERR_NO_ERROR)++found;if(found!=1)return false;}
            for(const auto& row:group.expected_partitions()){const auto physical=catalog.find({row.topic_name(),static_cast<std::int32_t>(row.partition())});if(physical==catalog.end()||physical->second.topic_id()!=row.topic_id()||physical->second.cluster_id()!=row.cluster_id())return false;}
            for(const auto& row:group.assigned_partitions())if(!wanted.emplace(PartKey{row.topic_name(),static_cast<std::int32_t>(row.partition())},row.final_committed()).second)return false;
        }
        if(wanted.size()!=assigned.values.size())return false;for(const auto* p:assigned.values){const auto found=wanted.find({p->topic(),p->partition()});if(found==wanted.end()||p->err()!=RdKafka::ERR_NO_ERROR||p->offset()!=found->second)return false;std::int64_t low=-1,high=-1;Watermarks(*consumer,{p->topic(),p->partition()},deadline,low,high);if(p->offset()<low||p->offset()>high)return false;}
        return true; // end增长合法，绝不把当前位置重置到新end。
    }catch(...){return false;}
}
KafkaConsumer::PreparedLease::PreparedLease(KafkaConsumer& owner):owner(owner),lock(owner.ownerMutex){}
KafkaConsumer::PreparedLease::~PreparedLease(){if(resumed&&!committed&&owner.consumer){NativeParts parts;if(owner.consumer->assignment(parts.values)!=RdKafka::ERR_NO_ERROR||owner.consumer->pause(parts.values)!=RdKafka::ERR_NO_ERROR)owner.readinessLost=true;}}
bool KafkaConsumer::PreparedLease::Resume(){if(resumed)return true;NativeParts parts;if(!owner.consumer||owner.consumer->assignment(parts.values)!=RdKafka::ERR_NO_ERROR)return false;resumed=true;return owner.consumer->resume(parts.values)==RdKafka::ERR_NO_ERROR;}
void KafkaConsumer::PreparedLease::Commit(){if(!resumed)throw std::logic_error("consumer lease not resumed");committed=true;owner.prepareClosed=false;owner.prepared.reset();}
std::unique_ptr<KafkaConsumer::PreparedLease> KafkaConsumer::AcquirePrepared(const Maintenance::ConsumerReadiness& expected){auto lease=std::unique_ptr<PreparedLease>(new PreparedLease(*this));if(!CurrentPreparedMatches(expected))return {};return lease;}

bool KafkaConsumer::CallbacksJoined(bool requireKnown) const{
    std::lock_guard<std::mutex> lock(dispatchLedger->mutex);return dispatchLedger->accepted==dispatchLedger->settled&&(!requireKnown||!dispatchLedger->unknown);
}
