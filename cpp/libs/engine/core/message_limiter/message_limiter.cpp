#include "message_limiter.h"

#include "table/proto/tip/common_error_tip.pb.h"
#include "macros/error_return.h"
#include "table/code/messagelimiter_table.h"
#include "time/system/time.h"

MessageLimiter::MessageLimiter(uint8_t defaultMaxRequests, uint32_t defaultTimeWindow)
    : defaultMaxRequests(defaultMaxRequests), defaultTimeWindow(defaultTimeWindow) {}

uint32_t MessageLimiter::TryConsumeRequestQuota(uint32_t messageId)
{
    const auto currentTime = TimeSystem::NowSeconds();

    auto &messageTimestamps = requestRecords[messageId];

    uint8_t maxAllowedRequests = defaultMaxRequests;
    uint64_t timeWindowDuration = defaultTimeWindow;

    // Override from per-message config if present
    if (const auto [configEntry, _] = MessageLimiterTableManager::Instance().FindByIdSilent(messageId);
        configEntry != nullptr)
    {
        maxAllowedRequests = configEntry->max_requests();
        timeWindowDuration = configEntry->time_window();
    }

    if (messageTimestamps.empty())
    {
        messageTimestamps.resize(maxAllowedRequests);
    }

    // Evict expired timestamps
    while (!messageTimestamps.empty() && currentTime - messageTimestamps.front() > timeWindowDuration)
    {
        messageTimestamps.pop_front();
    }

    if (messageTimestamps.size() < maxAllowedRequests)
    {
        messageTimestamps.push_back(currentTime);
        return kSuccess;
    }
    else
    {
        return MAKE_ERROR_MSG(kRateLimitExceeded,
                              "messageId=" << messageId);
    }
}
