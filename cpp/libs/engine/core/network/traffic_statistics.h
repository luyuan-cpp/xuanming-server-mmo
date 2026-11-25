#pragma once

#include <atomic>
#include <array>
#include <chrono>
#include <vector>

#include "rpc/service_metadata/rpc_event_registry.h"

namespace muduo { namespace net { class EventLoop; } }

// Per-message-id traffic counters. Cache-line aligned to avoid false sharing.
struct alignas(64) MessageTrafficCounters
{
    std::atomic<uint64_t> sendCount{0};
    std::atomic<uint64_t> recvCount{0};
    std::atomic<uint64_t> sendBytes{0};
    std::atomic<uint64_t> recvBytes{0};
    std::atomic<uint32_t> maxMessageSize{0};

    void Reset()
    {
        sendCount.store(0, std::memory_order_relaxed);
        recvCount.store(0, std::memory_order_relaxed);
        sendBytes.store(0, std::memory_order_relaxed);
        recvBytes.store(0, std::memory_order_relaxed);
        maxMessageSize.store(0, std::memory_order_relaxed);
    }
};

// Snapshot of one window period for a single message_id.
struct MessageTrafficSnapshot
{
    uint32_t messageId = 0;
    uint64_t sendCount = 0;
    uint64_t recvCount = 0;
    uint64_t sendBytes = 0;
    uint64_t recvBytes = 0;
    uint32_t maxMessageSize = 0;

    uint64_t TotalBytes() const { return sendBytes + recvBytes; }
    uint64_t TotalCount() const { return sendCount + recvCount; }
};

class TrafficStatsCollector
{
public:
    // Record an outgoing message.
    void RecordSend(uint32_t messageId, uint32_t byteSize);

    // Record an incoming message.
    void RecordRecv(uint32_t messageId, uint32_t byteSize);

    // Enable stats collection. Optionally auto-disable after `autoDisableMinutes` (0 = never).
    void Enable(uint32_t autoDisableMinutes = 0);

    // Disable stats collection and log final summary.
    void Disable();

    bool IsEnabled() const { return enabled.load(std::memory_order_relaxed); }

    // Called periodically (e.g., from a timer). Snapshots counters, resets them,
    // and logs a summary of the window.
    void ReportAndReset();

    // Get the report interval in seconds (default 30, configurable via env).
    uint32_t GetReportIntervalSeconds() const { return reportIntervalSeconds; }

    static TrafficStatsCollector &Instance();

private:
    TrafficStatsCollector();

    void UpdateMaxSize(std::atomic<uint32_t> &current, uint32_t newSize);
    void LogWindowSummary(double windowSeconds, const std::vector<MessageTrafficSnapshot> &snapshots);

    std::array<MessageTrafficCounters, kMaxRpcMethodCount> counters{};
    std::atomic<bool> enabled{false};
    std::chrono::steady_clock::time_point windowStart{};
    std::chrono::steady_clock::time_point autoDisableTime{};
    uint32_t reportIntervalSeconds = 30;
};

// Register the periodic traffic stats reporter on the event loop.
// If NODE_TRAFFIC_STATS_ENABLED=1 env is set, starts immediately.
// The reporter timer fires every `reportIntervalSeconds` and logs a summary.
void RegisterTrafficStatsReporter(muduo::net::EventLoop& loop);
