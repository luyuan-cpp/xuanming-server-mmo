#include "message_limiter/message_limiter.h"
#include <chrono>
#include <thread>
#include <gtest/gtest.h>
#include "table/proto/tip/common_error_tip.pb.h"

TEST(MessageLimiterTest, AllowInitialRequests) {
    MessageLimiter limiter(5, 60);  // allow 5 requests per 60 seconds
    uint32_t messageId = 12345;

    for (int i = 0; i < 5; ++i) {
        EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId)) << "request #" << i + 1 << " should be allowed";
    }

    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuota(messageId)) << "request #6 should be rejected";
}

TEST(MessageLimiterTest, ClearExpiredRequests) {
    MessageLimiter limiter(2, 2);  // allow 2 requests per 2 seconds
    uint32_t messageId = 67890;

    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId));
    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuota(messageId)) << "rate limit exceeded, should be rejected";

    // wait 3 seconds for the rate limit window to expire
    std::this_thread::sleep_for(std::chrono::seconds(3));

    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId)) << "should be allowed after window expires";
}

TEST(MessageLimiterTest, DifferentMessageIDs) {
    MessageLimiter limiter(3, 60);  // allow 3 requests per 60 seconds
    uint32_t messageId1 = 11111;
    uint32_t messageId2 = 22222;

    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId1));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId2));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId1));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId2));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId1));

    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuota(messageId1)) << "messageId1 exceeded limit, should be rejected";
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId2)) << "messageId2 within limit, should be allowed";
}
