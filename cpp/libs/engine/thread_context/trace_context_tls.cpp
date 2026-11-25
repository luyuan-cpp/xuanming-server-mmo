#include "thread_context/trace_context_tls.h"

#include <array>
#include <atomic>
#include <chrono>

namespace tracing {

namespace detail {
// 每个线程从「没有 trace、没有玩家、没有在录 span」开始,直到入口的作用域装上真实值。
thread_local TraceContext tlsTrace;
thread_local uint64_t tlsPlayerId = 0;
thread_local SpanRecorder* tlsRecorder = nullptr;
} // namespace detail

namespace {

// CurrentTraceHex 的缓存。记着生成十六进制时的原值,读的时候比对:不靠各修改路径置脏,
// 所以经 Set()、作用域析构或任何别的路径改了 trace,都不会读到过期的十六进制。
// 只含定长数组与标量,线程退出时无需析构。
struct TraceHexCache {
    TraceId traceId;
    SpanId spanId;
    bool filled = false;
    std::array<char, kTraceIdHexChars + kSpanIdHexChars> hex{};
};

thread_local TraceHexCache tlsTraceHex;

// 进程唯一的导出去处。acquire / release:装上之前对去处的初始化,对之后读到指针的线程可见。
std::atomic<SpanSink*> gSpanSink{nullptr};

uint64_t UnixNanosNow()
{
    const auto sinceEpoch = std::chrono::system_clock::now().time_since_epoch();
    const auto nanos = std::chrono::duration_cast<std::chrono::nanoseconds>(sinceEpoch).count();
    return nanos > 0 ? static_cast<uint64_t>(nanos) : 0;
}

int64_t SteadyNanosNow()
{
    return std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now().time_since_epoch())
        .count();
}

} // namespace

std::string_view CurrentTraceHex()
{
    const TraceContext& current = detail::tlsTrace;
    if (!current.IsValid()) {
        return {};
    }

    TraceHexCache& cache = tlsTraceHex;
    if (!cache.filled || cache.traceId.bytes != current.trace_id.bytes || cache.spanId.bytes != current.span_id.bytes) {
        WriteLowerHex(current.trace_id.bytes.data(), kTraceIdBytes, cache.hex.data());
        WriteLowerHex(current.span_id.bytes.data(), kSpanIdBytes, cache.hex.data() + kTraceIdHexChars);
        cache.traceId = current.trace_id;
        cache.spanId = current.span_id;
        cache.filled = true;
    }
    return std::string_view(cache.hex.data(), cache.hex.size());
}

void InstallSpanSink(SpanSink* sink) noexcept
{
    gSpanSink.store(sink, std::memory_order_release);
}

SpanSink* InstalledSpanSink() noexcept
{
    return gSpanSink.load(std::memory_order_acquire);
}

SpanRecorder::SpanRecorder(SpanRecorder&& other) noexcept
    : record(other.record), startSteadyNanos(other.startSteadyNanos), active(other.active)
{
    other.active = false;
}

SpanRecorder& SpanRecorder::operator=(SpanRecorder&& other) noexcept
{
    if (this != &other) {
        record = other.record;
        startSteadyNanos = other.startSteadyNanos;
        active = other.active;
        other.active = false;
    }
    return *this;
}

void SpanRecorder::Begin(const TraceContext& context, const SpanId& parent, bool parentIsRemote, SpanKind kind,
                         SpanSystem system, SpanName name) noexcept
{
    active = false;
    // 头部采样一律采(§3.4);flags 不带 sampled 的上下文(上游显式不采)按 ParentBased 不记录。
    if (!context.IsValid() || (context.flags & 0x01) == 0 || InstalledSpanSink() == nullptr) {
        return;
    }
    record = SpanRecord{};
    record.trace_id = context.trace_id;
    record.span_id = context.span_id;
    record.parent_span_id = parent;
    record.trace_flags = context.flags;
    record.kind = kind;
    record.system = system;
    record.parent_is_remote = parentIsRemote && parent.IsValid();
    record.SetName(name);
    record.start_unix_nano = UnixNanosNow();
    startSteadyNanos = SteadyNanosNow();
    active = true;
}

void SpanRecorder::SetError(std::string_view description) noexcept
{
    if (!active) {
        return;
    }
    record.status = SpanStatus::kError;
    record.SetStatusMessage(description);
}

void SpanRecorder::SetGrpcStatusCode(uint32_t code) noexcept
{
    if (!active) {
        return;
    }
    record.grpc_status_code = code;
    record.has_grpc_status = true;
}

void SpanRecorder::End() noexcept
{
    if (!active) {
        return;
    }
    active = false;
    // 结束时刻 = 开始的墙钟 + 单调时钟测得的时长:墙钟中途被校准也不会出现负时长或跳变(OTel SDK 的做法)。
    const int64_t elapsed = SteadyNanosNow() - startSteadyNanos;
    record.end_unix_nano = record.start_unix_nano + static_cast<uint64_t>(elapsed > 0 ? elapsed : 0);
    if (SpanSink* sink = InstalledSpanSink(); sink != nullptr) {
        sink->Submit(record);
    }
}

void MarkCurrentSpanError(std::string_view description) noexcept
{
    if (SpanRecorder* recorder = detail::tlsRecorder; recorder != nullptr) {
        recorder->SetError(description);
    }
}

PendingSpan PendingSpan::StartChildOf(const TraceContext& parent, SpanKind kind, SpanSystem system,
                                      SpanName name) noexcept
{
    PendingSpan span;
    if (!parent.IsValid()) {
        return span;
    }
    span.context = ChildSpan(parent);
    span.recorder.Begin(span.context, parent.span_id, false, kind, system, name);
    return span;
}

PendingSpan PendingSpan::TakeCurrentServerSpan(SpanSystem system) noexcept
{
    PendingSpan span;
    SpanRecorder* recorder = detail::tlsRecorder;
    if (recorder == nullptr || !recorder->Active() || recorder->Kind() != SpanKind::kServer || recorder->System() != system) {
        return span;
    }
    // 在录的最内层作用域装着的就是这个 span 的上下文:嵌套的作用域都会改写 tlsRecorder(不录的置空)。
    span.context = detail::tlsTrace;
    span.recorder = std::move(*recorder);   // 移走后作用域的记录器不再记录,退出时的 End 是空操作
    detail::tlsRecorder = nullptr;
    return span;
}

} // namespace tracing
