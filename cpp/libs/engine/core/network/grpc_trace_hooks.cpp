#include "core/network/grpc_trace_hooks.h"

#include <memory>
#include <string>
#include <utility>

#include "core/tracing/trace_context.h"
#include "muduo_grpc/status.h"
#include "thread_context/trace_context_tls.h"

namespace grpc_tracing
{

std::string_view GrpcSpanNameOf(const std::string &methodPath)
{
    std::string_view name(methodPath);
    if (!name.empty() && name.front() == '/')
    {
        name.remove_prefix(1);
    }
    return name;
}

muduo_grpc::UnaryCallback InjectTraceIntoUnaryCall(const std::string &method, muduo_grpc::Metadata &metadata,
                                                   muduo_grpc::UnaryCallback done)
{
    tracing::PendingSpan span;
    if (muduo_grpc::FindMetadata(metadata, kTraceparentKey) == nullptr)
    {
        span = tracing::PendingSpan::StartChildOf(tracing::Current(), tracing::SpanKind::kClient,
                                                  tracing::SpanSystem::kGrpc, tracing::SpanName{GrpcSpanNameOf(method)});
        if (span.Context().IsValid())
        {
            metadata.emplace_back(std::string(kTraceparentKey), span.Context().ToTraceparent());
        }
    }
    if (!span.Recording())
    {
        return tracing::Bind(std::move(done));
    }
    // UnaryCallback 是 std::function,要求可拷贝;PendingSpan 只能移动,放进 shared_ptr。只有真在记录时才分配。
    auto recorded = std::make_shared<tracing::PendingSpan>(std::move(span));
    return tracing::Bind([recorded, done = std::move(done)](muduo_grpc::UnaryCallResult &&result) mutable
                         {
        recorded->SetGrpcStatusCode(result.status.code_value());
        if (!result.status.ok())
        {
            recorded->SetError(muduo_grpc::StatusCodeName(result.status.code()));
        }
        recorded->End();
        done(std::move(result)); });
}

void DispatchUnderIncomingTrace(const std::string &methodPath, const muduo_grpc::Metadata &metadata,
                                const std::function<void()> &invoke)
{
    tracing::TraceContext incoming;
    if (const std::string *header = muduo_grpc::FindMetadata(metadata, kTraceparentKey); header != nullptr)
    {
        // 解析失败时 incoming 保持无效,下面开新根。
        (void)tracing::ParseTraceparent(*header, incoming);
    }
    const tracing::ScopedIncoming dispatchTrace(incoming, tracing::SpanKind::kServer, tracing::SpanSystem::kGrpc,
                                                tracing::SpanName{GrpcSpanNameOf(methodPath)});
    invoke();
}

} // namespace grpc_tracing
