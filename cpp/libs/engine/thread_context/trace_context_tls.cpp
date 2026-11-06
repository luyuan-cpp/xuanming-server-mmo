#include "thread_context/trace_context_tls.h"

#include <array>

namespace tracing {

namespace detail {
// 每个线程从「没有 trace、没有玩家」开始,直到入口的作用域装上真实值。
thread_local TraceContext tlsTrace;
thread_local uint64_t tlsPlayerId = 0;
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

} // namespace tracing
