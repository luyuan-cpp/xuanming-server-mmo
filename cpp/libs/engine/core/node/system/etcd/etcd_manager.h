#pragma once
#include <string>
#include "time/comp/timer_task_comp.h"
#include "node/system/etcd/etcd_transport.h"

class NodeInfo;

class EtcdManager
{
public:
	void Shutdown();

	std::string GetServiceName(uint32_t type);

	std::string MakeNodeEtcdPrefix(const NodeInfo &info);

	std::string MakeNodeEtcdKey(const NodeInfo &info);

	// Global-uniqueness allocation key for (node_type, node_id). Intentionally
	// zone-independent: two zones must not both claim the same (node_type,
	// node_id), it is what makes the routing node_id unique. Since 2026-09-08
	// this key is routing-only: permanent guids (item / tx / snapshot) come from
	// id segments (GuidSegmentRegistry) and no longer encode a node_id at all.
	// See RegisterNodeService for the CAS protocol.
	std::string MakeNodeAllocationPrefix(uint32_t nodeType);
	std::string MakeNodeAllocationKey(const NodeInfo &info);

	std::string MakeNodePortEtcdPrefix(const NodeInfo &nodeInfo);

	std::string MakeNodePortEtcdKey(const NodeInfo &nodeInfo);

	// reRegistering=true(临时失租后拿新租约重注册):分配键可能还挂在旧租约上,用
	// "不存在或已是我的"CAS(EtcdHelper::PutIfAbsentOrOwned)改挂新租约。初始引导仍是
	// 严格的 VERSION==0 CAS。
	void RegisterNodeService(bool reRegistering, bool retryUnknown,
	                         EtcdRegistrationCompletion<etcdserverpb::TxnResponse> done);

	// Second phase of node registration. Must only be called after the
	// allocation key CAS (phase 1 of RegisterNodeService) succeeds —
	// EtcdService 的注册任务负责保证顺序。
	// reRegistering=true 时无条件 Put(理由见实现)。
	void PublishNodeInfoAfterAllocation(bool reRegistering, bool retryUnknown,
	                                   EtcdRegistrationCompletion<etcdserverpb::TxnResponse> done);

	void UpdateNodeInfo();

	// reRegistering=true 表示"临时失租后拿新租约重新注册":此时端口 key 还挂在旧租约上,
	// 必须无条件 Put 改挂新租约。用 PutIfAbsent 会必然 CAS 失败,而 OnTxnFailed 在重注册
	// 模式下把任何失败都判成"身份被抢"并自杀 —— 那会让每次重注册都确定性地杀掉自己。
	// 端口 key 按 IP+端口作用域,别的节点产生不了同一个 key,所以无条件 Put 不会覆盖别人。
	void RegisterNodePort(bool reRegistering, bool retryUnknown,
	                      EtcdRegistrationCompletion<etcdserverpb::TxnResponse> done);

	void RequestNodeLease(EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse> done);

	// 只续节点 lease。旧的 WriteSnowFlakeGuard / MakeSnowFlakeGuardKey(每拍往分 zone Redis
	// 写 snowflake 水位)已删:永久 guid 改走号段,不再有需要跨重启保护的发号水位。
	void StartLeaseKeepAlive();

private:
	TimerTaskComp leaseKeepAliveTimer;
};
