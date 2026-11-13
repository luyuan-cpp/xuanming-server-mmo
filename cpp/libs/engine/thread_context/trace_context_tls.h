#pragma once

#include <cstdint>
#include <string_view>
#include <utility>

#include "core/tracing/span_record.h"
#include "core/tracing/trace_context.h"

// 每线程一份日志上下文:当前 trace 与当前玩家(docs/design/distributed-tracing.md §4)。
//
// 安装只用 RAII 作用域(ScopedSpan、ScopedRoot、ScopedIncoming、ScopedPlayerId),离开作用域恢复进入前的值。
// 入口不得裸调 Set() 而不恢复:那样会把上一个请求的上下文留给之后在同一线程上跑的回调(§4.2)。
// 异步续跑由投递方捕获 Current()、执行方用 ScopedSpan(captured) 原样装回(§4.3)。
//
// 阶段三(§10.3)起,接缝作用域还负责记录 span:ScopedRoot / ScopedIncoming 的带名字构造在作用域结束时把这个
// server / consumer span 交给导出去处(InstallSpanSink);出站的 client / producer span 用 PendingSpan,
// 发起时开、完成时结束,不装进线程上下文。没装导出去处(没配 OTLP 地址)时一律不记录,只多一次原子读。
//
// 线程局部,从不跨线程共享,读写都不加锁。读者:Node::AsyncOutput 给每行日志追加 [ctx ...] 后缀(§5.1);
// RpcChannel 发起调用时捕获当前 trace,回包时恢复。

namespace tracing {

class SpanRecorder;

namespace detail {
// 只给本头文件的内联函数和 trace_context_tls.cpp 用;别处一律走下面的函数与作用域类。
extern thread_local TraceContext tlsTrace;
extern thread_local uint64_t tlsPlayerId;
// 当前线程上最内层的在录 span(装进线程上下文的 server / consumer / 根 span);恢复作用域与不录的作用域里为 nullptr。
// 只给 MarkCurrentSpanError 与作用域类用。
extern thread_local SpanRecorder* tlsRecorder;
} // namespace detail

// 当前线程的 trace;没有时是默认构造值(IsValid() 为 false)。
inline const TraceContext& Current() { return detail::tlsTrace; }

// 直接替换当前 trace。只给 RAII 作用域的实现用(本文件的作用域类、单测夹具复位);入口代码请用作用域类,不要裸调。
inline void Set(const TraceContext& ctx) { detail::tlsTrace = ctx; }

// 当前线程正在处理的玩家;0 表示没有。
inline uint64_t CurrentPlayerId() { return detail::tlsPlayerId; }

// 当前 trace 的 48 个小写十六进制字符(32 位 trace_id 紧接 16 位 span_id);没有有效 trace 时返回空。
// 按需生成并缓存:缓存记着生成它的 trace_id 与 span_id,当前值变了才重算,所以无论 trace 经哪条路径改动,
// 都读不到过期的缓存。返回值指向线程局部缓冲,只在本线程、下一次调用之前有效。
std::string_view CurrentTraceHex();

// ---- span 的去处(§10.3)----

// 装上进程唯一的导出去处;nullptr = 摘下(之后结束的 span 一律不记录)。任何线程都可调用。
// 装上的对象必须活到进程退出:摘下的同时可能还有线程拿着旧指针正在 Submit(SpanSink 的约定)。
void InstallSpanSink(SpanSink* sink) noexcept;
SpanSink* InstalledSpanSink() noexcept;

// 一个 span 的记录器:起止时间与内容。只在开始时装着导出去处、并且这条 trace 采样(flags 最低位)时才真的记录,
// 否则一切调用都是空操作(不读时钟、不拷名字)。可移动,不可复制:移走之后原对象不再记录,End 恰好生效一次。
class SpanRecorder {
public:
    SpanRecorder() = default;
    SpanRecorder(SpanRecorder&& other) noexcept;
    SpanRecorder& operator=(SpanRecorder&& other) noexcept;
    SpanRecorder(const SpanRecorder&) = delete;
    SpanRecorder& operator=(const SpanRecorder&) = delete;
    // 没 End 的记录随析构丢掉,不补交:只有走完的 span 才有意义。
    ~SpanRecorder() = default;

    // context 是这个 span 自己的上下文(trace_id、本 span 的 span_id、flags);parent 全零 = 根 span。
    void Begin(const TraceContext& context, const SpanId& parent, bool parentIsRemote, SpanKind kind, SpanSystem system,
               SpanName name) noexcept;
    bool Active() const { return active_; }
    // 标成错误;说明按字节截断。没在记录时什么都不做。
    void SetError(std::string_view description) noexcept;
    void SetGrpcStatusCode(uint32_t code) noexcept;
    // 结束并交给导出去处。只生效一次;结束时导出去处已摘下则丢掉。
    void End() noexcept;

private:
    SpanRecord record_{};
    int64_t startSteadyNanos_{0};
    bool active_{false};
};

// 把当前线程上最内层的在录 span 标成错误(例如 RPC0 派发前的拒绝)。只在确知「当前在录的就是这次调用的 span」的
// 同步路径上用:异步完成路径上当前 span 可能属于别的请求,不要调用。当前没有在录的 span 时什么都不做。
void MarkCurrentSpanError(std::string_view description) noexcept;

// 出站的 client / producer span(RPC0 调用、gRPC 调用、Kafka 生产):发起时开,完成时 End;不装进线程上下文。
// Context() 是写上线的上下文(本 span 的 span_id 作对端的 parent),不论是否记录都与 parent 同一条 trace。
// 可移动,不可复制;没 End 的随析构丢掉。
class PendingSpan {
public:
    PendingSpan() = default;
    PendingSpan(PendingSpan&&) noexcept = default;
    PendingSpan& operator=(PendingSpan&&) noexcept = default;
    PendingSpan(const PendingSpan&) = delete;
    PendingSpan& operator=(const PendingSpan&) = delete;

    // parent 有效:开 parent 的子 span(新 span_id,沿用 trace_id 与 flags),装着导出去处且采样时记录。
    // parent 无效:返回空 span(Context() 无效、不记录)—— 不在 trace 里的调用不凭空开根(§3.3 后台任务不挂 trace)。
    static PendingSpan StartChildOf(const TraceContext& parent, SpanKind kind, SpanSystem system, SpanName name) noexcept;

    const TraceContext& Context() const { return context_; }
    bool Recording() const { return recorder_.Active(); }
    void SetError(std::string_view description) noexcept { recorder_.SetError(description); }
    void SetGrpcStatusCode(uint32_t code) noexcept { recorder_.SetGrpcStatusCode(code); }
    void End() noexcept { recorder_.End(); }

private:
    TraceContext context_;
    SpanRecorder recorder_;
};

// 作用域内装上一个上下文,退出时恢复进入前的 trace。
//   ScopedSpan span;            // 当前有 trace 就建它的子 span,没有就开新根。只记日志、不导出(给单测与无名的内部作用域用;
//                               // 在请求路径上不要用:它的 span_id 不会出现在 Tempo 里,挂在它下面的导出 span 会成为孤儿)
//   ScopedSpan span(captured);  // 原样装回投递时捕获的上下文(同一个 span_id),异步回调用它恢复(§4.3)
// captured 无效时作用域内没有 trace。
//
// 原样装回是 OTel 的 Context 语义(阶段三改的,原来是在捕获值下面再建子 span):回调里打的日志与发起的出站调用
// 都挂在捕获的那个 span 上,它是导出过的;要是另建一个只记日志的子 span,回调里发起的 client span 就会以它为父,
// 在 Tempo 里成了找不到父节点的孤儿。
class ScopedSpan {
public:
    ScopedSpan()
        : previous_(detail::tlsTrace), previousRecorder_(detail::tlsRecorder)
    {
        detail::tlsTrace = previous_.IsValid() ? ChildSpan(previous_) : NewRoot();
        detail::tlsRecorder = nullptr;
    }

    explicit ScopedSpan(const TraceContext& captured)
        : previous_(detail::tlsTrace), previousRecorder_(detail::tlsRecorder)
    {
        detail::tlsTrace = captured;
        detail::tlsRecorder = nullptr;
    }

    ~ScopedSpan()
    {
        detail::tlsTrace = previous_;
        detail::tlsRecorder = previousRecorder_;
    }

    ScopedSpan(const ScopedSpan&) = delete;
    ScopedSpan& operator=(const ScopedSpan&) = delete;
    ScopedSpan(ScopedSpan&&) = delete;
    ScopedSpan& operator=(ScopedSpan&&) = delete;

private:
    TraceContext previous_;
    SpanRecorder* previousRecorder_;
};

// 作用域内开一条新根(不看当前有没有 trace),退出时恢复进入前的值。只给「本进程就是起点」的入口用:
// gate 收到的客户端消息、建连与断线事件(设计 §3.3)。这些入口按定义不属于任何别的请求,
// 不接当前 trace,免得接上同一线程上残留的上下文。
// 带名字的构造另把这个根记成 server span,作用域结束时交给导出去处(§10.3)。
class ScopedRoot {
public:
    ScopedRoot()
        : previous_(detail::tlsTrace), previousRecorder_(detail::tlsRecorder)
    {
        detail::tlsTrace = NewRoot();
        detail::tlsRecorder = nullptr;
    }

    ScopedRoot(SpanSystem system, SpanName name)
        : previous_(detail::tlsTrace), previousRecorder_(detail::tlsRecorder)
    {
        detail::tlsTrace = NewRoot();
        recorder_.Begin(detail::tlsTrace, SpanId{}, false, SpanKind::kServer, system, name);
        detail::tlsRecorder = recorder_.Active() ? &recorder_ : nullptr;
    }

    ~ScopedRoot()
    {
        recorder_.End();
        detail::tlsTrace = previous_;
        detail::tlsRecorder = previousRecorder_;
    }

    void SetError(std::string_view description) noexcept { recorder_.SetError(description); }

    ScopedRoot(const ScopedRoot&) = delete;
    ScopedRoot& operator=(const ScopedRoot&) = delete;
    ScopedRoot(ScopedRoot&&) = delete;
    ScopedRoot& operator=(ScopedRoot&&) = delete;

private:
    TraceContext previous_;
    SpanRecorder* previousRecorder_;
    SpanRecorder recorder_;
};

// 作用域内接续收到的上下文:incoming 合法就在它下面建子 span,不合法就开新根;退出时恢复进入前的值。
// 给带上下文的入站用(RPC0 派发、gRPC 派发、Kafka 消费,设计 §4.2 第 2 条):不接线程上碰巧在场的上下文;
// 收到的上下文不合法也绝不拒绝请求。
// 带 kind / 名字的构造另把这个子 span 记成 server(RPC0 / gRPC)或 consumer(Kafka)span,作用域结束时交给导出去处:
// 只覆盖同步派发,handler Adopt 或守护段异步续跑之后的回复不延长它,完整耗时看调用方的 client span(§10.3)。
class ScopedIncoming {
public:
    explicit ScopedIncoming(const TraceContext& incoming)
        : previous_(detail::tlsTrace), previousRecorder_(detail::tlsRecorder)
    {
        detail::tlsTrace = incoming.IsValid() ? ChildSpan(incoming) : NewRoot();
        detail::tlsRecorder = nullptr;
    }

    ScopedIncoming(const TraceContext& incoming, SpanKind kind, SpanSystem system, SpanName name)
        : previous_(detail::tlsTrace), previousRecorder_(detail::tlsRecorder)
    {
        const bool continued = incoming.IsValid();
        detail::tlsTrace = continued ? ChildSpan(incoming) : NewRoot();
        recorder_.Begin(detail::tlsTrace, continued ? incoming.span_id : SpanId{}, continued, kind, system, name);
        detail::tlsRecorder = recorder_.Active() ? &recorder_ : nullptr;
    }

    ~ScopedIncoming()
    {
        recorder_.End();
        detail::tlsTrace = previous_;
        detail::tlsRecorder = previousRecorder_;
    }

    void SetError(std::string_view description) noexcept { recorder_.SetError(description); }

    ScopedIncoming(const ScopedIncoming&) = delete;
    ScopedIncoming& operator=(const ScopedIncoming&) = delete;
    ScopedIncoming(ScopedIncoming&&) = delete;
    ScopedIncoming& operator=(ScopedIncoming&&) = delete;

private:
    TraceContext previous_;
    SpanRecorder* previousRecorder_;
    SpanRecorder recorder_;
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

// 把一次性回调包一层:记下此刻的 trace,执行时用 ScopedSpan(captured) 原样装回,执行完恢复(设计 §4.3)。
// 此刻没有 trace 时,回调执行期间也没有 trace,不会接上执行线程上碰巧在场的上下文。
// 只用于一次性续跑:queueInLoop、runAfter、协程恢复。周期定时器(runEvery)不要用它:
// 发起它的那个请求的 trace 会挂在之后每一次 tick 上。返回的可调用对象在 callback 可拷贝时才可拷贝。
template <typename F>
auto Bind(F&& callback)
{
    return [captured = Current(), bound = std::forward<F>(callback)](auto&&... args) mutable -> decltype(auto) {
        ScopedSpan restore(captured);
        return bound(std::forward<decltype(args)>(args)...);
    };
}

} // namespace tracing
