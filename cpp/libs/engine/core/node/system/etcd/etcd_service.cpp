#include "etcd_service.h"
#include "etcd_helper.h"
#include "etcd_manager.h"
#include "etcd_registration_wait.h"
#include <boost/cobalt/task.hpp>
#include "node/system/discovery/discovery_resync_policy.h"
#include "node/system/discovery/service_discovery_manager.h"
#include <algorithm>
#include <unordered_set>
#include <utility>
#include <google/protobuf/util/json_util.h>
#include <boost/algorithm/string.hpp>
#include "node/system/node/node.h"
#include "node/system/node/node_allocator.h"
#include "thread_context/node_context_manager.h"
#include <node_config_manager.h>

EtcdService::~EtcdService()
{
	if (registrationTasks_) registrationTasks_->Stop();
}

void EtcdService::Init() {
	const std::string& etcdAddr = *tlsNodeConfigManager.GetBaseDeployConfig().etcd_hosts().begin();
	// 传输在 Node 构造期建好(loop 线程,loop 尚未运行);新传输的地址解析也在这里做完(§13.6)。
	ConfigureRegistration(*gNode->GetLoop(), {
		[](auto done) { gNode->GetEtcdManager().RequestNodeLease(std::move(done)); },
		[] { gNode->GetEtcdManager().StartLeaseKeepAlive(); },
		&NodeAllocator::AcquireNodePort,
		&NodeAllocator::SelectNodeId,
		[] { NodeAllocator::RecordLostId(gNode->GetNodeInfo().node_id()); },
		[](RegistrationStage stage) {
			const auto& info = gNode->GetNodeInfo();
			auto& manager = gNode->GetEtcdManager();
			return stage == RegistrationStage::kPort ? manager.MakeNodePortEtcdKey(info) :
				(stage == RegistrationStage::kAllocation ? manager.MakeNodeAllocationKey(info) : manager.MakeNodeEtcdKey(info));
		},
		[](RegistrationStage stage, bool reRegistering, bool retryUnknown, RegistrationIo::TxnDone done) {
			auto& manager = gNode->GetEtcdManager();
			switch (stage)
			{
			case RegistrationStage::kPort: manager.RegisterNodePort(reRegistering, retryUnknown, std::move(done)); break;
			case RegistrationStage::kAllocation: manager.RegisterNodeService(reRegistering, retryUnknown, std::move(done)); break;
			case RegistrationStage::kPublish: manager.PublishNodeInfoAfterAllocation(reRegistering, retryUnknown, std::move(done)); break;
			}
		},
		[] { gNode->StartRpcServer(); },
		[] { gNode->OnNodeIdConflictShutdown(NodeIdConflictReason::kReRegistrationFailed); },
	});
	transport_ = etcd_transport::Create(*gNode->GetLoop(), etcdAddr, *this);
	LOG_INFO << "etcd transport=" << transport_->Name()
			 << ": watch / keepalive streams are rebuilt with backoff, each prefix re-ranged and reconciled"
			 << " (docs/design/grpc-muduo-transport.md §13.5)";
	LOG_INFO << "EtcdService initialized with etcd: " << etcdAddr;
}

void EtcdService::ConfigureRegistration(muduo::net::EventLoop& loop, RegistrationIo io)
{
	registrationLoop_ = loop;
	registrationIo_ = std::move(io);
	registrationTasks_.emplace(loop);
}

void EtcdService::OnRange(const etcdserverpb::RangeRequest* request, const etcdserverpb::RangeResponse& reply)
{
	if (request == nullptr)
	{
		LOG_ERROR << "etcd range reply without its request; the transport must hand the request back for reconcile";
		return;
	}
	OnPrefixRange(request->key(), reply);
}

bool EtcdService::IsAllocationKey(const std::string& key)
{
	// 与 HandleServiceNodeStart / Node::HandleServiceNodeStop 的过滤同一口径:值是裸 uuid,不是发现数据。
	return key.find("/allocated/") != std::string::npos;
}

namespace
{
// 发现键的值是 NodeInfo JSON;解析失败返回空串(调用方按「无法判断」处理)。
std::string NodeUuidOf(const std::string& nodeJson)
{
	google::protobuf::util::JsonParseOptions parseOptions;
	parseOptions.ignore_unknown_fields = true;
	NodeInfo node;
	if (!google::protobuf::util::JsonStringToMessage(nodeJson, &node, parseOptions).ok())
	{
		return {};
	}
	return node.node_uuid();
}
} // namespace

void EtcdService::ApplyDiscoveryPut(const std::string& key, const std::string& value, int64_t modRevision,
									std::vector<PendingRemoval>& removals)
{
	const std::string currentUuid = NodeUuidOf(value);
	auto outcome = discoveryKeys_.ApplyPut(key, value, modRevision, currentUuid);
	if (!outcome.accepted)
	{
		// Range 与 watch 走不同的流:这条比已经处理过的事件(更新的 PUT 或删除)旧。
		LOG_DEBUG << "Skip stale discovery PUT " << key << " mod_revision=" << modRevision;
		return;
	}
	if (outcome.replacedValue && !outcome.replacedHolder.empty() && !currentUuid.empty() &&
		outcome.replacedHolder != currentUuid)
	{
		// 同一个键换了持有者:对端重启后按「最小空闲 id」拿回同一 node_id、以新 uuid 写回。发现快照按 uuid 区分节点,
		// 旧持有者此后再不会有指向它的事件(它的 DELETE 可能在流断期间错过,或因比这次快照旧被判过时),
		// 不摘就永久留成幽灵。两边 uuid 都解析得出且不同才考虑摘;真正摘不摘留到整批应用完再看(FlushPendingRemovals):
		// 旧持有者可能只是换了号、还挂在别的键上(Go 侧 ReallocateNewID 保留 uuid 换 node_id)。
		removals.push_back({key, std::move(*outcome.replacedValue), outcome.replacedHolder,
							"now held by node " + currentUuid});
	}
	HandlePutEvent(key, value);
}

void EtcdService::FlushPendingRemovals(std::vector<PendingRemoval>& removals)
{
	// 发现层按 uuid 摘节点(Node::ExecuteNodeRemoval),所以只摘已经不再挂在任何发现键上的 uuid:
	// 同一 uuid 可能同时(或在同一批里先后)挂在两个键上 —— 换号的 Go 节点、同一批里先 DELETE 后重新 PUT 的节点。
	// uuid 解析不了(空串)时无法判断,照旧交给发现层。
	for (auto& removal : removals)
	{
		if (discoveryKeys_.HoldsAnyKey(removal.uuid))
		{
			LOG_INFO << "etcd key " << removal.key << " no longer belongs to node " << removal.uuid << " ("
					 << removal.reason << "), but the node is still registered under another key; keeping it";
			continue;
		}
		LOG_INFO << "etcd key " << removal.key << " no longer belongs to node "
				 << (removal.uuid.empty() ? std::string("<unparsed>") : removal.uuid) << " (" << removal.reason
				 << "); removing it";
		HandleDeleteEvent(removal.key, removal.value);
	}
	removals.clear();
}

void EtcdService::OnPrefixRange(const std::string& prefix, const etcdserverpb::RangeResponse& reply)
{
	const int64_t snapshotRevision = reply.header().revision();
	// 不持有 map 元素的引用:下面的 HandlePutEvent / HandleDeleteEvent 可能经冲突关机重入本对象。
	const int64_t appliedRevision = appliedSnapshotRevision_[prefix];
	if (snapshotRevision < appliedRevision)
	{
		// 比已经应用过的快照旧(同一前缀多个 Range 同时在途、乱序到达):整条丢弃,不应用 PUT、不对账 ——
		// 否则它会把已删节点加回来(墓碑已被更新的快照清掉)。watch 若还没建,下面仍从已应用快照之后建。
		LOG_DEBUG << "Drop out-of-order etcd range for prefix " << prefix << " at revision " << snapshotRevision
				  << " (already applied " << appliedRevision << ")";
	}
	else
	{
		// 快照不完整一律不删(§10.2 第 3 条):截断、值解析失败、key 不属于前缀。
		bool complete = !reply.more();
		if (reply.more())
		{
			LOG_WARN << "etcd range for prefix " << prefix << " is truncated (more=true, count=" << reply.count()
					 << "); skip reconcile for this round";
		}

		std::unordered_set<std::string> snapshotKeys;
		std::vector<PendingRemoval> removals;
		for (const auto& kv : reply.kvs())
		{
			const std::string& key = kv.key();
			if (!boost::algorithm::starts_with(key, prefix))
			{
				complete = false;
				LOG_WARN << "etcd range for prefix " << prefix << " returned foreign key " << key << "; skip reconcile";
				HandlePutEvent(key, kv.value());
				continue;
			}
			if (IsAllocationKey(key))
			{
				HandlePutEvent(key, kv.value()); // 与旧路径一致:交给发现层,由它跳过
				continue;
			}
			if (NodeUuidOf(kv.value()).empty())
			{
				complete = false; // 值解析不了(或没有 uuid):AddServiceNode 会打出具体错误
			}
			snapshotKeys.insert(key);
			ApplyDiscoveryPut(key, kv.value(), kv.mod_revision(), removals);
		}

		for (auto& removed : discoveryKeys_.ApplySnapshot(prefix, snapshotKeys, snapshotRevision, complete))
		{
			removals.push_back({std::move(removed.key), std::move(removed.value), std::move(removed.holder),
								"absent from the range snapshot of " + prefix + " at revision " +
									std::to_string(snapshotRevision)});
		}
		appliedSnapshotRevision_[prefix] = snapshotRevision;
		// 整个快照应用完再摘:同一快照里旧持有者可能已经出现在另一个键上(换号的 Go 节点)。
		FlushPendingRemovals(removals);
	}

	// 这个前缀在当前流上还没有 watch:从已应用快照之后的第一个 revision 建,快照与 watch 之间不漏事件,
	// 也不从更旧的 revision 回放(重建前发出的旧 Range 晚到时同样如此)。
	const int64_t watchFrom = std::max(snapshotRevision, appliedRevision) + 1;
	auto& watch = prefixWatches_[prefix];
	if (!watch.createSent)
	{
		watch.createSent = true;
		pendingWatchCreates_.push_back(prefix);
		EtcdHelper::StartWatchingPrefix(prefix, watchFrom);
		LOG_INFO << "Watching prefix: " << prefix << " from revision " << watchFrom;
	}
}

void EtcdService::OnPut(const etcdserverpb::PutResponse& reply)
{
	LOG_TRACE << "Put response: " << reply.DebugString();
}

void EtcdService::OnDeleteRange(const etcdserverpb::DeleteRangeResponse&)
{
}

void EtcdService::OnKeepAlive(const etcdserverpb::LeaseKeepAliveResponse& reply)
{
	OnKeepAliveResponse(reply);
}

void EtcdService::OnWatch(const etcdserverpb::WatchResponse& reply)
{
	OnStreamWatchResponse(reply);
}

void EtcdService::OnStreamWatchResponse(const etcdserverpb::WatchResponse& response)
{
	// 注册链的触发点不变:第一个 watch 应答(即第一个 created)触发 LeaseGrant;重建后的 created 不再触发。
	if (!hasSentWatch)
	{
		RequestNodeLease();
		hasSentWatch = true;
	}

	if (response.created())
	{
		// etcd 在一条流上按收到 create 的顺序逐个回 created(创建失败也回,只是同时带 canceled)。
		if (pendingWatchCreates_.empty())
		{
			LOG_WARN << "etcd watch created (watch_id=" << response.watch_id() << ") without a pending create request";
		}
		else
		{
			std::string prefix = std::move(pendingWatchCreates_.front());
			pendingWatchCreates_.pop_front();
			prefixWatches_[prefix].watchId = response.watch_id();
			watchIdToPrefix_[response.watch_id()] = prefix;
			if (!response.canceled())
			{
				// 建成了:这个前缀的取消退避清零(discovery_resync_policy.h)。
				cancelRetryBackoffSeconds_.erase(prefix);
			}
			LOG_INFO << "etcd watch created: prefix=" << prefix << " watch_id=" << response.watch_id();
		}
	}

	ApplyWatchEvents(response);

	if (!response.canceled())
	{
		return;
	}
	const auto mapped = watchIdToPrefix_.find(response.watch_id());
	if (mapped == watchIdToPrefix_.end())
	{
		LOG_WARN << "etcd watch canceled for unknown watch_id=" << response.watch_id()
				 << " reason=" << response.cancel_reason();
		return;
	}
	const std::string prefix = mapped->second;
	LOG_WARN << "etcd watch canceled: prefix=" << prefix << " watch_id=" << response.watch_id()
			 << " compact_revision=" << response.compact_revision() << " reason=" << response.cancel_reason();
	// 只重建这一个前缀,不在同一流上重发全部前缀(旧实现会给每个前缀叠一个重复 watcher)。
	ResetPrefixWatch(prefix);
	if (response.compact_revision() != 0)
	{
		// 起点已被压缩:立刻重新 Range → 对账 → 从新快照的 R+1 建 watch(新起点必然在压缩点之后,不会再被拒)。
		EtcdHelper::RangeQuery(prefix);
		return;
	}
	// 其他原因的取消:以前等下一次 2 秒全量 Range 重建;兜底周期拉长到约 60 秒之后不能再等它,
	// 改为按前缀退避重拉(1 秒起、翻倍、封顶 30 秒),对端持续拒绝时不空转。
	ScheduleCanceledPrefixRetry(prefix);
}

void EtcdService::ScheduleCanceledPrefixRetry(const std::string& prefix)
{
	double& backoff = cancelRetryBackoffSeconds_[prefix];
	backoff = discovery_resync::NextCancelRetrySeconds(backoff);
	if (std::find(canceledPrefixesToRetry_.begin(), canceledPrefixesToRetry_.end(), prefix) == canceledPrefixesToRetry_.end())
	{
		canceledPrefixesToRetry_.push_back(prefix);
	}
	if (cancelRetryTimer_.IsActive())
	{
		// 已经排了一拍:这个前缀随那一拍一起重拉(最多晚到 30 秒的上限)。
		return;
	}
	LOG_WARN << "etcd watch for prefix " << prefix << " canceled; re-ranging in " << backoff << "s";
	cancelRetryTimer_.RunAfter(backoff, [this] {
		std::vector<std::string> prefixes;
		prefixes.swap(canceledPrefixesToRetry_);
		for (const auto& p : prefixes)
		{
			// 应答到了走 OnPrefixRange:对账,并在当前流上还没有 watch 时从快照之后建。
			EtcdHelper::RangeQuery(p);
		}
	});
}

void EtcdService::ApplyWatchEvents(const etcdserverpb::WatchResponse& response)
{
	// 一个应答里的事件全部应用完再摘节点(FlushPendingRemovals):同一批里先 DELETE 后重新 PUT 的节点
	// (失租重挂、换号)不必先摘再加。跨应答的先后照旧按到达顺序处理。
	std::vector<PendingRemoval> removals;
	for (const auto& event : response.events())
	{
		const std::string& key = event.kv().key();
		if (event.type() == mvccpb::Event_EventType::Event_EventType_PUT)
		{
			if (IsAllocationKey(key))
			{
				HandlePutEvent(key, event.kv().value());
				continue;
			}
			ApplyDiscoveryPut(key, event.kv().value(), event.kv().mod_revision(), removals);
			continue;
		}
		if (event.type() != mvccpb::Event_EventType::Event_EventType_DELETE)
		{
			continue;
		}
		if (IsAllocationKey(key))
		{
			HandleDeleteEvent(key, event.prev_kv().value()); // 发现层按 /allocated/ 跳过,与旧路径一致
			continue;
		}
		// 删除事件的 kv.mod_revision 就是删除发生的 revision;缺省时退回应答头的 revision。
		const int64_t deletedAt =
			event.kv().mod_revision() != 0 ? event.kv().mod_revision() : response.header().revision();
		auto outcome = discoveryKeys_.ApplyDelete(key, deletedAt);
		if (outcome.result == DiscoveryKeyIndex::DeleteResult::kStale)
		{
			LOG_INFO << "Ignore stale watch DELETE " << key << " at revision " << deletedAt
					 << ": a newer PUT of this key was applied, or this delete was already handled";
			continue;
		}
		// 不依赖 prev_kv(§10.2 第 4 条):有就用,没有按 key 取本地记录。
		std::string value = event.has_prev_kv() && !event.prev_kv().value().empty() ? event.prev_kv().value()
																					  : std::move(outcome.value);
		if (value.empty())
		{
			LOG_WARN << "etcd watch DELETE " << key << " carries no prev_kv and the key is not in the local index;"
					 << " nothing to remove";
			continue;
		}
		std::string uuid = outcome.result == DiscoveryKeyIndex::DeleteResult::kRemoved ? std::move(outcome.holder)
																						 : NodeUuidOf(value);
		removals.push_back({key, std::move(value), std::move(uuid),
							"deleted at revision " + std::to_string(deletedAt)});
	}
	FlushPendingRemovals(removals);
}

void EtcdService::ResetPrefixWatch(const std::string& prefix)
{
	const auto it = prefixWatches_.find(prefix);
	if (it == prefixWatches_.end())
	{
		return;
	}
	watchIdToPrefix_.erase(it->second.watchId);
	it->second = PrefixWatch{};
}

void EtcdService::OnWatchStreamRestarted()
{
	// 新流上一个 watch 都没有:旧流上的 watch_id、待回的 created 全部作废。逐前缀重新 Range,
	// 应答到了再对账、再从 R+1 建 watch(OnPrefixRange)。流断期间下线的对端在对账里补删。
	// 与 FetchServiceNodes 同一份前缀(zone-scoped 类型只看本 zone,control-plane-zone-scoping-20261105.md §3.1)。
	const auto prefixes = gNode->GetServiceDiscoveryManager().EffectiveDiscoveryPrefixes();
	LOG_WARN << "etcd watch stream rebuilt; re-ranging " << prefixes.size() << " prefixes before re-creating watches";
	prefixWatches_.clear();
	pendingWatchCreates_.clear();
	watchIdToPrefix_.clear();
	// 全部前缀马上重拉,排着的取消重拉不必再跑。
	cancelRetryTimer_.Cancel();
	canceledPrefixesToRetry_.clear();
	for (const auto& prefix : prefixes)
	{
		EtcdHelper::RangeQuery(prefix);
	}
}

void EtcdService::OnKeepAliveStreamRestarted()
{
	// §10.2 第 2 条:重建后立刻补一拍,不等下一次 keepalive 定时器。
	if (leaseId <= 0)
	{
		return;
	}
	LOG_INFO << "etcd keepalive stream rebuilt; sending an immediate keepalive for lease " << leaseId;
	EtcdHelper::KeepAliveLease(leaseId);
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

void EtcdService::PublishDiscoveryAfterGrpcReady()
{
	registrationLoop_->get().assertInLoopThread();
	grpcReady_ = true;
	if (auto done = std::exchange(grpcReadyCompletion_, {})) done(true);
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
	// see docs/stress/stress-3zone-2026-05-23-postmortem.md §E). The CAS
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
			// D77:忽略未知字段(永久保留)。严格解析时,更新版本的节点多带一个字段就会让
			// 这里解析失败、静默跳过劫持检测。
			google::protobuf::util::JsonParseOptions parseOptions;
			parseOptions.ignore_unknown_fields = true;
			if (google::protobuf::util::JsonStringToMessage(value, &remoteInfo, parseOptions).ok())
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

void EtcdService::RequestNodeLease()
{
	if (registrationStopped_ || registrationRunning_ || !registrationTasks_) return;
	auto& loop = registrationLoop_->get();
	loop.assertInLoopThread();
	registrationRunning_ = true;
	if (!registrationTasks_->Start(RegisterAsync(loop), "[EtcdRegistration]")) registrationRunning_ = false;
}

boost::cobalt::task<void> EtcdService::RegisterAsync(muduo::net::EventLoop& loop)
{
	// 注册任务与 EtcdService 同寿命。跨异步边界只交出本次等待的完成函数。
	struct RunningGuard
	{
		bool& running;
		~RunningGuard() { running = false; }
	} runningGuard{registrationRunning_};
	using Stage = RegistrationStage;
	constexpr double kResponseBudgetSeconds = 10.0;
	constexpr double kConflictRetrySeconds = 1.0;
	TimerTaskComp timer;

	while (!registrationStopped_)
	{
		restartRegistration_ = false;
		const bool reRegistering = registrationMode_ == RegistrationMode::kReRegisterExisting;
		LOG_INFO << "Requesting etcd lease. mode=" << RegistrationModeName(registrationMode_);
		auto lease = co_await etcd_registration::AwaitReply<etcdserverpb::LeaseGrantResponse>(loop, timer,
			kResponseBudgetSeconds, registrationIo_.requestLease);
		timer.Cancel();
		// 提交时可能先完成、再重入停止。收到成功也要复核本地注册仍可推进。
		if (registrationStopped_ || !lease) co_return;
		if (restartRegistration_) continue;
		if (!*lease)
		{
			LOG_ERROR << "No etcd LeaseGrant response within the budget; retrying. mode="
					  << RegistrationModeName(registrationMode_);
			continue;
		}

		leaseId = (*lease)->id();
		if (leaseId <= 0)
		{
			LOG_ERROR << "Invalid lease ID received.";
			co_return;
		}
		leaseTtlSeconds_ = (*lease)->ttl();
		lastKeepAliveAckTime_ = std::chrono::steady_clock::now();
		LOG_INFO << "Lease granted: id=" << leaseId << ", ttl=" << leaseTtlSeconds_ << "s";
		registrationIo_.startKeepAlive();

		Stage stage = Stage::kPort;
		bool retryUnknown = false;
		while (!restartRegistration_ && !registrationStopped_)
		{
			// CAS 结果未知时复核同一个键，不重新挑选端口或节点号。
			if (stage == Stage::kPort && !reRegistering && !retryUnknown && !registrationIo_.selectPort())
			{
				LOG_ERROR << "RPC port allocation failed; nothing published to etcd, retrying in 1s";
				const auto waited = co_await etcd_registration::Delay(loop, timer, kConflictRetrySeconds);
				if (registrationStopped_ || !waited || !*waited) co_return;
				continue;
			}
			if (stage == Stage::kAllocation && !reRegistering && !retryUnknown) registrationIo_.selectNodeId();

			// 普通引导的监听在 StartRpcServer 内就绪。维护预备态需等提交开放监听；
			// 失租重注册通常已经就绪，但维护预备态也必须守住这一道闸。
			if (stage == Stage::kPublish && !grpcReady_)
			{
				auto ready = co_await loop_async::AwaitCallback<bool>(loop, [&completion = grpcReadyCompletion_](auto done) {
					completion = std::move(done);
					return true;
				}, false);
				if (registrationStopped_ || !ready) co_return;
				if (!*ready || restartRegistration_) break;
			}

			const std::string key = registrationIo_.key(stage);
			auto reply = co_await etcd_registration::AwaitReply<etcdserverpb::TxnResponse>(loop, timer,
				kResponseBudgetSeconds, [&claim = registrationIo_.claim, stage, reRegistering, retryUnknown](auto done) {
					claim(stage, reRegistering, retryUnknown, std::move(done));
				});
			timer.Cancel();
			if (registrationStopped_ || !reply) co_return;
			if (restartRegistration_) break;
			if (!*reply)
			{
				LOG_ERROR << "No etcd txn response for key " << key << " within the budget; "
						  << "rechecking the same key. mode=" << RegistrationModeName(registrationMode_);
				retryUnknown = true;
				continue;
			}

			retryUnknown = false;
			if (!EtcdHelper::TxnClaimSucceeded(**reply))
			{
				if (reRegistering)
				{
					SetRegistrationMode(RegistrationMode::kInitialBoot, "re-registration failed");
					LOG_ERROR << "Node re-registration FAILED for key: " << key;
					registrationIo_.fenceIdentity();
					co_return;
				}
				if (stage == Stage::kAllocation) registrationIo_.recordLostNodeId();
				LOG_WARN << "etcd registration CAS rejected for " << key << "; retrying in 1s";
				const auto waited = co_await etcd_registration::Delay(loop, timer, kConflictRetrySeconds);
				if (registrationStopped_ || !waited || !*waited) co_return;
				continue;
			}

			if (stage == Stage::kPort)
			{
				stage = Stage::kAllocation;
				continue;
			}
			if (stage == Stage::kAllocation)
			{
				LOG_INFO << "Global node-id allocation acquired: " << key;
				if (!rpcPrepared_)
				{
					rpcPrepared_ = true;
					registrationIo_.startRpc();
				}
				stage = Stage::kPublish;
				continue;
			}

			if (reRegistering)
			{
				SetRegistrationMode(RegistrationMode::kInitialBoot, "re-registration succeeded");
				LOG_INFO << "Node re-registration successful, key=" << key;
			}
			else
			{
				LOG_INFO << "Node service info published: key=" << key;
			}
			co_return;
		}
	}
}

void EtcdService::Shutdown()
{
	if (!registrationStopped_)
	{
		registrationStopped_ = true;
		if (registrationTasks_) registrationTasks_->Stop();
		grpcReadyCompletion_ = {};
	}
	cancelRetryTimer_.Cancel();
	canceledPrefixesToRetry_.clear();
	SetRegistrationMode(RegistrationMode::kInitialBoot, "service shutdown");

	EtcdHelper::StopAllWatching();
	gNode->GetEtcdManager().Shutdown();

	// FinalizeShutdownInLoop 已经发出 LeaseRevoke，先停本地注册等待再拆传输。
	if (transport_)
	{
		transport_->Shutdown();
		transport_.reset();
	}
	prefixWatches_.clear();
	pendingWatchCreates_.clear();
	watchIdToPrefix_.clear();
}

void EtcdService::RequestReRegistration(const char *reason)
{
	if (!registrationLoop_) return;
	registrationLoop_->get().assertInLoopThread();
	if (registrationStopped_) return;
	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		LOG_DEBUG << "Re-registration already in progress, skipping duplicate attempt. reason=" << reason;
		return;
	}
	if (rpcPrepared_ || !registrationRunning_) SetRegistrationMode(RegistrationMode::kReRegisterExisting, reason);
	if (registrationRunning_)
	{
		// 旧调用结束或预算到期后只领新租约，不再推进失租前的步骤。若在等监听开放，立即唤醒。
		restartRegistration_ = true;
		if (auto done = std::exchange(grpcReadyCompletion_, {})) done(false);
		return;
	}
	RequestNodeLease();
}

void EtcdService::StopRegistrationRetries()
{
	if (registrationLoop_) registrationLoop_->get().assertInLoopThread();
	if (registrationStopped_) return;
	registrationStopped_ = true;
	if (registrationTasks_) registrationTasks_->Stop();
	grpcReadyCompletion_ = {};
	LOG_INFO << "Registration retries stopped (node identity is no longer ours).";
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

bool EtcdService::IsIdentityConfirmedFresh() const
{
	if (registrationStopped_ || leaseId <= 0 || leaseTtlSeconds_ <= 0)
	{
		return false;
	}
	if (registrationMode_ == RegistrationMode::kReRegisterExisting)
	{
		return false;
	}
	const auto elapsed = std::chrono::steady_clock::now() - lastKeepAliveAckTime_;
	const auto elapsedMs = std::chrono::duration_cast<std::chrono::milliseconds>(elapsed).count();
	return elapsedMs * 2 <= leaseTtlSeconds_ * 1000;
}

// 2026-09-08 起这里不再激活任何发号器(旧 ActivateSnowFlakeAfterGuard:读分 zone Redis
// 的 guard 后 OnNodeStart(路由 node_id) 再 StartRpcServer)。永久 guid 改走号段
// (docs/design/node-id-overhaul-plan-20260908.md §6 / §7.5),由 scene 的 GuidSegmentRegistry
// 经 DataService 领段,与 etcd 注册流无关;RPC 服务器在分配键 CAS 成功时直接启动(RegisterAsync)。
