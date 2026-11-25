// muduo_grpc::Server 与真实 grpc-go 客户端的互通用例(docs/design/grpc-muduo-transport.md §15.14 第 6 步)。
//
// 默认跳过:设 MUDUO_GRPC_SERVER_INTEROP_PORT=<port> 才运行。本进程在 127.0.0.1:<port> 上起服务并转 loop,
// 直到 Go 侧调用 Shutdown 且排空完成;等待上限 MUDUO_GRPC_SERVER_INTEROP_TIMEOUT_SEC 秒(默认 180)。
// 客户端是 go/shared/grpcinterop/muduo_server_interop_test.go(设 MUDUO_GRPC_SERVER_INTEROP_ADDR 才运行)。
// 跑法:先后台启动 `muduo_grpc_test.exe --gtest_filter=ServerInterop.*`,再在 go/shared 下执行
// `go test ./grpcinterop/ -run TestMuduoServerInterop -count=1 -v`。退出前打印一行 [GrpcServer] summary。
//
// 服务 /interop.Interop/<M>;请求与应答都是原始字节(本测试不链 protobuf,Go 侧用原始字节编解码器):
//   Echo      原样回请求字节
//   Fail      请求 "<code>|<message>" → 以该状态完成(message 原样:验证 grpc-message 百分号编码往返)
//   Defer     请求 "<ms>" → TryStartBusiness 后 ms 毫秒再回 "deferred:<ms>"(延后完成;客户端截止更短时
//             服务端截止到期回 DEADLINE_EXCEEDED,迟到的完成丢弃)
//   Metadata  回 "<key>=<value>\n" 列表,顺序同收到的顺序;-bin 的值已解掉线上那一层
//   Shutdown  回 "bye" 后在 handler 内 BeginShutdown:在途调用照常完成,新连接被拒

#include <gtest/gtest.h>

#include <charconv>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <utility>

#include "muduo_test_loop.h"

// muduo 头放在所有标准库头之后(它的 Windows 适配头 #define 了 off_t / pid_t 等名字)。
#include "muduo_grpc/method_table.h"
#include "muduo_grpc/server.h"
#include "muduo_grpc/server_call.h"
#include "muduo_grpc/status.h"

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

namespace
{

using muduo_grpc::Server;
using muduo_grpc::ServerCall;
using muduo_grpc::Status;
using muduo_grpc::StatusCode;
using namespace std::chrono_literals;

constexpr char kPortEnv[] = "MUDUO_GRPC_SERVER_INTEROP_PORT";
constexpr char kTimeoutEnv[] = "MUDUO_GRPC_SERVER_INTEROP_TIMEOUT_SEC";

std::optional<uint64_t> ParseUnsigned(std::string_view text)
{
    uint64_t value = 0;
    const auto [ptr, ec] = std::from_chars(text.data(), text.data() + text.size(), value);
    if (text.empty() || ec != std::errc{} || ptr != text.data() + text.size())
    {
        return std::nullopt;
    }
    return value;
}

struct InteropState
{
    std::weak_ptr<Server> server; // Shutdown handler 用;只弱持有
    bool shutdownRequested{false};
    bool drained{false};
};
using InteropStatePtr = std::shared_ptr<InteropState>;

class InteropService : public muduo_grpc::Service
{
public:
    InteropService(InteropStatePtr state, muduo::net::EventLoop &loop) : state(std::move(state)), loop(loop) {}

    void RegisterMethods(muduo_grpc::MethodTable &table) override
    {
        table.AddUnary("/interop.Interop/Echo", [](ServerCall call, std::string_view request)
                       {
            if (call.TryStartBusiness())
            {
                call.FinishOk(std::string(request));
            } });

        table.AddUnary("/interop.Interop/Fail", [](ServerCall call, std::string_view request)
                       {
            const size_t bar = request.find('|');
            const auto code = bar == std::string_view::npos ? std::nullopt : ParseUnsigned(request.substr(0, bar));
            if (!code || *code == 0 || *code > muduo_grpc::kMaxStatusCodeValue)
            {
                call.FinishError(Status(StatusCode::kInvalidArgument, "Fail expects \"<code>|<message>\" with code 1..16"));
                return;
            }
            call.FinishError(Status(static_cast<StatusCode>(*code), std::string(request.substr(bar + 1)))); });

        muduo::net::EventLoop &loop = this->loop;
        table.AddUnary("/interop.Interop/Defer", [&loop](ServerCall call, std::string_view request)
                       {
            const auto delayMs = ParseUnsigned(request);
            if (!delayMs || *delayMs > 60000)
            {
                call.FinishError(Status(StatusCode::kInvalidArgument, "Defer expects a delay in milliseconds (<= 60000)"));
                return;
            }
            if (!call.TryStartBusiness())
            {
                return;
            }
            // 句柄按值捕获进定时器:它只弱引用 Server(AGENTS §11.7);截止先到时这里的 FinishOk 计 late_finish。
            const uint64_t ms = *delayMs;
            loop.runAfter(static_cast<double>(ms) / 1000.0, [call, ms] { call.FinishOk("deferred:" + std::to_string(ms)); }); });

        table.AddUnary("/interop.Interop/Metadata", [](ServerCall call, std::string_view)
                       {
            std::string body;
            for (const auto &[key, value] : call.metadata())
            {
                body += key;
                body += '=';
                body += value;
                body += '\n';
            }
            call.FinishOk(std::move(body)); });

        const InteropStatePtr state = this->state;
        table.AddUnary("/interop.Interop/Shutdown", [state](ServerCall call, std::string_view)
                       {
            call.FinishOk("bye");
            if (state->shutdownRequested)
            {
                return;
            }
            state->shutdownRequested = true;
            if (const auto server = state->server.lock())
            {
                muduo_grpc::ServerShutdownOptions options;
                options.drainTimeout = 5000ms; // 足够让 Go 侧在途的 Defer 完成
                options.goawayPingTimeout = 1000ms;
                options.closeGrace = 1000ms;
                server->BeginShutdown(options, [state] { state->drained = true; });
            } });
    }

private:
    InteropStatePtr state;
    muduo::net::EventLoop &loop;
};

TEST(ServerInterop, ServesGrpcGoClientUntilShutdown)
{
    const char *portText = std::getenv(kPortEnv);
    if (portText == nullptr || *portText == '\0')
    {
        GTEST_SKIP() << "set " << kPortEnv << "=<port> to run the grpc-go server interop test";
    }
    const auto port = ParseUnsigned(portText);
    ASSERT_TRUE(port.has_value() && *port > 0 && *port <= 65535) << kPortEnv << "=" << portText;
    uint64_t timeoutSec = 180;
    if (const char *timeoutText = std::getenv(kTimeoutEnv); timeoutText != nullptr && *timeoutText != '\0')
    {
        const auto parsed = ParseUnsigned(timeoutText);
        ASSERT_TRUE(parsed.has_value() && *parsed > 0) << kTimeoutEnv << "=" << timeoutText;
        timeoutSec = *parsed;
    }

    muduo::Logger::setLogLevel(muduo::Logger::INFO);
    muduo_grpc_test::EnsureSocketsInitialized();
    muduo::net::EventLoop loop;
    const auto state = std::make_shared<InteropState>();
    InteropService service(state, loop);

    muduo_grpc::ServerOptions options;
    options.name = "grpc_interop";
    options.statsLogInterval = std::chrono::seconds(5);
    auto server = Server::Create(loop, "127.0.0.1", static_cast<uint16_t>(*port), options);
    ASSERT_NE(server, nullptr);
    ASSERT_TRUE(server->RegisterService(service));
    state->server = server;
    server->Start();
    std::cout << "[ServerInterop] serving on " << server->ListenAddress() << " until the Go client calls Shutdown"
              << std::endl;

    const bool drained = muduo_grpc_test::RunLoopUntil(loop, [&] { return state->drained; },
                                                       std::chrono::seconds(timeoutSec));
    const muduo_grpc::ServerStatsSnapshot stats = server->Stats();
    std::cout << "[GrpcServer] summary " << muduo_grpc::FormatServerStats(stats) << std::endl;
    server.reset(); // loop 外析构
    muduo_grpc_test::RunLoopFor(loop, 300ms);
    muduo::Logger::setLogLevel(muduo::Logger::WARN);

    EXPECT_TRUE(state->shutdownRequested) << "the Go client never called /interop.Interop/Shutdown";
    EXPECT_TRUE(drained) << "the server did not drain within " << timeoutSec << "s";
    EXPECT_GT(stats.dispatched, 0u);
    EXPECT_EQ(stats.dropped, 0u);
    EXPECT_EQ(stats.droppedOffLoop, 0u);
    EXPECT_EQ(stats.protocolErrors, 0u);
    EXPECT_EQ(stats.inFlight, 0u);
}

} // namespace
