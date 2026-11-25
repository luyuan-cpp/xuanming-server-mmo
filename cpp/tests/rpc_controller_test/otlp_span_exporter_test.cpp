// otlp_span_exporter_test —— span 导出器的回环用例(docs/design/distributed-tracing.md §10.3)。
//
// 导出器与一个假采集器(muduo TcpServer,按 HTTP/1.1 收 POST、按模式回响应)跑在同一个测试 EventLoopThread 上,
// 与节点里导出器挂在主 loop 上同一种用法。被锁定的契约:
//   * 按周期发、攒够一批提前发;请求是 POST /v1/traces + application/x-protobuf,正文解析得回原来的 span;
//   * 长连接跨批复用;对端 chunked + Connection: close 时这一批照样算成功,下一批重连;
//   * 采集器拒收(503):丢这一批、计数、不重试、不退避;连不上:丢这一批并退避,退避期间到点丢一批最旧的(只算 span 不算批次);
//   * 超过导出时限:丢这一批并关掉连接;
//   * 停机:有界冲刷剩下的;对端不回就在停机时限到点结束;没冲刷就析构时,剩下的按停机丢弃计数。

// 标准库与系统头必须先于 muduo 头(同 muduo_grpc_test/muduo_test_loop.cpp)。
#include <atomic>
#include <chrono>
#include <cstdint>
#include <functional>
#include <future>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <vector>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#include <windows.h>
#else
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>
#endif

#include <gtest/gtest.h>

#include "core/tracing/otlp_span_exporter.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "proto/opentelemetry/proto/collector/trace/v1/trace_service.pb.h"
#include "span_test_support.h"

namespace {

using namespace std::chrono_literals;

uint16_t FindFreeLoopbackPort()
{
#ifdef _WIN32
    static const bool initialized = [] {
        WSADATA data{};
        return WSAStartup(MAKEWORD(2, 2), &data) == 0;
    }();
    (void)initialized;
    const SOCKET s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (s == INVALID_SOCKET) {
        return 0;
    }
    int len = sizeof(sockaddr_in);
#else
    const int s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (s < 0) {
        return 0;
    }
    socklen_t len = sizeof(sockaddr_in);
#endif
    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = 0;
    uint16_t port = 0;
    if (::bind(s, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) == 0 &&
        ::getsockname(s, reinterpret_cast<sockaddr*>(&addr), &len) == 0) {
        port = ntohs(addr.sin_port);
    }
#ifdef _WIN32
    ::closesocket(s);
#else
    ::close(s);
#endif
    return port;
}

// 假采集器收到的一个请求。
struct ReceivedRequest {
    std::string head;   // 请求行加各头
    std::string body;
};

// 假采集器的共享状态:muduo 会把回调拷到连接上,服务器析构之后回调仍可能来,所以回调只持有它,不持有采集器。
struct CollectorState {
    enum class Mode {
        kOk,
        kServiceUnavailable,
        kNeverRespond,
        kChunkedThenClose,
    };

    std::atomic<Mode> mode{Mode::kOk};
    std::atomic<int> accepted{0};
    std::atomic<int> closed{0};
    mutable std::mutex mutex;
    std::vector<ReceivedRequest> requests;

    size_t RequestCount() const
    {
        std::lock_guard<std::mutex> lock(mutex);
        return requests.size();
    }

    std::vector<ReceivedRequest> Requests() const
    {
        std::lock_guard<std::mutex> lock(mutex);
        return requests;
    }
};

// 从缓冲里取出完整的请求(可能一次到多个,也可能只到一半),按模式回响应。
void ServeRequests(const std::shared_ptr<CollectorState>& state, const muduo::net::TcpConnectionPtr& conn,
                   muduo::net::Buffer* buffer)
{
    for (;;) {
        const std::string_view data(buffer->peek(), buffer->readableBytes());
        const size_t headEnd = data.find("\r\n\r\n");
        if (headEnd == std::string_view::npos) {
            return;
        }
        const std::string_view head = data.substr(0, headEnd);
        size_t contentLength = 0;
        constexpr std::string_view kLengthHeader = "\r\nContent-Length: ";
        if (const size_t at = head.find(kLengthHeader); at != std::string_view::npos) {
            contentLength = std::stoul(std::string(head.substr(at + kLengthHeader.size())));
        }
        if (data.size() < headEnd + 4 + contentLength) {
            return;
        }
        {
            std::lock_guard<std::mutex> lock(state->mutex);
            state->requests.push_back(
                ReceivedRequest{std::string(head), std::string(data.substr(headEnd + 4, contentLength))});
        }
        buffer->retrieve(headEnd + 4 + contentLength);
        switch (state->mode.load()) {
        case CollectorState::Mode::kOk:
            conn->send("HTTP/1.1 200 OK\r\nContent-Type: application/x-protobuf\r\nContent-Length: 0\r\n\r\n");
            break;
        case CollectorState::Mode::kServiceUnavailable:
            conn->send("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n");
            break;
        case CollectorState::Mode::kNeverRespond:
            break;
        case CollectorState::Mode::kChunkedThenClose:
            conn->send("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n0\r\n\r\n");
            conn->shutdown();
            return;
        }
    }
}

class OtlpSpanExporterLoopbackTest : public ::testing::Test {
protected:
    void SetUp() override
    {
        loop = std::ref(*loopThread.startLoop());
        port = FindFreeLoopbackPort();
        ASSERT_NE(port, 0);
        queue = std::make_unique<tracing::SpanExportQueue>(64, 512);
    }

    void TearDown() override
    {
        RunInLoopAndWait([this] {
            exporter.reset();
            server.reset();
        });
        // 排空 ~TcpServer / 连接释放排进队列的收尾(连接对象析构、forceCloseInLoop、connectDestroyed)。
        RunInLoopAndWait([] {});
        RunInLoopAndWait([] {});
        RunInLoopAndWait([] {});
    }

    muduo::net::EventLoop& Loop() { return loop->get(); }

    template <typename F>
    void RunInLoopAndWait(F&& work)
    {
        std::promise<void> done;
        Loop().runInLoop([&] {
            work();
            done.set_value();
        });
        done.get_future().wait();
    }

    static bool WaitUntil(const std::function<bool()>& condition, std::chrono::milliseconds timeout = 5000ms)
    {
        const auto deadline = std::chrono::steady_clock::now() + timeout;
        while (std::chrono::steady_clock::now() < deadline) {
            if (condition()) {
                return true;
            }
            std::this_thread::sleep_for(10ms);
        }
        return condition();
    }

    void StartCollector(CollectorState::Mode mode)
    {
        collector->mode = mode;
        RunInLoopAndWait([this] {
            server = std::make_unique<muduo::net::TcpServer>(&Loop(), muduo::net::InetAddress("127.0.0.1", port),
                                                              "fake-otlp-collector");
            const std::shared_ptr<CollectorState> state = collector;
            server->setConnectionCallback([state](const muduo::net::TcpConnectionPtr& conn) {
                if (conn->connected()) {
                    ++state->accepted;
                } else {
                    ++state->closed;
                }
            });
            server->setMessageCallback(
                [state](const muduo::net::TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp) {
                    ServeRequests(state, conn, buffer);
                });
            server->start();
        });
    }

    void StartExporter(const std::function<void(tracing::OtlpSpanExporterOptions&)>& tweak = {})
    {
        tracing::OtlpSpanExporterOptions options;
        const auto endpoint = tracing::ParseOtlpTracesEndpoint("http://127.0.0.1:" + std::to_string(port) + "/v1/traces");
        ASSERT_TRUE(endpoint.has_value());
        options.endpoint = *endpoint;
        const auto address = tracing::ResolveOtlpEndpoint(options.endpoint);   // IPv4 字面量:不做 DNS
        ASSERT_TRUE(address.has_value());
        options.address = *address;
        options.resource.serviceName = "test";
        options.scheduleDelay = 50ms;
        options.exportTimeout = 2000ms;
        options.shutdownTimeout = 2000ms;
        options.initialBackoff = 2000ms;
        options.maxBackoff = 4000ms;
        if (tweak) {
            tweak(options);
        }
        // 导出器只在 loop 线程上构造与使用,与节点里挂在主 loop 上一样。
        RunInLoopAndWait([&] {
            exporter = std::make_unique<tracing::OtlpSpanExporter>(Loop(), *queue, stats, std::move(options));
            exporter->Start();
        });
    }

    void Submit(uint8_t firstTag, int count)
    {
        for (int i = 0; i < count; ++i) {
            queue->Submit(span_test::MakeSpanRecord(static_cast<uint8_t>(firstTag + i)));
        }
    }

    bool ExporterFinished()
    {
        bool finished = false;
        RunInLoopAndWait([&] { finished = exporter->Finished(); });
        return finished;
    }

    static std::vector<std::string> SpanNamesOf(const ReceivedRequest& request)
    {
        ::opentelemetry::proto::collector::trace::v1::ExportTraceServiceRequest decoded;
        std::vector<std::string> names;
        if (!decoded.ParseFromString(request.body)) {
            return names;
        }
        for (const auto& resourceSpans : decoded.resource_spans()) {
            for (const auto& scopeSpans : resourceSpans.scope_spans()) {
                for (const auto& span : scopeSpans.spans()) {
                    names.push_back(span.name());
                }
            }
        }
        return names;
    }

    uint64_t Exported() const { return stats.exportedSpans.load(); }
    uint64_t Failed() const { return stats.failedSpans.load(); }

    // loopThread_ 排第一:最后析构,其余成员析构时 loop 仍在。
    muduo::net::EventLoopThread loopThread;
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop;   // 借用 loopThread_ 的 loop
    uint16_t port{0};
    std::shared_ptr<CollectorState> collector{std::make_shared<CollectorState>()};
    std::unique_ptr<muduo::net::TcpServer> server;
    std::unique_ptr<tracing::SpanExportQueue> queue;
    tracing::SpanExportStats stats;
    std::unique_ptr<tracing::OtlpSpanExporter> exporter;
};

} // namespace

TEST_F(OtlpSpanExporterLoopbackTest, ExportsOnTheScheduleWithTheOtlpHttpContract)
{
    StartCollector(CollectorState::Mode::kOk);
    StartExporter();
    Submit(1, 3);
    ASSERT_TRUE(WaitUntil([&] { return Exported() == 3; })) << "exported=" << Exported() << " failed=" << Failed();
    EXPECT_EQ(stats.exportedBatches.load(), 1u);

    const auto requests = collector->Requests();
    ASSERT_EQ(requests.size(), 1u);
    EXPECT_EQ(requests[0].head.rfind("POST /v1/traces HTTP/1.1\r\n", 0), 0u) << requests[0].head;
    EXPECT_NE(requests[0].head.find("\r\nContent-Type: application/x-protobuf"), std::string::npos);
    EXPECT_NE(requests[0].head.find("\r\nHost: 127.0.0.1:" + std::to_string(port)), std::string::npos);
    EXPECT_EQ(SpanNamesOf(requests[0]), (std::vector<std::string>{"span-1", "span-2", "span-3"}));
    EXPECT_EQ(queue->Size(), 0u);
}

TEST_F(OtlpSpanExporterLoopbackTest, ExportsEarlyWhenABatchIsFullWithoutWaitingForTheSchedule)
{
    queue = std::make_unique<tracing::SpanExportQueue>(64, 4);
    StartCollector(CollectorState::Mode::kOk);
    StartExporter([](tracing::OtlpSpanExporterOptions& options) {
        options.scheduleDelay = 60000ms;   // 周期到不了:只能靠攒够一批的提前发
        options.maxExportBatchSize = 4;
    });
    Submit(1, 4);
    ASSERT_TRUE(WaitUntil([&] { return Exported() == 4; }));
    Submit(5, 8);   // 发完一批还够一批:接着发,不等周期
    ASSERT_TRUE(WaitUntil([&] { return Exported() == 12; })) << "exported=" << Exported();
    const auto requests = collector->Requests();
    ASSERT_EQ(requests.size(), 3u);
    for (const auto& request : requests) {
        EXPECT_EQ(SpanNamesOf(request).size(), 4u);
    }
}

TEST_F(OtlpSpanExporterLoopbackTest, ReusesOneConnectionAcrossBatches)
{
    StartCollector(CollectorState::Mode::kOk);
    StartExporter();
    Submit(1, 2);
    ASSERT_TRUE(WaitUntil([&] { return Exported() == 2; }));
    Submit(3, 2);
    ASSERT_TRUE(WaitUntil([&] { return Exported() == 4; }));
    EXPECT_EQ(collector->RequestCount(), 2u);
    EXPECT_EQ(collector->accepted.load(), 1) << "HTTP/1.1 长连接,两批同一条连接";
}

TEST_F(OtlpSpanExporterLoopbackTest, ChunkedResponseWithConnectionCloseSucceedsAndTheNextBatchReconnects)
{
    StartCollector(CollectorState::Mode::kChunkedThenClose);
    StartExporter();
    Submit(1, 1);
    ASSERT_TRUE(WaitUntil([&] { return Exported() == 1; }));
    Submit(2, 1);
    ASSERT_TRUE(WaitUntil([&] { return Exported() == 2; }));
    EXPECT_EQ(collector->accepted.load(), 2);
    EXPECT_EQ(Failed(), 0u);
}

TEST_F(OtlpSpanExporterLoopbackTest, CollectorRejectionDropsTheBatchWithoutRetryOrBackoff)
{
    StartCollector(CollectorState::Mode::kServiceUnavailable);
    StartExporter();
    Submit(1, 3);
    ASSERT_TRUE(WaitUntil([&] { return Failed() == 3; }));
    EXPECT_EQ(stats.failedBatches.load(), 1u);
    std::this_thread::sleep_for(300ms);   // 好几个周期过去,也不重发这一批
    EXPECT_EQ(collector->RequestCount(), 1u);
    Submit(4, 2);   // 对端是通的,不退避:下一批照常发
    ASSERT_TRUE(WaitUntil([&] { return collector->RequestCount() == 2; }));
    ASSERT_TRUE(WaitUntil([&] { return Failed() == 5; }));
    EXPECT_EQ(Exported(), 0u);
}

TEST_F(OtlpSpanExporterLoopbackTest, UnreachableCollectorDropsTheBatchAndBacksOff)
{
    StartExporter();   // 没起采集器:端口上没人监听
    Submit(1, 3);
    ASSERT_TRUE(WaitUntil([&] { return Failed() == 3; }));
    EXPECT_EQ(stats.failedBatches.load(), 1u);
    Submit(4, 3);   // 退避 2 秒内:到点丢一批最旧的,不再尝试连接
    ASSERT_TRUE(WaitUntil([&] { return Failed() == 6; }, 1000ms));
    EXPECT_EQ(stats.failedBatches.load(), 1u) << "退避期间的丢弃只算 span,不算导出批次";
    EXPECT_EQ(queue->Size(), 0u) << "队列不会顶满";
}

TEST_F(OtlpSpanExporterLoopbackTest, ExportTimeoutDropsTheBatchAndClosesTheConnection)
{
    StartCollector(CollectorState::Mode::kNeverRespond);
    StartExporter([](tracing::OtlpSpanExporterOptions& options) { options.exportTimeout = 200ms; });
    Submit(1, 2);
    ASSERT_TRUE(WaitUntil([&] { return Failed() == 2; }));
    EXPECT_EQ(stats.failedBatches.load(), 1u);
    ASSERT_TRUE(WaitUntil([&] { return collector->closed.load() >= 1; })) << "超时的连接要关掉,不再复用";
}

TEST_F(OtlpSpanExporterLoopbackTest, ShutdownFlushesWhatIsLeft)
{
    StartCollector(CollectorState::Mode::kOk);
    StartExporter([](tracing::OtlpSpanExporterOptions& options) { options.scheduleDelay = 60000ms; });
    Submit(1, 5);
    RunInLoopAndWait([this] { exporter->BeginShutdown(); });
    ASSERT_TRUE(WaitUntil([&] { return ExporterFinished(); }));
    EXPECT_EQ(Exported(), 5u);
    EXPECT_EQ(collector->RequestCount(), 1u);
    EXPECT_EQ(queue->DroppedOnShutdown(), 0u);
}

TEST_F(OtlpSpanExporterLoopbackTest, ShutdownEndsAtItsTimeoutWhenTheCollectorNeverAnswers)
{
    StartCollector(CollectorState::Mode::kNeverRespond);
    StartExporter([](tracing::OtlpSpanExporterOptions& options) {
        options.scheduleDelay = 60000ms;
        options.exportTimeout = 30000ms;
        options.shutdownTimeout = 300ms;
    });
    Submit(1, 2);
    const auto started = std::chrono::steady_clock::now();
    RunInLoopAndWait([this] { exporter->BeginShutdown(); });
    ASSERT_TRUE(WaitUntil([&] { return ExporterFinished(); }, 3000ms));
    EXPECT_LT(std::chrono::steady_clock::now() - started, 2000ms) << "停机冲刷以 shutdownTimeout 为限";
    EXPECT_EQ(Failed(), 2u);
}

TEST_F(OtlpSpanExporterLoopbackTest, DestroyingBeforeTheFlushCountsLeftoversAsShutdownDrops)
{
    StartCollector(CollectorState::Mode::kOk);
    StartExporter([](tracing::OtlpSpanExporterOptions& options) { options.scheduleDelay = 60000ms; });
    Submit(1, 3);
    RunInLoopAndWait([this] { exporter.reset(); });
    EXPECT_EQ(queue->DroppedOnShutdown(), 3u);
    EXPECT_EQ(Exported(), 0u);
    Submit(4, 1);   // 导出器没了:回调已摘,放进来的只留在队列里
    EXPECT_EQ(queue->Size(), 1u);
}
