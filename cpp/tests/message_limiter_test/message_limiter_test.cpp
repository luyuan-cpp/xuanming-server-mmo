#include "message_limiter/message_limiter.h"

#include <gtest/gtest.h>
#include "table/proto/tip/common_error_tip.pb.h"

TEST(MessageLimiterTest, AllowInitialRequestsAndRejectOverflow) {
    MessageLimiter limiter(5, 60);  // allow 5 requests per 60 seconds
    constexpr uint32_t messageId = 12345;

    for (int i = 0; i < 5; ++i) {
        EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId, 1000 + static_cast<uint64_t>(i) * 100))
            << "request #" << i + 1 << " should be allowed";
    }

    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuotaAt(messageId, 1500))
        << "request #6 should be rejected";
}

TEST(MessageLimiterTest, ExactMillisecondSlidingWindowBoundary) {
    MessageLimiter limiter(2, 1);  // allow 2 requests per 1000 ms
    constexpr uint32_t messageId = 67890;

    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId, 1000));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId, 1400));
    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuotaAt(messageId, 1999))
        << "999ms after first request must still be rejected";

    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId, 2000))
        << "at exact 1000ms boundary first request expires and quota opens";
    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuotaAt(messageId, 2399))
        << "second request at 1400ms has not expired yet";
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId, 2400))
        << "at 2400ms second request expires";
}

TEST(MessageLimiterTest, DifferentMessageIDsAreIndependent) {
    MessageLimiter limiter(3, 60);
    constexpr uint32_t messageId1 = 11111;
    constexpr uint32_t messageId2 = 22222;

    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId1, 100));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId2, 100));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId1, 200));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId2, 200));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId1, 300));

    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuotaAt(messageId1, 400));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId2, 400));
}

TEST(MessageLimiterTest, ZeroMaxRequestsMeansUnlimited) {
    MessageLimiter limiter(0, 1);
    constexpr uint32_t messageId = 33333;

    for (int i = 0; i < 20; ++i) {
        EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId, 1000));
    }
}

TEST(MessageLimiterTest, BackwardClockDoesNotEvictOrUnderflow) {
    MessageLimiter limiter(2, 1);
    constexpr uint32_t messageId = 44444;

    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId, 5000));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuotaAt(messageId, 5100));
    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuotaAt(messageId, 4900))
        << "backward timestamp must not underflow and clear window";
}

TEST(MessageLimiterTest, SteadyClockDefaultEntryPointWorks) {
    MessageLimiter limiter(2, 60);
    constexpr uint32_t messageId = 55555;

    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId));
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId));
    EXPECT_EQ(kRateLimitExceeded, limiter.TryConsumeRequestQuota(messageId));
    limiter.Reset();
    EXPECT_EQ(kSuccess, limiter.TryConsumeRequestQuota(messageId));
}
