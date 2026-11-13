#pragma once

// 链路追踪阶段三(docs/design/distributed-tracing.md §10.3)单测共用的小工具:
//   * RecordingSink:收下结束的 span(线程安全),给「span 起止与父子关系」的用例核对;
//   * ScopedSinkInstall:作用域内装上一个去处,退出时装回原来的,不让去处漏给同进程里后面的用例;
//   * MakeSpanRecord:造一条内容可辨认的 SpanRecord,给队列、编码、导出器的用例用。
// 只有头文件,不依赖 gtest。

#include <cstdint>
#include <cstring>
#include <mutex>
#include <string_view>
#include <vector>

#include "core/tracing/span_record.h"
#include "thread_context/trace_context_tls.h"

namespace span_test {

class RecordingSink final : public tracing::SpanSink {
public:
    void Submit(const tracing::SpanRecord& span) noexcept override
    {
        std::lock_guard<std::mutex> lock(mutex_);
        spans_.push_back(span);
    }

    std::vector<tracing::SpanRecord> Spans() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return spans_;
    }

    size_t Count() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return spans_.size();
    }

    void Clear()
    {
        std::lock_guard<std::mutex> lock(mutex_);
        spans_.clear();
    }

private:
    mutable std::mutex mutex_;
    std::vector<tracing::SpanRecord> spans_;
};

class ScopedSinkInstall {
public:
    explicit ScopedSinkInstall(tracing::SpanSink* sink)
        : previous_(tracing::InstalledSpanSink())
    {
        tracing::InstallSpanSink(sink);
    }

    ~ScopedSinkInstall() { tracing::InstallSpanSink(previous_); }

    ScopedSinkInstall(const ScopedSinkInstall&) = delete;
    ScopedSinkInstall& operator=(const ScopedSinkInstall&) = delete;

private:
    tracing::SpanSink* previous_;
};

// 一条可辨认的 span:trace_id 的首字节、span_id 的末字节都是 tag(tag 非零时 id 合法),名字是 "span-<tag>"。
inline tracing::SpanRecord MakeSpanRecord(uint8_t tag, tracing::SpanKind kind = tracing::SpanKind::kServer)
{
    tracing::SpanRecord record;
    record.trace_id.bytes[0] = tag;
    record.trace_id.bytes[15] = 0x5A;
    record.span_id.bytes[7] = tag;
    record.span_id.bytes[0] = 0x3C;
    record.trace_flags = 0x01;
    record.kind = kind;
    record.start_unix_nano = 1'700'000'000'000'000'000ULL + tag;
    record.end_unix_nano = record.start_unix_nano + 1000;
    char name[16] = {};
    const int length = std::snprintf(name, sizeof(name), "span-%u", static_cast<unsigned>(tag));
    record.SetName(tracing::SpanName{std::string_view(name, static_cast<size_t>(length))});
    return record;
}

} // namespace span_test
