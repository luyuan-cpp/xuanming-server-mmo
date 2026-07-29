#pragma once
#include <string>
#include "time/comp/timer_task_comp.h"

class NodeInfo;

class EtcdManager
{
public:
	// 注册流程里"当前正在等响应的那一个 CAS key"。
	//
	// 这里是**单槽**而不是队列,因为注册流严格串行:port CAS -> alloc CAS -> info CAS,
	// 每一步都由上一步的响应驱动。原来用 std::deque 按 FIFO 弹出来猜"这个 txn 响应对应
	// 哪个 key",但生成的 grpc client 在 `!status.ok()` 时**根本不会调 handler** ——
	// 一次 etcd RPC 失败队列就永久错位,之后端口 key 的成功会被解释成 alloc key 成功,
	// 于是节点没占住 node_id 就激活了发号器,直接撞号。
	// 单槽让"响应对不上"变成可检测、可恢复的状态,而不是静默错位。
	const std::string &PendingTxnKey() const { return pendingTxnKey_; }
	void SetPendingTxnKey(const std::string &key);
	std::string TakePendingTxnKey();

	void Shutdown();

	std::string GetServiceName(uint32_t type);

	std::string MakeNodeEtcdPrefix(const NodeInfo &info);

	std::string MakeNodeEtcdKey(const NodeInfo &info);

	// Global-uniqueness allocation key for (node_type, node_id). Intentionally
	// zone-independent: two zones must not both claim the same (node_type,
	// node_id) or their Snowflake PlayerId/GuidId streams collide. See
	// RegisterNodeService for the CAS protocol that honours this key.
	std::string MakeNodeAllocationKey(const NodeInfo &info);

	std::string MakeNodePortEtcdPrefix(const NodeInfo &nodeInfo);

	std::string MakeNodePortEtcdKey(const NodeInfo &nodeInfo);

	void RegisterNodeService();

	// Second phase of node registration. Must only be called after the
	// allocation key CAS (phase 1 of RegisterNodeService) succeeds —
	// EtcdService::OnTxnSucceeded is responsible for that sequencing.
	void PublishNodeInfoAfterAllocation();

	void UpdateNodeInfo();

	void RegisterNodePort();

	void RequestNodeLease();

	void StartLeaseKeepAlive();

	static std::string MakeSnowFlakeGuardKey(const NodeInfo &info);

	void WriteSnowFlakeGuard();

private:
	TimerTaskComp leaseKeepAliveTimer;
	std::string pendingTxnKey_;
};