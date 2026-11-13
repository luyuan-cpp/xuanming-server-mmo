#pragma once

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <mutex>
#include <vector>

#include "core/tracing/span_record.h"

// 已结束 span 的有界队列(docs/design/distributed-tracing.md §10.3):业务线程放,导出线程取。
// 语义照 OTel SDK 规范的 BatchSpanProcessor:
//   * 有界:满了丢新来的 span 并计数,绝不阻塞放的一方;
//   * 攒到一批(batchThreshold)时通知导出线程提前发,不等下一个周期;
//   * 先进先出。
// 纯逻辑(一把互斥锁、一个预分配的环形缓冲),不依赖 muduo,单测在 cpp/tests/rpc_controller_test/span_export_queue_test.cpp。
//
// 生产用的是进程唯一的一份(ProcessSpanExportQueue):它是装进 tracing::InstallSpanSink 的对象,按 SpanSink 的约定活到进程退出。

namespace tracing {

class SpanExportQueue final : public SpanSink {
public:
    // capacity:最多存几个 span(规范默认 2048);batchThreshold:存到几个时通知导出线程(规范的每批上限,默认 512)。
    // 两个都必须 ≥ 1,batchThreshold 不大于 capacity;不满足时按 1 / capacity 收口,不抛异常。
    SpanExportQueue(size_t capacity, size_t batchThreshold);

    SpanExportQueue(const SpanExportQueue&) = delete;
    SpanExportQueue& operator=(const SpanExportQueue&) = delete;

    // 任何线程。满了就丢(计入 DroppedQueueFull)。放进去之后队列长度恰好到 batchThreshold 时,持锁调一次「攒够一批」回调。
    void Submit(const SpanRecord& span) noexcept override;

    // 取最旧的至多 maxCount 个,追加到 out 末尾,返回取出的个数。
    size_t PopBatch(std::vector<SpanRecord>& out, size_t maxCount);

    size_t Size() const;
    size_t Capacity() const { return capacity_; }
    size_t BatchThreshold() const { return batchThreshold_; }

    // 「攒够一批」回调,由导出线程设置,nullptr = 摘掉。回调在 Submit 的调用线程上、持着本队列的锁执行:
    // 必须便宜、不阻塞、不回头调本队列(通常是 queueInLoop 到导出线程)。持锁调用换来一个保证:
    // SetBatchReadyCallback(nullptr) 返回之后,不会再有回调在执行,导出线程可以放心拆掉回调引用的对象。
    void SetBatchReadyCallback(std::function<void()> callback);

    // 导出线程停下之前把剩下的全部丢掉,返回丢了几个(计入 DroppedOnShutdown)。
    size_t DiscardAll();

    uint64_t Accepted() const { return accepted_.load(std::memory_order_relaxed); }
    uint64_t DroppedQueueFull() const { return droppedQueueFull_.load(std::memory_order_relaxed); }
    uint64_t DroppedOnShutdown() const { return droppedOnShutdown_.load(std::memory_order_relaxed); }

private:
    mutable std::mutex mutex_;
    const size_t capacity_;
    const size_t batchThreshold_;
    std::vector<SpanRecord> ring_;   // 构造时一次分配 capacity_ 个
    size_t head_{0};                 // 最旧元素的下标
    size_t size_{0};
    std::function<void()> onBatchReady_;
    std::atomic<uint64_t> accepted_{0};
    std::atomic<uint64_t> droppedQueueFull_{0};
    std::atomic<uint64_t> droppedOnShutdown_{0};
};

// OTel SDK 规范(BatchSpanProcessor)的默认值,阶段三定案就用它们(设计 §10.3)。
inline constexpr size_t kDefaultSpanQueueCapacity = 2048;
inline constexpr size_t kDefaultSpanExportBatchSize = 512;

// 进程唯一的队列(容量与批量取上面的默认值),第一次调用时构造,有意不析构:它是装进 InstallSpanSink 的去处,
// 摘下之后仍可能有线程拿着旧指针在 Submit(SpanSink 的约定),进程退出时的静态析构不能先把它拆掉。
SpanExportQueue& ProcessSpanExportQueue();

} // namespace tracing
