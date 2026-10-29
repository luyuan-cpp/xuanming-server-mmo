#pragma once

// librdkafka 的 C++ 包装层(rdkafkacpp.h 声明的 RdKafka::* 类)在本仓库是
// **随工程一起从源码编译**的(third_party/librdkafka/src-cpp/*.cpp 已加入 infra),
// 而不是链接预编译的 rdkafka++.dll。原因:仓里那份 rdkafka++.dll 是 /MT(静态 CRT)
// Release 构建,它自带一套 CRT 与堆,std::string 的内存布局也按 _ITERATOR_DEBUG_LEVEL=0
// 排布;而本工程本地只能出 Debug(IDL=2,string 多一个 _Myproxy 指针,布局 40 vs 32 字节)。
// 于是 Conf::set(const std::string&, ...) 这类调用一跨 DLL 边界,参数就被按错误布局解读,
// 每个配置项都设置失败,库随即往 errstr 里写字符串 —— 那块内存在 DLL 的堆上分配、
// 却由本进程析构释放,直接堆损坏。表现是 gate/scene 启动时静默"卡住"(崩在
// KafkaConsumer::init 的 string 析构里,而崩溃处理器又卡在 boost::stacktrace 初始化 dbgeng),
// 既不退出也不打日志。把包装层编进本进程后,跨边界的只剩 rdkafka.dll 的 C 接口
// (不透明句柄 + const char*),与 CRT 无关。
//
// LIBRDKAFKA_STATICLIB 必须在包含 rdkafkacpp.h **之前**定义:否则 RD_EXPORT 会展开成
// __declspec(dllimport),而我们是在本模块内定义这些符号的。
#ifndef LIBRDKAFKA_STATICLIB
#define LIBRDKAFKA_STATICLIB
#endif

// C++ 包装层随工程编译后,它调用的 rd_kafka_* 全部落在 rdkafka.dll 的导入库里。
// 用 #pragma 而不是逐个工程加链接项:凡是链了 infra 的目标(三个节点 + 各 test
// 工程)都会自动带上,新增测试工程不用再记得补一条。
#ifdef _MSC_VER
#pragma comment(lib, "rdkafka.lib")
#endif

#include <atomic>
#include <cstdint>
#include <condition_variable>
#include <functional>
#include <memory>
#include <mutex>
#include <map>
#include <optional>
#include <string>
#include <thread>
#include <vector>

#include <rdkafkacpp.h>
#include "proto/common/database/rollback_database_table.pb.h"

namespace muduo { namespace net { class EventLoop; } }

#include "messaging/kafka/kafka_partition_assign_policy.h"

class KafkaConsumer : private RdKafka::RebalanceCb {
public:
	using MessageCallback = std::function<void(const std::string&, const std::string&)>;

	bool init(const std::string& brokers,
		const std::string& groupId,
		const std::vector<std::string>& topics,
		const std::vector<int32_t>& partitions,  // Partitions to consume
		const MessageCallback& callback,
		const KafkaPartitionAssignPolicy& assignPolicy = {},
        const sql_writer_maintenance_receipt::ManualConsumerAssignment& manualIdentity = {},
        bool prepareClosed = false);

	static KafkaConsumer& Instance() {
		thread_local KafkaConsumer instance;
		return instance;
	}

	~KafkaConsumer();

	bool start();
	void stop();

	// Non-blocking poll — keeps existing main-loop driver working while
	// callers migrate to background polling. Safe to call from the muduo
	// EventLoop. No-op if background polling is active.
	void poll();

	// Start a dedicated polling thread that drives consume() independently
	// of the muduo EventLoop. Decoded messages are dispatched via
	// EventLoop::queueInLoop on `dispatchLoop`, preserving the single-
	// threaded callback invariant the rest of the codebase relies on.
	//
	// Must be called AFTER init() succeeds. Idempotent: a second call is a
	// no-op once the thread is running.
	void startBackgroundPolling(muduo::net::EventLoop* dispatchLoop);


    using Maintenance = sql_writer_maintenance_receipt;
    bool HasManualAssignment() const { return manual_; }
    bool BeginManualDrain(); // P2之后在owner锁内固定leader尾部，继续消费至该位置再pause。
    bool ManualDrainReady();
    bool CallbacksJoined(bool requireKnown) const;
    // 快照可在专用文件/控制工作线程调用；有界网络查询只在owner锁内，poll/rebalance与open共用此锁。
    std::optional<Maintenance::ConsumerReadiness> PrepareReadiness(const Maintenance::Plan& plan,
        const std::string& manifest, const std::string& issuer, const std::string& instance);
    std::optional<Maintenance::ManualConsumerEvidence> ManualEvidence(const std::string& operation,
        const std::string& manifest, const std::string& issuer, const std::string& instance);
    // manager按固定consumer顺序全部取得lease、全部Resume，再在同临界区一次开SOURCE。
    // 任一步失败时析构重新pause；Commit只在SOURCE已实际打开后调用，禁止callback先做可失败副作用。
    class PreparedLease {
    public:
        ~PreparedLease();
        PreparedLease(const PreparedLease&) = delete;
        PreparedLease& operator=(const PreparedLease&) = delete;
        bool Resume();
        void Commit();
    private:
        friend class KafkaConsumer;
        explicit PreparedLease(KafkaConsumer& owner);
        KafkaConsumer& owner_;
        std::unique_lock<std::mutex> lock_;
        bool resumed_ = false;
        bool committed_ = false;
    };
    std::unique_ptr<PreparedLease> AcquirePrepared(const Maintenance::ConsumerReadiness& expected);

private:
	void backgroundPollLoop();
	void dispatch(const std::string& topic, std::string payload);
	void consumeOnce(bool blocking);
	// 向 broker 查 topic 的分区数。查不到返回 -1(区别于"查到 0")。
	int32_t queryTopicPartitionCount(const std::string& topic);

    void rebalance_cb(RdKafka::KafkaConsumer* consumer, RdKafka::ErrorCode error,
        std::vector<RdKafka::TopicPartition*>& partitions) override;
    bool CurrentPreparedMatches(const Maintenance::ConsumerReadiness& expected);
    Maintenance::ManualConsumerEvidence ReadManualLocked(const std::string& operation,
        const std::string& manifest, const std::string& issuer, const std::string& instance);
    // 本机账本不与正式proto重复：只计队列闭包的实际进入、回调返回及target析构。
    struct DispatchLedger { std::mutex mutex; std::uint64_t accepted=0, settled=0; bool unknown=false; };
    std::shared_ptr<DispatchLedger> dispatchLedger_=std::make_shared<DispatchLedger>();
    std::mutex ownerMutex_;
    std::string groupId_;
    std::vector<std::string> topics_;
    bool manual_=false, prepareClosed_=false, readinessLost_=false, successfulPoll_=false;
    std::uint64_t assignmentRevision_=0;
    Maintenance::ManualConsumerAssignment manualIdentity_;
    std::optional<Maintenance::ManualConsumerEvidence> stoppedManual_;
    std::optional<Maintenance::ConsumerReadiness> prepared_;
    std::map<std::int32_t,std::int64_t> manualStartOffsets_;
    std::map<std::int32_t,std::int64_t> manualDrainEnds_;
    bool manualDrainStarted_=false;
    // callback基类及账本活过native handle；stop先join线程、close/reset handle，最后才销毁本对象。
	std::unique_ptr<RdKafka::KafkaConsumer> consumer_;
	std::unique_ptr<RdKafka::Conf> conf_;
	MessageCallback msgCallback_;
	std::atomic<bool> running_{false};

	// Background poll thread state.
	std::thread pollThread_;
	std::atomic<bool> pollThreadRunning_{false};
	std::optional<std::reference_wrapper<muduo::net::EventLoop>> dispatchLoop_;
};
