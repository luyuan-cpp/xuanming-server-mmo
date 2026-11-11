#pragma once

// 生成的 gRPC 客户端在节点实体上的运行期接缝(docs/design/grpc-muduo-transport.md §14)。
//
// 第 3 批起传输是 muduo_grpc::Channel(实体上的 MuduoGrpcChannelPtr 组件);官方 CompletionQueue、
// GrpcClientRuntime 计数与 5ms 轮询都已删除。只在节点所属 EventLoop 线程使用。

#include <functional>
#include <memory>
#include <utility>

#include "entt/src/entt/entity/registry.hpp"
#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "muduo_grpc/channel.h"
// 用 engine 根目录相对路径:生成的客户端(grpc_client 工程)只把 cpp/libs/engine 放进包含路径。
#include "core/node/system/node/muduo_grpc_channel_slot.h"
#include "thread_context/trace_context_tls.h"

// 实体上的通道;实体无效或没挂通道返回空。
inline MuduoGrpcChannelPtr FindGrpcChannel(const entt::registry &registry, entt::entity entity)
{
    if (!registry.valid(entity))
    {
        return nullptr;
    }
    const auto *slot = registry.try_get<MuduoGrpcChannelPtr>(entity);
    return slot != nullptr ? *slot : nullptr;
}

// 这个节点实体现在能不能发 gRPC:挂着通道且通道没关。不代表已经连上(选点按 ConnectivityState 分级另看)。
inline bool HasUsableGrpcClient(const entt::registry &registry, entt::entity entity)
{
    const auto channel = FindGrpcChannel(registry, entity);
    return channel && channel->GetState(false) != muduo_grpc::ConnectivityState::kShutdown;
}

namespace grpc_client_runtime_detail
{
// 已排队、尚未执行完的本地完成数。一个线程只有一个当前 EventLoop;不放进 ECS:恰恰是缺通道 / 节点已拆时才会有。
inline thread_local size_t localCompletions{0};
} // namespace grpc_client_runtime_detail

// 关机拆除前的判据(Node::FinalizeTeardownInLoop):拆除过程中(Kafka / Redis 收尾回调里)还可能有人 Send,
// 那些本地失败排在拆除之后才执行;不为 0 时拆除要让一轮,等它们在 ECS / 日志仍活着时跑完。
[[nodiscard]] inline bool GrpcClientLocalCompletionsDrained()
{
    if (muduo::net::EventLoop::getEventLoopOfCurrentThread() == nullptr)
    {
        LOG_FATAL << "gRPC completion drain must be checked on its owning EventLoop thread";
    }
    return grpc_client_runtime_detail::localCompletions == 0;
}

// 本地就失败的调用(实体没有通道、请求序列化失败)与真实应答一样延后到下一轮投递:
// 保持「完成回调永不在 Send 栈内同步执行」。complete 捕获调用自己的状态副本,不借用 registry / 节点实体。
// 这条路不经过 muduo_grpc 通道的出站拦截,所以在这里用 tracing::Bind 包一层:回调在发起时的 trace 下执行,
// 与真实应答一致(docs/design/distributed-tracing.md §5.3)。
inline void QueueGrpcLocalCompletion(std::function<void()> complete)
{
    auto *loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
    if (loop == nullptr)
    {
        LOG_FATAL << "gRPC client must be used on its owning EventLoop thread";
    }
    ++grpc_client_runtime_detail::localCompletions;
    try
    {
        loop->queueInLoop([complete = tracing::Bind(std::move(complete))]() mutable
                          {
            struct CompletionFinished
            {
                ~CompletionFinished() { --grpc_client_runtime_detail::localCompletions; }
            } finished;
            complete(); });
    }
    catch (...)
    {
        --grpc_client_runtime_detail::localCompletions;
        throw;
    }
}
