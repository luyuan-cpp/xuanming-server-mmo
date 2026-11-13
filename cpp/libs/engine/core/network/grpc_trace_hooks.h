#pragma once

#include <functional>
#include <string>
#include <string_view>

#include "muduo_grpc/channel.h"
#include "muduo_grpc/metadata.h"

// gRPC 上的链路追踪(docs/design/distributed-tracing.md §5.3):接在 muduo_grpc 的两个通用钩子上。
// 线上用 W3C 的 traceparent 头,Go(go-zero)与 Java(Micrometer)都直接认,不需要任何转换。
// 两个函数都只在节点 EventLoop 线程上被 muduo_grpc 调用,读写的是该线程的日志上下文(thread_context/trace_context_tls.h)。
// 阶段三(§10.3)起两端各记一个 span:出站是 client span,入站是 server span,名字是 "pkg.Svc/Method"(OTel gRPC 约定)。

namespace grpc_tracing
{

inline constexpr std::string_view kTraceparentKey = "traceparent";

// span 名:":path" 去掉开头的 '/'("/pkg.Svc/Method" → "pkg.Svc/Method")。返回值指向 methodPath 内部。
std::string_view GrpcSpanNameOf(const std::string &methodPath);

// 业务通道的出站拦截(ChannelOptions::unaryInterceptor)。
//   * 当前线程有 trace、调用方也没写过 traceparent 时:开一个 client span(当前 span 的子 span),把它的上下文写成 traceparent,
//     对端的 server span 以它为父;调用方已经写了(转发)就原样留着,不另开 client span。
//   * 完成回调包一层:先结束 client span(记 rpc.grpc.status_code,非 OK 标成错误),再在发起时的 trace 下执行原回调,
//     与 RpcChannel 的回包一致;发起时没有 trace,回调里也没有。本地拒绝也走这个包装(拦截排在通道的所有本地检查之前)。
muduo_grpc::UnaryCallback InjectTraceIntoUnaryCall(const std::string &method, muduo_grpc::Metadata &metadata,
                                                   muduo_grpc::UnaryCallback done);

// 节点 gRPC 服务端的派发拦截(ServerOptions::dispatchInterceptor)。metadata 带合法的 traceparent 就在它下面建子 span,
// 否则开新根;这个 span 就是本次调用的 server span。作用域覆盖整个同步 handler,包括守护段;守护段发起异步步骤、
// 之后再 Proceed 的情况,由那个异步原语自己的恢复传下去,server span 不随之延长(完整耗时看调用方的 client span)。
// traceparent 不合法绝不拒绝请求。
void DispatchUnderIncomingTrace(const std::string &methodPath, const muduo_grpc::Metadata &metadata,
                                const std::function<void()> &invoke);

} // namespace grpc_tracing
