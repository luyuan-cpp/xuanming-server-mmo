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
		const std::vector<NodeEndpoint>& Masters() const { return endpoints; }

		bool IsMaster(const NodeEndpoint& endpoint) const;

	private:
		uint32_t InternEndpoint(const NodeEndpoint& endpoint);

		std::vector<uint32_t> owners;       // 下标 = slot,值 = endpoints_ 的下标
		std::vector<NodeEndpoint> endpoints;
	};

	// 刷新节拍:Request() 记下「该刷新了」(MOVED、断连、CLUSTERDOWN),周期到了也要刷,但两次开始之间至少隔 minInterval,
	// 同一时刻只有一次在途。在途期间再次 Request() 不丢:那次应答可能早于触发它的事件,结束后还要再刷一次。
	// 在途超过 inFlightTimeout 作废(断线重连 R19):应答可能一直不来(对端挂住、请求排在卡住的连接上),不设上限的话周期刷新与
	// 按需刷新都被挡住,只剩 MOVED 逐槽更新;那条连接本身由 RedisManager 的存活探测另行处置。每次开始带一个代次,迟到的应答按代次丢弃。
	class TopologyRefreshPolicy
	{
	public:
		TopologyRefreshPolicy(Duration period, Duration minInterval, Duration inFlightTimeout);

		void Request() { requested = true; }

		bool ShouldStart(TimePoint now) const;

		// 返回这次刷新的代次,应答回来时原样交回 OnFinished。
		uint64_t OnStarted(TimePoint now);

		// 失败(空应答、错误应答、解析失败)时保留请求,限速之后再试。generation 不是当前在途的那次(已按超时作废)时
		// 什么都不改、返回 false,调用方丢掉这份应答:之后可能已经开过新的一次,迟到的旧视图不能盖新视图。
		bool OnFinished(uint64_t generation, bool success);

		// 在途已超过 inFlightTimeout:作废这次在途、保留请求,返回 true(调用方记一次失败)。
		bool ExpireInFlight(TimePoint now);

	private:
		Duration period;
		Duration minInterval;
		Duration inFlightTimeout;
		std::optional<TimePoint> lastStart;
		uint64_t generation = 0;
		bool requested = false;
		bool inFlight = false;
	};
}
