#pragma once

#include <functional>
#include <string_view>

#include "muduo_grpc/channel.h"
#include "muduo_grpc/metadata.h"

// gRPC 上的链路追踪(docs/design/distributed-tracing.md §5.3):接在 muduo_grpc 的两个通用钩子上。
// 线上用 W3C 的 traceparent 头,Go(go-zero)与 Java(Micrometer)都直接认,不需要任何转换。
// 两个函数都只在节点 EventLoop 线程上被 muduo_grpc 调用,读写的是该线程的日志上下文(thread_context/trace_context_tls.h)。

namespace grpc_tracing
{

inline constexpr std::string_view kTraceparentKey = "traceparent";

// 业务通道的出站拦截(ChannelOptions::unaryInterceptor)。
//   * 当前线程有 trace 时往 metadata 写 traceparent;调用方已经写了就不重复写。
//   * 完成回调用 tracing::Bind 包一层,在发起时的 trace 下执行,与 RpcChannel 的回包一致;发起时没有 trace,回调里也没有。
//     本地拒绝也走这个包装(拦截排在通道的所有本地检查之前)。
muduo_grpc::UnaryCallback InjectTraceIntoUnaryCall(muduo_grpc::Metadata &metadata, muduo_grpc::UnaryCallback done);

// 节点 gRPC 服务端的派发拦截(ServerOptions::dispatchInterceptor)。metadata 带合法的 traceparent 就在它下面建子 span,
// 否则开新根;作用域覆盖整个同步 handler,包括守护段。守护段发起异步步骤、之后再 Proceed 的情况,由那个异步原语
// 自己的恢复传下去。traceparent 不合法绝不拒绝请求。
void DispatchUnderIncomingTrace(const muduo_grpc::Metadata &metadata, const std::function<void()> &invoke);

} // namespace grpc_tracing
