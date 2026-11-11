#include "etcd_manager.h"
#include <string_view>
#include <utility>
#include <vector>
#include "proto/common/base/common.pb.h"
#include "node/system/node/node_allocator.h"
#include "node/system/node/node_util.h"
#include <muduo/base/Logging.h>
#include "etcd_helper.h"
#include "grpc_client/etcd/etcd_grpc_client.h"
#include "thread_context/node_context_manager.h"
#include <node_config_manager.h>
#include <node/system/node/node.h>

void EtcdManager::Shutdown()
{
	leaseKeepAliveTimer.Cancel();
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

std::string EtcdManager::MakeNodeAllocationPrefix(uint32_t nodeType)
{
	// Zone-independent: the key contains node_type and node_id only. Two
	// zones issuing concurrent PutIfAbsent on the same (node_type, node_id)
	// will see exactly one CAS succeed; the loser has to pick a different
	// node_id. This matches the Go-side allocator layout so mixed-language
	// deployments share a single source of truth for node_id uniqueness.
	return GetServiceName(nodeType) +
		   "/allocated/node_type/" + std::to_string(nodeType) +
		   "/node_id/";
}

std::string EtcdManager::MakeNodeAllocationKey(const NodeInfo &info)
{
	return MakeNodeAllocationPrefix(info.node_type()) + std::to_string(info.node_id());
}

std::string EtcdManager::MakeNodePortEtcdPrefix(const NodeInfo &nodeInfo)
{
	return "/service/" + nodeInfo.endpoint().ip() + "/port/";
}

std::string EtcdManager::MakeNodePortEtcdKey(const NodeInfo &nodeInfo)
{
	return MakeNodePortEtcdPrefix(nodeInfo) + std::to_string(nodeInfo.endpoint().port());
}

void EtcdManager::RegisterNodeService(bool reRegistering, bool retryUnknown, EtcdHelper::TxnCompletion done)
{
	// Two-phase CAS under the same lease:
	//   Phase 1 (this call): claim the global allocation key
	//     `.../allocated/node_type/T/node_id/N`. Zone-independent, so two
	//     zones racing on the same node_id lose exactly one to version!=0
	//     and must try again with a different node_id.
	//   Phase 2 (EtcdService::RegisterAsync when the allocation key lands):
	//     publish the per-zone NodeInfo under the discovery key for watchers.
	//
	// Keeping the two phases strictly sequential (driven by RegisterAsync
	// rather than issuing both writes eagerly here) prevents a stale per-zone
	// info record from lingering in etcd after a lost allocation race —
	// otherwise Java Gateway's GateWatcher would see several phantom entries
	// for the same gate uuid and load-balance against phantom replicas.
	const auto &info = gNode->GetNodeInfo();
	const auto allocKey = MakeNodeAllocationKey(info);
	if (reRegistering)
	{
		// 分配键的 value 就是本节点 uuid:"不存在,或者 value 是我的 uuid"才改挂新租约。
		// 旧租约仍活着时 key 还在(VERSION!=0),以前的 VERSION==0 CAS 在这里必败,
		// 而重注册模式下的 OnTxnFailed 把它判成"身份被抢"并自杀 —— 每次重注册都会
		// 确定性地杀掉自己。只有 value 是**别人的** uuid 才是真的被抢。
		LOG_INFO << "Re-claiming global node-id allocation (absent-or-owned): " << allocKey;
		EtcdHelper::PutIfAbsentOrOwned(allocKey, info.node_uuid(), info.node_uuid(), gNode->GetLeaseId(), std::move(done));
	}
	else if (retryUnknown)
	{
		// 上一次 CAS 结局未知:只复核同一个键是不是本租约写下的,不换号。
		LOG_INFO << "Re-checking global node-id allocation after an unknown outcome: " << allocKey;
		EtcdHelper::PutIfAbsent(allocKey, info.node_uuid(), 0, gNode->GetLeaseId(), std::move(done), retryUnknown);
	}
	else
	{
		// 冲突时同一个 txn 读回全服这一类型已分配的号(只取键),下一次选号直接跳过它们。选号看的发现快照只是
		// 本节点看得到的同类(zone-scoped 类型只有本 zone,battle 只有自己),没有这份清单就会逐个去撞别的 zone 的号,
		// 每撞一次等 1 秒(docs/design/connection-fanout-subsetting-20261106.md §2、§3.4)。
		LOG_INFO << "Claiming global node-id allocation: " << allocKey;
		auto allocationPrefix = MakeNodeAllocationPrefix(info.node_type());
		EtcdHelper::PutIfAbsentListingOnConflict(
			allocKey, info.node_uuid(), gNode->GetLeaseId(), allocationPrefix,
			[allocationPrefix, done = std::move(done)](std::optional<etcdserverpb::TxnResponse> reply)
			{
				if (reply)
				{
					if (const auto *listing = EtcdHelper::TxnConflictListing(*reply))
					{
						std::vector<std::string_view> keys;
						keys.reserve(static_cast<std::size_t>(listing->kvs_size()));
						for (const auto &kv : listing->kvs())
						{
							keys.push_back(kv.key());
						}
						NodeAllocator::RecordAllocatedIds(allocationPrefix, keys);
					}
				}
				done(std::move(reply));
			});
	}
}

void EtcdManager::PublishNodeInfoAfterAllocation(bool reRegistering, bool retryUnknown, EtcdHelper::TxnCompletion done)
{
	// Second phase: allocation key CAS already succeeded, so (node_type,
	// node_id) is globally reserved. Now publish NodeInfo for service
	// discovery. Both keys share the same lease, so on shutdown / lease
	// revoke they disappear atomically.
	const auto &info = gNode->GetNodeInfo();
	const auto serviceKey = MakeNodeEtcdKey(info);
	LOG_INFO << "Registering node service to etcd with key: " << serviceKey;
	if (reRegistering)
	{
		// 重注册时无条件改挂新租约。服务键的 value 是 NodeInfo JSON(player_count 会变),
		// 没法用 Value==... 判"是不是我的";但它的身份由刚刚 CAS 成功的分配键定义 ——
		// 同 zone 里持有同一 (node_type, node_id) 的只可能是分配键的持有者,也就是我们。
		// 与端口 key 的重注册同一理由(RegisterNodePort)。
		EtcdHelper::PutWithLease(serviceKey, info, gNode->GetLeaseId(), std::move(done));
	}
	else
	{
		EtcdHelper::PutIfAbsent(serviceKey, info, gNode->GetLeaseId(), std::move(done), retryUnknown);
	}
}

void EtcdManager::UpdateNodeInfo()
{
	const auto serviceKey = MakeNodeEtcdKey(gNode->GetNodeInfo());
	EtcdHelper::PutServiceNodeInfo(gNode->GetNodeInfo(), serviceKey, gNode->GetLeaseId());
}

void EtcdManager::RegisterNodePort(bool reRegistering, bool retryUnknown, EtcdHelper::TxnCompletion done)
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
		EtcdHelper::PutWithLease(portKey, "", gNode->GetLeaseId(), std::move(done));
	}
	else
	{
		EtcdHelper::PutIfAbsent(portKey, "", 0, gNode->GetLeaseId(), std::move(done), retryUnknown);
	}
}

void EtcdManager::RequestNodeLease(EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse> done)
{
	uint64_t ttlSeconds = tlsNodeConfigManager.GetBaseDeployConfig().node_ttl_seconds();
	LOG_INFO << "[EtcdLease] Requesting lease with TTL: " << ttlSeconds
			 << " seconds. Time: " << muduo::Timestamp::now().toFormattedString();
	LOG_DEBUG << "[EtcdLease] Calling EtcdHelper::GrantLease...";
	EtcdHelper::GrantLease(ttlSeconds, std::move(done));
}

void EtcdManager::StartLeaseKeepAlive()
{
	leaseKeepAliveTimer.RunEvery(tlsNodeConfigManager.GetBaseDeployConfig().keep_alive_interval(), []()
								 {
		// 与其他 etcd 请求一样只经 EtcdHelper 发出(传输接缝,docs/design/grpc-muduo-transport.md §13.4)。
		EtcdHelper::KeepAliveLease(gNode->GetLeaseId());
		LOG_DEBUG << "Keeping node alive, lease_id: " << gNode->GetLeaseId(); });
}
