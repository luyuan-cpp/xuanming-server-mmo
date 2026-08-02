#pragma once

#ifdef __linux__
#include <absl/log/initialize.h>
#include <csignal>
#else
#include "grpc/third_party/abseil-cpp/absl/log/initialize.h"
#include <Windows.h>
#endif
#include "muduo/net/EventLoop.h"
#include "node/system/node/node.h"
#include "node/system/node/node_kafka_command_handler.h"
#include "table/code/all_table.h"
#include "thread_context/ecs_context.h"
#include "thread_context/redis_manager.h"

#include <google/protobuf/stubs/common.h>

#include <memory>
#include <type_traits>
#include <utility>

// Every node executable must define this function (typically generated
// in handler/event/event_handler.cpp by the code generator).
void RegisterNodeEvents();

namespace node {
namespace entry {

// ── THooks convention ──────────────────────────────────────────────
// A plain struct with optional nested types.  Only declare what you need.
//
//   struct MyNodeHooks {
//       struct TableLoadHandler { static void OnLoaded(); };
//       using KafkaCommandType = contracts::kafka::MyNodeCommand;
//   };
//
// Omit any member to skip that hook.  Pass void (the default) to skip all.
// ────────────────────────────────────────────────────────────────────

namespace detail {

template <typename T, typename = void>
struct has_table_load_handler : std::false_type {};
template <typename T>
struct has_table_load_handler<T, std::void_t<typename T::TableLoadHandler>> : std::true_type {};

template <typename T, typename = void>
struct has_kafka_command_type : std::false_type {};
template <typename T>
struct has_kafka_command_type<T, std::void_t<typename T::KafkaCommandType>> : std::true_type {};

template <typename THooks>
void ApplyPreConstructionHooks()
{
    if constexpr (!std::is_void_v<THooks> && has_table_load_handler<THooks>::value) {
        OnTablesLoadSuccess([] { THooks::TableLoadHandler::OnLoaded(); });
    }
}

template <typename THooks>
void ApplyPostConstructionHooks(Node& node)
{
    ::RegisterNodeEvents();
    if constexpr (!std::is_void_v<THooks> && has_kafka_command_type<THooks>::value) {
        node.SetKafkaHandlers([](Node& n) {
            return kafka::RegisterKafkaCommandHandler<typename THooks::KafkaCommandType>(n);
        });
    }
}

} // namespace detail

// ── Entry helper ──────────────────────────────────────────────────

namespace detail
{

#ifdef __linux__
    inline volatile std::sig_atomic_t gShutdownSignalPending = 0;

    // POSIX 信号上下文只允许异步信号安全操作;Node::Shutdown()、
    // 日志、内存分配和 EventLoop 唤醒都禁止在 handler 内执行。
    inline void HandleShutdownSignal(int) noexcept
    {
        gShutdownSignalPending = 1;
    }
#endif

    // 安装操作系统信号 handler, SIGTERM/SIGINT 转入优雅停机。
    inline void InstallSignalHandlers(muduo::net::EventLoop &loop)
    {
#ifdef __linux__
        // 在正常 EventLoop 上下文轮询 sig_atomic_t 标志。100ms 有界延迟
        // 换来实际信号 handler 严格异步信号安全。
        loop.runEvery(0.1, [&loop]
                      {
            if (gShutdownSignalPending == 0)
            {
                return;
            }
            gShutdownSignalPending = 0;
            if (gNode)
            {
                gNode->RequestShutdown();
            }
            else
            {
                loop.quit();
            } });
        ::signal(SIGTERM, &HandleShutdownSignal);
        ::signal(SIGINT, &HandleShutdownSignal);
#else
        // Windows: use SetConsoleCtrlHandler for Ctrl+C / service stop.
        static muduo::net::EventLoop *sLoop = &loop;
        ::SetConsoleCtrlHandler([](DWORD ctrlType) -> BOOL
                                {
        if (ctrlType == CTRL_C_EVENT || ctrlType == CTRL_CLOSE_EVENT ||
            ctrlType == CTRL_SHUTDOWN_EVENT) {
            // Node::Shutdown() 会在 gRPC drain 与其余 teardown 都完成后 quit。
            // 这里不能紧接着提前 quit,否则 loop 不再消费在途 gRPC handler 的
            // runInLoop 任务,关机协调线程会重新卡住。
            if (gNode) {
                gNode->Shutdown();
            } else if (sLoop) {
                sLoop->quit();
            }
            return TRUE;
        }
        return FALSE; }, TRUE);
#endif
    }

} // namespace detail (signal)

template <typename StartNodeFn>
int RunNodeMain(StartNodeFn&& startNode)
{
    absl::InitializeLog();
    muduo::net::EventLoop loop;
    startNode(loop);
    // 兜底处理未经 Node::FinalizeShutdownInLoop() 就离开 loop 的入口;
    // 调用幂等,Hiredis Channel 必须在 EventLoop 析构前释放。
    tlsRedis.Shutdown();
    tlsEcs.Clear();
    google::protobuf::ShutdownProtobufLibrary();
    return 0;
}

template <typename THandler, typename TContext, typename THooks = void, typename ConfigureFn>
int RunSimpleNodeMainWithOwnedContext(uint32_t nodeType,
                                      Node::CanConnectNodeTypeList connectTo,
                                      ConfigureFn&& configure)
{
    return RunNodeMain([
        nodeType,
        connectTo = std::move(connectTo),
        configure = std::forward<ConfigureFn>(configure)
    ](muduo::net::EventLoop& loop) mutable {
        auto context = std::make_unique<TContext>();
        detail::ApplyPreConstructionHooks<THooks>();
        THandler handler;
        Node node(&loop, nodeType, std::move(connectTo), &handler);
        detail::ApplyPostConstructionHooks<THooks>(node);
        detail::InstallSignalHandlers(loop);
        configure(node, *context);
        loop.loop();
    });
}

} // namespace entry
} // namespace node
