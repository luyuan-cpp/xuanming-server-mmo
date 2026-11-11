#include "core/network/grpc_trace_hooks.h"

#include <string>
#include <utility>

#include "core/tracing/trace_context.h"
#include "thread_context/trace_context_tls.h"

namespace grpc_tracing
{

muduo_grpc::UnaryCallback InjectTraceIntoUnaryCall(muduo_grpc::Metadata &metadata, muduo_grpc::UnaryCallback done)
{
    const tracing::TraceContext &current = tracing::Current();
    if (current.IsValid() && muduo_grpc::FindMetadata(metadata, kTraceparentKey) == nullptr)
    {
        metadata.emplace_back(std::string(kTraceparentKey), current.ToTraceparent());
    }
    return tracing::Bind(std::move(done));
}

void DispatchUnderIncomingTrace(const muduo_grpc::Metadata &metadata, const std::function<void()> &invoke)
{
    tracing::TraceContext incoming;
    if (const std::string *header = muduo_grpc::FindMetadata(metadata, kTraceparentKey); header != nullptr)
    {
        // 解析失败时 incoming 保持无效,下面开新根。
        (void)tracing::ParseTraceparent(*header, incoming);
    }
    const tracing::ScopedIncoming dispatchTrace(incoming);
    invoke();
}

} // namespace grpc_tracing
