#include "kafka_manager.h"
#include "proto/common/base/config.pb.h"
#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <boost/algorithm/string/join.hpp>
#include <limits>
#include "google/protobuf/util/message_differencer.h"
#include "messaging/kafka/kafka_consumer.h"
#include "messaging/kafka/kafka_producer.h"

namespace {
std::string JoinBrokers(const KafkaConfig& config) {
	std::vector<std::string> brokersVec;
	for (const auto& broker : config.brokers()) {
		brokersVec.push_back(broker);
	}

	return boost::algorithm::join(brokersVec, ",");
}
}

KafkaManager::KafkaManager() = default;

KafkaManager::~KafkaManager() {
	Shutdown();
}

bool KafkaManager::Init(const KafkaConfig& config) {
	// 生产端 broker 与本节点有没有消费者无关,无条件登记。setBrokers 只存字符串,
	// librdkafka 句柄仍在第一次 send() 时惰性创建。
	//
	// 2026-10 之前这里只在 Kafka.GroupID 为空时登记;非空时走一次不带回调的 Subscribe,
	// 那次调用必然在登记 broker 之前返回 false(返回值又被 Node::InitKafka 丢掉)。只生产不消费的
	// 节点(battle)因此在 GroupID 非空的部署里拿不到 broker,每次 send() 都被拒 —— K8s 的 node
	// ConfigMap 曾写死 GroupID: "game-consumer-group"(k8s_deploy.ps1 的 New-NodeConfigMapYaml;
	// 2026-10-15 的 P0-B 批 2 改为从 bin/etc 镜像)。见 docs/handoff/global-singletons-p0-findings-20261014.md P0-B。
	const std::string brokers = JoinBrokers(config);
	if (brokers.empty()) {
		LOG_ERROR << "KafkaManager::Init failed: Kafka.Brokers is empty.";
		return false;
	}
	KafkaProducer::Instance().setBrokers(brokers);
	// 没有顶层默认订阅:消费一律经 Node::RegisterKafkaMessageHandler -> Subscribe,组名由那里给出。
	return true;
}

bool KafkaManager::Subscribe(const KafkaConfig& config,
	const std::vector<std::string>& topics,
	const std::string& groupId,
	const std::vector<int32_t>& partitions,
	KafkaMessageCallback callback,
	const KafkaPartitionAssignPolicy& assignPolicy,
    const Maintenance::ManualConsumerAssignment& manualIdentity) {
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

	auto consumer = std::make_shared<KafkaConsumer>();
	if (!consumer->init(
		brokers,
		effectiveGroupId,
		topics,
		partitions,
		[state = callbacks_, callback = std::move(callback)](const std::string& topic, const std::string& payload) {
			{ std::lock_guard<std::mutex> lock(state->mutex);
			  if (state->accepted == std::numeric_limits<std::uint64_t>::max()) { state->unknown = true; return; }
			  ++state->accepted; }
			try { callback(topic, payload); }
			catch (...) { std::lock_guard<std::mutex> lock(state->mutex); state->unknown = true; ++state->completed; throw; }
			{ std::lock_guard<std::mutex> lock(state->mutex); ++state->completed; }
		},
		assignPolicy, manualIdentity, prepareOnly_
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
	if (shutdown_) { return false; }

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
    // stop已close/reset native并join；保留只读快照owner直到P3原件写完，异步控制工作仅持shared owner。
	// stop 已真实 join，之后不会再向 loop 投递；FIFO 标记排在已有回调之后。
	const auto state = callbacks_;
	const auto joined = [state] { std::lock_guard<std::mutex> lock(state->mutex); state->joined = true; };
	if (dispatchLoop_) { dispatchLoop_->get().queueInLoop(joined); } else { joined(); }
}

bool KafkaManager::ConsumersJoined() const {
    for(const auto& consumer:consumers_)if(consumer&&!consumer->CallbacksJoined(false))return false;
    std::lock_guard<std::mutex> lock(callbacks_->mutex);
    return consumersStopped_ && callbacks_->joined && callbacks_->accepted == callbacks_->completed;
}

bool KafkaManager::LocalWorkJoined() const {
    const auto producers = KafkaProducer::ProcessPublicationFrontier();
    return shutdown_ && ConsumersJoined() && producers.closed && producers.activeSends == 0 && producers.activeCallbacks == 0 && producers.openHandles == 0;
}

std::optional<std::string> KafkaManager::ClosedPublicationFrontier() const {
    for(const auto& consumer:consumers_)if(consumer&&!consumer->CallbacksJoined(true))return std::nullopt;
    const auto producers = KafkaProducer::ProcessPublicationFrontier();
    std::lock_guard<std::mutex> lock(callbacks_->mutex);
    if (!shutdown_ || !producers.Settled() || !callbacks_->joined || callbacks_->unknown || callbacks_->accepted != callbacks_->completed) { return std::nullopt; }
    // 编码仅为本 owner 的运行时账本；正式 proto 身份和原件摘要由 Node 绑定。
    return std::string("kafka-frontier-v1:") + std::to_string(producers.accepted) + ":" +
        std::to_string(producers.confirmed) + ":" + std::to_string(callbacks_->accepted) + ":" + std::to_string(callbacks_->completed);
}

bool KafkaManager::Shutdown(std::chrono::milliseconds producerFlushTimeout) {
    if (!shutdown_) {
        shutdown_ = true;
        StopConsumers();
        KafkaProducer::CloseProcessAdmission();
        shutdownResult_ = KafkaProducer::Instance().StopAndJoin(producerFlushTimeout);
    }
    // 其它线程必须在自己的 poll/析构中完成 native join；不从主线程访问其 TLS 句柄。
    // 首次返回 false 后允许只读重查；UNKNOWN 永久保留，绝不重发消息补成功。
    const bool settled = shutdownResult_ && ClosedPublicationFrontier().has_value();
    if (!settled) { LOG_ERROR << "Kafka lifecycle HOLD: producer/callback frontier is not settled"; }
    return settled;
}

std::future<std::optional<KafkaManager::Maintenance::ConsumerReadiness>> KafkaManager::PrepareReadiness(const Maintenance::Plan& plan,const std::string& manifest,const std::string& issuer,const std::string& instance){
    const auto owners=consumers_;const auto state=prepared_;const bool allowed=prepareOnly_&&!consumersStopped_&&!shutdown_;
    return std::async(std::launch::async,[owners,state,allowed,plan,manifest,issuer,instance]()->std::optional<Maintenance::ConsumerReadiness>{
        if(!allowed)return std::nullopt;Maintenance::ConsumerReadiness aggregate;std::vector<Maintenance::ConsumerReadiness> reports;
        for(const auto& owner:owners){auto report=owner->PrepareReadiness(plan,manifest,issuer,instance);if(!report)return std::nullopt;
            for(const auto& group:report->groups())*aggregate.add_groups()=group;
            for(const auto& manual:report->manual_assignments())*aggregate.add_manual_assignments()=manual;
            reports.push_back(std::move(*report));
        }
        std::lock_guard<std::mutex> lock(state->mutex);state->aggregate=aggregate;state->owners=std::move(reports);return aggregate;
    });
}
bool KafkaManager::OpenPrepared(const Maintenance::ConsumerReadiness& expected,const std::function<bool()>& openSource){
    if(!prepareOnly_||consumersStopped_||shutdown_||!openSource)return false;
    std::vector<Maintenance::ConsumerReadiness> reports;
    {std::lock_guard<std::mutex> lock(prepared_->mutex);if(!prepared_->aggregate||!google::protobuf::util::MessageDifferencer::Equals(expected,*prepared_->aggregate)||prepared_->owners.size()!=consumers_.size())return false;reports=prepared_->owners;}
    std::vector<std::unique_ptr<KafkaConsumer::PreparedLease>> leases;
    for(std::size_t i=0;i<consumers_.size();++i){auto lease=consumers_[i]->AcquirePrepared(reports[i]);if(!lease)return false;leases.push_back(std::move(lease));}
    for(auto& lease:leases)if(!lease->Resume())return false;
    // 所有native owner锁仍被lease持有；失败会pause，成功才在同临界区最后打开真实SOURCE。
    if(!openSource())return false;
    for(auto& lease:leases)lease->Commit();prepareOnly_=false;return true;
}
std::future<std::optional<KafkaManager::Maintenance::ManualConsumerEvidence>> KafkaManager::ManualEvidence(const std::string& operation,const std::string& manifest,const std::string& issuer,const std::string& instance){
    const auto owners=consumers_;
    return std::async(std::launch::async,[owners,operation,manifest,issuer,instance]()->std::optional<Maintenance::ManualConsumerEvidence>{
        Maintenance::ManualConsumerEvidence aggregate;aggregate.set_operation_id(operation);aggregate.set_manifest_digest(manifest);aggregate.set_accepting_closed(true);aggregate.set_callbacks_joined(true);
        for(const auto& owner:owners){if(!owner->HasManualAssignment())continue;auto report=owner->ManualEvidence(operation,manifest,issuer,instance);if(!report)return std::nullopt;
            for(const auto& assignment:report->assignments())*aggregate.add_assignments()=assignment;
            aggregate.set_accepting_closed(aggregate.accepting_closed()&&report->accepting_closed());aggregate.set_callbacks_joined(aggregate.callbacks_joined()&&report->callbacks_joined());
            if(aggregate.accepted()>std::numeric_limits<std::uint64_t>::max()-report->accepted()||aggregate.settled()>std::numeric_limits<std::uint64_t>::max()-report->settled()||aggregate.unresolved_publications()>std::numeric_limits<std::uint64_t>::max()-report->unresolved_publications())return std::nullopt;
            aggregate.set_accepted(aggregate.accepted()+report->accepted());aggregate.set_settled(aggregate.settled()+report->settled());aggregate.set_unresolved_publications(aggregate.unresolved_publications()+report->unresolved_publications());
        }
        return aggregate;
    });
}

std::future<bool> KafkaManager::BeginManualDrain(){
    const auto owners=consumers_;return std::async(std::launch::async,[owners]{for(const auto& owner:owners)if(!owner->BeginManualDrain())return false;return true;});
}
bool KafkaManager::ManualDrainReady() const{for(const auto& owner:consumers_)if(!owner->ManualDrainReady())return false;return true;}
