#include "etcd_manager.h"
#include "proto/common/base/common.pb.h"
#include "node/system/node/node_util.h"
#include <muduo/base/Logging.h>
#include "etcd_helper.h"
#include <thread_context/redis_manager.h>
#include "grpc_client/etcd/etcd_grpc_client.h"
#include "thread_context/node_context_manager.h"
#include <node_config_manager.h>
#include <node/system/node/node.h>
#include <time/system/time.h>

void EtcdManager::Shutdown()
{
	leaseKeepAliveTimer.Cancel();
	// 走 Take 而不是直接 clear,维持"有 pending key ⟺ 超时定时器在跑"这个不变量。
	// 调用方是 EtcdService::Shutdown,此时 EtcdService 还活着,回调进去只是 Cancel,安全。
	TakePendingTxnKey();
}

void EtcdManager::SetPendingTxnKey(const std::string &key)
{
	if (!pendingTxnKey_.empty() && pendingTxnKey_ != key)
	{
		// 注册流是串行的,这里非空说明上一个 CAS 的响应从来没回来(etcd RPC 失败时
		// 生成的 client 不会调 handler)。把它打出来 —— 以前这种情况是静默地在队列里
		// 多压一个 key,从此所有响应错位一格。
		LOG_ERROR << "Overwriting an unanswered pending etcd txn key: old=" << pendingTxnKey_
				  << " new=" << key;
	}
	pendingTxnKey_ = key;
	// 不变量:有 pending key ⟺ 超时定时器在跑。
	gNode->GetServiceDiscoveryManager().etcdService.ArmTxnTimeout();
}

std::string EtcdManager::TakePendingTxnKey()
{
	std::string key;
	key.swap(pendingTxnKey_);
	gNode->GetServiceDiscoveryManager().etcdService.CancelTxnTimeout();
	return key;
}

std::string EtcdManager::GetServiceName(uint32_t type)
{
	return eNodeType_Name(type) + ".rpc";
}

std::string EtcdManager::MakeNodeEtcdPrefix(const NodeInfo &info)
{
	return GetServiceName(info.node_type()) +
		   "/zone/" + std::to_string(info.zone_id()) +
		   "/node_type/" + std::to_string(info.node_type()) +
		   "/node_id/";
}

std::string EtcdManager::MakeNodeEtcdKey(const NodeInfo &info)
{
	return MakeNodeEtcdPrefix(info) + std::to_string(info.node_id());
}

std::string EtcdManager::MakeNodeAllocationKey(const NodeInfo &info)
{
	// Zone-independent: the key contains node_type and node_id only. Two
	// zones issuing concurrent PutIfAbsent on the same (node_type, node_id)
	// will see exactly one CAS succeed; the loser has to pick a different
	// node_id. This matches the Go-side allocator layout so mixed-language
	// deployments share a single source of truth for node_id uniqueness.
	return GetServiceName(info.node_type()) +
		   "/allocated/node_type/" + std::to_string(info.node_type()) +
		   "/node_id/" + std::to_string(info.node_id());
}

std::string EtcdManager::MakeNodePortEtcdPrefix(const NodeInfo &nodeInfo)
{
	return "/service/" + nodeInfo.endpoint().ip() + "/port/";
}

std::string EtcdManager::MakeNodePortEtcdKey(const NodeInfo &nodeInfo)
{
	return MakeNodePortEtcdPrefix(nodeInfo) + std::to_string(nodeInfo.endpoint().port());
}

void EtcdManager::RegisterNodeService()
{
	// Two-phase CAS under the same lease:
	//   Phase 1 (this call): claim the global allocation key
	//     `.../allocated/node_type/T/node_id/N`. Zone-independent, so two
	//     zones racing on the same node_id lose exactly one to version!=0
	//     and must try again with a different node_id.
	//   Phase 2 (EtcdService::OnTxnSucceeded when the allocation key lands):
	//     publish the per-zone NodeInfo under the discovery key for watchers.
	//
	// Keeping the two phases strictly sequential (driven by OnTxnSucceeded
	// rather than issuing both writes eagerly here) prevents a stale per-zone
	// info record from lingering in etcd after a lost allocation race —
	// otherwise Java Gateway's GateWatcher would see several phantom entries
	// for the same gate uuid and load-balance against phantom replicas.
	const auto &info = gNode->GetNodeInfo();
	const auto allocKey = MakeNodeAllocationKey(info);
	LOG_INFO << "Claiming global node-id allocation: " << allocKey;
	EtcdHelper::PutIfAbsent(allocKey, info.node_uuid(), 0, gNode->GetLeaseId());
	SetPendingTxnKey(allocKey);
}

void EtcdManager::PublishNodeInfoAfterAllocation()
{
	// Second phase: allocation key CAS already succeeded, so (node_type,
	// node_id) is globally reserved. Now publish NodeInfo for service
	// discovery. Both keys share the same lease, so on shutdown / lease
	// revoke they disappear atomically.
	const auto &info = gNode->GetNodeInfo();
	const auto serviceKey = MakeNodeEtcdKey(info);
	LOG_INFO << "Registering node service to etcd with key: " << serviceKey;
	EtcdHelper::PutIfAbsent(serviceKey, info, gNode->GetLeaseId());
	SetPendingTxnKey(serviceKey);
	LOG_INFO << "Registered node to etcd: " << info.DebugString();
}

void EtcdManager::UpdateNodeInfo()
{
	const auto serviceKey = MakeNodeEtcdKey(gNode->GetNodeInfo());
	EtcdHelper::PutServiceNodeInfo(gNode->GetNodeInfo(), serviceKey, gNode->GetLeaseId());
}

void EtcdManager::RegisterNodePort(bool reRegistering)
{
	const auto portKey = MakeNodePortEtcdKey(gNode->GetNodeInfo());
	LOG_INFO << "Registering node port to etcd with key: " << portKey;

	// 重注册(临时失租后拿了新租约)时**必须**用无条件 Put,不能再 PutIfAbsent。
	//
	// 原因:RequestReRegistration 只在旧租约仍健康时才触发,此刻 portKey 还挂在旧租约上,
	// VERSION==0 的 CAS 必然失败;而 OnTxnFailed 在 kReRegisterExisting 模式下把**任何**
	// key 的失败一律判成"身份被抢",直接 OnNodeIdConflictShutdown —— fence 发号器、
	// 清退玩家、进程退出。也就是说每一次重注册都会确定性地把自己杀掉。
	//
	// 为什么无条件 Put 是安全的:portKey = /service/<ip>/port/<port>,按 **IP+端口** 作用域。
	// 本进程正是绑在这个 IP:端口 上的那个进程,别的节点不可能产生同一个 key
	// (换机器 IP 不同,同机换实例端口不同)。所以"key 还在"只可能是自己旧租约的残留,
	// 重新挂到新租约即可,不存在覆盖别人的可能。
	// 与之相对,node_id / 分配键那两把是**跨进程竞争**的,仍然保持 CAS 语义不变。
	if (reRegistering)
	{
		EtcdHelper::PutWithLease(portKey, "", gNode->GetLeaseId());
	}
	else
	{
		EtcdHelper::PutIfAbsent(portKey, "", 0, gNode->GetLeaseId());
	}
	SetPendingTxnKey(portKey);
	LOG_INFO << "Registered node port to etcd: " << gNode->GetNodeInfo().endpoint().port();
}

void EtcdManager::RequestNodeLease()
{
	uint64_t ttlSeconds = tlsNodeConfigManager.GetBaseDeployConfig().node_ttl_seconds();
	LOG_INFO << "[EtcdLease] Requesting lease with TTL: " << ttlSeconds
			 << " seconds. Time: " << muduo::Timestamp::now().toFormattedString();
	LOG_DEBUG << "[EtcdLease] Calling EtcdHelper::GrantLease...";
	EtcdHelper::GrantLease(ttlSeconds);
	LOG_INFO << "[EtcdLease] Lease request completed.";
}

void EtcdManager::StartLeaseKeepAlive()
{
	leaseKeepAliveTimer.RunEvery(tlsNodeConfigManager.GetBaseDeployConfig().keep_alive_interval(), []()
								 {
		etcdserverpb::LeaseKeepAliveRequest req;
		req.set_id(gNode->GetLeaseId());
		SendLeaseLeaseKeepAlive(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), req);
		LOG_DEBUG << "Keeping node alive, lease_id: " << gNode->GetLeaseId();

		gNode->GetEtcdManager().WriteSnowFlakeGuard(); });
}

// guard key 必须与 node_id 的唯一域一致 —— 也就是 MakeNodeAllocationKey 用的
// 全局 (node_type, node_id),**不带 zone**。旧 key 里带了 zone_id,于是
// "zone 1 的 node_id=3 退出、zone 2 抢到 node_id=3" 这种正常的跨 zone 回收
// 会读到一把不存在的 guard,新持有者直接从 step=0 开始发号,与旧持有者在同一秒
// 发出的号逐位重复。
//
// 注意:guard 值本身仍然写在**分 zone 的 Redis 实例**里,所以跨 zone 时新持有者
// 依然读不到旧值。这条路径由 ActivateSnowFlakeAfterGuard 的"无条件按本机当前秒
// 兜底"覆盖;这里去掉 zone 段是为了同一 zone 内回收时不再漏读,并让 key 语义
// 与唯一域对齐。
std::string EtcdManager::MakeSnowFlakeGuardKey(const NodeInfo &info)
{
	return "snowflake_guard:" + std::to_string(info.node_type()) + ":" + std::to_string(info.node_id());
}

void EtcdManager::WriteSnowFlakeGuard()
{
	auto &redis = tlsRedis.GetZoneRedis();
	if (!redis || !redis->connected())
	{
		return;
	}

	constexpr uint32_t guardTtl = 600; // 10 minutes — long enough for any restart scenario
	const auto &info = gNode->GetNodeInfo();
	std::string key = MakeSnowFlakeGuardKey(info);
	uint64_t nowSeconds = TimeSystem::NowSecondsUTC();

	redis->command([](hiredis::Hiredis *, redisReply *) {},
				   "SETEX %s %u %llu", key.c_str(), guardTtl, static_cast<unsigned long long>(nowSeconds));
}
