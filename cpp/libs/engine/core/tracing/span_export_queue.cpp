#include "core/tracing/span_export_queue.h"

#include <utility>

namespace tracing {

namespace {

size_t ClampCapacity(size_t capacity) { return capacity == 0 ? 1 : capacity; }

size_t ClampThreshold(size_t threshold, size_t capacity)
{
    if (threshold == 0) {
        return 1;
    }
    return threshold > capacity ? capacity : threshold;
}

} // namespace

SpanExportQueue::SpanExportQueue(size_t capacity, size_t batchThreshold)
    : capacity_(ClampCapacity(capacity)),
      batchThreshold_(ClampThreshold(batchThreshold, ClampCapacity(capacity))),
      ring_(capacity_)
{
}

void SpanExportQueue::Submit(const SpanRecord& span) noexcept
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (size_ >= capacity_) {
        droppedQueueFull_.fetch_add(1, std::memory_order_relaxed);
        return;
    }
    ring_[(head_ + size_) % capacity_] = span;
    ++size_;
    accepted_.fetch_add(1, std::memory_order_relaxed);
    // 只在长度「恰好到」门槛时通知一次:导出线程每发完一批会自己复核还够不够一批,不需要每放一个都叫醒它。
    if (size_ == batchThreshold_ && onBatchReady_) {
        onBatchReady_();
    }
}

size_t SpanExportQueue::PopBatch(std::vector<SpanRecord>& out, size_t maxCount)
{
    std::lock_guard<std::mutex> lock(mutex_);
    const size_t count = maxCount < size_ ? maxCount : size_;
    out.reserve(out.size() + count);
    for (size_t i = 0; i < count; ++i) {
        out.push_back(ring_[head_]);
        head_ = (head_ + 1) % capacity_;
    }
    size_ -= count;
    if (size_ == 0) {
        head_ = 0;
    }
    return count;
}

size_t SpanExportQueue::Size() const
{
    std::lock_guard<std::mutex> lock(mutex_);
    return size_;
}

void SpanExportQueue::SetBatchReadyCallback(std::function<void()> callback)
{
    std::lock_guard<std::mutex> lock(mutex_);
    onBatchReady_ = std::move(callback);
}

size_t SpanExportQueue::DiscardAll()
{
    std::lock_guard<std::mutex> lock(mutex_);
    const size_t dropped = size_;
    size_ = 0;
    head_ = 0;
    droppedOnShutdown_.fetch_add(dropped, std::memory_order_relaxed);
    return dropped;
}

SpanExportQueue& ProcessSpanExportQueue()
{
    // 有意泄漏,理由见头文件。
    static SpanExportQueue* const queue = new SpanExportQueue(kDefaultSpanQueueCapacity, kDefaultSpanExportBatchSize);
    return *queue;
}

} // namespace tracing
