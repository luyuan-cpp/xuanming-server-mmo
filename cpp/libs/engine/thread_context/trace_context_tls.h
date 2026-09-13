#pragma once

#include <string>

#include "core/tracing/trace_context.h"

// Per-thread Trace Context slot (todo.md #152 slice A).
//
// Holds the active TraceContext for the current thread. Set at RPC entry
// from a parsed `traceparent` header (or by allocating a fresh root when
// there isn't one); consumed by:
//   - log lines via the TLOG_* macros below, so every log entry carries
//     a trace_id field on-format
//   - outgoing RPC stubs that need to write `traceparent` into the next
//     hop's metadata (slice C will pick this up)
//   - error_reporter::Record (slice A integration in #250) — could read
//     this to embed trace_id in every reported event for cross-event
//     correlation
//
// Lifetime: thread-local, lives for the duration of the thread. Reset at
// each RPC dispatch entry. A NOT-SET state is represented by a default-
// constructed TraceContext (both IDs all-zero, `IsValid()` returns false).
//
// Performance note (Review O2 fix, 2026-05-17): the 8-char log prefix
// is cached in TLS alongside the context so TLOG_* in the hot path
// reads a thread_local std::string by const-ref rather than re-running
// trace_id.ToHex() + substr(0, 8) on every log line. The cache is
// refreshed only by the four mutator paths: assignment to tlsTrace,
// `Set()`, `Clear()`, and `ScopedSpan`'s ctor/dtor. Read paths are
// branch + thread_local read, no allocation.

namespace tracing {

// The TLS slot. `extern thread_local` rather than `inline thread_local`
// keeps the symbol single-definition across translation units.
// (Per-RPC-call state deliberately does NOT live in TLS any more; it travels
// explicitly as RpcController — see network/rpc_controller.h.)
extern thread_local TraceContext tlsTrace;

// Cached 8-char hex prefix for the current trace. Empty when tlsTrace
// is not valid. Updated only by RefreshLogPrefix() below, called from
// every TraceContext mutator. The single-thread-local nature means
// reads from TLOG_* are race-free with respect to the writer; we never
// share a TraceContext across threads.
extern thread_local std::string tlsTraceLogPrefix;

// Recompute tlsTraceLogPrefix from the current tlsTrace. Cheap when
// invalid (clears the string), one substr + trace_id.ToHex() call when
// valid — done once per mutation rather than once per log line.
inline void RefreshLogPrefix()
{
    if (!tlsTrace.IsValid()) {
        tlsTraceLogPrefix.clear();
        return;
    }
    const std::string hex = tlsTrace.trace_id.ToHex();
    tlsTraceLogPrefix.assign(hex, 0, 8);
}

// RAII scoped span: save the current context on entry, install a child
// span derived from it, restore the parent on exit. Use to bracket a
// section of work that should appear as a child span in traces.
//
// Usage:
//   {
//       tracing::ScopedSpan span;          // child of current tlsTrace
//       DoTheThing();                       // logs carry the child span_id
//   }                                       // tlsTrace restored
//
// Note: this doesn't emit a span record (timing, name, etc.) — that's
// slice F. For now it just gives child-span correlation in logs.
class ScopedSpan {
public:
    ScopedSpan()
        : prev_(tlsTrace)
    {
        if (prev_.IsValid()) {
            tlsTrace = ChildSpan(prev_);
        } else {
            tlsTrace = NewRoot();
        }
        RefreshLogPrefix();
    }

    explicit ScopedSpan(const TraceContext& parent)
        : prev_(tlsTrace)
    {
        tlsTrace = ChildSpan(parent);
        RefreshLogPrefix();
    }

    ~ScopedSpan()
    {
        tlsTrace = prev_;
        RefreshLogPrefix();
    }

    ScopedSpan(const ScopedSpan&) = delete;
    ScopedSpan& operator=(const ScopedSpan&) = delete;
    ScopedSpan(ScopedSpan&&) = delete;
    ScopedSpan& operator=(ScopedSpan&&) = delete;

private:
    TraceContext prev_;
};

// Read-only accessor for the cached prefix. Used by TLOG_* macros below.
inline const std::string& LogPrefix() { return tlsTraceLogPrefix; }

// Install or replace the current thread's TraceContext. Use at RPC
// dispatch entry — either from a parsed header (incoming traceparent)
// or by calling `tlsTrace = tracing::NewRoot();` for an origin trace.
//
// Prefer Set() over direct `tlsTrace = ...` assignment — Set() refreshes
// the log-prefix cache. Direct assignment skips the refresh, leaving
// TLOG_* output stale.
inline void Set(const TraceContext& ctx)
{
    tlsTrace = ctx;
    RefreshLogPrefix();
}

// Clear the current thread's TraceContext. Use when leaving an RPC
// handler if nothing further on this thread should belong to the
// trace (rare — the next handler will overwrite anyway).
inline void Clear()
{
    tlsTrace = TraceContext{};
    tlsTraceLogPrefix.clear();
}

// Read the current thread's TraceContext (or default if unset).
inline const TraceContext& Current() { return tlsTrace; }

} // namespace tracing

// ─── Trace-aware logging macros ─────────────────────────────────────
//
// TLOG_* wraps LOG_* with a trace_id prefix when a trace is active.
// Falls back to plain LOG_* when no trace is set, so callers that don't
// participate in tracing pay zero cost beyond a thread_local read +
// an empty()-branch.
//
// Performance (Review O2 fix): the prefix is read from a TLS-cached
// std::string by const-ref. No substr / ToHex per log line; no per-call
// heap allocation in the hot path. Refresh of the cache happens only
// when the TraceContext is mutated.
//
// We deliberately do NOT modify muduo's Logging — that's framework code
// (CLAUDE.md §2 #1). These macros prepend `[trace=xxxxxxxx]` to the
// message body. Grepping `trace=abc1234d` across all node logs
// correlates a single trace's spans across nodes.

#define TLOG_INFO  LOG_INFO  << (tracing::LogPrefix().empty() ? "" : "[trace=") << tracing::LogPrefix() << (tracing::LogPrefix().empty() ? "" : "] ")
#define TLOG_WARN  LOG_WARN  << (tracing::LogPrefix().empty() ? "" : "[trace=") << tracing::LogPrefix() << (tracing::LogPrefix().empty() ? "" : "] ")
#define TLOG_ERROR LOG_ERROR << (tracing::LogPrefix().empty() ? "" : "[trace=") << tracing::LogPrefix() << (tracing::LogPrefix().empty() ? "" : "] ")
#define TLOG_DEBUG LOG_DEBUG << (tracing::LogPrefix().empty() ? "" : "[trace=") << tracing::LogPrefix() << (tracing::LogPrefix().empty() ? "" : "] ")
