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
#include "thread_context/node_context_manager.h"
#include <node_config_manager.h>

void EtcdService::Init() {
	InitHandlers();

	const std::string& etcdAddr = *tlsNodeConfigManager.GetBaseDeployConfig().etcd_hosts().begin();
	auto& grpcChannelCache = gNode->GetGrpcChannelCache();
	auto channel = grpcChannelCache.GetOrCreateChannel(etcdAddr);
	LOG_INFO << "gRPC client config: ResourceQuota max threads=" << grpcChannelCache.ConfiguredMaxThreads()
		<< ", backup poll interval ms=" << grpcChannelCache.ConfiguredBackupPollIntervalMs();

	// GetOrCreateGlobalEntity(不是 GetGlobalEntity):etcd 客户端的全部状态
	// (CompletionQueue / KV / Watch / Lease stub / watch 流)都挂在这个全局
	// 实体上,它必须真正经 registry.create() 诞生。此前配合 globalEntities_
	// 未初始化的 bug,这里拿到的是零残留的幽灵 entity 0,从未 create 过,
	// 全靠 EnTT 池语义宽容才能跑;修掉初始化后若仍用 GetGlobalEntity,
	// 拿到的会是 entt::null,emplace 直接 UB。后续 etcd_helper / etcd_manager
	// 的发送路径都在 Init 之后执行,继续用 GetGlobalEntity 拿的就是这里
	// 创建好的实体。
	InitGrpcNode(channel, tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetOrCreateGlobalEntity(EtcdNodeService));

	grpcHandlerTimer.RunEvery(0.005, [] {
		for (auto& registry : tlsNodeContextManager.GetAllRegistries()) {
			HandleCompletedQueueMessage(registry);
		}
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
		// 超时定时器已由 TakePendingTxnKey 一并取消(两者成对维护)。

		// 重注册用的是"不存在或已是我的"嵌套 txn,外层 succeeded 只反映"不存在"那一半。
		if (EtcdHelper::TxnClaimSucceeded(reply))
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
			gNode->GetEtcdManager().RegisterNodeService(/*reRegistering=*/true);
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
			gNode->GetEtcdManager().PublishNodeInfoAfterAllocation(/*reRegistering=*/true);
			return;
		}
		// Initial boot: kick off RPC/gRPC startup now. The discovery publish is
		// deferred to PublishDiscoveryAfterGrpcReady() (called at the end of
		// Node::StartGrpcServer) so peers can't dial our gRPC port before it's
		// bound. Without this deferral, scene_manager sees the node via etcd
		// watch within milliseconds of the alloc-key txn, dials gRPC, and gets
		// "connection refused" because StartGrpcServer hasn't run yet.
		//
		// 路由 node_id 到手就起 RPC,**不等任何发号器**:永久 guid 走号段(scene 的
		// GuidSegmentRegistry 经 DataService.AllocateIdSegment 领段,与 etcd 无关),
		// scene 的 DependencyGate 用 "id segments ready" 条件挡住玩家进入;gate / battle 不铸永久 guid。
		gNode->StartRpcServer();
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

	// Initial-boot serviceKey publish confirmed (OnTxnSucceeded(allocKey) started
	// the RPC/gRPC servers, which then published the discovery key).
	LOG_INFO << "Node service info published: node_id=" << gNode->GetNodeInfo().node_id();
}

void EtcdService::PublishDiscoveryAfterGrpcReady()
{
	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		// Re-registration already published in OnTxnSucceeded(allocKey).
		return;
	}
	gNode->GetEtcdManager().PublishNodeInfoAfterAllocation(/*reRegistering=*/false);
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

	// 与 txn 超时同一预算、同一理由(见 ArmTxnTimeout)。
	constexpr double kLeaseGrantBudgetSec = 10.0;
	leaseGrantTimeoutTimer_.RunAfter(kLeaseGrantBudgetSec, [this] { OnLeaseGrantTimeout(); });
}

void EtcdService::OnLeaseGrantTimeout()
{
	if (!leaseRequestInFlight_ || registrationStopped_)
	{
		return;
	}
	leaseRequestInFlight_ = false;
	LOG_ERROR << "No etcd LeaseGrant response within the budget; retrying. mode="
			  << RegistrationModeName(registrationMode_);
	RequestNodeLease();
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
	txnTimeoutTimer.Cancel();
	leaseGrantTimeoutTimer_.Cancel();
	leaseRequestInFlight_ = false;
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

void EtcdService::RequestReRegistration(const char *reason)
{
	if (registrationStopped_)
	{
		return;
	}
	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		LOG_DEBUG << "Re-registration already in progress, skipping duplicate attempt. reason=" << reason;
		return;
	}
	SetRegistrationMode(RegistrationMode::kReRegisterExisting, reason);
	RequestNodeLease();
}

void EtcdService::OnLeaseGranted(const etcdserverpb::LeaseGrantResponse &reply)
{
	leaseRequestInFlight_ = false;
	leaseGrantTimeoutTimer_.Cancel();
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
	gNode->GetEtcdManager().TakePendingTxnKey(); // 顺带取消超时定时器
	LOG_INFO << "Registration retries stopped (node identity is no longer ours).";
}

void EtcdService::ArmTxnTimeout()
{
	if (registrationStopped_)
	{
		return;
	}

	// 预算推导:一次 etcd CAS 的正常往返是毫秒级;10s 覆盖 etcd 短暂重启 / 主从切换
	// 后客户端重连的时间,又不会让一次真正丢掉的响应把节点挂很久。
	// TODO: 待压测实测复核。
	constexpr double kTxnResponseBudgetSec = 10.0;

	txnTimeoutTimer.RunAfter(kTxnResponseBudgetSec, [this] { OnTxnTimeout(); });
}

void EtcdService::CancelTxnTimeout()
{
	txnTimeoutTimer.Cancel();
}

void EtcdService::OnTxnTimeout()
{
	if (registrationStopped_)
	{
		return;
	}

	// TakePendingTxnKey 会顺带 Cancel 本定时器 —— TimerTaskComp::OnTimer 对
	// "回调里取消自己"是安全的(one-shot 先清 timerId 再拷贝 callback 调用)。
	const std::string lost = gNode->GetEtcdManager().TakePendingTxnKey();
	if (lost.empty())
	{
		return;
	}

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
	if (reply.id() != leaseId)
	{
		// 失租重拿后,旧 lease 的最后几拍 TTL=0 应答还会从流上回来;按 id 过滤,
		// 不然会把刚拿到的新 lease 再拖进一轮重注册。
		LOG_INFO << "Ignoring keepalive response for stale lease " << reply.id() << " (current " << leaseId << ")";
		return;
	}

	if (reply.ttl() <= 0)
	{
		// lease 过期**不等于**身份被抢:etcd leader 切换、keepalive 被 IO 抢占、冻结感知
		// 延迟都会走到这里。ID 唯一性不靠 lease(永久 guid 走号段,由数据库 CAS 领段保证),
		// 路由身份靠分配键的 CAS。所以这里只是拿新 lease 把 key 重新挂上;
		// 分配键的 value 若已经是别人的,重注册的 CAS 会失败并走 OnNodeIdConflictShutdown。
		LOG_ERROR << "Lease keepalive returned TTL=0 (lease " << leaseId << " expired on etcd). node_id="
				  << gNode->GetNodeInfo().node_id()
				  << ". Re-granting a lease and re-attaching routing keys; identity is decided by the CAS, not by the lease.";
		RequestReRegistration("keepalive returned TTL<=0");
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

// 2026-09-08 起这里不再激活任何发号器(旧 ActivateSnowFlakeAfterGuard:读分 zone Redis
// 的 guard 后 OnNodeStart(路由 node_id) 再 StartRpcServer)。永久 guid 改走号段
// (docs/design/node-id-overhaul-plan-20260908.md §6 / §7.5),由 scene 的 GuidSegmentRegistry
// 经 DataService 领段,与 etcd 注册流无关;RPC 服务器在分配键 CAS 成功时直接启动(OnTxnSucceeded)。
