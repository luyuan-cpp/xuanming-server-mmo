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
	//
	// 这两个函数与 EtcdService 的超时定时器成对维护,不变量是
	// **有 pending key ⟺ 超时定时器在跑**;响应丢了由定时器重发同一条幂等 CAS 链。
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

	// reRegistering=true 表示"临时失租后拿新租约重新注册":此时端口 key 还挂在旧租约上,
	// 必须无条件 Put 改挂新租约。用 PutIfAbsent 会必然 CAS 失败,而 OnTxnFailed 在重注册
	// 模式下把任何失败都判成"身份被抢"并自杀 —— 那会让每次重注册都确定性地杀掉自己。
	// 端口 key 按 IP+端口作用域,别的节点产生不了同一个 key,所以无条件 Put 不会覆盖别人。
	void RegisterNodePort(bool reRegistering = false);

	void RequestNodeLease();

	void StartLeaseKeepAlive();

	static std::string MakeSnowFlakeGuardKey(const NodeInfo &info);

	void WriteSnowFlakeGuard();

private:
	TimerTaskComp leaseKeepAliveTimer;
	std::string pendingTxnKey_;
};