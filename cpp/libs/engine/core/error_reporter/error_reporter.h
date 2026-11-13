#pragma once

#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <ctime>
#include <deque>
#include <fstream>
#include <mutex>
#include <sstream>
#include <string>
#include <string_view>
#include <vector>

#include "core/tracing/trace_context.h"
#include "thread_context/trace_context_tls.h"

// Error aggregation buffer (todo.md #250).
//
// Sentry-style "collect errors and ship to a central server" is the goal,
// but picking a vendor (self-host vs SaaS) and wiring an HTTP egress
// path are deployment-level decisions that don't belong in the engine.
// What DOES belong here is an in-process aggregator: every error site
// (the unified #70/#125 hook, the #105 crash handler, MessageLimiter
// rejections, #236 illegal-packet counter trips) gets a one-line API
// to record an event; downstream shippers — Prometheus exporter, a
// future HTTP push, a SIGUSR2 dump-to-file — pull from a single queue.
//
// This decouples "where do errors get recorded?" (everywhere — easy)
// from "where do they get shipped?" (one place — under deployment
// control). The same buffer also serves as the source of truth for
// "what happened in the last N minutes?" when an on-call grabs a node
// to triage.
//
// Threading: the buffer is shared across muduo's EventLoop threads, the
// Kafka consumer pool, and the proto-gen worker. A small mutex protects
// the deque; the contention point is one push per error (rare by design)
// vs the legal-message throughput path, so the lock is well outside any
// hot path.
//
// Capacity: bounded ring (default 1024 events). When full, the OLDEST
// event is dropped — recent errors are usually more interesting for
// triage than old ones, and dropping silently keeps the recorder
// always non-blocking. A monotonic dropped-count exposes the
// drop-pressure to monitoring.
//
// 日志上下文:每条事件还记下录入那一刻线程上的 trace 与玩家(docs/design/distributed-tracing.md §13 第 4 条),
// 拿转储里的 trace_id 就能在 Loki 里查到这次请求在各服务的全部日志。口径与日志行尾后缀相同(§5.1):
// 没有 trace 就不带 trace_id / span_id,没有玩家就不带 player_id。

namespace error_reporter {

struct Event {
    int64_t wall_ms{0};        // std::chrono::system_clock millis since epoch
    uint32_t error_code{0};     // domain-specific; 0 = "no code, just text"
    std::string tag;            // short label, e.g. "AssignGate", "Logout"
    std::string message;        // human-readable detail
    tracing::TraceContext trace; // 录入时线程上的 trace;IsValid() 为 false 表示没有
    uint64_t player_id{0};      // 录入时线程上的玩家;0 表示没有
};

class Buffer {
public:
    explicit Buffer(size_t capacity = 1024) : capacity_(capacity) {}

    // Record one event. Allocates only inside the lock (mutex + deque
    // push); call sites are expected to be on the rare error path, so
    // the cost is amortized below noise.
    // trace / playerId 由调用方显式传入(业务走下面的 error_reporter::Record,取当前线程的值),
    // 本类不读线程局部状态,单测可以直接构造。
    void Record(uint32_t error_code, std::string_view tag, std::string_view message,
                const tracing::TraceContext& trace, uint64_t playerId)
    {
        Event e;
        e.wall_ms = std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::system_clock::now().time_since_epoch()).count();
        e.error_code = error_code;
        e.tag.assign(tag);
        e.message.assign(message);
        e.trace = trace;
        e.player_id = playerId;

        std::lock_guard<std::mutex> lock(mutex_);
        if (buffer_.size() >= capacity_) {
            buffer_.pop_front();
            ++dropped_count_;
        }
        buffer_.push_back(std::move(e));
        ++total_count_;
    }

    // Move out all currently buffered events. Atomically clears the
    // internal buffer; subsequent Record() calls start from empty.
    // Intended for periodic shippers (metric exporter, HTTP push loop).
    std::vector<Event> Drain()
    {
        std::lock_guard<std::mutex> lock(mutex_);
        std::vector<Event> out;
        out.reserve(buffer_.size());
        for (auto& e : buffer_) {
            out.push_back(std::move(e));
        }
        buffer_.clear();
        return out;
    }

    // Copy out without clearing — for SIGUSR2 dump-to-file or a "what
    // happened?" diagnostic RPC.
    std::vector<Event> Snapshot() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return std::vector<Event>(buffer_.begin(), buffer_.end());
    }

    // Counters for Prometheus-style observability. Both are monotonic
    // since process start.
    uint64_t TotalCount() const { return total_count_.load(std::memory_order_relaxed); }
    uint64_t DroppedCount() const { return dropped_count_.load(std::memory_order_relaxed); }
    size_t CurrentSize() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return buffer_.size();
    }

private:
    mutable std::mutex mutex_;
    std::deque<Event> buffer_;
    const size_t capacity_;
    std::atomic<uint64_t> total_count_{0};
    std::atomic<uint64_t> dropped_count_{0};
};

// Process-wide singleton. Acquire-then-Record from any thread. The
// instance is leaked at process exit (Meyers-singleton pattern keeps
// it alive until program termination, avoiding "report an error during
// shutdown teardown" lifetime races).
inline Buffer& Instance()
{
    static Buffer buffer;
    return buffer;
}

// One-liner for the call-site convenience. Equivalent to
// `Instance().Record(...)` with the calling thread's log context:
// 带上当前线程的 trace 与玩家(trace_context_tls.h),调用点不用自己取。
inline void Record(uint32_t error_code, std::string_view tag, std::string_view message)
{
    Instance().Record(error_code, tag, message, tracing::Current(), tracing::CurrentPlayerId());
}

// JSONL serialization of an event. One event per line so a downstream
// log shipper (or `cat | jq`) can stream-process the dump file.
//
// Format: { "wall_ms": <int>, "error_code": <int>, "tag": "...",
//           "message": "...", "trace_id": "<32 hex>", "span_id": "<16 hex>",
//           "player_id": "<decimal>" }
//
// trace_id / span_id 只在录入时有 trace 才写,player_id 只在有玩家时写(同日志行尾后缀,
// distributed-tracing.md §5.1)。player_id 写成十进制字符串:jq 等按双精度解析数字的工具
// 读超过 2^53 的整数会丢精度(同 Go 日志的口径,§6.2)。
//
// Strings are escaped for the limited set of JSON-required characters
// only (no UTF-8 reformatting — the bytes pass through unchanged so a
// CJK message stays readable). The escape covers: ", \, control chars
// (\b \f \n \r \t plus \u00xx for the rest below 0x20). Acceptable for
// JSONL parsers; full RFC 8259 compliance is overkill for an internal
// dump file.
inline std::string EscapeJsonString(std::string_view in)
{
    std::string out;
    out.reserve(in.size() + 8);
    for (char c : in) {
        const unsigned char u = static_cast<unsigned char>(c);
        switch (c) {
            case '"':  out += "\\\""; break;
            case '\\': out += "\\\\"; break;
            case '\b': out += "\\b"; break;
            case '\f': out += "\\f"; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (u < 0x20) {
                    char buf[8];
                    std::snprintf(buf, sizeof(buf), "\\u%04x", u);
                    out += buf;
                } else {
                    out += c;
                }
        }
    }
    return out;
}

inline std::string EventToJsonl(const Event& e)
{
    std::ostringstream oss;
    oss << "{\"wall_ms\":" << e.wall_ms
        << ",\"error_code\":" << e.error_code
        << ",\"tag\":\"" << EscapeJsonString(e.tag) << "\""
        << ",\"message\":\"" << EscapeJsonString(e.message) << "\"";
    if (e.trace.IsValid()) {
        oss << ",\"trace_id\":\"" << e.trace.trace_id.ToHex() << "\""
            << ",\"span_id\":\"" << e.trace.span_id.ToHex() << "\"";
    }
    if (e.player_id != 0) {
        oss << ",\"player_id\":\"" << e.player_id << "\"";
    }
    oss << "}";
    return oss.str();
}

// Dump the current Snapshot() to a file in JSONL format. Intended as
// the SIGUSR2 / on-demand triage hook (todo.md #250 slice D). The file
// is opened with truncation so each dump replaces the previous one;
// callers who want a history should rotate the path themselves.
//
// Header line (also JSONL-shaped) carries the buffer counters so the
// file is self-describing — `head -1 dump.jsonl | jq` shows what's
// in the rest:
//   { "_kind":"header", "total":N, "dropped":N, "current":N, "wall_ms":N }
//
// Returns true on successful write of all events. Best-effort — on
// IO failure we log a warning to stderr (no muduo include from this
// header to keep it dependency-light) and return false.
inline bool DumpSnapshotToFile(const std::string& path)
{
    auto& buf = Instance();
    auto events = buf.Snapshot();
    const auto total = buf.TotalCount();
    const auto dropped = buf.DroppedCount();
    const auto current = buf.CurrentSize();

    std::ofstream out(path, std::ios::trunc);
    if (!out) {
        std::fprintf(stderr, "[error_reporter] DumpSnapshotToFile failed to open %s\n",
                     path.c_str());
        return false;
    }

    const auto nowMs = std::chrono::duration_cast<std::chrono::milliseconds>(
        std::chrono::system_clock::now().time_since_epoch()).count();
    out << "{\"_kind\":\"header\""
        << ",\"total\":" << total
        << ",\"dropped\":" << dropped
        << ",\"current\":" << current
        << ",\"wall_ms\":" << nowMs
        << "}\n";

    for (const auto& e : events) {
        out << EventToJsonl(e) << "\n";
    }
    out.flush();
    return out.good();
}

// Convenience: build a default dump path under `logs/` with PID +
// wall-time stamp. Lets ops trigger a dump without having to provide
// a path each time. Returns the path that was written to (or an empty
// string on failure).
inline std::string DumpSnapshotToDefaultPath()
{
    const auto t = std::time(nullptr);
    char tsBuf[24];
    std::strftime(tsBuf, sizeof(tsBuf), "%Y%m%d_%H%M%S", std::localtime(&t));

    std::ostringstream pathStream;
    pathStream << "logs/error_reporter_dump_" << tsBuf << ".jsonl";
    const std::string path = pathStream.str();

    if (!DumpSnapshotToFile(path)) {
        return std::string();
    }
    return path;
}

} // namespace error_reporter
