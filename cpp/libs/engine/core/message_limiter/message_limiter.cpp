#include "message_limiter.h"

#include <chrono>

#include "macros/error_return.h"
#include "table/code/messagelimiter_table.h"
#include "table/proto/tip/common_error_tip.pb.h"

MessageLimiter::MessageLimiter(uint32_t defaultMaxRequests, uint32_t defaultTimeWindowSeconds)
    : defaultMaxRequests(defaultMaxRequests),
      defaultTimeWindowSeconds(defaultTimeWindowSeconds) {}

uint64_t MessageLimiter::NowSteadyMs()
{
    const auto now = std::chrono::steady_clock::now().time_since_epoch();
    return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(now).count());
}

uint32_t MessageLimiter::TryConsumeRequestQuota(uint32_t messageId)
{
    return TryConsumeRequestQuotaAt(messageId, NowSteadyMs());
}

uint32_t MessageLimiter::TryConsumeRequestQuotaAt(uint32_t messageId, uint64_t nowMs)
{
    uint32_t maxAllowedRequests = defaultMaxRequests;
    uint64_t windowSeconds = defaultTimeWindowSeconds;

    if (const auto [configEntry, _] = MessageLimiterTableManager::Instance().FindByIdSilent(messageId);
        configEntry != nullptr)
    {
        maxAllowedRequests = configEntry->max_requests();
        windowSeconds = configEntry->time_window() > 0 ? configEntry->time_window() : defaultTimeWindowSeconds;
    }

    // max_requests == 0 means explicitly unlimited (aligned with Java MessageLimit.unlimited()).
    if (maxAllowedRequests == 0)
    {
        return kSuccess;
    }

    const uint64_t windowMs = windowSeconds * 1000ULL;
    auto &messageTimestamps = requestRecords[messageId];

    // Exact monotonic millisecond sliding window: evict once elapsed >= windowMs.
    while (!messageTimestamps.empty()
           && nowMs >= messageTimestamps.front()
           && (nowMs - messageTimestamps.front()) >= windowMs)
    {
        messageTimestamps.pop_front();
    }

    if (messageTimestamps.size() < maxAllowedRequests)
    {
        messageTimestamps.push_back(nowMs);
        return kSuccess;
    }

    return MAKE_ERROR_MSG(kRateLimitExceeded, "messageId=" << messageId);
}
