#pragma once

#include <cstdint>
#include <string_view>

#include "core/tracing/trace_context.h"

// 每线程一份日志上下文:当前 trace 与当前玩家(docs/design/distributed-tracing.md §4)。
//
// 安装只用 RAII 作用域(ScopedSpan、ScopedPlayerId,以及各入口自己的派发作用域),离开作用域恢复进入前的值。
// 入口不得裸调 Set() 而不恢复:那样会把上一个请求的上下文留给之后在同一线程上跑的回调(§4.2)。
// 异步续跑由投递方捕获 Current()、执行方用 ScopedSpan(captured) 装回(§4.3)。
//
// 线程局部,从不跨线程共享,读写都不加锁。读者:Node::AsyncOutput 给每行日志追加 [ctx ...] 后缀(§5.1);
// RpcChannel 发起调用时捕获当前 trace,回包时恢复。

namespace tracing {

namespace detail {
// 只给本头文件的内联函数和 trace_context_tls.cpp 用;别处一律走下面的函数与作用域类。
extern thread_local TraceContext tlsTrace;
extern thread_local uint64_t tlsPlayerId;
} // namespace detail

// 当前线程的 trace;没有时是默认构造值(IsValid() 为 false)。
inline const TraceContext& Current() { return detail::tlsTrace; }

// 直接替换当前 trace。只给 RAII 作用域的实现用(本文件的 ScopedSpan、RpcChannel 的派发作用域);
// 入口代码请用作用域类,不要裸调。
inline void Set(const TraceContext& ctx) { detail::tlsTrace = ctx; }

// 当前线程正在处理的玩家;0 表示没有。
inline uint64_t CurrentPlayerId() { return detail::tlsPlayerId; }

// 当前 trace 的 48 个小写十六进制字符(32 位 trace_id 紧接 16 位 span_id);没有有效 trace 时返回空。
// 按需生成并缓存:缓存记着生成它的 trace_id 与 span_id,当前值变了才重算,所以无论 trace 经哪条路径改动,
// 都读不到过期的缓存。返回值指向线程局部缓冲,只在本线程、下一次调用之前有效。
std::string_view CurrentTraceHex();

// 作用域内建一个子 span,退出时恢复进入前的 trace。
//   ScopedSpan span;          // 当前有 trace 就建它的子 span,没有就开新根
//   ScopedSpan span(parent);  // 建 parent 的子 span;异步回调用它装回投递时捕获的 trace
// parent 无效时作用域内没有 trace(只换了 span_id,trace_id 仍为全零)。
class ScopedSpan {
public:
    ScopedSpan()
        : previous_(detail::tlsTrace)
    {
        detail::tlsTrace = previous_.IsValid() ? ChildSpan(previous_) : NewRoot();
    }

    explicit ScopedSpan(const TraceContext& parent)
        : previous_(detail::tlsTrace)
    {
        detail::tlsTrace = ChildSpan(parent);
    }

    ~ScopedSpan() { detail::tlsTrace = previous_; }

    ScopedSpan(const ScopedSpan&) = delete;
    ScopedSpan& operator=(const ScopedSpan&) = delete;
    ScopedSpan(ScopedSpan&&) = delete;
    ScopedSpan& operator=(ScopedSpan&&) = delete;

private:
    TraceContext previous_;
};

// 作用域内把当前玩家设成 playerId(0 表示没有玩家),退出时恢复进入前的值。
class ScopedPlayerId {
public:
    explicit ScopedPlayerId(uint64_t playerId)
        : previous_(detail::tlsPlayerId)
    {
        detail::tlsPlayerId = playerId;
    }

    ~ScopedPlayerId() { detail::tlsPlayerId = previous_; }

    ScopedPlayerId(const ScopedPlayerId&) = delete;
    ScopedPlayerId& operator=(const ScopedPlayerId&) = delete;
    ScopedPlayerId(ScopedPlayerId&&) = delete;
    ScopedPlayerId& operator=(ScopedPlayerId&&) = delete;

private:
    uint64_t previous_;
};

} // namespace tracing
