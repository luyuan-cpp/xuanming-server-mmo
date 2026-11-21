#pragma once

#include <cstdint>
#include <deque>
#include <unordered_map>

// Per-session, per-message-id monotonic sliding window rate limiter.
// Shared by Gate (GateClientSessionHandler) and Battle direct edge (BattleClientEdge).
class MessageLimiter {
public:
    MessageLimiter() = default;

    MessageLimiter(uint32_t defaultMaxRequests, uint32_t defaultTimeWindowSeconds);

    // Consumes quota using the process monotonic steady clock (milliseconds).
    // Returns kSuccess when allowed or kRateLimitExceeded (1008) when rejected.
    uint32_t TryConsumeRequestQuota(uint32_t messageId);

    // Deterministic entry point for tests and callers providing a monotonic millisecond timestamp.
    uint32_t TryConsumeRequestQuotaAt(uint32_t messageId, uint64_t nowMs);

    void Reset() { requestRecords.clear(); }

private:
    static uint64_t NowSteadyMs();

    uint32_t defaultMaxRequests{3};       // max requests per window; 0 = unlimited
    uint32_t defaultTimeWindowSeconds{1}; // window duration in seconds
    std::unordered_map<uint32_t, std::deque<uint64_t>> requestRecords;
};
