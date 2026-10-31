#pragma once

// 集群拓扑(docs/design/redis-player-store-cluster.md §5.6),纯逻辑:解析 CLUSTER SLOTS、维护 slot → 主节点表、
// 决定什么时候该刷新。不碰网络,时间由调用方传入。单线程使用。

#include <hiredis/hiredis.h>

#include <cstdint>
#include <optional>
#include <string>
#include <vector>

#include "redis_node_endpoint.h"
#include "redis_slot_queues.h"

namespace redis_cluster
{
	struct SlotRange
	{
		uint16_t first = 0;
		uint16_t last = 0;
		NodeEndpoint master;
	};

	struct ClusterTopology
	{
		std::vector<SlotRange> ranges;
	};

	// 解析 CLUSTER SLOTS 的应答:[[first, last, [ip, port, ...], replica...], ...],副本不看(不读副本)。
	// 任何一段不合法(含端点不是 IP 字面量、端点为 NULL)都判整个应答无效,不部分生效;空数组同样无效
	// (节点还没加入集群时就是这样,套用会清空路由表)。失败时 error 写明原因。
	std::optional<ClusterTopology> ParseClusterSlots(const redisReply& reply, std::string& error);

	class SlotOwnerTable
	{
	public:
		SlotOwnerTable();

		// slot 当前的主节点;没有主(未覆盖,或拓扑还没到)时为空。指针只在下一次修改本表之前有效。
		const NodeEndpoint* Owner(uint16_t slot) const;

		// standalone:全部 slot 归一个节点。
		void AssignAll(const NodeEndpoint& endpoint);

		// 收到 MOVED:只改这一个 slot。全量视图等拓扑刷新。
		void ApplyMoved(uint16_t slot, const NodeEndpoint& endpoint);

		// 用整份拓扑替换。
		void ApplyTopology(const ClusterTopology& topology);

		// 当前表里出现过的全部主节点,去重。
		const std::vector<NodeEndpoint>& Masters() const { return endpoints_; }

		bool IsMaster(const NodeEndpoint& endpoint) const;

	private:
		uint32_t InternEndpoint(const NodeEndpoint& endpoint);

		std::vector<uint32_t> owners_;       // 下标 = slot,值 = endpoints_ 的下标
		std::vector<NodeEndpoint> endpoints_;
	};

	// 刷新节拍:Request() 记下「该刷新了」(MOVED、断连、CLUSTERDOWN),周期到了也要刷,但两次开始之间至少隔 minInterval,
	// 同一时刻只有一次在途。在途期间再次 Request() 不丢:那次应答可能早于触发它的事件,结束后还要再刷一次。
	class TopologyRefreshPolicy
	{
	public:
		TopologyRefreshPolicy(Duration period, Duration minInterval);

		void Request() { requested_ = true; }

		bool ShouldStart(TimePoint now) const;

		void OnStarted(TimePoint now);

		// 失败(空应答、错误应答、解析失败)时保留请求,限速之后再试。
		void OnFinished(bool success);

	private:
		Duration period_;
		Duration minInterval_;
		std::optional<TimePoint> lastStart_;
		bool requested_ = false;
		bool inFlight_ = false;
	};

	// 重连间隔 = 基础间隔 + [0, jitterSec) 的抖动,避免主切换时全体客户端同时回连。
	// unitRandom 是调用方给的 [0, 1) 随机数,超出范围时收进这个区间。
	double JitteredReconnectIntervalSec(double baseSec, double jitterSec, double unitRandom);
}
