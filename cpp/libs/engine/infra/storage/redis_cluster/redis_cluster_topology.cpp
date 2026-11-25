#include "redis_cluster_topology.h"

#include <algorithm>
#include <cstddef>
#include <limits>
#include <string_view>

#include "redis_slot.h"

namespace redis_cluster
{
	namespace
	{
		constexpr uint32_t kNoOwner = std::numeric_limits<uint32_t>::max();

		bool IsInteger(const redisReply* reply)
		{
			return reply != nullptr && reply->type == REDIS_REPLY_INTEGER;
		}

		std::optional<SlotRange> ParseSlotRange(const redisReply* entry, std::string& error)
		{
			if (entry == nullptr || entry->type != REDIS_REPLY_ARRAY || entry->elements < 3)
			{
				error = "slot range entry is not an array of at least 3 elements";
				return std::nullopt;
			}
			const redisReply* first = entry->element[0];
			const redisReply* last = entry->element[1];
			const redisReply* master = entry->element[2];
			if (!IsInteger(first) || !IsInteger(last))
			{
				error = "slot range bounds are not integers";
				return std::nullopt;
			}
			if (first->integer < 0 || last->integer >= redis_slot::kSlotCount || first->integer > last->integer)
			{
				error = "slot range " + std::to_string(first->integer) + "-" + std::to_string(last->integer) + " is invalid";
				return std::nullopt;
			}
			if (master == nullptr || master->type != REDIS_REPLY_ARRAY || master->elements < 2)
			{
				error = "master entry is not an array of at least 2 elements";
				return std::nullopt;
			}
			const redisReply* ip = master->element[0];
			const redisReply* port = master->element[1];
			if (ip == nullptr || ip->type != REDIS_REPLY_STRING || ip->str == nullptr || !IsInteger(port))
			{
				error = "master endpoint is missing or NULL";
				return std::nullopt;
			}
			const std::string_view host(ip->str, ip->len);
			std::optional<NodeEndpoint> endpoint = MakeNodeEndpoint(host, port->integer);
			if (!endpoint)
			{
				error = "master endpoint is not an IP literal with a valid port: " + std::string(host) + ":" + std::to_string(port->integer);
				return std::nullopt;
			}
			SlotRange range;
			range.first = static_cast<uint16_t>(first->integer);
			range.last = static_cast<uint16_t>(last->integer);
			range.master = std::move(*endpoint);
			return range;
		}
	}

	std::optional<ClusterTopology> ParseClusterSlots(const redisReply& reply, std::string& error)
	{
		if (reply.type != REDIS_REPLY_ARRAY)
		{
			error = "CLUSTER SLOTS reply is not an array";
			return std::nullopt;
		}
		if (reply.elements == 0)
		{
			error = "CLUSTER SLOTS reply is empty";
			return std::nullopt;
		}
		ClusterTopology topology;
		topology.ranges.reserve(reply.elements);
		for (std::size_t i = 0; i < reply.elements; ++i)
		{
			std::optional<SlotRange> range = ParseSlotRange(reply.element[i], error);
			if (!range)
			{
				return std::nullopt;
			}
			topology.ranges.push_back(std::move(*range));
		}
		return topology;
	}

	SlotOwnerTable::SlotOwnerTable()
		: owners(redis_slot::kSlotCount, kNoOwner)
	{
	}

	const NodeEndpoint* SlotOwnerTable::Owner(uint16_t slot) const
	{
		if (slot >= redis_slot::kSlotCount)
		{
			return nullptr;
		}
		const uint32_t index = owners[slot];
		return index == kNoOwner ? nullptr : &endpoints[index];
	}

	void SlotOwnerTable::AssignAll(const NodeEndpoint& endpoint)
	{
		endpoints.assign(1, endpoint);
		std::fill(owners.begin(), owners.end(), uint32_t{0});
	}

	void SlotOwnerTable::ApplyMoved(uint16_t slot, const NodeEndpoint& endpoint)
	{
		if (slot >= redis_slot::kSlotCount)
		{
			return;
		}
		owners[slot] = InternEndpoint(endpoint);
	}

	void SlotOwnerTable::ApplyTopology(const ClusterTopology& topology)
	{
		std::fill(owners.begin(), owners.end(), kNoOwner);
		endpoints.clear();
		for (const SlotRange& range : topology.ranges)
		{
			const uint32_t index = InternEndpoint(range.master);
			const uint32_t last = range.last;
			for (uint32_t slot = range.first; slot <= last; ++slot)
			{
				owners[slot] = index;
			}
		}
	}

	bool SlotOwnerTable::IsMaster(const NodeEndpoint& endpoint) const
	{
		return std::find(endpoints.begin(), endpoints.end(), endpoint) != endpoints.end();
	}

	uint32_t SlotOwnerTable::InternEndpoint(const NodeEndpoint& endpoint)
	{
		const auto found = std::find(endpoints.begin(), endpoints.end(), endpoint);
		if (found != endpoints.end())
		{
			return static_cast<uint32_t>(found - endpoints.begin());
		}
		endpoints.push_back(endpoint);
		return static_cast<uint32_t>(endpoints.size() - 1);
	}

	TopologyRefreshPolicy::TopologyRefreshPolicy(Duration period, Duration minInterval, Duration inFlightTimeout)
		: period(period)
		, minInterval(minInterval)
		, inFlightTimeout(inFlightTimeout)
	{
	}

	bool TopologyRefreshPolicy::ShouldStart(TimePoint now) const
	{
		if (inFlight)
		{
			return false;
		}
		if (!lastStart)
		{
			return requested;
		}
		const Duration sinceLast = now - *lastStart;
		if (sinceLast < minInterval)
		{
			return false;
		}
		return requested || sinceLast >= period;
	}

	uint64_t TopologyRefreshPolicy::OnStarted(TimePoint now)
	{
		inFlight = true;
		requested = false;
		lastStart = now;
		return ++generation;
	}

	bool TopologyRefreshPolicy::OnFinished(uint64_t generation, bool success)
	{
		if (!inFlight || generation != this->generation)
		{
			return false;
		}
		inFlight = false;
		if (!success)
		{
			requested = true;
		}
		return true;
	}

	bool TopologyRefreshPolicy::ExpireInFlight(TimePoint now)
	{
		if (!inFlight || !lastStart || now - *lastStart < inFlightTimeout)
		{
			return false;
		}
		inFlight = false;
		requested = true;
		return true;
	}
}
