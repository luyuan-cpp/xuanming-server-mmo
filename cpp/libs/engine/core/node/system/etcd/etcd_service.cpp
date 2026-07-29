#include "etcd_service.h"
#include "etcd_helper.h"
#include "etcd_manager.h"
#include <google/protobuf/util/json_util.h>
#include <boost/algorithm/string.hpp>
#include "node/system/node/node.h"
#include "node/system/node/node_allocator.h"
#include "grpc_client/grpc_init_client.h"
#include "grpc_client/etcd/etcd_grpc_client.h"
#include "node/system/grpc_channel_cache.h"
#include "thread_context/redis_manager.h"
#include "thread_context/node_context_manager.h"
#include <node_config_manager.h>
#include <thread_context/snow_flake_manager.h>
#include <time/system/time.h>

void EtcdService::Init() {
	InitHandlers();

	const std::string& etcdAddr = *tlsNodeConfigManager.GetBaseDeployConfig().etcd_hosts().begin();
	auto& grpcChannelCache = gNode->GetGrpcChannelCache();
	auto channel = grpcChannelCache.GetOrCreateChannel(etcdAddr);
	LOG_INFO << "gRPC client config: ResourceQuota max threads=" << grpcChannelCache.ConfiguredMaxThreads()
		<< ", backup poll interval ms=" << grpcChannelCache.ConfiguredBackupPollIntervalMs();

	InitGrpcNode(channel, tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService));

	grpcHandlerTimer.RunEvery(0.005, [this] {
		for (auto& registry : tlsNodeContextManager.GetAllRegistries()) {
			HandleCompletedQueueMessage(registry);
		}
		CheckPendingTxnDeadline();
		});

	LOG_INFO << "EtcdService initialized with etcd: " << etcdAddr;
}

void EtcdService::InitHandlers() {
	InitKVHandlers();
	InitWatchHandlers();
	InitLeaseHandlers();
	InitTxnHandlers();
}

void EtcdService::InitKVHandlers() {
	etcdserverpb::AsyncKVRangeHandler = [this](const ClientContext& ctx, const etcdserverpb::RangeResponse& reply) {
		int64_t nextRevision = reply.header().revision() + 1;
		std::unordered_map<std::string, bool> prefixSeen;

		for (const auto& prefix : tlsNodeConfigManager.GetBaseDeployConfig().service_discovery_prefixes()) {
			prefixSeen[prefix] = false;
		}

		for (const auto& kv : reply.kvs()) {
			HandlePutEvent(kv.key(), kv.value());
			for (auto& [prefix, seen] : prefixSeen) {
				if (kv.key().rfind(prefix, 0) == 0) {
					seen = true;
				}
			}
		}

		for (const auto& [prefix, _] : prefixSeen) {
			revision[prefix] = nextRevision;
		}

		if (!hasSentRange) {
			StartWatchingPrefixes();
			hasSentRange = true;
		}
		};

	etcdserverpb::AsyncKVPutHandler = [this](const ClientContext &context, const etcdserverpb::PutResponse &reply)
	{
		LOG_TRACE << "Put response: " << reply.DebugString();
	};

	etcdserverpb::AsyncKVDeleteRangeHandler = [](const ClientContext& context, const etcdserverpb::DeleteRangeResponse& reply) {};

	auto emptyHandler = [](const ClientContext& context, const ::google::protobuf::Message& reply) {};
	if (!etcdserverpb::AsyncKVCompactHandler) {
		etcdserverpb::AsyncKVCompactHandler = emptyHandler;
	}
}

void EtcdService::InitWatchHandlers() {
	etcdserverpb::AsyncWatchWatchHandler = [this](const ClientContext& ctx, const etcdserverpb::WatchResponse& response) {
		OnWatchResponse(response);
		};
}

void EtcdService::InitLeaseHandlers() {
	etcdserverpb::AsyncLeaseLeaseGrantHandler = [this](const ClientContext& context, const etcdserverpb::LeaseGrantResponse& reply) {
		OnLeaseGranted(reply);
		};

	etcdserverpb::AsyncLeaseLeaseKeepAliveHandler = [this](const ClientContext& context, const etcdserverpb::LeaseKeepAliveResponse& reply) {
		OnKeepAliveResponse(reply);
		};
}

void EtcdService::InitTxnHandlers() {
	etcdserverpb::AsyncKVTxnHandler = [this](const ClientContext &context, const etcdserverpb::TxnResponse &reply)
	{
		LOG_TRACE << "Txn response: " << reply.DebugString();

		std::string key = gNode->GetEtcdManager().TakePendingTxnKey();
		if (key.empty())
		{
			LOG_WARN << "Ignore txn response: no CAS is currently pending.";
			return;
		}
		txnDeadline_ = std::chrono::steady_clock::time_point{}; // 响应到了,撤销超时

		if (reply.succeeded())
		{
			OnTxnSucceeded(key);
		}
		else
		{
			OnTxnFailed(key);
		}
	};
}

const char* EtcdService::RegistrationModeName(RegistrationMode mode) const {
	switch (mode) {
	case RegistrationMode::kInitialBoot:
		return "InitialBoot";
	case RegistrationMode::kReRegisterExisting:
		return "ReRegisterExisting";
	default:
		return "Unknown";
	}
}

void EtcdService::SetRegistrationMode(RegistrationMode mode, const char* reason) {
	if (registrationMode_ == mode) {
		return;
	}

	LOG_INFO << "Registration mode: " << RegistrationModeName(registrationMode_)
		<< " -> " << RegistrationModeName(mode)
		<< ", reason=" << reason;
	registrationMode_ = mode;
}

bool EtcdService::IsNodePortKey(const std::string& key) const {
	return boost::algorithm::starts_with(key, gNode->GetEtcdManager().MakeNodePortEtcdPrefix(gNode->GetNodeInfo()));
}

bool EtcdService::IsNodeIdKey(const std::string& key) const {
	return boost::algorithm::starts_with(key, gNode->GetEtcdManager().MakeNodeEtcdPrefix(gNode->GetNodeInfo()));
}

bool EtcdService::IsNodeAllocationKey(const std::string& key) const {
	const auto &info = gNode->GetNodeInfo();
	const auto allocPrefix = gNode->GetEtcdManager().GetServiceName(info.node_type()) +
							 "/allocated/node_type/" + std::to_string(info.node_type()) +
							 "/node_id/";
	return boost::algorithm::starts_with(key, allocPrefix);
}

void EtcdService::OnTxnSucceeded(const std::string& key) {
	if (IsNodePortKey(key)) {
		if (registrationMode_ == RegistrationMode::kReRegisterExisting) {
			// Re-register path keeps the same node_id and only refreshes lease-bound etcd records.
			gNode->GetEtcdManager().RegisterNodeService();
			return;
		}

		NodeAllocator::AcquireNode();
		return;
	}

	// Allocation key success is the gate that authorizes publishing the
	// per-zone NodeInfo. Doing the info write only now (instead of eagerly
	// inside RegisterNodeService) guarantees no stale per-zone record is
	// left behind when two zones race for the same node_id.
	if (IsNodeAllocationKey(key)) {
		LOG_INFO << "Global node-id allocation acquired: " << key;
		if (registrationMode_ == RegistrationMode::kReRegisterExisting)
		{
			// Re-registration path: gRPC server stayed up across the lease loss,
			// so peers won't get "connection refused" if we advertise immediately.
			gNode->GetEtcdManager().PublishNodeInfoAfterAllocation();
			return;
		}
		// Initial boot: kick off RPC/gRPC startup now. The discovery publish is
		// deferred to PublishDiscoveryAfterGrpcReady() (called at the end of
		// Node::StartGrpcServer) so peers can't dial our gRPC port before it's
		// bound. Without this deferral, scene_manager sees the node via etcd
		// watch within milliseconds of the alloc-key txn, dials gRPC, and gets
		// "connection refused" because StartGrpcServer hasn't run yet.
		ActivateSnowFlakeAfterGuard();
		return;
	}

	if (!IsNodeIdKey(key)) {
		LOG_WARN << "Unexpected txn success key: " << key;
		return;
	}

	if (registrationMode_ == RegistrationMode::kReRegisterExisting) {
		SetRegistrationMode(RegistrationMode::kInitialBoot, "re-registration succeeded");
		LOG_INFO << "Node re-registration successful, node_id=" << gNode->GetNodeInfo().node_id();
		return;
	}

	// Initial-boot serviceKey publish confirmed. SnowFlake was already activated
	// from OnTxnSucceeded(allocKey) -> ActivateSnowFlakeAfterGuard (which also
	// started the gRPC server, which then published the discovery key).
	LOG_INFO << "Node service info published: node_id=" << gNode->GetNodeInfo().node_id();
}

void EtcdService::PublishDiscoveryAfterGrpcReady()
{
	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		// Re-registration already published in OnTxnSucceeded(allocKey).
		return;
	}
	gNode->GetEtcdManager().PublishNodeInfoAfterAllocation();
}

void EtcdService::OnTxnFailed(const std::string &key)
{
	if (registrationStopped_)
	{
		LOG_INFO << "Ignoring txn failure for " << key << ": registration retries already stopped.";
		return;
	}

	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		SetRegistrationMode(RegistrationMode::kInitialBoot, "re-registration failed");
		LOG_ERROR << "Node re-registration FAILED for key: " << key
				  << ", node_id=" << gNode->GetNodeInfo().node_id()
				  << ". Another node has claimed this ID - SnowFlake collision is inevitable if we "
					 "keep minting. Fencing ID generation, persisting and relocating players, "
					 "then terminating.";
		gNode->OnNodeIdConflictShutdown(NodeIdConflictReason::kReRegistrationFailed);
		return;
	}

	// Lost the race for this (node_type, node_id) globally — another zone
	// claimed it microseconds before us. Record the loss so the next
	// AcquireNode pass deterministically skips this id (the local
	// ServiceNodeList snapshot lags the etcd watch stream during cold
	// start, so without this hint we'd loop on the same id forever).
	if (IsNodeAllocationKey(key))
	{
		const auto lostId = gNode->GetNodeInfo().node_id();
		NodeAllocator::RecordLostId(lostId);
		LOG_INFO << "Global node-id allocation lost: " << key
				 << " (id=" << lostId << "); reallocating after backoff";
		acquireNodeTimer.RunAfter(1, [] { NodeAllocator::AcquireNode(); });
		return;
	}

	if (IsNodeIdKey(key))
	{
		acquireNodeTimer.RunAfter(1, [] { NodeAllocator::AcquireNode(); });
		return;
	}

	acquirePortTimer.RunAfter(1, [this] { AcquirePortWithRetry(); });
}

void EtcdService::StartWatchingPrefixes()
{
	for (const auto &prefix : tlsNodeConfigManager.GetBaseDeployConfig().service_discovery_prefixes())
	{
		EtcdHelper::StartWatchingPrefix(prefix, revision[prefix]);
		LOG_INFO << "Watching prefix: " << prefix << " from revision " << revision[prefix];
	}
}

void EtcdService::HandlePutEvent(const std::string &key, const std::string &value)
{
	// Hijack detection: if another node overwrote our exact node_id key
	// with a different UUID, our identity is stolen.
	//
	// The check MUST be skipped while we're still in the boot window before
	// NodeId CAS has committed a real id. During that window MakeNodeEtcdKey
	// renders to ".../node_id/0" — a transient key that EVERY un-allocated
	// peer of the same {zone, type} hashes to. Each peer Watch'es it; the
	// first peer to PUT (during NodeAllocator's CAS attempt) trips the
	// "different UUID under my key" branch in every other booting peer and
	// sends them all into kReRegistrationFailed FATAL — even though no one
	// ever owned node_id=0 in the first place.
	//
	// Stress run 2026-05-23, third pass: zone-1 launched 6 cpp processes
	// (2 gate + 4 scene) concurrently, 4 of them committed suicide inside a
	// 2-second window with this exact symptom (z1_scene_2/3/4 + z1_gate_2;
	// see docs/design/stress-3zone-2026-05-23-postmortem.md §E). The CAS
	// loser path in NodeAllocator already handles "id taken" by retrying
	// with the next id, so we don't need Watch-level enforcement during
	// boot — it's redundant and hostile to concurrent same-zone launches.
	//
	// Real hijacks (a re-registration races a stale lease holder) only
	// matter once we've successfully bound a non-zero id, so gating the
	// FATAL on `myInfo.node_id() != 0` keeps that protection intact while
	// removing the boot-time false positive.
	const auto &myInfo = gNode->GetNodeInfo();
	if (myInfo.node_id() != 0)
	{
		const auto myKey = gNode->GetEtcdManager().MakeNodeEtcdKey(myInfo);
		if (key == myKey)
		{
			NodeInfo remoteInfo;
			if (google::protobuf::util::JsonStringToMessage(value, &remoteInfo).ok())
			{
				if (!remoteInfo.node_uuid().empty() &&
					remoteInfo.node_uuid() != myInfo.node_uuid())
				{
					LOG_ERROR << "Node ID hijack detected via Watch! key=" << key
							  << " my_uuid=" << myInfo.node_uuid()
							  << " remote_uuid=" << remoteInfo.node_uuid()
							  << ". Fencing ID generation, persisting and relocating players, "
								 "then terminating.";
					gNode->OnNodeIdConflictShutdown(NodeIdConflictReason::kReRegistrationFailed);
					return;
				}
			}
		}
	}

	gNode->GetServiceDiscoveryManager().HandleServiceNodeStart(key, value);
}

void EtcdService::HandleDeleteEvent(const std::string &key, const std::string &value)
{
	gNode->HandleServiceNodeStop(key, value);
}

void EtcdService::OnWatchResponse(const etcdserverpb::WatchResponse &response)
{
	// Registration flow map:
	// 1) Initial boot: Watch ready -> Lease -> Port CAS -> NodeId CAS -> StartRpcServer
	// 2) Re-register: Health monitor detects missing snapshot -> Lease -> Port CAS -> NodeId CAS (same node_id)
	//    If same node_id is already occupied, process exits via LOG_FATAL to protect identity invariants.
	if (!hasSentWatch)
	{
		RequestNodeLease();
		hasSentWatch = true;
	}

	if (response.canceled())
	{
		LOG_WARN << "Watch canceled: " << response.cancel_reason() << ", will re-establish watches";
		ScheduleWatchReconnect();
		return;
	}

	// Track revision for reconnect
	if (response.header().revision() > 0)
	{
		for (auto &[prefix, rev] : revision)
		{
			if (response.header().revision() + 1 > rev)
			{
				rev = response.header().revision() + 1;
			}
		}
	}

	for (const auto &event : response.events())
	{
		if (event.type() == mvccpb::Event_EventType::Event_EventType_PUT)
		{
			HandlePutEvent(event.kv().key(), event.kv().value());
		}
		else if (event.type() == mvccpb::Event_EventType::Event_EventType_DELETE)
		{
			HandleDeleteEvent(event.kv().key(), event.prev_kv().value());
		}
	}
}

void EtcdService::ScheduleWatchReconnect()
{
	static constexpr double kWatchReconnectDelaySec = 2.0;
	watchReconnectTimer.RunAfter(kWatchReconnectDelaySec, [this]
								 {
	LOG_INFO << "Re-establishing etcd watches after stream break";
	StartWatchingPrefixes(); });
}

void EtcdService::RequestNodeLease()
{
	if (leaseRequestInFlight_)
	{
		LOG_TRACE << "Skip lease request because one is already in flight. mode="
				  << RegistrationModeName(registrationMode_);
		return;
	}

	leaseRequestInFlight_ = true;
	LOG_INFO << "Requesting etcd lease. mode=" << RegistrationModeName(registrationMode_);
	gNode->GetEtcdManager().RequestNodeLease();
}

void EtcdService::StartLeaseKeepAlive()
{
	gNode->GetEtcdManager().StartLeaseKeepAlive();
}

void EtcdService::RegisterService()
{
	gNode->GetEtcdManager().RegisterNodeService();
}

void EtcdService::Shutdown()
{
	grpcHandlerTimer.Cancel();
	acquireNodeTimer.Cancel();
	acquirePortTimer.Cancel();
	watchReconnectTimer.Cancel();
	leaseRequestInFlight_ = false;
	txnDeadline_ = std::chrono::steady_clock::time_point{};
	SetRegistrationMode(RegistrationMode::kInitialBoot, "service shutdown");

	auto emptyHandler = [](const ClientContext &, const ::google::protobuf::Message &) {};
	etcdserverpb::AsyncKVRangeHandler = emptyHandler;
	etcdserverpb::AsyncKVPutHandler = emptyHandler;
	etcdserverpb::AsyncKVDeleteRangeHandler = emptyHandler;
	etcdserverpb::AsyncKVTxnHandler = emptyHandler;
	etcdserverpb::AsyncWatchWatchHandler = emptyHandler;
	etcdserverpb::AsyncLeaseLeaseGrantHandler = emptyHandler;

	EtcdHelper::StopAllWatching();
	gNode->GetEtcdManager().Shutdown();
}

void EtcdService::RequestReRegistration()
{
	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		LOG_DEBUG << "Re-registration already in progress, skipping duplicate attempt.";
		return;
	}
	SetRegistrationMode(RegistrationMode::kReRegisterExisting, "health monitor missing local node snapshot");
	RequestNodeLease();
}

void EtcdService::OnLeaseGranted(const etcdserverpb::LeaseGrantResponse &reply)
{
	leaseRequestInFlight_ = false;
	leaseId = reply.id();

	if (leaseId <= 0)
	{
		LOG_ERROR << "Invalid lease ID received.";
		return;
	}

	leaseTtlSeconds_ = reply.ttl();
	lastKeepAliveAckTime_ = std::chrono::steady_clock::now();
	LOG_INFO << "Lease granted: id=" << leaseId << ", ttl=" << leaseTtlSeconds_ << "s";

	StartLeaseKeepAlive();

	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		NodeAllocator::ReRegisterExistingNode();
	}
	else
	{
		AcquirePortWithRetry(); // Only acquire port initially
	}
}

void EtcdService::StopRegistrationRetries()
{
	registrationStopped_ = true;
	acquireNodeTimer.Cancel();
	acquirePortTimer.Cancel();
	gNode->GetEtcdManager().TakePendingTxnKey();
	txnDeadline_ = std::chrono::steady_clock::time_point{};
	LOG_INFO << "Registration retries stopped (node identity is no longer ours).";
}

void EtcdService::CheckPendingTxnDeadline()
{
	if (registrationStopped_)
	{
		return;
	}

	// 预算推导:一次 etcd CAS 的正常往返是毫秒级;10s 覆盖 etcd 短暂重启 / 主从切换
	// 后客户端重连的时间,又不会让一次真正丢掉的响应把节点挂很久。
	// TODO: 待压测实测复核。
	constexpr auto kTxnResponseBudget = std::chrono::seconds(10);

	const auto &pending = gNode->GetEtcdManager().PendingTxnKey();
	if (pending.empty())
	{
		txnDeadline_ = std::chrono::steady_clock::time_point{};
		return;
	}

	const auto now = std::chrono::steady_clock::now();
	if (txnDeadline_ == std::chrono::steady_clock::time_point{})
	{
		txnDeadline_ = now + kTxnResponseBudget;
		return;
	}
	if (now < txnDeadline_)
	{
		return;
	}

	const std::string lost = gNode->GetEtcdManager().TakePendingTxnKey();
	txnDeadline_ = std::chrono::steady_clock::time_point{};
	LOG_ERROR << "No etcd txn response for key " << lost << " within the budget; "
			  << "restarting the registration phase. mode=" << RegistrationModeName(registrationMode_);

	// 到期后**重查权威**(重发同一条幂等 CAS 链),不是假设成功继续往下走。
	// 重注册路径保留原 node_id 与原端口;初始引导路径从端口阶段重来。
	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		NodeAllocator::ReRegisterExistingNode();
		return;
	}
	AcquirePortWithRetry();
}

void EtcdService::AcquirePortWithRetry()
{
	if (registrationStopped_)
	{
		return;
	}
	if (NodeAllocator::AcquireNodePort())
	{
		return;
	}

	// 端口暂时分配不到(本机端口被占 / 区间耗尽)。这里刻意什么都不发布:
	// 旧实现会把 port=0 注册进 etcd,于是这个节点以 "ip:0" 出现在服务发现里,
	// 别的节点拿 0 端口去连,只会得到一串莫名其妙的连接失败,而且它还占着 node_id。
	// 退避重试,复用 acquirePortTimer,不新开一套定时器状态机。
	LOG_ERROR << "RPC port allocation failed for node_type=" << gNode->GetNodeType()
			  << "; nothing published to etcd, retrying in 1s";
	acquirePortTimer.RunAfter(1, [this]
							  { AcquirePortWithRetry(); });
}

void EtcdService::OnKeepAliveResponse(const etcdserverpb::LeaseKeepAliveResponse &reply)
{
	if (reply.ttl() <= 0)
	{
		LOG_ERROR << "Lease keepalive returned TTL=0, lease has expired on etcd server. "
					 "node_id="
				  << gNode->GetNodeInfo().node_id()
				  << ". Another node may claim this ID - fencing ID generation, persisting and "
					 "relocating players, then terminating.";
		gNode->OnNodeIdConflictShutdown(NodeIdConflictReason::kLeaseExpiredByEtcd);
		return;
	}

	lastKeepAliveAckTime_ = std::chrono::steady_clock::now();
	LOG_TRACE << "Lease keepalive ACK, ttl=" << reply.ttl();
}

bool EtcdService::IsLeasePresumablyExpired() const
{
	if (leaseTtlSeconds_ <= 0)
	{
		return false; // Lease not yet granted
	}

	auto elapsed = std::chrono::steady_clock::now() - lastKeepAliveAckTime_;
	auto elapsedSeconds = std::chrono::duration_cast<std::chrono::seconds>(elapsed).count();
	return elapsedSeconds > leaseTtlSeconds_;
}

// 激活本进程的业务发号器,并在此之前 **无条件** 打一个启动 guard。
//
// 为什么必须无条件:node_id 的唯一域是全局 (node_type, node_id)(见
// MakeNodeAllocationKey),而 guard 是写在**分 zone 的 Redis**里的。所以
//   * 跨 zone 复用同一个 node_id 时,新持有者根本读不到旧持有者写的 guard;
//   * Redis 没连上时旧代码直接"不 guard"就开始发号。
// 这两条路径下,只要旧持有者在同一日历秒里发过号(优雅退出 + 立刻重启就会发生),
// 新持有者从 step=0 重新发,产出的 ID 与旧的**逐位相同**。
//
// 因此这里的规则改成:
//   1. guard 底线 = 当前秒,永远生效(Redis 挂了、key 不存在都一样);
//   2. 读到上一任写的 last_ts 时,取 max(last_ts, now) —— 覆盖"旧持有者所在机器
//      时钟比本机快"的情况。
// 代价是本进程第一个 ID 最多晚 1 秒发出,换掉一整类静默重号。
void EtcdService::ActivateSnowFlakeAfterGuard()
{
	const auto &info = gNode->GetNodeInfo();
	const std::string guardKey = EtcdManager::MakeSnowFlakeGuardKey(info);

	auto activate = [](uint32_t nodeId, uint64_t guardSeconds, const char *source)
	{
		tlsSnowflakeManager.OnNodeStart(nodeId);
		tlsSnowflakeManager.SetGuardTime(guardSeconds);
		LOG_INFO << "SnowFlake activated: node_id=" << nodeId
				 << ", guard_to=" << guardSeconds
				 << " (" << source << "). First ID lands after that second.";
		gNode->StartRpcServer();
	};

	auto &redis = tlsRedis.GetZoneRedis();
	if (!redis || !redis->connected())
	{
		LOG_WARN << "Redis not connected; applying local-clock SnowFlake guard for node_id=" << info.node_id();
		activate(info.node_id(), TimeSystem::NowSecondsUTC(), "local clock, redis unavailable");
		return;
	}

	redis->command(
		[nodeId = info.node_id(), guardKey, activate](hiredis::Hiredis *, redisReply *reply)
		{
			const uint64_t now = TimeSystem::NowSecondsUTC();
			uint64_t guardSeconds = now;
			const char *source = "local clock, no previous holder";

			if (reply != nullptr && reply->type == REDIS_REPLY_STRING)
			{
				const uint64_t lastTs = std::strtoull(reply->str, nullptr, 10);
				if (lastTs > guardSeconds)
				{
					guardSeconds = lastTs;
					source = "previous holder last-seen (ahead of local clock)";
				}
				else
				{
					source = "local clock, previous holder seen";
				}
			}

			activate(nodeId, guardSeconds, source);
		},
		"GET %s", guardKey.c_str());
}
