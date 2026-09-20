#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>

#include "messaging/kafka/kafka_producer_policy.h"

// ---------------------------------------------------------------------------
// kafka_producer_policy_test — KafkaProducer 的纯决策逻辑。
//
// 这里不连 broker、不创建 librdkafka 句柄:被测的头文件只依赖 RdKafka::ErrorCode 这个枚举,
// 时间点全部由用例传入。要盯住的三件事:
//   1) produce() 的返回码 → 处置动作的映射。映射错了的表现都很隐蔽:把 ERR__FATAL 当成普通
//      错误,节点从此一条 Kafka 消息都发不出去却不会崩;把普通错误当成 fatal,则每次发送都在
//      销毁、重建一整套 librdkafka 线程。
//   2) 重建的最小间隔:fatal 的成因未消除时不能形成重建风暴。
//   3) 失败日志限流:broker 长时间不可用时成千上万条消息同刻超时,日志不能被打满,
//      同时被压掉的条数必须在下一个窗口补报出来(不能悄悄少报)。
//
// main() 在 kafka_command_test.cpp 里,本文件不再定义。
// ---------------------------------------------------------------------------

namespace {

using kafka_producer_policy::ClassifyProduceResult;
using kafka_producer_policy::FailureLogThrottle;
using kafka_producer_policy::IsFatalDeliveryError;
using kafka_producer_policy::IsPurgeDeliveryError;
using kafka_producer_policy::ProduceAction;
using kafka_producer_policy::RebuildGate;

using Clock = std::chrono::steady_clock;

// 用例里的「现在」:一个固定起点加偏移,不读墙钟。
Clock::time_point At(std::chrono::milliseconds offset)
{
    return Clock::time_point{} + std::chrono::hours(1) + offset;
}

} // namespace

TEST(ClassifyProduceResult, NoErrorIsAccepted)
{
    EXPECT_EQ(ProduceAction::kAccepted, ClassifyProduceResult(RdKafka::ERR_NO_ERROR));
}

TEST(ClassifyProduceResult, QueueFullAsksForOneRetryAfterPoll)
{
    EXPECT_EQ(ProduceAction::kRetryAfterPoll, ClassifyProduceResult(RdKafka::ERR__QUEUE_FULL));
}

TEST(ClassifyProduceResult, FatalAsksForRebuild)
{
    EXPECT_EQ(ProduceAction::kRebuildProducer, ClassifyProduceResult(RdKafka::ERR__FATAL));
}

TEST(ClassifyProduceResult, OtherErrorsAreRejectedWithoutRetry)
{
    // 重试没有意义的错误:消息过大、未知 topic / 分区、句柄状态不对。
    EXPECT_EQ(ProduceAction::kReject, ClassifyProduceResult(RdKafka::ERR_MSG_SIZE_TOO_LARGE));
    EXPECT_EQ(ProduceAction::kReject, ClassifyProduceResult(RdKafka::ERR__UNKNOWN_TOPIC));
    EXPECT_EQ(ProduceAction::kReject, ClassifyProduceResult(RdKafka::ERR__UNKNOWN_PARTITION));
    EXPECT_EQ(ProduceAction::kReject, ClassifyProduceResult(RdKafka::ERR__STATE));
    // 超时类错误出现在投递回执里,不会由 produce() 同步返回;万一返回了也不能被当成 fatal。
    EXPECT_EQ(ProduceAction::kReject, ClassifyProduceResult(RdKafka::ERR__MSG_TIMED_OUT));
}

TEST(DeliveryErrorClassification, OnlyFatalInvalidatesTheProducerInstance)
{
    EXPECT_TRUE(IsFatalDeliveryError(RdKafka::ERR__FATAL));

    // 「这一条没送到」不等于「实例作废」:超时与 purge 都不能触发重建。
    EXPECT_FALSE(IsFatalDeliveryError(RdKafka::ERR__MSG_TIMED_OUT));
    EXPECT_FALSE(IsFatalDeliveryError(RdKafka::ERR__PURGE_QUEUE));
    EXPECT_FALSE(IsFatalDeliveryError(RdKafka::ERR__PURGE_INFLIGHT));
    EXPECT_FALSE(IsFatalDeliveryError(RdKafka::ERR_NO_ERROR));
}

TEST(DeliveryErrorClassification, PurgeErrorsAreRecognised)
{
    EXPECT_TRUE(IsPurgeDeliveryError(RdKafka::ERR__PURGE_QUEUE));
    EXPECT_TRUE(IsPurgeDeliveryError(RdKafka::ERR__PURGE_INFLIGHT));
    EXPECT_FALSE(IsPurgeDeliveryError(RdKafka::ERR__FATAL));
    EXPECT_FALSE(IsPurgeDeliveryError(RdKafka::ERR__MSG_TIMED_OUT));
}

TEST(RebuildGate, FirstRebuildIsAlwaysAllowed)
{
    RebuildGate gate(std::chrono::seconds(5));
    EXPECT_TRUE(gate.TryAcquire(At(std::chrono::milliseconds(0))));
}

TEST(RebuildGate, RefusesWithinTheMinimumIntervalAndAllowsAtTheBoundary)
{
    RebuildGate gate(std::chrono::seconds(5));
    ASSERT_TRUE(gate.TryAcquire(At(std::chrono::milliseconds(0))));

    EXPECT_FALSE(gate.TryAcquire(At(std::chrono::milliseconds(1))));
    EXPECT_FALSE(gate.TryAcquire(At(std::chrono::milliseconds(4999))));
    EXPECT_TRUE(gate.TryAcquire(At(std::chrono::milliseconds(5000))));
}

TEST(RebuildGate, ARefusedAttemptDoesNotPushTheWindowForward)
{
    // 被拒的尝试不能刷新计时起点,否则持续的 send() 会让重建永远排不上。
    RebuildGate gate(std::chrono::seconds(5));
    ASSERT_TRUE(gate.TryAcquire(At(std::chrono::milliseconds(0))));
    ASSERT_FALSE(gate.TryAcquire(At(std::chrono::milliseconds(3000))));
    ASSERT_FALSE(gate.TryAcquire(At(std::chrono::milliseconds(4000))));

    EXPECT_TRUE(gate.TryAcquire(At(std::chrono::milliseconds(5000))));
}

TEST(RebuildGate, EachSuccessfulRebuildStartsANewInterval)
{
    RebuildGate gate(std::chrono::seconds(5));
    ASSERT_TRUE(gate.TryAcquire(At(std::chrono::milliseconds(0))));
    ASSERT_TRUE(gate.TryAcquire(At(std::chrono::milliseconds(7000))));

    EXPECT_FALSE(gate.TryAcquire(At(std::chrono::milliseconds(11999))));
    EXPECT_TRUE(gate.TryAcquire(At(std::chrono::milliseconds(12000))));
}

TEST(FailureLogThrottle, LogsTheFirstBurstThenSuppressesWithinTheWindow)
{
    FailureLogThrottle throttle(3, std::chrono::seconds(10));

    for (int i = 0; i < 3; ++i)
    {
        const auto decision = throttle.OnFailure(At(std::chrono::milliseconds(i)));
        EXPECT_TRUE(decision.logThisOne) << "failure #" << i;
        EXPECT_EQ(0u, decision.suppressedBeforeThis);
    }
    for (int i = 3; i < 10; ++i)
    {
        const auto decision = throttle.OnFailure(At(std::chrono::milliseconds(i)));
        EXPECT_FALSE(decision.logThisOne) << "failure #" << i;
        EXPECT_EQ(0u, decision.suppressedBeforeThis);
    }
    EXPECT_EQ(7u, throttle.PendingSuppressed());
}

TEST(FailureLogThrottle, ReportsTheSuppressedCountOnTheFirstFailureOfTheNextWindow)
{
    FailureLogThrottle throttle(2, std::chrono::seconds(10));
    for (int i = 0; i < 5; ++i)
    {
        throttle.OnFailure(At(std::chrono::milliseconds(i)));
    }
    ASSERT_EQ(3u, throttle.PendingSuppressed());

    // 新窗口的第一条:自己要逐条记录,并且把上个窗口压掉的 3 条补报出来。
    const auto first = throttle.OnFailure(At(std::chrono::seconds(10)));
    EXPECT_TRUE(first.logThisOne);
    EXPECT_EQ(3u, first.suppressedBeforeThis);
    EXPECT_EQ(0u, throttle.PendingSuppressed());

    // 补报只发生一次。
    const auto second = throttle.OnFailure(At(std::chrono::seconds(10) + std::chrono::milliseconds(1)));
    EXPECT_TRUE(second.logThisOne);
    EXPECT_EQ(0u, second.suppressedBeforeThis);
}

TEST(FailureLogThrottle, WindowStartsAtTheFirstFailureNotAtConstruction)
{
    // 构造之后很久才出现第一条失败,这一条仍然属于一个全新的窗口。
    FailureLogThrottle throttle(1, std::chrono::seconds(10));

    const auto first = throttle.OnFailure(At(std::chrono::hours(3)));
    EXPECT_TRUE(first.logThisOne);
    EXPECT_EQ(0u, first.suppressedBeforeThis);

    const auto second = throttle.OnFailure(At(std::chrono::hours(3) + std::chrono::seconds(9)));
    EXPECT_FALSE(second.logThisOne);

    const auto third = throttle.OnFailure(At(std::chrono::hours(3) + std::chrono::seconds(10)));
    EXPECT_TRUE(third.logThisOne);
    EXPECT_EQ(1u, third.suppressedBeforeThis);
}

TEST(FailureLogThrottle, ZeroBurstSuppressesEverythingButStillCounts)
{
    FailureLogThrottle throttle(0, std::chrono::seconds(10));
    EXPECT_FALSE(throttle.OnFailure(At(std::chrono::milliseconds(0))).logThisOne);
    EXPECT_FALSE(throttle.OnFailure(At(std::chrono::milliseconds(1))).logThisOne);
    EXPECT_EQ(2u, throttle.PendingSuppressed());
}

TEST(FailureLogThrottle, TakeExpiredSuppressedReturnsNothingWhileTheWindowIsStillOpen)
{
    FailureLogThrottle throttle(1, std::chrono::seconds(10));
    throttle.OnFailure(At(std::chrono::milliseconds(0)));
    throttle.OnFailure(At(std::chrono::milliseconds(1)));
    throttle.OnFailure(At(std::chrono::milliseconds(2)));
    ASSERT_EQ(2u, throttle.PendingSuppressed());

    // 窗口没过期:什么都不取,也不改变状态。
    EXPECT_EQ(0u, throttle.TakeExpiredSuppressed(At(std::chrono::milliseconds(9999))));
    EXPECT_EQ(2u, throttle.PendingSuppressed());
}

TEST(FailureLogThrottle, TakeExpiredSuppressedReportsTheTailWindowWhenNoFurtherFailureArrives)
{
    // 回归:故障恢复后不再有失败。旧实现里被压掉的条数只随「下一条失败」补报,
    // 于是最后一个窗口(以及单窗口突发)里除前 burst 条之外的丢失永远不进日志。
    FailureLogThrottle throttle(1, std::chrono::seconds(10));
    for (int i = 0; i < 6; ++i)
    {
        throttle.OnFailure(At(std::chrono::milliseconds(i)));
    }
    ASSERT_EQ(5u, throttle.PendingSuppressed());

    EXPECT_EQ(5u, throttle.TakeExpiredSuppressed(At(std::chrono::seconds(10))));
    EXPECT_EQ(0u, throttle.PendingSuppressed());
    // 只报一次。
    EXPECT_EQ(0u, throttle.TakeExpiredSuppressed(At(std::chrono::seconds(30))));
}

TEST(FailureLogThrottle, NextFailureAfterATakeStartsAFreshWindowWithoutReportingAgain)
{
    FailureLogThrottle throttle(1, std::chrono::seconds(10));
    throttle.OnFailure(At(std::chrono::milliseconds(0)));
    throttle.OnFailure(At(std::chrono::milliseconds(1)));
    ASSERT_EQ(1u, throttle.TakeExpiredSuppressed(At(std::chrono::seconds(10))));

    // 已经补报过的条数不能再随下一条失败报第二遍;这一条属于新窗口,要逐条记录。
    const auto next = throttle.OnFailure(At(std::chrono::seconds(11)));
    EXPECT_TRUE(next.logThisOne);
    EXPECT_EQ(0u, next.suppressedBeforeThis);
}

TEST(FailureLogThrottle, TakeExpiredSuppressedIsANoOpWhenNothingWasSuppressed)
{
    FailureLogThrottle throttle(5, std::chrono::seconds(10));
    EXPECT_EQ(0u, throttle.TakeExpiredSuppressed(At(std::chrono::seconds(100)))); // 从未有过失败
    throttle.OnFailure(At(std::chrono::milliseconds(0)));
    EXPECT_EQ(0u, throttle.TakeExpiredSuppressed(At(std::chrono::seconds(100)))); // 有失败但没压掉任何一条

    // 没压掉东西时不能把窗口关掉:否则窗口内的后续失败会被当成新窗口,burst 规则被绕开。
    FailureLogThrottle tight(1, std::chrono::seconds(10));
    tight.OnFailure(At(std::chrono::milliseconds(0)));
    ASSERT_EQ(0u, tight.TakeExpiredSuppressed(At(std::chrono::milliseconds(5))));
    EXPECT_FALSE(tight.OnFailure(At(std::chrono::milliseconds(6))).logThisOne);
}

TEST(FailureLogThrottle, TakeSuppressedDrainsUnconditionallyAndKeepsTheWindow)
{
    FailureLogThrottle throttle(1, std::chrono::seconds(10));
    throttle.OnFailure(At(std::chrono::milliseconds(0)));
    throttle.OnFailure(At(std::chrono::milliseconds(1)));
    throttle.OnFailure(At(std::chrono::milliseconds(2)));

    // 窗口还开着也照取(重建 / 析构收尾用),取完清零。
    EXPECT_EQ(2u, throttle.TakeSuppressed());
    EXPECT_EQ(0u, throttle.TakeSuppressed());

    // 窗口没有被重置:同一窗口内的后续失败仍然被压,并重新开始计数。
    const auto later = throttle.OnFailure(At(std::chrono::milliseconds(3)));
    EXPECT_FALSE(later.logThisOne);
    EXPECT_EQ(1u, throttle.PendingSuppressed());
}
