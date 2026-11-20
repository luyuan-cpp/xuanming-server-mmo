// span_export_queue_test —— 导出队列的契约(docs/design/distributed-tracing.md §10.3,BatchSpanProcessor 语义)。
//
// 被锁定的契约:满了丢新来的并计数、不阻塞;先进先出、环形回绕不乱序;长度恰好到一批时通知一次;
// 摘掉的回调不再被调用;DiscardAll 计停机丢弃;退化的容量与门槛按 1 / 容量收口;多线程放入时总数守恒、不超容量。

#include <gtest/gtest.h>

#include <atomic>
#include <thread>
#include <vector>

#include "core/tracing/span_export_queue.h"
#include "span_test_support.h"

namespace {

uint8_t TagOf(const tracing::SpanRecord& record) { return record.span_id.bytes[7]; }

} // namespace

TEST(SpanExportQueueTest, DropsNewSpansWhenFullAndCountsThem)
{
    tracing::SpanExportQueue queue(3, 3);
    for (uint8_t tag = 1; tag <= 5; ++tag) {
        queue.Submit(span_test::MakeSpanRecord(tag));
    }
    EXPECT_EQ(queue.Size(), 3u);
    EXPECT_EQ(queue.Accepted(), 3u);
    EXPECT_EQ(queue.DroppedQueueFull(), 2u);

    std::vector<tracing::SpanRecord> out;
    EXPECT_EQ(queue.PopBatch(out, 10), 3u);
    ASSERT_EQ(out.size(), 3u);
    EXPECT_EQ(TagOf(out[0]), 1);   // 丢的是新来的,留下最早的三个
    EXPECT_EQ(TagOf(out[2]), 3);
}

TEST(SpanExportQueueTest, PopBatchIsFifoBoundedAndWrapsAroundTheRing)
{
    tracing::SpanExportQueue queue(4, 4);
    for (uint8_t tag = 1; tag <= 3; ++tag) {
        queue.Submit(span_test::MakeSpanRecord(tag));
    }
    std::vector<tracing::SpanRecord> out;
    EXPECT_EQ(queue.PopBatch(out, 2), 2u);
    for (uint8_t tag = 4; tag <= 6; ++tag) {   // 写指针回绕到环的开头
        queue.Submit(span_test::MakeSpanRecord(tag));
    }
    EXPECT_EQ(queue.Size(), 4u);
    EXPECT_EQ(queue.PopBatch(out, 10), 4u);
    ASSERT_EQ(out.size(), 6u);   // 追加到 out 末尾
    for (size_t i = 0; i < out.size(); ++i) {
        EXPECT_EQ(TagOf(out[i]), static_cast<uint8_t>(i + 1)) << i;
    }
    EXPECT_EQ(queue.Size(), 0u);
    EXPECT_EQ(queue.PopBatch(out, 10), 0u);
}

TEST(SpanExportQueueTest, NotifiesOnceWhenTheLengthReachesTheBatchThreshold)
{
    tracing::SpanExportQueue queue(10, 3);
    int notifications = 0;
    queue.SetBatchReadyCallback([&notifications] { ++notifications; });
    queue.Submit(span_test::MakeSpanRecord(1));
    queue.Submit(span_test::MakeSpanRecord(2));
    EXPECT_EQ(notifications, 0);
    queue.Submit(span_test::MakeSpanRecord(3));
    EXPECT_EQ(notifications, 1);
    queue.Submit(span_test::MakeSpanRecord(4));
    EXPECT_EQ(notifications, 1) << "超过门槛之后不再每放一个都叫一次";

    std::vector<tracing::SpanRecord> out;
    queue.PopBatch(out, 10);
    for (uint8_t tag = 5; tag <= 7; ++tag) {
        queue.Submit(span_test::MakeSpanRecord(tag));
    }
    EXPECT_EQ(notifications, 2) << "取空之后再攒够一批,再通知一次";

    queue.SetBatchReadyCallback(nullptr);
    queue.PopBatch(out, 10);
    for (uint8_t tag = 8; tag <= 10; ++tag) {
        queue.Submit(span_test::MakeSpanRecord(tag));
    }
    EXPECT_EQ(notifications, 2) << "摘掉的回调不再被调用";
}

TEST(SpanExportQueueTest, DiscardAllCountsShutdownDrops)
{
    tracing::SpanExportQueue queue(8, 8);
    for (uint8_t tag = 1; tag <= 5; ++tag) {
        queue.Submit(span_test::MakeSpanRecord(tag));
    }
    EXPECT_EQ(queue.DiscardAll(), 5u);
    EXPECT_EQ(queue.Size(), 0u);
    EXPECT_EQ(queue.DroppedOnShutdown(), 5u);
    EXPECT_EQ(queue.DroppedQueueFull(), 0u);
    queue.Submit(span_test::MakeSpanRecord(9));
    std::vector<tracing::SpanRecord> out;
    ASSERT_EQ(queue.PopBatch(out, 1), 1u);
    EXPECT_EQ(TagOf(out[0]), 9);
}

TEST(SpanExportQueueTest, ClampsDegenerateSizes)
{
    tracing::SpanExportQueue zero(0, 0);
    EXPECT_EQ(zero.Capacity(), 1u);
    EXPECT_EQ(zero.BatchThreshold(), 1u);
    tracing::SpanExportQueue tooBigThreshold(5, 9);
    EXPECT_EQ(tooBigThreshold.Capacity(), 5u);
    EXPECT_EQ(tooBigThreshold.BatchThreshold(), 5u);
    EXPECT_EQ(tracing::ProcessSpanExportQueue().Capacity(), tracing::kDefaultSpanQueueCapacity);
    EXPECT_EQ(tracing::ProcessSpanExportQueue().BatchThreshold(), tracing::kDefaultSpanExportBatchSize);
}

TEST(SpanExportQueueTest, ConcurrentSubmittersNeverExceedTheCapacity)
{
    tracing::SpanExportQueue queue(512, 512);
    constexpr int kThreads = 4;
    constexpr int kPerThread = 1000;
    std::vector<std::thread> threads;
    for (int t = 0; t < kThreads; ++t) {
        threads.emplace_back([&queue, t] {
            for (int i = 0; i < kPerThread; ++i) {
                queue.Submit(span_test::MakeSpanRecord(static_cast<uint8_t>(t + 1)));
            }
        });
    }
    for (auto& thread : threads) {
        thread.join();
    }
    EXPECT_EQ(queue.Size(), 512u);
    EXPECT_EQ(queue.Accepted() + queue.DroppedQueueFull(), static_cast<uint64_t>(kThreads * kPerThread));
}
