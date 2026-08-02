#include "node.h"
#include <regex>
#include <boost/uuid/uuid_io.hpp>
#include "table/code/all_table.h"
#include "node/system/etcd/etcd_helper.h"
#include "config.h"
#include "google/protobuf/util/json_util.h"
#include "google/protobuf/util/message_differencer.h"
#include "grpc_client/grpc_init_client.h"
#include "grpc_client/etcd/etcd_grpc_client.h"
#include "log/constants/log_constants.h"
#include "log/system/console_log.h"
#include "proto/common/event/server_event.pb.h"
#include "muduo/base/TimeZone.h"
#include "network/process_info.h"
#include "network/rpc_session.h"
#include "node/system/node/node_util.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/event/node_event.pb.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "thread_context/redis_manager.h"
#include "thread_context/snow_flake_manager.h"
#include "time/system/time.h"
#include "core/utils/debug/stacktrace_system.h"
#include "build_info/build_info.h"
#include "error_reporter/error_reporter.h"
#include "network/node_utils.h"
#include "node/system/node/thread_observability.h"
#include "network/traffic_statistics.h"
#include <boost/algorithm/string.hpp>
#include "node/system/etcd/etcd_service.h"
#include "node/system/node/node_connector.h"
#include "thread_context/node_context_manager.h"
#include <node_config_manager.h>
#include <atomic>
#include <future>
#include <chrono>
#include <cerrno>
#include <csignal>
#include <optional>

namespace
{
	std::atomic<Node *> gNodeAtomic{nullptr};
	constexpr std::chrono::seconds kGrpcDrainTimeout{2};
	constexpr std::chrono::seconds kShutdownDrainBudget{15};
	constexpr std::chrono::seconds kShutdownCompletionWaitTimeout{20};

	// Diagnostic stack-dump signal handler (todo.md #216).
	//
	// Operators send the diagnostic signal to a running node:
	//   Linux:    kill -USR1 <pid>
	//   Windows:  Ctrl+Break in the console (delivered as SIGBREAK)
	//
	// Async-signal-safety strategy (Review R1 fix, 2026-05-17):
	//   The signal handler ONLY sets a sig_atomic_t flag — no mutex,
	//   no allocation, no stdio, no localtime, no LOG_* (muduo's
	//   logger acquires locks). The EventLoop poll function
	//   `DrainPendingDiagnosticWork` runs every 250 ms and consumes
	//   pending flags, executing the actually-heavy work (boost::stacktrace,
	//   error_reporter dump, file I/O) from normal main-thread context
	//   where mutex / fstream / localtime are fine.
	//
	// Cost of the 250 ms poll latency: acceptable for a diagnostic
	// trigger. Ops sending the signal is already a manual action; a
	// quarter-second delay before the dump file appears is invisible.
	//
	// Registration is idempotent and best-effort: failures only log a
	// warning.
	std::atomic<bool> gDiagnosticSignalInstalled{false};

	// sig_atomic_t guarantees writes are atomic from a signal handler.
	// volatile prevents the compiler from optimizing the read in the
	// poll function away even though it doesn't see who writes it.
	volatile std::sig_atomic_t gDumpStackPending{0};
	volatile std::sig_atomic_t gDumpErrorReporterPending{0};
	volatile std::sig_atomic_t gLastDumpSignum{0};

	// Async-signal-safe: only assigns to sig_atomic_t. Everything else
	// (re-install via std::signal, the actual dump work) is moved to
	// the EventLoop drain function.
	void HandleDiagnosticSignal(int signum)
	{
		gLastDumpSignum = static_cast<sig_atomic_t>(signum);
		gDumpStackPending = 1;
		gDumpErrorReporterPending = 1;
		// Re-install the handler; std::signal is technically not in the
		// POSIX async-signal-safe list, but is widely implemented as
		// safe-enough across glibc/musl/Windows MSVCRT and the
		// alternative (sigaction) needs platform-specific wrapping that
		// muduo doesn't have today. Re-install lets subsequent triggers
		// keep working without ourselves transitioning to sigaction.
		std::signal(signum, &HandleDiagnosticSignal);
	}

	// SIGUSR2 dump-only handler (Linux). Same async-signal-safe shape
	// as HandleDiagnosticSignal: only sets a flag.
#ifndef _WIN32
	std::atomic<bool> gErrorReporterSignalInstalled{false};

	void HandleErrorReporterDumpSignal(int signum)
	{
		gDumpErrorReporterPending = 1;
		std::signal(signum, &HandleErrorReporterDumpSignal);
	}

	void InstallErrorReporterDumpHandlerOnce()
	{
		bool expected = false;
		if (!gErrorReporterSignalInstalled.compare_exchange_strong(expected, true))
		{
			return;
		}
		if (std::signal(SIGUSR2, &HandleErrorReporterDumpSignal) == SIG_ERR)
		{
			LOG_WARN << "Failed to install SIGUSR2 error_reporter dump handler; errno=" << errno;
			gErrorReporterSignalInstalled.store(false);
			return;
		}
		LOG_INFO << "Error-reporter dump handler installed: send SIGUSR2 to PID (todo #250 slice D)";
	}
#else
	inline void InstallErrorReporterDumpHandlerOnce() {}
#endif

	// Drain function run from the EventLoop on a 250 ms cadence (set up
	// in Node::Initialize). Consumes the sig_atomic_t flags set by the
	// signal handlers and performs the actually-heavy work from a normal
	// context where mutex / fstream / localtime / LOG_* are all safe.
	//
	// We do NOT bother making the flag-check atomic vs the work — the
	// worst case is "signal arrives twice in 250 ms and we coalesce into
	// one dump", which is benign for a diagnostic dump that already
	// overwrites the previous file on each run.
	void DrainPendingDiagnosticWork()
	{
		// Snapshot + clear so a new signal arriving during the dump
		// schedules another pass on the next tick.
		const bool dumpStack = (gDumpStackPending != 0);
		const bool dumpReporter = (gDumpErrorReporterPending != 0);
		if (!dumpStack && !dumpReporter)
		{
			return;
		}
		const int signum = static_cast<int>(gLastDumpSignum);
		gDumpStackPending = 0;
		gDumpErrorReporterPending = 0;

		if (dumpStack)
		{
			DumpProcessStackTraceOnSignal(signum);
		}

		if (dumpReporter)
		{
			const auto path = error_reporter::DumpSnapshotToDefaultPath();
			if (!path.empty()) {
				LOG_INFO << "Diagnostic dump: error_reporter snapshot -> " << path;
			} else {
				LOG_WARN << "Diagnostic dump: error_reporter snapshot failed";
			}
		}
	}

	void InstallDiagnosticSignalHandlerOnce()
	{
		bool expected = false;
		if (!gDiagnosticSignalInstalled.compare_exchange_strong(expected, true))
		{
			return;
		}

#ifdef _WIN32
		const int diagSignum = SIGBREAK;
		const char *signalName = "SIGBREAK";
#else
		const int diagSignum = SIGUSR1;
		const char *signalName = "SIGUSR1";
#endif
		if (std::signal(diagSignum, &HandleDiagnosticSignal) == SIG_ERR)
		{
			LOG_WARN << "Failed to install diagnostic stack-dump signal handler ("
					 << signalName << "); errno=" << errno;
			gDiagnosticSignalInstalled.store(false);
			return;
		}
		LOG_INFO << "Diagnostic stack-dump handler installed: send " << signalName
				 << " to PID to trigger (todo #216)";
	}

	// Fatal-signal crash dump (todo.md #105).
	//
	// When the process is terminated abnormally (SIGSEGV / SIGABRT / SIGFPE /
	// SIGILL) all in-game players lose their session without going through
	// HandleExitGameNode, so there is no "logout time" recorded anywhere for
	// incident forensics. This handler stamps a wall-clock millisecond
	// timestamp + the surviving stack trace + the process thread count to
	// the log stream right before the process dies, giving on-call a "the
	// node was alive at T=…" anchor without depending on Redis writes
	// completing during a crash (they won't).
	//
	// What we DON'T do here:
	//   - Try to dump live player_id list. The actor registry is in an
	//     unknown state (we got here by SIGSEGV); walking it could deadlock
	//     or re-fault. Per-player time is recovered from the regular save
	//     trail; this handler covers "the node itself".
	//   - Re-raise / chain to a previous handler. We let the OS finish the
	//     job after logging. The default disposition for SIGSEGV is core
	//     dump + terminate, which is what we want for post-mortem.
	//
	// Cost: same async-signal-unsafe caveat as #216 — boost::stacktrace
	// allocates. This is the last-gasp triage path; we accept the risk.
	std::atomic<bool> gFatalSignalInstalled{false};
	std::atomic<bool> gFatalSignalFiring{false};

	void HandleFatalSignal(int signum)
	{
		// Guard against re-entry — if the handler itself faults the OS will
		// re-deliver the signal and we'd otherwise spin. compare_exchange to
		// "already firing" turns the second hit into a fast bail-out.
		bool expected = false;
		if (!gFatalSignalFiring.compare_exchange_strong(expected, true))
		{
			std::signal(signum, SIG_DFL);
			std::raise(signum);
			return;
		}

		const auto nowMs = std::chrono::duration_cast<std::chrono::milliseconds>(
			std::chrono::system_clock::now().time_since_epoch()).count();
		const int threadCount = GetProcessThreadCountForDiagnostic();

		LOG_ERROR << "=== FATAL SIGNAL (todo #105) === signal=" << signum
				  << " process_terminating_at_ms=" << nowMs
				  << " process_thread_count=" << threadCount;
		LOG_ERROR << GetCurrentStackTraceAsString(kMaxEntries);
		LOG_ERROR << "=== End fatal signal dump; restoring SIG_DFL and re-raising ===";

		// Restore default disposition and re-raise so the OS produces the
		// usual core dump / terminate behavior. Don't try to clean up — that
		// is exactly what post-mortem analysis of the core file is for.
		std::signal(signum, SIG_DFL);
		std::raise(signum);
	}

	void InstallFatalSignalHandlerOnce()
	{
		bool expected = false;
		if (!gFatalSignalInstalled.compare_exchange_strong(expected, true))
		{
			return;
		}

		// SIGSEGV / SIGABRT / SIGFPE / SIGILL are POSIX-portable and also
		// honored by Windows MSVCRT runtime, so the same install loop works
		// on both target platforms.
		const int fatalSignals[] = { SIGSEGV, SIGABRT, SIGFPE, SIGILL };
		for (int sig : fatalSignals)
		{
			if (std::signal(sig, &HandleFatalSignal) == SIG_ERR)
			{
				LOG_WARN << "Failed to install fatal-signal crash-dump handler for signal=" << sig
						 << "; errno=" << errno;
			}
		}
		LOG_INFO << "Fatal-signal crash-dump handler installed for SIGSEGV/SIGABRT/SIGFPE/SIGILL (todo #105)";
	}

	const char *GetNonEmptyEnv(const char *name)
	{
		const char *value = std::getenv(name);
		if (value == nullptr || value[0] == '\0')
		{
			return nullptr;
		}
		return value;
	}

	std::string ResolveNodeIp()
	{
		// Prefer K8s Downward API pod IP, then explicit override, then legacy hostname resolve.
		if (const char *podIp = GetNonEmptyEnv("POD_IP"))
		{
			return podIp;
		}

		if (const char *nodeIp = GetNonEmptyEnv("NODE_IP"))
		{
			return nodeIp;
		}

		return localip();
	}

	std::optional<uint16_t> TryResolveNodePortFromEnv()
	{
		const char *rawPort = GetNonEmptyEnv("RPC_PORT");
		if (rawPort == nullptr)
		{
			rawPort = GetNonEmptyEnv("NODE_PORT");
		}

		if (rawPort == nullptr)
		{
			return std::nullopt;
		}

		errno = 0;
		char *endPtr = nullptr;
		const long parsedPort = std::strtol(rawPort, &endPtr, 10);
		if (errno != 0 || endPtr == rawPort || *endPtr != '\0' || parsedPort <= 0 || parsedPort > 65535)
		{
			LOG_WARN << "Ignore invalid env port value. RPC_PORT/NODE_PORT=" << rawPort;
			return std::nullopt;
		}

		return static_cast<uint16_t>(parsedPort);
	}

	std::string FormatKafkaPartitions(const std::vector<int32_t> &partitions)
	{
		if (partitions.empty())
		{
			return "all";
		}

		std::vector<std::string> partitionTokens;
		partitionTokens.reserve(partitions.size());
		for (int32_t partition : partitions)
		{
			partitionTokens.emplace_back(std::to_string(partition));
		}

		return boost::algorithm::join(partitionTokens, ",");
	}
}

std::unordered_map<std::string, std::unique_ptr<::google::protobuf::Service>> gNodeService;

Node *gNode;

Node::Node(muduo::net::EventLoop *loop, const std::string &logFilePath)
	: eventLoop(loop), logSystem(logFilePath, kMaxLogFileRollSize, 1)
{
	if (eventLoop == nullptr)
	{
		LOG_FATAL << "Node requires a valid EventLoop pointer.";
	}

	// Start async log system early so all constructor logs have correct formatting/colors.
	muduo::Logger::setOutput(AsyncOutput);
	logSystem.start();

	// Set timezone before any LOG_xxx call. Without a valid timezone, muduo appends 'Z'
	// to the timestamp (9 bytes instead of 8), shifting the log-level character past
	// kLoginInfoInex and causing LogToConsole to fall into the default (red) color.
	SetupTimeZone();

	LOG_INFO << "Node created, log file: " << logFilePath;
	if (gNode != nullptr && gNode != this)
	{
		LOG_FATAL << "Multiple Node instances detected. existing=" << gNode << ", new=" << this;
	}

	gNode = this;
	gNodeAtomic.store(this, std::memory_order_release);
	tlsEcs.nodeGlobalRegistry.emplace<ServiceNodeList>(tlsEcs.GrpcNodeEntity());
}

Node::Node(muduo::net::EventLoop *loop,
		   uint32_t nodeType,
		   CanConnectNodeTypeList connectTo,
		   ::google::protobuf::Service *replyService)
	: Node(loop, "logs/cpp_nodes/" + NodeUtils::NodeTypeToShortName(nodeType))
{
	replyService_ = replyService;
	GetNodeInfo().set_node_type(nodeType);
	targetNodeTypeWhitelist = std::move(connectTo);
	Initialize();
	node::observability::RegisterThreadObservability(*eventLoop, "logs/" + NodeUtils::NodeTypeToShortName(nodeType));
	RegisterTrafficStatsReporter(*eventLoop);
}

Node::~Node()
{
	Shutdown();

	// Node 通常在 loop.loop() 返回后离开作用域。若该返回不是由正常
	// FinalizeShutdownInLoop() 触发,Shutdown() 会在本线程启动异步 gRPC drain,
	// 但已经退出的 loop 不会再消费 worker 投递回来的完成回调。此时重新驱动
	// 同一个 loop,直到既有 finalizer 调 quit(),避免析构 joinable thread 或让
	// 捕获 this 的完成回调落到已销毁对象上。
	if (eventLoop != nullptr && eventLoop->isInLoopThread())
	{
		eventLoop->assertInLoopThread();
		bool fallbackLogged = false;
		for (;;)
		{
			std::unique_lock<std::mutex> lock(shutdownCompletionMutex_);
			if (shutdownComplete_)
			{
				break;
			}
			lock.unlock();
			if (!fallbackLogged)
			{
				LOG_WARN << "EventLoop exited before node shutdown completed; running fallback drain loop";
				fallbackLogged = true;
			}
			eventLoop->loop();
		}
	}

	// EventLoop 外销毁时,Shutdown() 的公开等待预算只用于向调用方报告卡顿;
	// 析构本身不能在仍有捕获 this 的回调/worker 时继续释放成员。
	if (eventLoop != nullptr)
	{
		std::unique_lock<std::mutex> lock(shutdownCompletionMutex_);
		shutdownCompletionCv_.wait(lock, [this]
								   { return shutdownComplete_; });
	}
	if (grpcShutdownThread_.joinable())
	{
		grpcShutdownThread_.join();
	}

	if (gNode == this)
	{
		gNodeAtomic.store(nullptr, std::memory_order_release);
		gNode = nullptr;
	}
}

int64_t Node::GetLeaseId() const
{
	return serviceDiscoveryManager.etcdService.GetLeaseId();
}

NodeInfo &Node::GetNodeInfo() const
{
	return tlsEcs.globalRegistry.get_or_emplace<NodeInfo>(tlsEcs.GlobalEntity());
}

void Node::Initialize()
{
	eventLoop->assertInLoopThread();
	LOG_DEBUG << "Node initializing...";
	build_info::LogStartupBanner();
	InstallDiagnosticSignalHandlerOnce();
	InstallErrorReporterDumpHandlerOnce();
	InstallFatalSignalHandlerOnce();
	// Drain the sig_atomic_t flags set by the diagnostic / error_reporter
	// signal handlers (Review R1 fix). 250 ms cadence is fast enough that
	// ops doesn't notice the latency between sending SIGUSR1/SIGUSR2 and
	// the dump file appearing, slow enough to be free CPU overhead.
	eventLoop->runEvery(0.25, &DrainPendingDiagnosticWork);
	RegisterHandlers();
	RegisterEventHandlers();
	LoadConfigs();
	InitLogSystem();
	InitRpcServer();
	LoadAllConfigData();
	InitKafka();
	InitEtcdService();

	LOG_INFO << "gRPC client config: ResourceQuota max threads=" << grpc_channel_cache::ConfiguredMaxThreads()
			 << ", backup poll interval ms=" << grpc_channel_cache::ConfiguredBackupPollIntervalMs()
			 << ", EventEngine pool reserve=" << (grpc_channel_cache::ConfiguredThreadPoolReserveThreads() > 0 ? std::to_string(grpc_channel_cache::ConfiguredThreadPoolReserveThreads()) : std::string("default"))
			 << ", EventEngine pool max=" << (grpc_channel_cache::ConfiguredThreadPoolMaxThreads() > 0 ? std::to_string(grpc_channel_cache::ConfiguredThreadPoolMaxThreads()) : std::string("unlimited"));

	LOG_DEBUG << "Node initialization complete.";
}

void Node::InitRpcServer()
{
	NodeInfo &localNodeInfo = GetNodeInfo();
	const std::string endpointIp = ResolveNodeIp();
	localNodeInfo.mutable_endpoint()->set_ip(endpointIp);

	// Port is resolved later by AcquireNodePort() via etcd, unless overridden by env.
	if (const auto envPort = TryResolveNodePortFromEnv(); envPort)
	{
		localNodeInfo.mutable_endpoint()->set_port(*envPort);
		LOG_INFO << "Node port from environment: " << *envPort;
	}

	LOG_INFO << "Node endpoint resolved. ip=" << endpointIp;
	localNodeInfo.set_node_type(GetNodeType());
	localNodeInfo.set_scene_node_type(tlsNodeConfigManager.GetGameConfig().scene_node_type());
	localNodeInfo.set_protocol_type(PROTOCOL_TCP);
	localNodeInfo.set_launch_time(TimeSystem::NowMicrosecondsUTC());
	localNodeInfo.set_zone_id(tlsNodeConfigManager.GetGameConfig().zone_id());

	localNodeInfo.set_node_uuid(boost::uuids::to_string(uuidGenerator()));

	const auto &redisHost = tlsNodeConfigManager.GetGameConfig().zone_redis().host();
	const auto redisPort = static_cast<uint16_t>(tlsNodeConfigManager.GetGameConfig().zone_redis().port());
	muduo::net::InetAddress zoneRedisAddress(redisPort);

	// If the host looks like an IP address, use it directly without DNS resolve
	bool isIpLiteral = !redisHost.empty() && (std::isdigit(static_cast<unsigned char>(redisHost[0])) || redisHost.find(':') != std::string::npos);
	if (isIpLiteral)
	{
		zoneRedisAddress = muduo::net::InetAddress(redisHost, redisPort);
	}
	else if (!muduo::net::InetAddress::resolve(redisHost, &zoneRedisAddress))
	{
		LOG_WARN << "DNS resolve failed for Redis host '" << redisHost << "', treating as literal IP";
		zoneRedisAddress = muduo::net::InetAddress(redisHost, redisPort);
	}
	LOG_INFO << "Zone Redis address: " << zoneRedisAddress.toIpPort();
	tlsRedis.Connect(eventLoop, zoneRedisAddress);

	LOG_DEBUG << "Node info: " << localNodeInfo.DebugString();
}

void Node::InitKafka()
{
	kafkaManager.Init(tlsNodeConfigManager.GetBaseDeployConfig().kafka());
}

void Node::StartKafkaPolling()
{
	// Drive Kafka consumption from a dedicated thread, dispatching decoded
	// messages back into the muduo EventLoop. The legacy 100ms timer model
	// shared the loop with RPC / client TCP / scene-response forwarding;
	// during the 45k stress run (stress-1zone-45k-2026-05-30) the loop
	// stalled long enough for librdkafka to leave the group, leaving
	// gate-group-1 with no active members and Kafka command lag at 17k.
	//
	// queueInLoop preserves the single-threaded callback invariant the rest
	// of the codebase relies on — the consumer thread never touches ECS or
	// node state directly, only enqueues the existing callback.
	kafkaManager.StartBackgroundPolling(eventLoop);
}

bool Node::RegisterKafkaMessageHandler(const std::vector<std::string> &topics,
									   const std::string &groupId,
									   KafkaMessageHandler handler,
									   const std::vector<int32_t> &partitions)
{
	if (topics.empty())
	{
		LOG_ERROR << "RegisterKafkaMessageHandler failed: topics is empty.";
		return false;
	}

	if (!handler)
	{
		LOG_ERROR << "RegisterKafkaMessageHandler failed: handler is null.";
		return false;
	}

	auto &kafkaConfig = tlsNodeConfigManager.GetBaseDeployConfig().kafka();
	if (!GetKafkaManager().Subscribe(kafkaConfig, topics, groupId, partitions, std::move(handler)))
	{
		LOG_ERROR << "Kafka subscribe failed. group_id=" << groupId;
		return false;
	}

	LOG_INFO << "Kafka subscribe succeeded. group_id=" << groupId
			 << ", topics=" << boost::algorithm::join(topics, ",")
			 << ", partitions=" << FormatKafkaPartitions(partitions);

	StartKafkaPolling();
	kafkaPollingStarted = true;

	return true;
}

void Node::InitEtcdService()
{
	serviceDiscoveryManager.Init();
}

void Node::RegisterGrpcService(grpc::Service *service)
{
	if (service == nullptr)
	{
		LOG_WARN << "Attempted to register null gRPC service, skipping.";
		return;
	}
	grpcServices_.push_back(service);
}

void Node::StartGrpcServer()
{
	if (grpcServices_.empty())
	{
		// No gRPC server to start. The discovery publish was deferred from
		// OnTxnSucceeded(allocKey), so we must publish now or this node will
		// never become discoverable to peers.
		serviceDiscoveryManager.etcdService.PublishDiscoveryAfterGrpcReady();
		return;
	}

	const auto &grpcEp = GetNodeInfo().grpc_endpoint();
	if (grpcEp.port() == 0)
	{
		LOG_ERROR << "gRPC server port not allocated, skipping gRPC server start.";
		// Same as above: publish now so the node still becomes discoverable
		// even when gRPC startup is skipped.
		serviceDiscoveryManager.etcdService.PublishDiscoveryAfterGrpcReady();
		return;
	}

	const std::string serverAddress = grpcEp.ip() + ":" + std::to_string(grpcEp.port());

	grpc::ServerBuilder builder;
	builder.AddListeningPort(serverAddress, grpc::InsecureServerCredentials());

	// gRPC sync server thread pool size.
	// Round 14: bumped default 2 -> 8. Under 45k-bot stress with high reconnect
	// churn, scene_manager.EnterScene -> scene-node ReleasePlayer was hitting a
	// 500ms cliff because only 2 pollers were available to drain incoming RPCs
	// while gRPC handlers waited on the muduo loop dispatch round-trip
	// (runInLoop + promise/future). 8 pollers give enough slack for burst
	// reconnect handling without measurable thread overhead on idle nodes.
	// Override via GRPC_SERVER_MAX_POLLERS env if needed.
	int maxPollers = 8;
	if (const char *env = GetNonEmptyEnv("GRPC_SERVER_MAX_POLLERS"))
	{
		const int parsed = std::atoi(env);
		if (parsed >= 1)
		{
			maxPollers = parsed;
		}
	}
	builder.SetSyncServerOption(grpc::ServerBuilder::NUM_CQS, 1);
	builder.SetSyncServerOption(grpc::ServerBuilder::MIN_POLLERS, 1);
	builder.SetSyncServerOption(grpc::ServerBuilder::MAX_POLLERS, maxPollers);
	LOG_INFO << "gRPC server config: max_pollers=" << maxPollers;

	for (auto *svc : grpcServices_)
	{
		builder.RegisterService(svc);
	}

	grpcServer_ = builder.BuildAndStart();
	if (!grpcServer_)
	{
		LOG_FATAL << "Failed to start gRPC server on " << serverAddress;
		return;
	}

	// 这里刻意**不**起线程调 grpcServer_->Wait()。
	// BuildAndStart() 返回时 sync server 的 ThreadManager 已经在自己的线程上收请求了,
	// Wait() 本身不干活 —— 它的实现只是 `while (started_ && !shutdown_notified_)
	// shutdown_cv_.Wait(&mu_);`(src/cpp/server/server_cc.cc),真正的排空全部发生在
	// Shutdown() 内部(逐个 ThreadManager Shutdown + Wait)。
	// 所以那条专职线程整个生命周期只是躺在条件变量上,纯粹是白占一个线程。
	LOG_INFO << "gRPC server started on " << serverAddress;

	// gRPC port is now bound and accepting connections (BuildAndStart returns
	// after listener bind per grpc docs). Safe to advertise this node in etcd
	// so peers can discover and dial us. Without this deferral, scene_manager
	// would see the node via etcd watch and dial gRPC before the port is open,
	// triggering "connection refused" + dead-node markings during cold start.
	serviceDiscoveryManager.etcdService.PublishDiscoveryAfterGrpcReady();
}

void Node::ShutdownGrpcServer()
{
	eventLoop->assertInLoopThread();
	if (!grpcServer_)
	{
		shutdownGrpcDrainComplete_ = true;
		MaybeFinalizeShutdownInLoop();
		return;
	}

	LOG_INFO << "Shutting down gRPC server...";

	// Server::Shutdown() 最后会同步 ThreadManager::Wait()。不能在 muduo loop
	// 线程上直接调用:Scene gRPC handler 正阻塞在 future.get(),等的就是该 loop
	// 执行 runInLoop 任务。两边互等会永久死锁。
	//
	// 仅给 Shutdown() 加 deadline 仍不够。gRPC 在 grace deadline 到期后会
	// cancel_all_calls(),但随后照样 ThreadManager::Wait();取消 call 不会把正在
	// 执行且卡在 std::future 上的用户 handler 强行弹栈。
	//
	// 因此只在关机窗口临时起一条协调线程跑同步 Shutdown(),muduo loop 继续消费
	// 已排队的 handler。Shutdown 返回后再 queueInLoop 回来完成其余 teardown。
	grpc::Server *const server = grpcServer_.get();

	grpcShutdownThread_ = std::thread([this, server]
									 {
		const auto drainStart = std::chrono::steady_clock::now();
		server->Shutdown(std::chrono::system_clock::now() + kGrpcDrainTimeout);
		const auto drainElapsed = std::chrono::steady_clock::now() - drainStart;

		eventLoop->queueInLoop([this, drainElapsed]
							   {
			eventLoop->assertInLoopThread();

			// gRPC 不返回"是否打到 grace deadline"的布尔值。耗时达到预算时只能
			// 诚实记录为"预算已耗尽或 handler 自身退场更慢",不能断言取消一定发生。
			if (drainElapsed >= kGrpcDrainTimeout)
			{
				LOG_WARN << "gRPC drain consumed its grace budget; calls may have been cancelled. timeout_ms="
						 << std::chrono::duration_cast<std::chrono::milliseconds>(kGrpcDrainTimeout).count();
			}

			// queueInLoop 已经由协调线程发出;join 只等它从 queueInLoop 返回并退栈,
			// 不再等待任何 gRPC handler,所以不会重新制造 loop/poller 互等。
			if (grpcShutdownThread_.joinable())
			{
				grpcShutdownThread_.join();
			}
			grpcServer_.reset();
			LOG_INFO << "gRPC server shut down. drain_ms="
					 << std::chrono::duration_cast<std::chrono::milliseconds>(drainElapsed).count();

			shutdownGrpcDrainComplete_ = true;
			MaybeFinalizeShutdownInLoop(); });
	});
}

void Node::OnNodeIdConflictShutdown(NodeIdConflictReason reason)
{
	// 四个触发点(keepalive TTL=0 / 本地租约 deadline / 重注册 CAS 失败 / Watch 发现
	// 身份被抢)都可能重复触发,而且 keepalive 定时器还在跑,必须幂等。
	if (conflictShutdownStarted_)
	{
		return;
	}
	conflictShutdownStarted_ = true;

	LOG_ERROR << "Node identity conflict detected (reason=" << static_cast<int>(reason)
			  << "), node_id=" << GetNodeId()
			  << ". Fencing ID generation and draining before exit.";

	// 1) 立刻停止发号。etcd 已经可以把这个 node_id 交给别的进程,再发一个号就是
	//    确定性撞号。fence 必须在业务收尾之前 —— 收尾只需要存盘和路由,不需要新 ID。
	tlsSnowflakeManager.Fence();

	// 2) 停掉所有会再次触发注册 / 冲突判定的重试,避免收尾期间又跑一遍分配流程 ——
	//    这台节点已经不是 node_id 的合法持有者,重抢端口 / 重占 node_id 只会干扰接手的进程。
	//    注意重试定时器住在 EtcdService 里,不是 Node 里。
	serviceHealthMonitorTimer.Cancel();
	serviceDiscoveryManager.etcdService.StopRegistrationRetries();

	// 3) 业务收尾:scene 节点在这里存盘并把玩家改派到别的 scene node。
	if (onConflictShutdownFn_)
	{
		LOG_INFO << "Running conflict-shutdown hook...";
		onConflictShutdownFn_(*this, reason);
	}

	// 4) 有界等待收尾落地后再退出。
	//    旧实现是 hook 返回后立刻 LOG_FATAL(muduo 的 FATAL 会 abort),而存盘是异步的
	//    (Redis 命令还在发送缓冲、DBTask 还在 Kafka producer 队列),abort 等于把这一
	//    批玩家数据直接丢掉。
	StartConflictDrainWatchdog();
}

void Node::StartConflictDrainWatchdog()
{
	// 收尾预算。推导:收尾要等的是"每个在线玩家一次 Redis 存盘往返 + 一次
	// SceneManager EnterScene 改派";正常情况下几百玩家在百毫秒级完成,15s 是给
	// Redis / SceneManager 抖动留的保守上限。到期即退出,不无限等下去 ——
	// 这是有界兜底,不是"等一会儿就当成功"(到期会明确打 ERROR 并报告未落地的数量)。
	// TODO: 待压测实测复核该预算。
	constexpr double kDrainPollIntervalSec = 0.1;
	constexpr auto kDrainBudget = std::chrono::seconds(15);

	conflictDrainDeadline_ = std::chrono::steady_clock::now() + kDrainBudget;

	conflictDrainTimer.RunEvery(kDrainPollIntervalSec, [this]
								{
		const bool drained = !conflictDrainCompleteFn_ || conflictDrainCompleteFn_(*this);
		const bool expired = std::chrono::steady_clock::now() >= conflictDrainDeadline_;
		if (!drained && !expired)
		{
			return;
		}

		conflictDrainTimer.Cancel();
		if (drained)
		{
			LOG_INFO << "Conflict drain complete, shutting down. node_id=" << GetNodeId();
		}
		else
		{
			LOG_ERROR << "Conflict drain budget exceeded; shutting down with work still in flight. node_id="
					  << GetNodeId();
		}

		// 走正常关闭路径:它会跑 before-shutdown hook、关 gRPC、flush Kafka producer、
		// 释放 etcd 租约,最后 quit 事件循环 —— 这些正是 abort 会跳过的东西。
		Shutdown(); });
}

void Node::StartRpcServer()
{
	eventLoop->assertInLoopThread();
	if (rpcServer)
	{
		LOG_TRACE << "RPC server already started, skipping.";
		return;
	}

	NodeInfo &localNodeInfo = GetNodeInfo();
	muduo::net::InetAddress rpcListenAddress(localNodeInfo.endpoint().ip(), localNodeInfo.endpoint().port());

	rpcServer = std::make_unique<RpcServerPtr::element_type>(eventLoop, rpcListenAddress);
	rpcServer->start();
	auto *nodeReplyService = GetNodeReplyService();
	if (nodeReplyService != nullptr)
	{
		rpcServer->registerService(nodeReplyService);
	}
	else
	{
		LOG_WARN << "Node reply service is null, skip registerService for node_type=" << GetNodeInfo().node_type();
	}

	for (auto &[serviceName, service] : gNodeService)
	{
		if (service == nullptr)
		{
			LOG_WARN << "Skip null node service registration for key=" << serviceName;
			continue;
		}

		rpcServer->registerService(service.get());
	}

	NodeConnector::ConnectAllNodes();

	if (!RegisterKafkaHandlers())
	{
		LOG_FATAL << "RegisterKafkaHandlers failed for node_type=" << GetNodeInfo().node_type();
	}

	StartNodeRegistrationHealthMonitor();

	StartGrpcServer();

	tlsEcs.dispatcher.trigger<OnServerStart>();

	auto nodeTypeName = boost::to_upper_copy(eNodeType_Name(GetNodeInfo().node_type()));
	const auto &ep = GetNodeInfo().endpoint();
	const auto &deployConfig = tlsNodeConfigManager.GetBaseDeployConfig();
	const auto &gameConfig = tlsNodeConfigManager.GetGameConfig();

	// Build connects-to list
	std::string connectsTo;
	for (auto nodeType : targetNodeTypeWhitelist)
	{
		if (!connectsTo.empty())
			connectsTo += ", ";
		connectsTo += eNodeType_Name(nodeType);
	}
	if (connectsTo.empty())
		connectsTo = "(none)";

	// Build Kafka brokers string
	std::string kafkaBrokers = boost::algorithm::join(
		std::vector<std::string>(deployConfig.kafka().brokers().begin(), deployConfig.kafka().brokers().end()), ", ");
	if (kafkaBrokers.empty())
		kafkaBrokers = "(not configured)";

	// Build etcd hosts string
	std::string etcdHosts = boost::algorithm::join(
		std::vector<std::string>(deployConfig.etcd_hosts().begin(), deployConfig.etcd_hosts().end()), ", ");
	if (etcdHosts.empty())
		etcdHosts = "(not configured)";

	// Build gRPC listen string
	std::string grpcListen = "(disabled)";
	if (GetNodeInfo().grpc_endpoint().port() > 0)
	{
		grpcListen = GetNodeInfo().grpc_endpoint().ip() + ":" + std::to_string(GetNodeInfo().grpc_endpoint().port());
	}

	// Print startup banner to both log file and stdout/console so it is always visible.
	const std::string banner =
		"\n\n"
		"=============================================================\n"
		"  " +
		nodeTypeName + " NODE STARTED SUCCESSFULLY\n"
					   "=============================================================\n"
					   "  Listen:      " +
		ep.ip() + ":" + std::to_string(ep.port()) + "\n"
													"  gRPC:        " +
		grpcListen + "\n"
					 "  node_id:     " +
		std::to_string(GetNodeId()) + "\n"
									  "  node_uuid:   " +
		GetNodeInfo().node_uuid() + "\n"
									"  zone_id:     " +
		std::to_string(gameConfig.zone_id()) + "\n"
											   "  etcd:        " +
		etcdHosts + "\n"
					"  kafka:       " +
		kafkaBrokers + "\n"
					   "  redis:       " +
		gameConfig.zone_redis().host() + ":" + std::to_string(gameConfig.zone_redis().port()) + "\n"
																								"  connects_to: " +
		connectsTo + "\n"
					 "  log_level:   " +
		std::to_string(deployConfig.log_level()) + "\n"
												   "=============================================================\n";
	LOG_INFO << banner;

	if (afterStartFn_)
		afterStartFn_(*this);
}

void Node::RequestShutdown()
{
	// 已开始(包括已完成)时不要再向可能已经退出的 EventLoop 投递捕获 this 的
	// 重复任务。EventLoop 外的 Shutdown() 仍会在本调用返回后等待完成条件。
	if (shutdownStarted.load(std::memory_order_acquire))
	{
		return;
	}

	if (eventLoop == nullptr)
	{
		return;
	}

	if (eventLoop->isInLoopThread())
	{
		ShutdownInLoop();
		return;
	}

	eventLoop->queueInLoop([this]
						   { ShutdownInLoop(); });
}

void Node::Shutdown()
{
	if (eventLoop == nullptr)
	{
		return;
	}

	RequestShutdown();
	if (eventLoop->isInLoopThread())
	{
		return;
	}

	std::unique_lock<std::mutex> lock(shutdownCompletionMutex_);
	if (!shutdownCompletionCv_.wait_for(lock, kShutdownCompletionWaitTimeout,
										[this]
										{ return shutdownComplete_; }))
	{
		LOG_ERROR << "Node shutdown timed out waiting for loop thread. timeout_s="
				  << std::chrono::duration_cast<std::chrono::seconds>(kShutdownCompletionWaitTimeout).count();
	}
}

void Node::ShutdownInLoop()
{
	eventLoop->assertInLoopThread();
	if (shutdownStarted.exchange(true, std::memory_order_acq_rel))
	{
		return;
	}

	LOG_DEBUG << "Node shutting down...";

	// 先封住 Kafka 入站并 join 后台 poller,避免 drain 期间继续向 loop 投递
	// command / migration。producer 保持可用,供玩家存盘继续发送 DBTask。
	kafkaManager.StopConsumers();

	if (beforeShutdownFn_)
	{
		LOG_INFO << "Running before-shutdown hook...";
		beforeShutdownFn_(*this);
	}

	// 业务 barrier 与 gRPC drain 并行推进。先执行 hook 生成待落地工作,随后立刻
	// 关闭 gRPC 入口;EventLoop 保持运行,让在途 handler 与 Redis 回调完成。
	StartShutdownDrainWatchdog();
	ShutdownGrpcServer();
	MaybeFinalizeShutdownInLoop();
}

void Node::StartShutdownDrainWatchdog()
{
	eventLoop->assertInLoopThread();

	if (!shutdownDrainCompleteFn_)
	{
		return;
	}

	constexpr double kDrainPollIntervalSec = 0.1;
	shutdownDrainDeadline_ = std::chrono::steady_clock::now() + kShutdownDrainBudget;
	LOG_INFO << "Waiting for before-shutdown work to drain. timeout_s="
			 << std::chrono::duration_cast<std::chrono::seconds>(kShutdownDrainBudget).count();

	// 即使 predicate 暂时为 true 也不能锁存:在 gRPC 完全退场前,某个在途
	// handler 仍可能向业务侧追加工作。每 100ms 都现场重查,grpc 回调也会重查。
	shutdownDrainTimer.RunEvery(kDrainPollIntervalSec, [this]
								{ MaybeFinalizeShutdownInLoop(); });
}

void Node::MaybeFinalizeShutdownInLoop()
{
	eventLoop->assertInLoopThread();
	if (shutdownFinalizationStarted_)
	{
		return;
	}

	const bool businessDrained = !shutdownDrainCompleteFn_ || shutdownDrainCompleteFn_(*this);
	const bool businessExpired = shutdownDrainCompleteFn_ &&
		std::chrono::steady_clock::now() >= shutdownDrainDeadline_;
	if (!shutdownGrpcDrainComplete_ || (!businessDrained && !businessExpired))
	{
		return;
	}

	shutdownFinalizationStarted_ = true;
	shutdownDrainTimer.Cancel();
	if (businessDrained)
	{
		LOG_INFO << "Before-shutdown drain complete.";
	}
	else
	{
		LOG_ERROR << "Before-shutdown drain budget exceeded; continuing with work still in flight. timeout_s="
				  << std::chrono::duration_cast<std::chrono::seconds>(kShutdownDrainBudget).count();
	}
	FinalizeShutdownInLoop();
}

void Node::FinalizeShutdownInLoop()
{
	eventLoop->assertInLoopThread();

	grpcHandlerTimer.Cancel();
	serviceHealthMonitorTimer.Cancel();
	conflictDrainTimer.Cancel();
	shutdownDrainTimer.Cancel();
	// node_id / 端口的重试定时器由 serviceDiscoveryManager.Shutdown() -> EtcdService::Shutdown()
	// 取消,不在这里。
	ReleaseNodeId();
	serviceDiscoveryManager.Shutdown();
	kafkaManager.Shutdown();
	// Hiredis 持有 muduo Channel 和待回调命令;必须在 EventLoop 与 Scene
	// MessageAsyncClient 回调目标都存活时释放。
	tlsRedis.Shutdown();
	// Cleared by tlsEcs.Clear() below.
	tlsEcs.Clear();
#ifdef WIN32
	muduo::Logger::setOutput(LogToConsole);
#endif
	logSystem.stop();
	LOG_DEBUG << "Node shutdown complete.";

	{
		std::lock_guard<std::mutex> lock(shutdownCompletionMutex_);
		shutdownComplete_ = true;
	}
	shutdownCompletionCv_.notify_all();
	eventLoop->quit();
}

void Node::InitLogSystem()
{
	// Log system already started in constructor; just apply config-driven log level.
	auto logLevel = static_cast<muduo::Logger::LogLevel>(
		tlsNodeConfigManager.GetBaseDeployConfig().log_level());
	muduo::Logger::setLogLevel(logLevel);
}

void Node::RegisterEventHandlers()
{
	tlsEcs.dispatcher.sink<OnConnected2TcpServerEvent>().connect<&Node::OnServerConnected>(*this);
}

void Node::LoadConfigs()
{
	tlsNodeConfigManager = NodeConfigManager();
	readBaseDeployConfig("etc/base_deploy_config.yaml", tlsNodeConfigManager.GetBaseDeployConfig());

	// GAME_CONFIG_PATH lets a shared container image load a role-specific
	// game_config (for example game_config_instance.yaml) without mounting
	// a different ConfigMap. Falls back to etc/game_config.yaml.
	std::string gameConfigPath = "etc/game_config.yaml";
	if (const char *envPath = std::getenv("GAME_CONFIG_PATH");
	    envPath != nullptr && envPath[0] != '\0')
	{
		gameConfigPath = envPath;
		LOG_INFO << "GAME_CONFIG_PATH env override applied: " << gameConfigPath;
	}
	readGameConfig(gameConfigPath, tlsNodeConfigManager.GetGameConfig());
	gNodeConfigManager = tlsNodeConfigManager;
}

void Node::LoadAllConfigData()
{
	LoadTablesAsync();
}

void Node::SetupTimeZone()
{
#ifdef __linux__
	const muduo::TimeZone hkTz = muduo::TimeZone::loadZoneFile("zoneinfo/Asia/Hong_Kong");
#else
	const muduo::TimeZone hkTz(8 * 3600, "zoneinfo/Asia/Hong_Kong");
#endif // __linux__
	muduo::Logger::setTimeZone(hkTz);
}

void Node::StopWatchingServiceNodes()
{
	EtcdHelper::StopAllWatching();
}

void Node::ReleaseNodeId()
{
	EtcdHelper::RevokeLeaseAndCleanup(serviceDiscoveryManager.etcdService.GetLeaseId());
}

// These functions are provided by each node binary; the linker resolves them.
void InitReply();
void InitPlayerService();
void InitPlayerServiceReplied();
void InitServiceHandler();
void Node::RegisterHandlers()
{
	InitMessageInfo();
	InitReply();
	InitPlayerService();
	InitPlayerServiceReplied();
	InitServiceHandler();
}

void Node::AsyncOutput(const char *msg, int len)
{
	Node *activeNode = gNodeAtomic.load(std::memory_order_acquire);
	if (activeNode != nullptr)
	{
		activeNode->Log().append(msg, len);
	}
#ifdef WIN32
	LogToConsole(msg, len);
#endif
}

bool Node::IsCurrentNode(const NodeInfo &candidateNode) const
{
	return NodeUtils::IsSameNode(candidateNode.node_uuid(), GetNodeInfo().node_uuid());
}

void Node::HandleServiceNodeStop(const std::string &key, const std::string &nodeJson)
{
	eventLoop->assertInLoopThread();
	LOG_INFO << "Service node stop, key: " << key << ", value: " << nodeJson;

	// Mirror HandleServiceNodeStart: allocation-slot keys store a raw uuid,
	// not a NodeInfo JSON. Etcd lease revoke fires DELETE for both the
	// allocation key AND the discovery key in the same shutdown, so without
	// this guard every clean stop produces a spurious "Parse node JSON
	// failed" error line per node type. The real removal is driven by the
	// matching discovery-key DELETE that comes through alongside it.
	if (key.find("/allocated/") != std::string::npos)
	{
		LOG_TRACE << "Skip allocation-slot delete for service discovery: " << key;
		return;
	}

	NodeInfo stoppedNode;
	auto parseResult = google::protobuf::util::JsonStringToMessage(nodeJson, &stoppedNode);
	if (!parseResult.ok())
	{
		LOG_ERROR << "Parse node JSON failed, key: " << key
				  << ", JSON: " << nodeJson
				  << ", Error: " << parseResult.message().data();
		return;
	}

	if (!eNodeType_IsValid(stoppedNode.node_type()))
	{
		LOG_TRACE << "Unknown service type for key: " << key;
		return;
	}

	if (stoppedNode.node_uuid().empty())
	{
		LOG_WARN << "Ignore service node stop with empty node_uuid. key=" << key;
		return;
	}

	// Mirror the AddServiceNode zone filter: zone-scoped services never make
	// it into this node's registry when they come from a foreign zone, so a
	// stop event for them must not trigger ExecuteNodeRemoval — that path
	// keys removal by (node_type, node_id) for TCP nodes and would otherwise
	// destroy the local entity at the same node_id slot even though it
	// belongs to a different zone.
	if (NodeUtils::IsZoneScopedNodeType(stoppedNode.node_type()) &&
		stoppedNode.zone_id() != GetNodeInfo().zone_id())
	{
		LOG_INFO << "Skip zone-scoped stop event for foreign zone. key=" << key
				 << ", node_zone=" << stoppedNode.zone_id()
				 << ", self_zone=" << GetNodeInfo().zone_id();
		return;
	}

	const auto graceSeconds = tlsNodeConfigManager.GetBaseDeployConfig().node_removal_grace_seconds();

	// --- Grace period path: defer removal so breakpoint-paused nodes can re-register ---
	if (graceSeconds > 0)
	{
		const auto &nodeUuid = stoppedNode.node_uuid();

		// Already pending? Reset timer.
		if (pendingNodeRemovals_.count(nodeUuid))
		{
			LOG_INFO << "Node removal already pending, resetting grace timer. uuid=" << nodeUuid;
			pendingNodeRemovals_.erase(nodeUuid);
		}

		auto pending = std::make_unique<PendingNodeRemoval>();
		pending->nodeInfo.CopyFrom(stoppedNode);

		// Capture uuid by value for the timer callback.
		std::string capturedUuid = nodeUuid;
		pending->timer.RunAfter(static_cast<double>(graceSeconds), [this, capturedUuid]()
								{
			auto it = pendingNodeRemovals_.find(capturedUuid);
			if (it == pendingNodeRemovals_.end())
			{
				return; // Already cancelled by a re-register PUT event.
			}

			LOG_WARN << "Grace period expired, removing node. uuid=" << capturedUuid;
			NodeInfo expiredNode;
			expiredNode.CopyFrom(it->second->nodeInfo);
			pendingNodeRemovals_.erase(it);

			// Execute the actual removal (same logic as the immediate path below).
			ExecuteNodeRemoval(expiredNode); });

		pendingNodeRemovals_[nodeUuid] = std::move(pending);
		LOG_INFO << "Node removal deferred for " << graceSeconds << "s grace period. uuid=" << nodeUuid;
		return;
	}

	// --- Immediate removal path (production: grace_seconds == 0) ---
	ExecuteNodeRemoval(stoppedNode);
}

void Node::CancelPendingNodeRemoval(const std::string &nodeUuid)
{
	auto it = pendingNodeRemovals_.find(nodeUuid);
	if (it == pendingNodeRemovals_.end())
	{
		return;
	}

	LOG_INFO << "Node re-registered during grace period, cancelling pending removal. uuid=" << nodeUuid;
	it->second->timer.Cancel();
	pendingNodeRemovals_.erase(it);
}

void Node::ExecuteNodeRemoval(const NodeInfo &stoppedNode)
{
	auto &serviceNodesByType = tlsEcs.nodeGlobalRegistry.get_or_emplace<ServiceNodeList>(tlsEcs.GrpcNodeEntity());
	auto &nodesOfStoppedType = *serviceNodesByType[stoppedNode.node_type()].mutable_node_list();

	// Remove stale node snapshot first so service discovery state stays consistent.
	int removedSnapshotCount = 0;
	for (int i = nodesOfStoppedType.size() - 1; i >= 0; --i)
	{
		const auto &cachedNodeSnapshot = nodesOfStoppedType.Get(i);
		if (!NodeUtils::IsSameNode(cachedNodeSnapshot.node_uuid(), stoppedNode.node_uuid()))
		{
			continue;
		}

		nodesOfStoppedType.DeleteSubrange(i, 1);
		++removedSnapshotCount;
	}

	if (removedSnapshotCount == 0)
	{
		LOG_WARN << "Service node stop did not match local cache. node_id=" << stoppedNode.node_id()
				 << ", node_uuid=" << stoppedNode.node_uuid()
				 << ", node_type=" << stoppedNode.node_type();
	}
	else
	{
		LOG_INFO << "Removed " << removedSnapshotCount << " stale node record(s) for uuid=" << stoppedNode.node_uuid();
	}

	const auto stoppedNodeType = stoppedNode.node_type();
	const auto stoppedNodeId = stoppedNode.node_id();
	const auto stoppedNodeUuid = stoppedNode.node_uuid();
	const auto stoppedProtocolType = stoppedNode.protocol_type();

	// Important: destroy network entities after current channel dispatch cycle.
	// Direct destroy inside etcd watch callback can invalidate activeChannels_.
	GetLoop()->queueInLoop([stoppedNodeType, stoppedNodeId, stoppedNodeUuid, stoppedProtocolType]()
						   {
		entt::registry& nodeRegistry = tlsNodeContextManager.GetRegistry(stoppedNodeType);

		// Both TCP and gRPC nodes are now keyed by uuid: TCP entities used to
		// be allocated as `entt::entity{node_id}` (see ConnectToTcpNode pre-
		// uuid-refactor) but that scheme aliased nodes from different zones
		// onto the same slot and required a parallel removal path. With both
		// node_connector paths using auto-assigned entity ids and uuid for
		// dedup, removal is uniformly a uuid scan.
		(void)stoppedProtocolType;
		(void)stoppedNodeId;
		for (const auto& [entity, nodeInfo] : nodeRegistry.view<NodeInfo>().each()) {
			if (!NodeUtils::IsSameNode(nodeInfo.node_uuid(), stoppedNodeUuid)) {
				continue;
			}

			OnNodeRemoveEvent nodeRemovedEvent;
			nodeRemovedEvent.set_entity(entt::to_integral(entity));
			nodeRemovedEvent.set_node_type(stoppedNodeType);
			tlsEcs.dispatcher.trigger(nodeRemovedEvent);

			DestroyEntity(nodeRegistry, entity);
			return;
		}
		LOG_WARN << "Service node stop: entity not found for uuid=" << stoppedNodeUuid
			<< ", node_type=" << stoppedNodeType
			<< ", node_id=" << stoppedNodeId; });
	LOG_INFO << "Service node stopped : " << stoppedNode.DebugString();
}

void Node::OnServerConnected(const OnConnected2TcpServerEvent &connectedEvent)
{
	eventLoop->assertInLoopThread();
	if (rpcServer == nullptr)
	{
		return;
	}

	auto &connection = connectedEvent.conn_;
	if (!connection->connected())
	{
		LOG_INFO << "Client disconnected: " << connection->peerAddress().toIpPort();
		return;
	}
	LOG_INFO << "Connected to server: " << connection->peerAddress().toIpPort();
	for (uint32_t nodeType = 0; nodeType < eNodeType_ARRAYSIZE; ++nodeType)
	{
		nodeRegistrationManager.TryRegisterNodeSession(nodeType, connection);
	}
}

void Node::StartNodeRegistrationHealthMonitor()
{
	serviceHealthMonitorTimer.RunEvery(tlsNodeConfigManager.GetBaseDeployConfig().health_check_interval(),
									   [this, reRegistrationRequested = false]() mutable
									   {
										   if (rpcServer == nullptr)
										   {
											   return;
										   }

										   // Check lease deadline first — this catches network partitions where
										   // the watch stream is dead and the local ServiceNodeList is stale.
										   if (serviceDiscoveryManager.etcdService.IsLeasePresumablyExpired())
										   {
											   LOG_ERROR << "Lease deadline exceeded: no keepalive ACK from etcd within TTL. "
															"node_id="
														 << GetNodeInfo().node_id()
														 << ". Etcd has likely expired our lease; another node may claim this ID. "
															"Fencing ID generation, persisting and relocating players, then terminating.";
											   OnNodeIdConflictShutdown(NodeIdConflictReason::kLeaseDeadlineExceeded);
											   return;
										   }

										   auto &currentNode = GetNodeInfo();

										   auto &serviceNodesByType = tlsEcs.nodeGlobalRegistry.get_or_emplace<ServiceNodeList>(tlsEcs.GrpcNodeEntity());
										   auto &registeredNodesForType = *serviceNodesByType[currentNode.node_type()].mutable_node_list();
										   for (const auto &registeredNode : registeredNodesForType)
										   {
											   if (IsCurrentNode(registeredNode))
											   {
												   reRegistrationRequested = false;
												   return;
											   }
										   }

										   // Node snapshot disappeared from service discovery.
										   // Re-register with the same node_id; do not run full re-allocation flow.
										   if (reRegistrationRequested)
										   {
											   return;
										   }

										   serviceDiscoveryManager.etcdService.RequestReRegistration();
										   reRegistrationRequested = true;
									   });
}
