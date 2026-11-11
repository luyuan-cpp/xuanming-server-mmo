// muduo_grpc::Server 回环单测(docs/design/grpc-muduo-transport.md §15.14 第 5 步)。
//
// 测试线程自己建 muduo EventLoop;同一个 loop 上跑被测 Server 和客户端,用 RunLoopUntil(loop.runEvery + quit)驱动,
// 不起线程、不死循环。客户端两种:
//   * muduo_grpc::Channel —— 标准 gRPC 客户端(与生产上 Go 调用方同一套线上字节);
//   * RawConnection —— muduo TcpClient + 内存 nghttp2 客户端(TestH2Client)或裸字节:发 Channel 永远不会发的东西
//     (RST_STREAM、grpc-timeout 0、HTTP/1.1、只连不发、只发 magic、停止读取造成背压、用很小的流控窗口卡住应答、
//     请求体只传一半),并直接观察线上的 GOAWAY / RST 以及 EOF 之前到达了哪些帧。
//
// 生命周期(AGENTS.md §11.7):RawConnection 只存 weak_ptr<TcpConnection>,绑进 TcpClient 回调的是 weak_ptr<自己>。
// handler 只捕获 shared_ptr<HandlerState> 与 loop 引用;保存的 ServerCall 只弱引用 Server。
// TearDown 先清掉保存的句柄、转空 loop,再在 loop 外析构 Server(server.h「析构前置」允许),最后再转一次 loop
// 让 forceClose / connectDestroyed 跑完,EventLoop 最后析构。

#include <gtest/gtest.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <functional>
#include <map>
#include <memory>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "muduo_test_loop.h"
#include "test_h2_client.h"

// muduo 头放在所有标准库头之后(它的 Windows 适配头 #define 了 off_t / pid_t 等名字)。
#include "muduo_grpc/channel.h"
#include "muduo_grpc/metadata.h"
#include "muduo_grpc/method_table.h"
#include "muduo_grpc/server.h"
#include "muduo_grpc/server_call.h"
#include "muduo_grpc/status.h"

#include "muduo/base/Logging.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"

namespace
{

using muduo_grpc::Channel;
using muduo_grpc::Metadata;
using muduo_grpc::Server;
using muduo_grpc::ServerCall;
using muduo_grpc::ServerOptions;
using muduo_grpc::ServerShutdownOptions;
using muduo_grpc::ServerStatsSnapshot;
using muduo_grpc::Status;
using muduo_grpc::StatusCode;
using muduo_grpc::UnaryCallRequest;
using muduo_grpc::UnaryCallResult;
using muduo_grpc_test::HeaderList;
using muduo_grpc_test::TestH2Client;
using muduo_grpc_test::TestH2ClientOptions;
using muduo_grpc_test::TestH2Peer;
using H2Stream = muduo_grpc_test::ClientStream;
using namespace std::chrono_literals;
namespace h2 = muduo_grpc_test::h2;

constexpr char kServiceName[] = "test.Echo";
constexpr char kEchoPath[] = "/test.Echo/Echo";
constexpr char kHoldPath[] = "/test.Echo/Hold";                     // 开始业务后保存句柄,不完成
constexpr char kHoldNotStartedPath[] = "/test.Echo/HoldNotStarted"; // 只保存句柄(模拟等 Agones 许可)
constexpr char kDropPath[] = "/test.Echo/Drop";                     // 句柄直接丢掉
constexpr char kTwicePath[] = "/test.Echo/Twice";                   // 连续完成三次
constexpr char kOrderPath[] = "/test.Echo/Order";                   // 记录派发顺序与所在 loop 轮次
// gate 的 x-session-detail-bin 应用层值(线上再编一层 base64,服务端只解线上那层)。
constexpr char kSessionAppValue[] = "CgQIARABEgIIAQ==";
// battle 守护段的跨语言契约文案:服务端自己产生的 UNAVAILABLE 永远不能等于它(server.h 线上不变量)。
constexpr char kBattleNotAllocatable[] = "battle_not_allocatable";
// 背压用例的应答大小(见夹具 StartBackpressuredCalls)与 HTTP/2 流控窗口上限(RFC 9113 §6.9.1:2^31-1)。
constexpr size_t kBackpressureResponseBytes = 32 * 1024 * 1024;
constexpr int32_t kMaxWindowBytes = 0x7fffffff;
// 流控用例:客户端流级接收窗口只有 16KiB,应答 256KiB —— 窗口内的一段发出之后,剩余 DATA 与 trailers 被 HTTP/2 流控
// 卡在服务端的 nghttp2 里(不是卡在输出缓冲:16KiB 加控制帧远小于回环上的内核缓冲)。
constexpr uint32_t kSmallStreamWindowBytes = 16 * 1024;
constexpr size_t kFlowControlledResponseBytes = 256 * 1024;

// ── 被测服务 ─────────────────────────────────────────────────────────────

struct HandlerState
{
    std::vector<ServerCall> held;
    size_t invocations{0};
    std::string lastMethod;
    std::string lastPeer;
    Metadata lastMetadata;
    bool lastHadDeadline{false};
    std::vector<uint64_t> orderIds;
    std::vector<int64_t> orderIterations;
};
using HandlerStatePtr = std::shared_ptr<HandlerState>;

class TestService : public muduo_grpc::Service
{
public:
    TestService(HandlerStatePtr state, muduo::net::EventLoop &loop) : state_(std::move(state)), loop_(loop) {}

    void RegisterMethods(muduo_grpc::MethodTable &table) override
    {
        const HandlerStatePtr state = state_;
        table.AddUnary(kEchoPath, [state](ServerCall call, std::string_view request)
                       {
            ++state->invocations;
            state->lastMethod = call.method();
            state->lastPeer = call.peer();
            state->lastMetadata = call.metadata();
            state->lastHadDeadline = call.deadline().has_value();
            ASSERT_TRUE(call.TryStartBusiness());
            call.FinishOk("echo:" + std::string(request)); });
        table.AddUnary(kHoldPath, [state](ServerCall call, std::string_view)
                       {
            ++state->invocations;
            ASSERT_TRUE(call.TryStartBusiness());
            state->held.push_back(std::move(call)); });
        table.AddUnary(kHoldNotStartedPath, [state](ServerCall call, std::string_view)
                       {
            ++state->invocations;
            state->held.push_back(std::move(call)); });
        table.AddUnary(kDropPath, [state](ServerCall, std::string_view) { ++state->invocations; });
        table.AddUnary(kTwicePath, [state](ServerCall call, std::string_view)
                       {
            ++state->invocations;
            call.FinishOk("first");
            call.FinishOk("second");
            call.FinishError(Status(StatusCode::kInternal, "third")); });
        muduo::net::EventLoop &loop = loop_;
        table.AddUnary(kOrderPath, [state, &loop](ServerCall call, std::string_view)
                       {
            ++state->invocations;
            state->orderIds.push_back(call.id());
            state->orderIterations.push_back(loop.iteration());
            call.FinishOk("ok"); });
    }

private:
    HandlerStatePtr state_;
    muduo::net::EventLoop &loop_;
};

// 同一路径登记两次:RegisterService 必须返回 false。
class DuplicateService : public muduo_grpc::Service
{
public:
    void RegisterMethods(muduo_grpc::MethodTable &table) override
    {
        table.AddUnary("/dup.Svc/M", [](ServerCall, std::string_view) {});
        table.AddUnary("/dup.Svc/M", [](ServerCall, std::string_view) {});
    }
};

// ── 原始客户端 ───────────────────────────────────────────────────────────

class RawConnection : public std::enable_shared_from_this<RawConnection>
{
public:
    enum class Mode : uint8_t
    {
        kHttp2, // 连上即发 client magic + SETTINGS;收到的字节喂给 TestH2Client 并自动回 ACK
        kBytes, // 什么都不自动发;收到的字节原样累计
    };

    RawConnection(muduo::net::EventLoop &loop, uint16_t port, Mode mode, TestH2ClientOptions h2Options = {})
        : client_(&loop, muduo::net::InetAddress("127.0.0.1", port), "server_test_raw_client"),
          mode_(mode),
          h2_(std::move(h2Options))
    {
    }

    ~RawConnection()
    {
        // §11.7:拆除前显式 forceClose,不依赖 ~TcpClient 的 use_count()==1 判断。
        const auto conn = client_.connection();
        if (conn)
        {
            conn->forceClose();
            return;
        }
        client_.stop();
    }

    RawConnection(const RawConnection &) = delete;
    RawConnection &operator=(const RawConnection &) = delete;

    // 必须已由 shared_ptr 持有。
    void Connect()
    {
        const std::weak_ptr<RawConnection> weakSelf = weak_from_this();
        client_.setConnectionCallback([weakSelf](const muduo::net::TcpConnectionPtr &conn)
                                      {
            if (const auto self = weakSelf.lock())
            {
                self->HandleConnection(conn);
            } });
        client_.setMessageCallback(
            [weakSelf](const muduo::net::TcpConnectionPtr &, muduo::net::Buffer *buffer, muduo::Timestamp)
            {
                if (const auto self = weakSelf.lock())
                {
                    self->HandleMessage(*buffer);
                    return;
                }
                buffer->retrieveAll();
            });
        client_.setConnectFailedCallback([weakSelf](int)
                                         {
            if (const auto self = weakSelf.lock())
            {
                self->connectFailed_ = true;
                self->client_.stop(); // 不让 Connector 自己重试
            } });
        client_.connect();
    }

    bool connected() const { return connected_; }
    bool disconnected() const { return disconnected_; }
    bool connectFailed() const { return connectFailed_; }
    size_t bytesReceived() const { return bytesReceived_; }
    const std::string &rawReceived() const { return raw_; }
    bool h2Failed() const { return h2Failed_; }
    TestH2Client &h2() { return h2_; }

    // 把 TestH2Client 里待发的帧一次写进 TCP(一次 send:同一批帧在服务端的同一次读里到达)。
    void Flush()
    {
        SendRaw(h2_.TakeOutput());
    }

    void SendRaw(std::string_view bytes)
    {
        const auto conn = connection_.lock();
        if (!conn || !conn->connected() || bytes.empty())
        {
            return;
        }
        conn->send(bytes.data(), static_cast<int>(bytes.size()));
    }

    // 客户端主动断开(forceClose 排到下一轮;fd 随 TcpConnection 析构关闭,服务端看到 FIN)。
    void Close()
    {
        if (const auto conn = connection_.lock())
        {
            conn->forceClose();
        }
    }

    // 停止 / 恢复从 socket 读(背压用例:对端不读,服务端写满内核缓冲后输出积在自己的输出缓冲里)。
    void StopReading()
    {
        if (const auto conn = connection_.lock())
        {
            conn->stopRead();
        }
    }

    void StartReading()
    {
        if (const auto conn = connection_.lock())
        {
            conn->startRead();
        }
    }

    // 断连(读到 EOF 或连接被关)那一刻把这些流与连接级 GOAWAY 拍下来:TCP 有序,拍到的就是 FIN 之前到达的全部帧,
    // 用来证明服务端是先把帧写完、再写半关的。
    void SnapshotAtDisconnect(std::vector<int32_t> streamIds) { snapshotStreamIds_ = std::move(streamIds); }
    const std::map<int32_t, H2Stream> &streamsAtDisconnect() const { return streamsAtDisconnect_; }
    const std::vector<muduo_grpc_test::ReceivedGoaway> &goawaysAtDisconnect() const { return goawaysAtDisconnect_; }

private:
    void HandleConnection(const muduo::net::TcpConnectionPtr &conn)
    {
        if (conn->connected())
        {
            connected_ = true;
            connection_ = conn;
            conn->setTcpNoDelay(true);
            if (mode_ == Mode::kHttp2)
            {
                Flush(); // client magic + SETTINGS(TestH2Client 构造时已提交)
            }
            return;
        }
        for (const int32_t streamId : snapshotStreamIds_)
        {
            if (const H2Stream *stream = h2_.FindStream(streamId))
            {
                streamsAtDisconnect_[streamId] = *stream;
            }
        }
        goawaysAtDisconnect_ = h2_.goawaysReceived();
        disconnected_ = true;
        connection_.reset();
    }

    void HandleMessage(muduo::net::Buffer &buffer)
    {
        const std::string_view bytes(buffer.peek(), buffer.readableBytes());
        bytesReceived_ += bytes.size();
        if (mode_ == Mode::kHttp2)
        {
            if (!h2_.Receive(bytes))
            {
                h2Failed_ = true;
            }
            buffer.retrieveAll();
            Flush(); // SETTINGS ACK / PING ACK / WINDOW_UPDATE
            return;
        }
        raw_.append(bytes.data(), bytes.size());
        buffer.retrieveAll();
    }

    muduo::net::TcpClient client_;
    const Mode mode_;
    TestH2Client h2_;
    std::weak_ptr<muduo::net::TcpConnection> connection_; // §11.7:只存 weak,所有权在 TcpClient
    bool connected_{false};
    bool disconnected_{false};
    bool connectFailed_{false};
    bool h2Failed_{false};
    size_t bytesReceived_{0};
    std::string raw_;
    std::vector<int32_t> snapshotStreamIds_;
    std::map<int32_t, H2Stream> streamsAtDisconnect_;
    std::vector<muduo_grpc_test::ReceivedGoaway> goawaysAtDisconnect_;
};

using RawConnectionPtr = std::shared_ptr<RawConnection>;

HeaderList GrpcRequestHeaders(const std::string &path, std::optional<std::string> grpcTimeout = std::nullopt)
{
    HeaderList headers = {
        {":method", "POST"},
        {":scheme", "http"},
        {":path", path},
        {":authority", "127.0.0.1"},
        {"te", "trailers"},
        {"content-type", "application/grpc"},
    };
    if (grpcTimeout)
    {
        headers.emplace_back("grpc-timeout", *grpcTimeout);
    }
    return headers;
}

// trailers-only 时 grpc-status 在第一个头块里,否则在 trailers 里。
std::optional<std::string> GrpcStatusOf(const H2Stream &stream)
{
    if (const std::string *value = stream.FindTrailer("grpc-status"))
    {
        return *value;
    }
    if (const std::string *value = stream.FindResponseHeader("grpc-status"))
    {
        return *value;
    }
    return std::nullopt;
}

std::string GrpcMessageOf(const H2Stream &stream)
{
    if (const std::string *value = stream.FindTrailer("grpc-message"))
    {
        return *value;
    }
    if (const std::string *value = stream.FindResponseHeader("grpc-message"))
    {
        return *value;
    }
    return std::string();
}

// ── Channel 调用结果 ─────────────────────────────────────────────────────

struct CallOutcome
{
    int count{0};
    UnaryCallResult result;
};
using CallOutcomePtr = std::shared_ptr<CallOutcome>;

CallOutcomePtr StartCall(Channel &channel, const std::string &path, std::string message,
                         std::chrono::milliseconds timeout = 5000ms, Metadata metadata = {})
{
    auto outcome = std::make_shared<CallOutcome>();
    UnaryCallRequest request;
    request.method = path;
    request.message = std::move(message);
    request.metadata = std::move(metadata);
    request.deadline = std::chrono::steady_clock::now() + timeout;
    channel.StartUnaryCall(std::move(request), [outcome](UnaryCallResult &&result)
                           {
        ++outcome->count;
        outcome->result = std::move(result); });
    return outcome;
}

bool AllDone(const std::vector<CallOutcomePtr> &outcomes)
{
    return std::all_of(outcomes.begin(), outcomes.end(), [](const CallOutcomePtr &o) { return o->count > 0; });
}

// onDrained 的调用次数与第一次调用的时刻(steady)。
struct DrainRecord
{
    int count{0};
    std::chrono::steady_clock::time_point firstAt{};
};
using DrainRecordPtr = std::shared_ptr<DrainRecord>;

std::function<void()> RecordDrain(DrainRecordPtr record)
{
    return [record = std::move(record)]
    {
        if (record->count++ == 0)
        {
            record->firstAt = std::chrono::steady_clock::now();
        }
    };
}

std::set<uint64_t> NewThreadIds(const std::set<uint64_t> &before, const std::set<uint64_t> &now)
{
    std::set<uint64_t> added;
    for (const uint64_t id : now)
    {
        if (before.count(id) == 0)
        {
            added.insert(id);
        }
    }
    return added;
}

// ── 日志捕获(探针静默用例):WARN / ERROR 在 muduo 里不受日志级别过滤 ─────────

std::string &CapturedLog()
{
    static std::string captured;
    return captured;
}

void CaptureLogOutput(const char *message, int len)
{
    CapturedLog().append(message, static_cast<size_t>(len));
}

void StdoutLogOutput(const char *message, int len)
{
    std::fwrite(message, 1, static_cast<size_t>(len), stdout);
}

// 去掉与服务端无关的平台噪声行。Windows 版 muduo 的 Channel 在只报 POLLHUP(不带 POLLIN)时打一行
// WARN "Channel::handle_event() POLLHUP",而 WSAPoll 对端 FIN 只报 POLLHUP(docs/PROGRESS.md 已记录);
// TcpConnection 不调 doNotLogHup,三份 muduo 树按 §15.3 不改。生产的 Linux 上 epoll 对 FIN 报
// EPOLLIN | EPOLLRDHUP,不会有这一行,所以只在 Windows 上滤掉它;其余 WARN / ERROR 照样让用例失败。
std::string WithoutPlatformHangupNoise(const std::string &log)
{
#ifdef _WIN32
    std::string kept;
    size_t begin = 0;
    while (begin < log.size())
    {
        const size_t newline = log.find('\n', begin);
        const size_t end = newline == std::string::npos ? log.size() : newline + 1;
        const std::string_view line(log.data() + begin, end - begin);
        if (line.find("Channel::handle_event() POLLHUP") == std::string_view::npos)
        {
            kept.append(line.data(), line.size());
        }
        begin = end;
    }
    return kept;
#else
    return log;
#endif
}

// 作用域内把 muduo 日志改写进 CapturedLog();断言中途失败也会恢复成写 stdout。
class LogCaptureScope
{
public:
    LogCaptureScope()
    {
        CapturedLog().clear();
        muduo::Logger::setOutput(&CaptureLogOutput);
    }
    ~LogCaptureScope() { muduo::Logger::setOutput(&StdoutLogOutput); }
    LogCaptureScope(const LogCaptureScope &) = delete;
    LogCaptureScope &operator=(const LogCaptureScope &) = delete;
};

// ── 夹具 ────────────────────────────────────────────────────────────────

class ServerLoopbackTest : public ::testing::Test
{
protected:
    static void SetUpTestSuite()
    {
        muduo::Logger::setLogLevel(muduo::Logger::WARN);
        muduo_grpc_test::EnsureSocketsInitialized();
    }

    void SetUp() override
    {
        port_ = muduo_grpc_test::FindFreeLoopbackPort();
        ASSERT_NE(port_, 0) << "no free loopback port";
    }

    void TearDown() override
    {
        // 保存的句柄先放掉(服务端还在时会回 UNAVAILABLE),再让排队的回调、forceClose、connectDestroyed 跑完。
        state_->held.clear();
        RunFor(200ms);
        server_.reset(); // loop 外析构:server.h「析构前置」允许
        RunFor(200ms);
    }

    static ServerOptions QuietOptions()
    {
        ServerOptions options;
        options.statsLogInterval = std::chrono::seconds(0);
        return options;
    }

    std::shared_ptr<Server> StartServer(ServerOptions options = QuietOptions())
    {
        server_ = Server::Create(loop_, "127.0.0.1", port_, std::move(options));
        EXPECT_NE(server_, nullptr);
        if (!server_)
        {
            return nullptr;
        }
        EXPECT_TRUE(server_->RegisterService(service_));
        server_->Start();
        return server_;
    }

    std::shared_ptr<Channel> MakeChannel()
    {
        auto channel = Channel::Create(loop_, "127.0.0.1", port_);
        EXPECT_NE(channel, nullptr);
        return channel;
    }

    RawConnectionPtr MakeRaw(RawConnection::Mode mode, bool waitConnected = true, TestH2ClientOptions h2Options = {})
    {
        auto raw = std::make_shared<RawConnection>(loop_, port_, mode, std::move(h2Options));
        raw->Connect();
        if (waitConnected)
        {
            EXPECT_TRUE(RunUntil([&] { return raw->connected(); }, 5s)) << "raw client did not connect";
        }
        return raw;
    }

    // 在原始 HTTP/2 连接上发一个完整的 unary 请求(一次 send);返回流 id。
    int32_t SendRawRequest(RawConnection &raw, const std::string &path, std::optional<std::string> grpcTimeout,
                           std::string_view payload = "payload")
    {
        const int32_t streamId = raw.h2().OpenStream(GrpcRequestHeaders(path, std::move(grpcTimeout)), false);
        EXPECT_GT(streamId, 0) << raw.h2().lastError();
        EXPECT_TRUE(raw.h2().SendData(streamId, TestH2Peer::Frame(payload), true)) << raw.h2().lastError();
        raw.Flush();
        return streamId;
    }

    // 背压用例(§15.16 第 2 条)的共同准备。客户端把流级与连接级接收窗口都开到 2^31-1,否则服务端先被 HTTP/2
    // 流控卡住、到不了输出缓冲;三个调用都派发之后客户端停止读取,再完成一个 32MiB 的应答 —— 远超回环上两端
    // 内核缓冲(Linux tcp_wmem / tcp_rmem 上限默认 4MiB / 6MiB)加 1MiB 背压阈值,服务端输出缓冲必然到达阈值,
    // 剩余的 DATA 积在 nghttp2 里。之后排空到期提交的 REFUSED / UNAVAILABLE / 最终 GOAWAY 要等输出缓冲里积压的 DATA
    // 写出去之后才能写出。
    struct BackpressuredCalls
    {
        RawConnectionPtr raw;
        int32_t big{0};      // 业务已完成,32MiB 应答大部分还积在服务端
        int32_t waiting{0};  // 业务未开始:排空到期回 REFUSED_STREAM
        int32_t started{0};  // 业务已开始:排空到期回 UNAVAILABLE "server shutting down"
    };

    BackpressuredCalls StartBackpressuredCalls()
    {
        BackpressuredCalls calls;
        ServerOptions options = QuietOptions();
        options.maxSendMessageBytes = kBackpressureResponseBytes;
        if (StartServer(options) == nullptr)
        {
            return calls;
        }
        TestH2ClientOptions h2Options;
        h2Options.initialSettings = {{h2::kSettingsInitialWindowSize, static_cast<uint32_t>(kMaxWindowBytes)}};
        h2Options.localConnectionWindowBytes = kMaxWindowBytes;
        calls.raw = MakeRaw(RawConnection::Mode::kHttp2, true, std::move(h2Options));
        RunFor(100ms); // SETTINGS / ACK 往返完成:服务端已按客户端的大窗口发数据,客户端也已按大窗口收
        calls.big = SendRawRequest(*calls.raw, kHoldPath, std::nullopt);
        calls.waiting = SendRawRequest(*calls.raw, kHoldNotStartedPath, std::nullopt);
        calls.started = SendRawRequest(*calls.raw, kHoldPath, std::nullopt);
        EXPECT_TRUE(RunUntil([&] { return state_->held.size() == 3; }, 5s));
        if (state_->held.size() != 3)
        {
            calls.raw.reset();
            return calls;
        }
        calls.raw->StopReading();
        // 派发按请求收完的先后(先进先出):held[0] 是 big。
        state_->held.front().FinishOk(std::string(kBackpressureResponseBytes, 'x'));
        RunFor(200ms); // 服务端写满内核缓冲,输出缓冲到阈值后停止从 nghttp2 取帧
        return calls;
    }

    bool RunUntil(const std::function<bool()> &done, std::chrono::milliseconds timeout)
    {
        return muduo_grpc_test::RunLoopUntil(loop_, done, timeout);
    }

    void RunFor(std::chrono::milliseconds duration) { muduo_grpc_test::RunLoopFor(loop_, duration); }

    ServerStatsSnapshot Stats() const { return server_ ? server_->Stats() : ServerStatsSnapshot{}; }

    // 声明顺序 = 析构逆序:EventLoop 最先构造、最后析构;server_ 最后声明、最先析构。
    muduo::net::EventLoop loop_;
    uint16_t port_{0};
    HandlerStatePtr state_ = std::make_shared<HandlerState>();
    TestService service_{state_, loop_};
    std::shared_ptr<Server> server_;
};

// ── 用例 ────────────────────────────────────────────────────────────────

TEST_F(ServerLoopbackTest, CreateRejectsNonCanonicalAddressesAndZeroPort)
{
    EXPECT_EQ(Server::Create(loop_, "localhost", port_), nullptr);
    EXPECT_EQ(Server::Create(loop_, "127.0.0.01", port_), nullptr);
    EXPECT_EQ(Server::Create(loop_, "::1", port_), nullptr);
    EXPECT_EQ(Server::Create(loop_, "127.0.0.1", 0), nullptr);
}

TEST_F(ServerLoopbackTest, RegisterServiceRejectsDuplicatesAndLateRegistration)
{
    {
        const auto server = Server::Create(loop_, "127.0.0.1", port_, QuietOptions());
        ASSERT_NE(server, nullptr);
        DuplicateService duplicate;
        EXPECT_FALSE(server->RegisterService(duplicate)) << "duplicate path must fail registration";
    }
    ASSERT_NE(StartServer(), nullptr);
    TestService late(state_, loop_);
    EXPECT_FALSE(server_->RegisterService(late)) << "registration after Start must fail";
}

TEST_F(ServerLoopbackTest, EchoRoundTripCarriesMetadataDeadlineAndPeer)
{
    ASSERT_NE(StartServer(), nullptr);
    EXPECT_EQ(server_->listenAddress(), "127.0.0.1:" + std::to_string(port_));
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto outcome = StartCall(*channel, kEchoPath, "ping", 5000ms,
                                   {{"x-trace-id", "abc"}, {"x-session-detail-bin", kSessionAppValue}});
    ASSERT_TRUE(RunUntil([&] { return outcome->count > 0; }, 5s));
    RunFor(100ms);
    EXPECT_EQ(outcome->count, 1);
    ASSERT_TRUE(outcome->result.status.ok()) << outcome->result.status.ToString();
    EXPECT_EQ(outcome->result.response, "echo:ping");

    EXPECT_EQ(state_->invocations, 1u);
    EXPECT_EQ(state_->lastMethod, kEchoPath);
    EXPECT_FALSE(state_->lastPeer.empty());
    EXPECT_TRUE(state_->lastHadDeadline) << "grpc-timeout must become a server-side deadline";
    const std::string *trace = muduo_grpc::FindMetadata(state_->lastMetadata, "x-trace-id");
    ASSERT_NE(trace, nullptr);
    EXPECT_EQ(*trace, "abc");
    const std::string *session = muduo_grpc::FindMetadata(state_->lastMetadata, "x-session-detail-bin");
    ASSERT_NE(session, nullptr);
    EXPECT_EQ(*session, kSessionAppValue) << "only the wire layer of -bin is decoded";
    EXPECT_EQ(muduo_grpc::FindMetadata(state_->lastMetadata, "user-agent"), nullptr);
    EXPECT_EQ(muduo_grpc::FindMetadata(state_->lastMetadata, "grpc-timeout"), nullptr);

    const ServerStatsSnapshot stats = Stats();
    EXPECT_EQ(stats.dispatched, 1u);
    EXPECT_EQ(stats.inFlight, 0u);
    EXPECT_EQ(stats.accepted, 1u);
    EXPECT_EQ(stats.codes.at(0), 1u);
    EXPECT_EQ(stats.methods.at("Echo").at(0), 1u);
    EXPECT_EQ(stats.dropped, 0u);
}

TEST_F(ServerLoopbackTest, UnknownServiceAndMethodAreUnimplementedWithGrpcGoMessages)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto unknownService = StartCall(*channel, "/nope.Svc/M", "x");
    const auto unknownMethod = StartCall(*channel, std::string("/") + kServiceName + "/Nope", "x");
    ASSERT_TRUE(RunUntil([&] { return unknownService->count > 0 && unknownMethod->count > 0; }, 5s));

    EXPECT_EQ(unknownService->result.status.code(), StatusCode::kUnimplemented);
    EXPECT_EQ(unknownService->result.status.message(), "unknown service nope.Svc");
    EXPECT_EQ(unknownMethod->result.status.code(), StatusCode::kUnimplemented);
    EXPECT_EQ(unknownMethod->result.status.message(), "unknown method Nope for service test.Echo");
    EXPECT_EQ(state_->invocations, 0u);
    EXPECT_EQ(Stats().unimplemented, 2u);
    EXPECT_EQ(Stats().dispatched, 0u);
}

TEST_F(ServerLoopbackTest, ZeroGrpcTimeoutIsRejectedWithoutDispatch)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2);
    const int32_t streamId = SendRawRequest(*raw, kEchoPath, std::string("0n"));
    ASSERT_TRUE(RunUntil([&]
                         {
        const H2Stream *stream = raw->h2().FindStream(streamId);
        return stream != nullptr && GrpcStatusOf(*stream).has_value(); }, 5s));
    const H2Stream *stream = raw->h2().FindStream(streamId);
    ASSERT_NE(stream, nullptr);
    EXPECT_EQ(GrpcStatusOf(*stream).value_or(""), "4");
    EXPECT_EQ(GrpcMessageOf(*stream), "deadline exceeded before dispatch");
    EXPECT_EQ(state_->invocations, 0u);
    EXPECT_EQ(Stats().dispatched, 0u);
    // 截止在发出前就已到的合法请求:计 deadline_before_start,不算畸形请求(server_session.h OnRequestRejected)。
    EXPECT_EQ(Stats().deadlineBeforeStart, 1u);
    EXPECT_EQ(Stats().rejectedProtocol, 0u);
    EXPECT_EQ(Stats().codes.at(4), 1u);
    EXPECT_FALSE(raw->h2Failed()) << raw->h2().lastError();
}

TEST_F(ServerLoopbackTest, DeferredCallExpiresAtServerDeadlineAndLateFinishIsDropped)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2);

    // 业务已开始(延后完成):截止到期立即回 DEADLINE_EXCEEDED,业务不打断,迟到的完成丢弃。
    const int32_t started = SendRawRequest(*raw, kHoldPath, std::string("200m"));
    // 业务未开始(在等许可):截止到期回 DEADLINE_EXCEEDED,之后 TryStartBusiness 为假。
    const int32_t waiting = SendRawRequest(*raw, kHoldNotStartedPath, std::string("200m"));
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 2; }, 5s));
    ASSERT_TRUE(RunUntil([&]
                         {
        const H2Stream *a = raw->h2().FindStream(started);
        const H2Stream *b = raw->h2().FindStream(waiting);
        return a != nullptr && b != nullptr && GrpcStatusOf(*a).has_value() && GrpcStatusOf(*b).has_value(); }, 5s));

    const H2Stream *startedStream = raw->h2().FindStream(started);
    const H2Stream *waitingStream = raw->h2().FindStream(waiting);
    EXPECT_EQ(GrpcStatusOf(*startedStream).value_or(""), "4");
    EXPECT_EQ(GrpcMessageOf(*startedStream), "deadline exceeded");
    EXPECT_EQ(GrpcStatusOf(*waitingStream).value_or(""), "4");
    EXPECT_EQ(GrpcMessageOf(*waitingStream), "deadline exceeded before the call started");
    EXPECT_EQ(Stats().deadlineAfterStart, 1u);
    EXPECT_EQ(Stats().deadlineBeforeStart, 1u);
    EXPECT_EQ(Stats().inFlight, 0u);

    const size_t framesBefore = startedStream->frames.size();
    for (const ServerCall &call : state_->held)
    {
        EXPECT_FALSE(call.IsActive());
        EXPECT_FALSE(call.TryStartBusiness()) << "a completed call must never start business";
        call.FinishOk("late");
    }
    state_->held.clear();
    RunFor(100ms);
    EXPECT_EQ(Stats().lateFinish, 2u);
    EXPECT_EQ(Stats().dropped, 0u) << "completed calls are not dropped when their handles go away";
    EXPECT_EQ(raw->h2().FindStream(started)->frames.size(), framesBefore) << "nothing is sent after the deadline reply";
}

TEST_F(ServerLoopbackTest, ResetBeforeDispatchCancelsSilently)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2);
    RunFor(100ms); // 握手完成(服务端 SETTINGS / ACK 往返)

    // HEADERS + DATA(END_STREAM) + RST_STREAM 在同一次 send 里:服务端在同一次 Receive 里先登记再撤销,
    // 派发泵随后跳过它。
    const int32_t streamId = raw->h2().OpenStream(GrpcRequestHeaders(kHoldPath), false);
    ASSERT_GT(streamId, 0);
    ASSERT_TRUE(raw->h2().SendData(streamId, TestH2Peer::Frame("x"), true));
    ASSERT_TRUE(raw->h2().SendRstStream(streamId, h2::kCancel));
    raw->Flush();

    ASSERT_TRUE(RunUntil([&] { return Stats().cancelledBeforeStart == 1; }, 5s));
    RunFor(100ms);
    EXPECT_EQ(state_->invocations, 0u) << "a call reset before dispatch must not reach the handler";
    EXPECT_EQ(Stats().dispatched, 0u);
    EXPECT_EQ(Stats().inFlight, 0u);
    const H2Stream *stream = raw->h2().FindStream(streamId);
    ASSERT_NE(stream, nullptr);
    EXPECT_TRUE(stream->responseHeaders.empty()) << "no reply for a cancelled call";
}

TEST_F(ServerLoopbackTest, ResetAfterBusinessStartedAbortsAndLateFinishIsCounted)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2);
    const int32_t streamId = SendRawRequest(*raw, kHoldPath, std::nullopt);
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 1; }, 5s));

    ASSERT_TRUE(raw->h2().SendRstStream(streamId, h2::kCancel));
    raw->Flush();
    ASSERT_TRUE(RunUntil([&] { return Stats().abortedAfterStart == 1; }, 5s));
    EXPECT_FALSE(state_->held.front().IsActive());

    state_->held.front().FinishOk("too late");
    state_->held.clear();
    RunFor(100ms);
    EXPECT_EQ(Stats().lateFinish, 1u);
    EXPECT_EQ(Stats().dropped, 0u);
    EXPECT_EQ(Stats().inFlight, 0u);
    EXPECT_TRUE(raw->h2().FindStream(streamId)->responseHeaders.empty());
}

TEST_F(ServerLoopbackTest, RepeatedFinishOnlyTheFirstOneIsSent)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    const auto outcome = StartCall(*channel, kTwicePath, "x");
    ASSERT_TRUE(RunUntil([&] { return outcome->count > 0; }, 5s));
    RunFor(100ms);
    EXPECT_EQ(outcome->count, 1);
    ASSERT_TRUE(outcome->result.status.ok()) << outcome->result.status.ToString();
    EXPECT_EQ(outcome->result.response, "first");
    EXPECT_EQ(Stats().lateFinish, 2u);
    EXPECT_EQ(Stats().codes.size(), 1u);
    EXPECT_EQ(Stats().codes.at(0), 1u);
}

TEST_F(ServerLoopbackTest, DroppingAllHandlesRepliesUnavailable)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto immediate = StartCall(*channel, kDropPath, "x");
    const auto later = StartCall(*channel, kHoldNotStartedPath, "x");
    ASSERT_TRUE(RunUntil([&] { return immediate->count > 0 && state_->held.size() == 1; }, 5s));
    state_->held.clear(); // 最后一个副本在 loop 线程析构
    ASSERT_TRUE(RunUntil([&] { return later->count > 0; }, 5s));

    for (const auto &outcome : {immediate, later})
    {
        EXPECT_EQ(outcome->result.status.code(), StatusCode::kUnavailable);
        EXPECT_EQ(outcome->result.status.message(), "server dropped the call without completing it");
        EXPECT_NE(outcome->result.status.message(), kBattleNotAllocatable);
    }
    EXPECT_EQ(Stats().dropped, 2u);
    EXPECT_EQ(Stats().droppedOffLoop, 0u);
    EXPECT_EQ(Stats().inFlight, 0u);
}

TEST_F(ServerLoopbackTest, FinishAfterServerDestroyedIsHarmless)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    const auto outcome = StartCall(*channel, kHoldPath, "x");
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 1; }, 5s));

    server_.reset(); // loop 外析构:连接被 forceClose,调用静默收口
    const ServerCall &call = state_->held.front();
    EXPECT_FALSE(call.IsActive());
    EXPECT_TRUE(call.ServerDraining()) << "a destroyed server counts as draining";
    EXPECT_FALSE(call.TryStartBusiness());
    call.FinishOk("after destruction");
    call.FinishError(Status(StatusCode::kInternal, "after destruction"));
    state_->held.clear();

    ASSERT_TRUE(RunUntil([&] { return outcome->count > 0; }, 5s));
    EXPECT_EQ(outcome->result.status.code(), StatusCode::kUnavailable) << outcome->result.status.ToString();
}

TEST_F(ServerLoopbackTest, InFlightLimitRejectsWithResourceExhaustedBeforeBusiness)
{
    ServerOptions options = QuietOptions();
    options.maxInFlightCalls = 4;
    ASSERT_NE(StartServer(options), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    std::vector<CallOutcomePtr> outcomes;
    for (int i = 0; i < 6; ++i)
    {
        outcomes.push_back(StartCall(*channel, kHoldNotStartedPath, "x"));
    }
    ASSERT_TRUE(RunUntil([&]
                         {
        size_t done = 0;
        for (const auto &o : outcomes)
        {
            done += o->count > 0 ? 1 : 0;
        }
        return done == 2 && state_->held.size() == 4; }, 5s));
    for (const auto &outcome : outcomes)
    {
        if (outcome->count == 0)
        {
            continue;
        }
        EXPECT_EQ(outcome->result.status.code(), StatusCode::kResourceExhausted);
        EXPECT_EQ(outcome->result.status.message(), "server in-flight call limit (4) reached");
    }
    EXPECT_EQ(Stats().rejectedOverload, 2u);
    EXPECT_EQ(Stats().inFlight, 4u);

    for (const ServerCall &call : state_->held)
    {
        call.FinishOk("released");
    }
    state_->held.clear();
    ASSERT_TRUE(RunUntil([&] { return AllDone(outcomes); }, 5s));
    size_t ok = 0;
    for (const auto &outcome : outcomes)
    {
        ok += outcome->result.status.ok() ? 1 : 0;
    }
    EXPECT_EQ(ok, 4u);
    EXPECT_EQ(Stats().inFlightPeak, 4u);
}

TEST_F(ServerLoopbackTest, PerConnectionStreamLimitMakesTheClientQueue)
{
    ServerOptions options = QuietOptions();
    options.session.maxConcurrentStreams = 2;
    ASSERT_NE(StartServer(options), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    std::vector<CallOutcomePtr> outcomes;
    for (int i = 0; i < 5; ++i)
    {
        outcomes.push_back(StartCall(*channel, kHoldPath, "x", 10000ms));
    }
    size_t rounds = 0;
    while (!AllDone(outcomes) && rounds < 10)
    {
        ++rounds;
        ASSERT_TRUE(RunUntil([&] { return !state_->held.empty() || AllDone(outcomes); }, 5s));
        RunFor(100ms); // 给客户端机会多发:超过 SETTINGS_MAX_CONCURRENT_STREAMS 的流必须留在客户端排队
        EXPECT_LE(state_->held.size(), 2u);
        EXPECT_LE(Stats().inFlight, 2u);
        for (const ServerCall &call : state_->held)
        {
            call.FinishOk("ok");
        }
        state_->held.clear();
    }
    ASSERT_TRUE(RunUntil([&] { return AllDone(outcomes); }, 5s));
    for (const auto &outcome : outcomes)
    {
        EXPECT_TRUE(outcome->result.status.ok()) << outcome->result.status.ToString();
    }
    EXPECT_EQ(Stats().inFlightPeak, 2u);
    EXPECT_EQ(Stats().refusedOnShutdown, 0u);
}

TEST_F(ServerLoopbackTest, BurstOf200IsDispatchedFifoAndBoundedPerTurn)
{
    ServerOptions options = QuietOptions();
    options.maxDispatchPerTurn = 16;
    options.dispatchTurnBudget = std::chrono::microseconds(10000000); // 只让「每轮个数」生效
    ASSERT_NE(StartServer(options), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    std::vector<CallOutcomePtr> outcomes;
    for (int i = 0; i < 200; ++i)
    {
        outcomes.push_back(StartCall(*channel, kOrderPath, "x", 10000ms));
    }
    ASSERT_TRUE(RunUntil([&] { return AllDone(outcomes); }, 10s));
    for (const auto &outcome : outcomes)
    {
        EXPECT_TRUE(outcome->result.status.ok()) << outcome->result.status.ToString();
    }

    ASSERT_EQ(state_->orderIds.size(), 200u);
    // 调用 id 按「请求收完」的先后分配:派发顺序必须与之一致(全节点一个先进先出队列)。
    EXPECT_TRUE(std::is_sorted(state_->orderIds.begin(), state_->orderIds.end()));
    EXPECT_EQ(std::set<uint64_t>(state_->orderIds.begin(), state_->orderIds.end()).size(), 200u);
    // 同一轮 loop 内派发的个数不超过上限;剩余的落到后面的轮次(中间让出给定时器与 I/O)。
    std::map<int64_t, size_t> perIteration;
    for (const int64_t iteration : state_->orderIterations)
    {
        ++perIteration[iteration];
    }
    EXPECT_GE(perIteration.size(), 200u / 16);
    for (const auto &[iteration, count] : perIteration)
    {
        EXPECT_LE(count, 16u) << "iteration " << iteration;
    }
    EXPECT_LE(Stats().maxDispatchedPerTurn, 16u);
    EXPECT_EQ(Stats().dispatched, 200u);
}

TEST_F(ServerLoopbackTest, GracefulShutdownDrainsInFlightCallsAndRefusesNewConnections)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    const auto inFlight = StartCall(*channel, kHoldPath, "x", 20000ms);
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 1; }, 5s));
    const uint64_t acceptedBefore = Stats().accepted;

    auto drained = std::make_shared<int>(0);
    auto secondDrained = std::make_shared<int>(0);
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 15000ms; // 给新连接被拒(Windows 回环拒绝要约 2s)留足时间,不让排空先到期
    shutdown.goawayPingTimeout = 500ms;
    shutdown.closeGrace = 300ms;
    server_->BeginShutdown(shutdown, [drained] { ++*drained; });
    EXPECT_EQ(*drained, 0) << "onDrained must never run inside BeginShutdown";
    EXPECT_TRUE(server_->draining());
    server_->BeginShutdown(shutdown, [secondDrained] { ++*secondDrained; }); // 幂等:忽略

    RunFor(100ms); // Acceptor 在排队任务里销毁;两段 GOAWAY 走完
    EXPECT_TRUE(state_->held.front().ServerDraining());
    EXPECT_TRUE(state_->held.front().IsActive()) << "accepted calls keep running while draining";

    const auto newChannel = MakeChannel();
    ASSERT_NE(newChannel, nullptr);
    const auto rejected = StartCall(*newChannel, kEchoPath, "x", 3000ms);
    ASSERT_TRUE(RunUntil([&] { return rejected->count > 0; }, 6s));
    EXPECT_FALSE(rejected->result.status.ok()) << "the listener must be closed while draining";
    EXPECT_EQ(Stats().accepted, acceptedBefore) << "no connection is accepted after BeginShutdown";
    EXPECT_EQ(*drained, 0) << "an in-flight call keeps the server from draining";

    state_->held.front().FinishOk("finished while draining");
    state_->held.clear();
    ASSERT_TRUE(RunUntil([&] { return inFlight->count > 0; }, 5s));
    ASSERT_TRUE(inFlight->result.status.ok()) << inFlight->result.status.ToString();
    EXPECT_EQ(inFlight->result.response, "finished while draining");

    ASSERT_TRUE(RunUntil([&] { return *drained > 0; }, 5s));
    RunFor(200ms);
    EXPECT_EQ(*drained, 1) << "onDrained exactly once";
    EXPECT_EQ(*secondDrained, 0);
    EXPECT_EQ(server_->connectionCount(), 0u);
    EXPECT_EQ(Stats().refusedOnShutdown, 0u);
    EXPECT_EQ(Stats().unavailableOnShutdown, 0u);
    EXPECT_EQ(Stats().dropped, 0u);
}

TEST_F(ServerLoopbackTest, GoawayIsTwoPhaseOnTheWire)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2);
    const int32_t streamId = SendRawRequest(*raw, kHoldPath, std::nullopt);
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 1; }, 5s));

    auto drained = std::make_shared<int>(0);
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 5000ms;
    shutdown.goawayPingTimeout = 1000ms;
    shutdown.closeGrace = 300ms;
    server_->BeginShutdown(shutdown, [drained] { ++*drained; });
    // 客户端自动回 PING ACK:第二段在 ACK 到达后立即发出,不必等满 goawayPingTimeout。
    ASSERT_TRUE(RunUntil([&] { return raw->h2().goawaysReceived().size() >= 2; }, 900ms));
    const auto &goaways = raw->h2().goawaysReceived();
    EXPECT_EQ(goaways[0].lastStreamId, h2::kMaxStreamId);
    EXPECT_EQ(goaways[0].errorCode, h2::kNoError);
    EXPECT_EQ(goaways[1].lastStreamId, streamId);
    EXPECT_EQ(goaways[1].errorCode, h2::kNoError);
    EXPECT_GE(raw->h2().pingsReceived(), 1u);

    state_->held.front().FinishOk("done");
    state_->held.clear();
    ASSERT_TRUE(RunUntil([&]
                         {
        const H2Stream *stream = raw->h2().FindStream(streamId);
        return stream != nullptr && GrpcStatusOf(*stream).has_value(); }, 5s));
    EXPECT_EQ(GrpcStatusOf(*raw->h2().FindStream(streamId)).value_or(""), "0");
    EXPECT_EQ(raw->h2().FindStream(streamId)->body, TestH2Peer::Frame("done"));
    // 没有打开的流、最终 GOAWAY 已写出 → 写半关;客户端随后断开(或宽限到期被 forceClose),连接表清空即排空完成。
    ASSERT_TRUE(RunUntil([&] { return *drained > 0 && raw->disconnected(); }, 5s));
    EXPECT_EQ(*drained, 1);
}

TEST_F(ServerLoopbackTest, DrainTimeoutRefusesUnstartedAndFailsStartedCalls)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2);
    const int32_t waiting = SendRawRequest(*raw, kHoldNotStartedPath, std::nullopt);
    const int32_t started = SendRawRequest(*raw, kHoldPath, std::nullopt);
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 2; }, 5s));
    raw->SnapshotAtDisconnect({waiting, started});

    auto drained = std::make_shared<int>(0);
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 300ms;
    shutdown.goawayPingTimeout = 100ms;
    shutdown.closeGrace = 200ms;
    server_->BeginShutdown(shutdown, [drained] { ++*drained; });
    ASSERT_TRUE(RunUntil([&]
                         {
        const H2Stream *a = raw->h2().FindStream(waiting);
        const H2Stream *b = raw->h2().FindStream(started);
        return a != nullptr && b != nullptr && a->rstReceived && GrpcStatusOf(*b).has_value(); }, 5s));

    EXPECT_EQ(raw->h2().FindStream(waiting)->rstErrorCode, h2::kRefusedStream)
        << "business not started: REFUSED_STREAM lets the client retry safely";
    const H2Stream *startedStream = raw->h2().FindStream(started);
    EXPECT_EQ(GrpcStatusOf(*startedStream).value_or(""), "14");
    EXPECT_EQ(GrpcMessageOf(*startedStream), "server shutting down");
    EXPECT_NE(GrpcMessageOf(*startedStream), kBattleNotAllocatable);
    EXPECT_EQ(Stats().refusedOnShutdown, 1u);
    EXPECT_EQ(Stats().unavailableOnShutdown, 1u);
    EXPECT_EQ(Stats().inFlight, 0u);

    for (const ServerCall &call : state_->held)
    {
        call.FinishOk("late");
    }
    state_->held.clear();
    ASSERT_TRUE(RunUntil([&] { return *drained > 0; }, 5s));
    EXPECT_EQ(Stats().lateFinish, 2u);
    EXPECT_EQ(Stats().dropped, 0u);
    ASSERT_TRUE(RunUntil([&] { return raw->disconnected(); }, 2s));
    RunFor(100ms);
    EXPECT_EQ(*drained, 1);

    // 对端正常读取:REFUSED / UNAVAILABLE / 最终 GOAWAY 都在 EOF 之前到达(服务端先冲出、再写半关)。
    const auto &atEof = raw->streamsAtDisconnect();
    ASSERT_EQ(atEof.count(waiting), 1u);
    ASSERT_EQ(atEof.count(started), 1u);
    EXPECT_TRUE(atEof.at(waiting).rstReceived);
    EXPECT_EQ(atEof.at(waiting).rstErrorCode, h2::kRefusedStream);
    EXPECT_EQ(GrpcStatusOf(atEof.at(started)).value_or(""), "14");
    EXPECT_EQ(GrpcMessageOf(atEof.at(started)), "server shutting down");
    ASSERT_FALSE(raw->goawaysAtDisconnect().empty());
    EXPECT_EQ(raw->goawaysAtDisconnect().back().lastStreamId, started) << "the final GOAWAY precedes the EOF";
    EXPECT_EQ(raw->goawaysAtDisconnect().back().errorCode, h2::kNoError);
}

TEST_F(ServerLoopbackTest, DrainTimeoutUnderBackpressureForceClosesAfterCloseGrace)
{
    // 对端停止读取、服务端输出被背压时排空到期:不 assert,onDrained 恰好一次。积压的应答写不完,会话一直有帧要写,
    // 所以始终不写半关;对端也不关,连接由从排空到期起算的 closeGrace 硬上限 forceClose。
    const BackpressuredCalls calls = StartBackpressuredCalls();
    ASSERT_NE(calls.raw, nullptr);

    const auto drained = std::make_shared<DrainRecord>();
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 300ms;
    shutdown.goawayPingTimeout = 100ms; // 对端不读,PING ACK 不会来:满时长后直接提交第二段
    shutdown.closeGrace = 300ms;
    const auto shutdownAt = std::chrono::steady_clock::now();
    server_->BeginShutdown(shutdown, RecordDrain(drained));
    ASSERT_TRUE(RunUntil([&] { return drained->count > 0; }, 5s));
    RunFor(200ms);

    EXPECT_EQ(drained->count, 1) << "onDrained exactly once";
    // 硬上限从排空到期那一刻起算(steady 复核,不会早到):连接只能是 closeGrace 到期被强关的;
    // 它是上限,不会因为积压一直写不完而往后拖(上界留足调度余量)。
    EXPECT_GE(drained->firstAt - shutdownAt, shutdown.drainTimeout + shutdown.closeGrace);
    EXPECT_LT(drained->firstAt - shutdownAt, shutdown.drainTimeout + shutdown.closeGrace + 2000ms);
    EXPECT_EQ(server_->connectionCount(), 0u);
    EXPECT_EQ(Stats().refusedOnShutdown, 1u);
    EXPECT_EQ(Stats().unavailableOnShutdown, 1u);
    EXPECT_EQ(Stats().inFlight, 0u);
    EXPECT_EQ(Stats().dropped, 0u);
}

TEST_F(ServerLoopbackTest, DrainTimeoutUnderBackpressureFlushesEverythingBeforeTheHalfClose)
{
    // §15.16 第 2 条的回归用例:排空到期时输出被背压,REFUSED / UNAVAILABLE / 最终 GOAWAY 只能排在积压的 DATA 之后,
    // 它们也必须在 EOF 之前送达。最初它们留在 nghttp2 里,排空到期立即写半关之后再也写不出去,对端读完积压只看到 EOF。
    // 现在排空到期不写半关:对端恢复读取后服务端照常按背压逐段写出(nghttp2 让控制帧排在 DATA 前面),
    // 会话把帧全部交出、所有流关闭之后才写半关。
    const BackpressuredCalls calls = StartBackpressuredCalls();
    ASSERT_NE(calls.raw, nullptr);
    calls.raw->SnapshotAtDisconnect({calls.big, calls.waiting, calls.started});

    const auto drained = std::make_shared<DrainRecord>();
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 300ms;
    shutdown.goawayPingTimeout = 100ms;
    shutdown.closeGrace = 20000ms; // 只作硬上限:全部写完之后才发 FIN,对端读到 EOF 即关,远早于宽限
    const auto shutdownAt = std::chrono::steady_clock::now();
    server_->BeginShutdown(shutdown, RecordDrain(drained));
    // 排空到期:两个调用已收口,应答已提交;客户端此前一直没读,什么都还没看到,连接照常保持。
    ASSERT_TRUE(RunUntil([&] { return Stats().refusedOnShutdown == 1 && Stats().unavailableOnShutdown == 1; }, 5s));
    EXPECT_FALSE(calls.raw->h2().FindStream(calls.waiting)->rstReceived);
    EXPECT_EQ(drained->count, 0);
    EXPECT_EQ(server_->connectionCount(), 1u);

    calls.raw->StartReading();
    ASSERT_TRUE(RunUntil([&] { return calls.raw->disconnected(); }, 15s));
    EXPECT_FALSE(calls.raw->h2Failed()) << calls.raw->h2().lastError();

    const auto &atEof = calls.raw->streamsAtDisconnect();
    ASSERT_EQ(atEof.size(), 3u);
    EXPECT_TRUE(atEof.at(calls.waiting).rstReceived) << "REFUSED_STREAM must reach the peer before the EOF";
    EXPECT_EQ(atEof.at(calls.waiting).rstErrorCode, h2::kRefusedStream);
    EXPECT_EQ(GrpcStatusOf(atEof.at(calls.started)).value_or(""), "14") << "UNAVAILABLE must reach the peer before the EOF";
    EXPECT_EQ(GrpcMessageOf(atEof.at(calls.started)), "server shutting down");
    EXPECT_NE(GrpcMessageOf(atEof.at(calls.started)), kBattleNotAllocatable);
    // 业务已完成的应答同样完整送达(DATA + trailers),而不是被 EOF 截断。
    EXPECT_EQ(GrpcStatusOf(atEof.at(calls.big)).value_or(""), "0");
    EXPECT_EQ(atEof.at(calls.big).body.size(), kBackpressureResponseBytes + 5);
    const auto &goaways = calls.raw->goawaysAtDisconnect();
    ASSERT_GE(goaways.size(), 2u);
    EXPECT_EQ(goaways.front().lastStreamId, h2::kMaxStreamId);
    EXPECT_EQ(goaways.back().lastStreamId, calls.started);
    EXPECT_EQ(goaways.back().errorCode, h2::kNoError);

    ASSERT_TRUE(RunUntil([&] { return drained->count > 0; }, 5s));
    // 早于宽限到期:连接是对端读到 EOF 后关闭的,不是 forceClose。
    EXPECT_LT(drained->firstAt - shutdownAt, shutdown.drainTimeout + shutdown.closeGrace);
    RunFor(100ms);
    EXPECT_EQ(drained->count, 1) << "onDrained exactly once";
    EXPECT_EQ(server_->connectionCount(), 0u);
    EXPECT_EQ(Stats().dropped, 0u);
}

TEST_F(ServerLoopbackTest, DrainTimeoutKeepsWritingAFlowControlledResponseUntilThePeerOpensTheWindow)
{
    // 排空到期时,已 FinishOk 的应答 DATA 正被对端流级窗口卡住(nghttp2 取不出来,服务端输出缓冲是空的)。
    // 修复前排空到期立即写半关:对端随后发 WINDOW_UPDATE 放出来的 DATA 再也写不出去,对端读完窗口内的那一段就看到
    // EOF(grpc-go 报 UNAVAILABLE)。现在连接继续读写,对端收齐 DATA 与 trailers 之后才见 EOF。
    ASSERT_NE(StartServer(), nullptr);
    TestH2ClientOptions h2Options;
    h2Options.initialSettings = {{h2::kSettingsInitialWindowSize, kSmallStreamWindowBytes}};
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2, true, std::move(h2Options));
    RunFor(100ms); // SETTINGS / ACK 往返完成:服务端按客户端的小流级窗口发数据,客户端也按小窗口收
    const int32_t big = SendRawRequest(*raw, kHoldPath, std::nullopt);
    const int32_t waiting = SendRawRequest(*raw, kHoldNotStartedPath, std::nullopt);
    const int32_t started = SendRawRequest(*raw, kHoldPath, std::nullopt);
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 3; }, 5s));
    raw->SnapshotAtDisconnect({big, waiting, started});

    // 客户端停止读取:窗口内的 16KiB 发出之后不会再有 WINDOW_UPDATE,剩余 DATA 与 trailers 被流控卡在 nghttp2 里。
    // 派发按请求收完的先后(先进先出):held[0] 是 big。
    raw->StopReading();
    const std::string response(kFlowControlledResponseBytes, 'y');
    state_->held.front().FinishOk(response);
    RunFor(200ms);

    const auto drained = std::make_shared<DrainRecord>();
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 300ms;
    shutdown.goawayPingTimeout = 100ms; // 对端不读,PING ACK 不会来:满时长后直接提交第二段
    shutdown.closeGrace = 20000ms;      // 只作硬上限:应答写完之后才发 FIN,对端读到 EOF 即关,远早于宽限
    const auto shutdownAt = std::chrono::steady_clock::now();
    server_->BeginShutdown(shutdown, RecordDrain(drained));
    ASSERT_TRUE(RunUntil([&] { return Stats().refusedOnShutdown == 1 && Stats().unavailableOnShutdown == 1; }, 5s));
    RunFor(200ms);
    EXPECT_EQ(server_->connectionCount(), 1u) << "the connection stays open while a committed response is blocked";
    EXPECT_EQ(drained->count, 0);

    // 对端恢复读取:读到窗口内的 DATA 后自动回 WINDOW_UPDATE,服务端在同一条连接上把 DATA 与 trailers 写完才写半关。
    raw->StartReading();
    ASSERT_TRUE(RunUntil([&] { return raw->disconnected(); }, 10s));
    EXPECT_FALSE(raw->h2Failed()) << raw->h2().lastError();

    const auto &atEof = raw->streamsAtDisconnect();
    ASSERT_EQ(atEof.size(), 3u);
    const H2Stream &bigAtEof = atEof.at(big);
    EXPECT_EQ(bigAtEof.body.size(), kFlowControlledResponseBytes + 5) << "every DATA byte must arrive before the EOF";
    EXPECT_TRUE(bigAtEof.body == TestH2Peer::Frame(response));
    EXPECT_EQ(GrpcStatusOf(bigAtEof).value_or(""), "0") << "the trailers must arrive before the EOF";
    EXPECT_TRUE(bigAtEof.endStreamReceived);
    EXPECT_TRUE(atEof.at(waiting).rstReceived);
    EXPECT_EQ(atEof.at(waiting).rstErrorCode, h2::kRefusedStream);
    EXPECT_EQ(GrpcStatusOf(atEof.at(started)).value_or(""), "14");
    EXPECT_EQ(GrpcMessageOf(atEof.at(started)), "server shutting down");
    ASSERT_FALSE(raw->goawaysAtDisconnect().empty());
    EXPECT_EQ(raw->goawaysAtDisconnect().back().lastStreamId, started);
    EXPECT_EQ(raw->goawaysAtDisconnect().back().errorCode, h2::kNoError);

    ASSERT_TRUE(RunUntil([&] { return drained->count > 0; }, 5s));
    // 早于宽限到期:连接是对端读到 EOF 后关闭的,不是 forceClose。
    EXPECT_LT(drained->firstAt - shutdownAt, shutdown.drainTimeout + shutdown.closeGrace);
    RunFor(100ms);
    EXPECT_EQ(drained->count, 1) << "onDrained exactly once";
    EXPECT_EQ(server_->connectionCount(), 0u);
    EXPECT_EQ(Stats().inFlight, 0u);
    EXPECT_EQ(Stats().dropped, 0u);
}

TEST_F(ServerLoopbackTest, DrainTimeoutForceClosesWhenThePeerNeverOpensTheWindow)
{
    // 对端照常读取,却始终不给流控窗口(INITIAL_WINDOW_SIZE = 0):已 FinishOk 的应答一个 DATA 字节都发不出去。
    // 排空到期后控制帧照常送达,但写半关要等这条流写完 —— 等不到,从排空到期起算的 closeGrace 硬上限到期 forceClose;
    // 不 assert,onDrained 恰好一次。
    ASSERT_NE(StartServer(), nullptr);
    TestH2ClientOptions h2Options;
    h2Options.initialSettings = {{h2::kSettingsInitialWindowSize, 0u}};
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2, true, std::move(h2Options));
    RunFor(100ms); // SETTINGS / ACK 往返完成:服务端看到的流级发送窗口是 0
    const int32_t blocked = SendRawRequest(*raw, kHoldPath, std::nullopt);
    const int32_t waiting = SendRawRequest(*raw, kHoldNotStartedPath, std::nullopt);
    const int32_t started = SendRawRequest(*raw, kHoldPath, std::nullopt);
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 3; }, 5s));
    raw->SnapshotAtDisconnect({blocked, waiting, started});
    state_->held.front().FinishOk("blocked by flow control");
    // 应答头不受流控,照常送达;DATA 与 trailers 卡在服务端。
    ASSERT_TRUE(RunUntil([&]
                         {
        const H2Stream *stream = raw->h2().FindStream(blocked);
        return stream != nullptr && !stream->responseHeaders.empty(); }, 5s));

    const auto drained = std::make_shared<DrainRecord>();
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 300ms;
    shutdown.goawayPingTimeout = 100ms;
    shutdown.closeGrace = 1000ms;
    const auto shutdownAt = std::chrono::steady_clock::now();
    server_->BeginShutdown(shutdown, RecordDrain(drained));
    // 排空到期:REFUSED / UNAVAILABLE / 最终 GOAWAY 都不受流控,对端照常收到。
    ASSERT_TRUE(RunUntil([&]
                         {
        const H2Stream *a = raw->h2().FindStream(waiting);
        const H2Stream *b = raw->h2().FindStream(started);
        return a != nullptr && b != nullptr && a->rstReceived && GrpcStatusOf(*b).has_value(); }, 5s));
    const auto expiredAt = std::chrono::steady_clock::now();
    RunFor(300ms); // 远小于 closeGrace
    EXPECT_FALSE(raw->disconnected()) << "no FIN while a committed response is still blocked by flow control";
    EXPECT_EQ(server_->connectionCount(), 1u);
    EXPECT_EQ(drained->count, 0);

    ASSERT_TRUE(RunUntil([&] { return drained->count > 0; }, 5s));
    RunFor(200ms);
    EXPECT_EQ(drained->count, 1) << "onDrained exactly once";
    // 硬上限从排空到期那一刻起算(steady 复核,不会早到),也不会往后拖(上界留足调度余量)。
    EXPECT_GE(drained->firstAt - shutdownAt, shutdown.drainTimeout + shutdown.closeGrace);
    EXPECT_LT(drained->firstAt - expiredAt, shutdown.closeGrace + 1000ms);
    EXPECT_EQ(server_->connectionCount(), 0u);
    EXPECT_EQ(Stats().refusedOnShutdown, 1u);
    EXPECT_EQ(Stats().unavailableOnShutdown, 1u);
    EXPECT_EQ(Stats().inFlight, 0u);
    EXPECT_EQ(Stats().dropped, 0u);

    ASSERT_TRUE(RunUntil([&] { return raw->disconnected(); }, 2s));
    const auto &atClose = raw->streamsAtDisconnect();
    ASSERT_EQ(atClose.size(), 3u);
    EXPECT_TRUE(atClose.at(blocked).body.empty()) << "the peer never opened the window";
    EXPECT_FALSE(GrpcStatusOf(atClose.at(blocked)).has_value()) << "the blocked response never completed";
    EXPECT_EQ(atClose.at(waiting).rstErrorCode, h2::kRefusedStream);
    EXPECT_EQ(GrpcStatusOf(atClose.at(started)).value_or(""), "14");
    ASSERT_FALSE(raw->goawaysAtDisconnect().empty());
    EXPECT_EQ(raw->goawaysAtDisconnect().back().lastStreamId, started);
    EXPECT_EQ(raw->goawaysAtDisconnect().back().errorCode, h2::kNoError);
}

TEST_F(ServerLoopbackTest, DrainTimeoutRefusesStreamsStillUploadingTheRequest)
{
    // 排空到期时请求体还在上传的流:业务必然没开始,立即回 REFUSED_STREAM(对端可透明重试),不等它传完,
    // 也不让它拖住写半关直到硬上限;它从头到尾都不会被派发。
    ASSERT_NE(StartServer(), nullptr);
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2);
    RunFor(100ms); // 握手完成(服务端 SETTINGS / ACK 往返)
    const std::string frame = TestH2Peer::Frame("half uploaded payload");
    const int32_t uploading = raw->h2().OpenStream(GrpcRequestHeaders(kEchoPath), false);
    ASSERT_GT(uploading, 0) << raw->h2().lastError();
    ASSERT_TRUE(raw->h2().SendData(uploading, std::string_view(frame).substr(0, frame.size() / 2), false))
        << raw->h2().lastError();
    raw->Flush();
    RunFor(100ms); // 服务端已收下头部(路由通过)与前一半请求体
    raw->SnapshotAtDisconnect({uploading});

    const auto drained = std::make_shared<DrainRecord>();
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 300ms;
    shutdown.goawayPingTimeout = 100ms;
    shutdown.closeGrace = 20000ms; // 只作硬上限
    const auto shutdownAt = std::chrono::steady_clock::now();
    server_->BeginShutdown(shutdown, RecordDrain(drained));
    ASSERT_TRUE(RunUntil([&]
                         {
        const H2Stream *stream = raw->h2().FindStream(uploading);
        return stream != nullptr && stream->rstReceived; }, 5s));
    EXPECT_EQ(raw->h2().FindStream(uploading)->rstErrorCode, h2::kRefusedStream);
    EXPECT_TRUE(raw->h2().FindStream(uploading)->responseHeaders.empty());
    EXPECT_EQ(Stats().refusedOnShutdown, 1u);
    EXPECT_EQ(Stats().unavailableOnShutdown, 0u);

    // 没有别的流:最终 GOAWAY 已写出,REFUSED 写出后流即关闭,会话既不想读也不想写 → 写半关;对端读到 EOF 即关。
    ASSERT_TRUE(RunUntil([&] { return drained->count > 0 && raw->disconnected(); }, 5s));
    EXPECT_LT(drained->firstAt - shutdownAt, shutdown.drainTimeout + shutdown.closeGrace);
    RunFor(100ms);
    EXPECT_EQ(drained->count, 1) << "onDrained exactly once";
    EXPECT_EQ(state_->invocations, 0u) << "a request still uploading at drain expiry is never dispatched";
    EXPECT_EQ(Stats().dispatched, 0u);
    EXPECT_EQ(Stats().inFlight, 0u);
    ASSERT_FALSE(raw->goawaysAtDisconnect().empty());
    EXPECT_EQ(raw->goawaysAtDisconnect().back().lastStreamId, uploading);
    EXPECT_EQ(raw->goawaysAtDisconnect().back().errorCode, h2::kNoError);
}

TEST_F(ServerLoopbackTest, DrainTimeoutRefusesUploadingStreamsEvenAfterHeaderTimesAreTrimmed)
{
    // §15.17:头部收完时刻的记录只为截止起算,有上限 —— 对端在头部之后 RST 的流不会再有回调、记录残留,超限即修剪最老的一半。
    // 修复前排空到期按这份记录拒上传中的流:慢上传的老流被修剪掉之后漏拒,只能等它传完、或等 closeGrace 硬上限 forceClose。
    // 现在按会话状态枚举(ServerSession::AcceptedUndispatchedStreams),修剪不影响停机。
    ServerOptions options = QuietOptions();
    options.session.maxConcurrentStreams = 8;
    ASSERT_NE(StartServer(options), nullptr);
    const auto raw = MakeRaw(RawConnection::Mode::kHttp2);
    RunFor(100ms); // 握手完成(服务端 SETTINGS / ACK 往返)

    // 慢上传的流:id 最小,修剪时最先被丢。
    const std::string frame = TestH2Peer::Frame("half uploaded payload");
    const int32_t uploading = raw->h2().OpenStream(GrpcRequestHeaders(kEchoPath), false);
    ASSERT_GT(uploading, 0) << raw->h2().lastError();
    ASSERT_TRUE(raw->h2().SendData(uploading, std::string_view(frame).substr(0, frame.size() / 2), false))
        << raw->h2().lastError();
    raw->Flush();
    RunFor(50ms);

    // 记录上限与 server.cpp 同式:max(16, 2 × maxConcurrentStreams) = 16。再开 20 个流,每个收完头部(路由通过、登记时刻)
    // 后由对端 RST:记录残留,总数越过上限,最老的(含 uploading)被修剪。分批开,同时打开的流不超过 maxConcurrentStreams。
    constexpr int kResidualStreams = 20;
    constexpr int kBatch = 4;
    for (int opened = 0; opened < kResidualStreams; opened += kBatch)
    {
        std::vector<int32_t> batch;
        for (int i = 0; i < kBatch; ++i)
        {
            const int32_t streamId = raw->h2().OpenStream(GrpcRequestHeaders(kEchoPath), false);
            ASSERT_GT(streamId, 0) << raw->h2().lastError();
            batch.push_back(streamId);
        }
        raw->Flush();
        RunFor(30ms);
        for (const int32_t streamId : batch)
        {
            ASSERT_TRUE(raw->h2().SendRstStream(streamId, h2::kCancel)) << raw->h2().lastError();
        }
        raw->Flush();
        RunFor(30ms);
    }
    raw->SnapshotAtDisconnect({uploading});

    const auto drained = std::make_shared<DrainRecord>();
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 300ms;
    shutdown.goawayPingTimeout = 100ms;
    shutdown.closeGrace = 20000ms; // 只作硬上限:修复前漏拒的流要拖到这里才被 forceClose
    const auto shutdownAt = std::chrono::steady_clock::now();
    server_->BeginShutdown(shutdown, RecordDrain(drained));
    ASSERT_TRUE(RunUntil([&]
                         {
        const H2Stream *stream = raw->h2().FindStream(uploading);
        return stream != nullptr && stream->rstReceived; }, 5s))
        << "a stream still uploading at drain expiry must be refused even if its header time was trimmed";
    EXPECT_EQ(raw->h2().FindStream(uploading)->rstErrorCode, h2::kRefusedStream);
    EXPECT_TRUE(raw->h2().FindStream(uploading)->responseHeaders.empty());
    EXPECT_EQ(Stats().refusedOnShutdown, 1u) << "the reset streams are already closed and are not refused again";
    EXPECT_EQ(Stats().unavailableOnShutdown, 0u);

    // 拒掉之后没有打开的流:最终 GOAWAY 已写出、会话既不想读也不想写 → 写半关,对端读到 EOF 即关,远早于硬上限。
    ASSERT_TRUE(RunUntil([&] { return drained->count > 0 && raw->disconnected(); }, 5s));
    EXPECT_LT(drained->firstAt - shutdownAt, shutdown.drainTimeout + shutdown.closeGrace);
    RunFor(100ms);
    EXPECT_EQ(drained->count, 1) << "onDrained exactly once";
    EXPECT_TRUE(raw->streamsAtDisconnect().at(uploading).rstReceived) << "REFUSED_STREAM must reach the peer before the EOF";
    EXPECT_EQ(state_->invocations, 0u);
    EXPECT_EQ(Stats().dispatched, 0u);
    EXPECT_EQ(Stats().inFlight, 0u);
}

TEST_F(ServerLoopbackTest, ChannelSharesTheServerAddressRule)
{
    // channel.cpp 以前有一份私有校验,用有符号 from_chars 解析每段,会接受 "-1.2.3.4" / "1.2.3.-0"
    // (muduo InetAddress 随后落成 0.0.0.0,Linux 上会连到本机);现在客户端与服务端共用 address.h 的规则。
    for (const char *ip : {"-1.2.3.4", "1.2.3.-0", "+1.2.3.4", "127.0.0.01", "localhost"})
    {
        EXPECT_EQ(Channel::Create(loop_, ip, port_), nullptr) << ip;
        EXPECT_EQ(Server::Create(loop_, ip, port_), nullptr) << ip;
    }
}

TEST_F(ServerLoopbackTest, ShutdownWithoutConnectionsDrainsAsynchronously)
{
    ASSERT_NE(StartServer(), nullptr);
    auto drained = std::make_shared<int>(0);
    server_->BeginShutdown(ServerShutdownOptions{}, [drained] { ++*drained; });
    EXPECT_EQ(*drained, 0);
    ASSERT_TRUE(RunUntil([&] { return *drained > 0; }, 2s));
    RunFor(100ms);
    EXPECT_EQ(*drained, 1);
}

TEST_F(ServerLoopbackTest, BeginShutdownFromTimerCallbackWhileConnectionsArrive)
{
    ASSERT_NE(StartServer(), nullptr);
    // 先发起一批连接(非阻塞 connect,内核在 loop 转起来之前就完成握手、排进 backlog),
    // 再在第一轮的定时器回调里 BeginShutdown:Acceptor 的 channel 很可能与定时器在同一轮活跃,
    // 同步销毁 Acceptor 会撞 removeChannel 的断言;排队销毁则安全,销毁前到达的接入一律 close。
    std::vector<RawConnectionPtr> clients;
    for (int i = 0; i < 16; ++i)
    {
        clients.push_back(MakeRaw(RawConnection::Mode::kBytes, false));
    }
    auto drained = std::make_shared<int>(0);
    const std::weak_ptr<Server> weakServer = server_;
    ServerShutdownOptions shutdown;
    shutdown.drainTimeout = 500ms;
    shutdown.goawayPingTimeout = 100ms;
    shutdown.closeGrace = 100ms;
    loop_.runAfter(0.0, [weakServer, drained, shutdown]
                   {
        if (const auto server = weakServer.lock())
        {
            server->BeginShutdown(shutdown, [drained] { ++*drained; });
        } });
    ASSERT_TRUE(RunUntil([&] { return *drained > 0; }, 5s));
    RunFor(200ms);
    EXPECT_EQ(*drained, 1);
    EXPECT_EQ(server_->connectionCount(), 0u);
    EXPECT_LE(Stats().accepted, 16u);
    EXPECT_EQ(Stats().accepted, Stats().closedBeforePreface) << "none of these clients ever sent a byte";
    clients.clear();
}

TEST_F(ServerLoopbackTest, DestroyingTheServerWithLiveConnectionsInAQueuedTask)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    const auto outcome = StartCall(*channel, kHoldPath, "x");
    const auto idle = MakeRaw(RawConnection::Mode::kHttp2);
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 1 && server_->connectionCount() == 2; }, 5s));

    // 在 queueInLoop 任务里析构(不在 I/O 事件处理期间):剩余连接 forceClose,close 回调照样排 connectDestroyed。
    loop_.queueInLoop([this] { server_.reset(); });
    ASSERT_TRUE(RunUntil([&] { return outcome->count > 0 && idle->disconnected(); }, 5s));
    EXPECT_EQ(server_, nullptr);
    EXPECT_EQ(outcome->result.status.code(), StatusCode::kUnavailable) << outcome->result.status.ToString();
    state_->held.front().FinishOk("after destruction");
    state_->held.clear();
    RunFor(200ms);
}

TEST_F(ServerLoopbackTest, ProbeConnectionsAreSilent)
{
    ASSERT_NE(StartServer(), nullptr);
    constexpr int kProbes = 5;
    {
        const LogCaptureScope capture;
        for (int i = 0; i < kProbes; ++i)
        {
            const auto probe = MakeRaw(RawConnection::Mode::kBytes);
            RunFor(50ms); // 本端 SETTINGS 推迟到收到对端首字节:这段时间里服务端一个字节都不能写
            EXPECT_EQ(probe->bytesReceived(), 0u) << "probe #" << i;
            probe->Close();
            ASSERT_TRUE(RunUntil([&] { return Stats().closedBeforePreface == static_cast<uint64_t>(i + 1); }, 5s));
        }
        RunFor(100ms);
    }
    EXPECT_EQ(Stats().protocolErrors, 0u);
    EXPECT_EQ(Stats().accepted, static_cast<uint64_t>(kProbes));
    EXPECT_EQ(server_->connectionCount(), 0u);
    const std::string serverLog = WithoutPlatformHangupNoise(CapturedLog());
    EXPECT_TRUE(serverLog.empty()) << "probes must not log at WARN or above:\n" << serverLog;
}

TEST_F(ServerLoopbackTest, HandshakeTimeoutClosesSilentAndMagicOnlyConnections)
{
    ServerOptions options = QuietOptions();
    options.handshakeTimeout = 200ms;
    ASSERT_NE(StartServer(options), nullptr);

    const auto silent = MakeRaw(RawConnection::Mode::kBytes);
    ASSERT_TRUE(RunUntil([&] { return silent->disconnected(); }, 3s));
    EXPECT_EQ(silent->bytesReceived(), 0u);
    EXPECT_EQ(Stats().handshakeTimeout, 1u);
    EXPECT_EQ(Stats().closedBeforePreface, 1u);

    // 只发 client magic、不发 SETTINGS:服务端写出自己的 SETTINGS,但握手不算完成,照样超时。
    const auto magicOnly = MakeRaw(RawConnection::Mode::kBytes);
    magicOnly->SendRaw("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n");
    ASSERT_TRUE(RunUntil([&] { return magicOnly->disconnected(); }, 3s));
    EXPECT_GT(magicOnly->bytesReceived(), 0u) << "server SETTINGS follow the first client byte";
    EXPECT_EQ(Stats().handshakeTimeout, 2u);
    EXPECT_EQ(Stats().closedBeforePreface, 1u);
    EXPECT_EQ(Stats().protocolErrors, 0u);
}

TEST_F(ServerLoopbackTest, Http11RequestIsAProtocolErrorAndGetsNoBytes)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto client = MakeRaw(RawConnection::Mode::kBytes);
    client->SendRaw("GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n");
    ASSERT_TRUE(RunUntil([&] { return client->disconnected(); }, 3s));
    EXPECT_EQ(client->bytesReceived(), 0u) << client->rawReceived();
    EXPECT_EQ(Stats().protocolErrors, 1u);
    EXPECT_EQ(Stats().closedBeforePreface, 0u);
}

TEST_F(ServerLoopbackTest, NoThreadIsCreatedByTheServer)
{
    // 预热一轮(建 Server、连接、调用、排空、析构):Winsock 等系统组件的惰性初始化发生在快照之前。
    {
        ASSERT_NE(StartServer(), nullptr);
        const auto channel = MakeChannel();
        ASSERT_NE(channel, nullptr);
        const auto warmup = StartCall(*channel, kEchoPath, "warmup");
        ASSERT_TRUE(RunUntil([&] { return warmup->count > 0; }, 5s));
        ASSERT_TRUE(warmup->result.status.ok()) << warmup->result.status.ToString();
    }
    RunFor(200ms);
    server_.reset();
    RunFor(300ms);

    const std::set<uint64_t> before = muduo_grpc_test::SnapshotThreadIds();
    ASSERT_FALSE(before.empty());

    port_ = muduo_grpc_test::FindFreeLoopbackPort();
    ASSERT_NE(port_, 0);
    ASSERT_NE(StartServer(), nullptr);
    auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    std::vector<CallOutcomePtr> outcomes;
    for (int i = 0; i < 50; ++i)
    {
        outcomes.push_back(StartCall(*channel, kHoldPath, "call-" + std::to_string(i), 10000ms));
    }
    ASSERT_TRUE(RunUntil([&] { return state_->held.size() == 50; }, 5s));
    const std::set<uint64_t> during = muduo_grpc_test::SnapshotThreadIds();
    for (const ServerCall &call : state_->held)
    {
        call.FinishOk("ok");
    }
    state_->held.clear();
    ASSERT_TRUE(RunUntil([&] { return AllDone(outcomes); }, 5s));

    auto drained = std::make_shared<int>(0);
    server_->BeginShutdown(ServerShutdownOptions{}, [drained] { ++*drained; });
    channel.reset();
    ASSERT_TRUE(RunUntil([&] { return *drained > 0; }, 5s));
    server_.reset();
    RunFor(300ms);
    const std::set<uint64_t> after = muduo_grpc_test::SnapshotThreadIds();

    EXPECT_TRUE(NewThreadIds(before, during).empty())
        << NewThreadIds(before, during).size() << " new thread(s) while calls were in flight";
    EXPECT_TRUE(NewThreadIds(before, after).empty())
        << NewThreadIds(before, after).size() << " new thread(s) after the server was destroyed";
    EXPECT_LE(after.size(), before.size());
}

TEST(ServerStatsFormatTest, FieldOrderIsStable)
{
    ServerStatsSnapshot stats;
    stats.listen = "10.0.0.7:7001";
    stats.connections = 1;
    stats.connectionsPeak = 2;
    stats.accepted = 3;
    stats.closedBeforePreface = 4;
    stats.handshakeTimeout = 5;
    stats.protocolErrors = 6;
    stats.inFlight = 7;
    stats.inFlightPeak = 8;
    stats.dispatched = 9;
    stats.maxBacklog = 10;
    stats.maxDispatchedPerTurn = 11;
    stats.rejectedOverload = 12;
    stats.rejectedProtocol = 13;
    stats.unimplemented = 14;
    stats.deadlineBeforeStart = 15;
    stats.deadlineAfterStart = 16;
    stats.cancelledBeforeStart = 17;
    stats.abortedAfterStart = 18;
    stats.refusedOnShutdown = 19;
    stats.unavailableOnShutdown = 20;
    stats.lateFinish = 21;
    stats.dropped = 22;
    stats.droppedOffLoop = 23;
    stats.codes = {{0, 100}, {14, 2}};
    stats.methods = {{"CreateBattle", {{0, 3}, {14, 1}}}, {"DestroyBattle", {{0, 2}}}};
    EXPECT_EQ(muduo_grpc::FormatServerStats(stats),
              "listen=10.0.0.7:7001 conns=1/2 accepted=3 closed_before_preface=4 handshake_timeout=5 "
              "protocol_errors=6 inflight=7/8 dispatched=9 max_backlog=10 max_dispatched_per_turn=11 "
              "rejected_overload=12 rejected_protocol=13 unimplemented=14 deadline_before_start=15 "
              "deadline_after_start=16 cancelled_before_start=17 aborted_after_start=18 refused_on_shutdown=19 "
              "unavailable_on_shutdown=20 late_finish=21 dropped=22 dropped_off_loop=23 codes=0:100,14:2 "
              "methods={CreateBattle:0=3,CreateBattle:14=1,DestroyBattle:0=2}");

    EXPECT_EQ(muduo_grpc::FormatServerStats(ServerStatsSnapshot{}),
              "listen= conns=0/0 accepted=0 closed_before_preface=0 handshake_timeout=0 protocol_errors=0 "
              "inflight=0/0 dispatched=0 max_backlog=0 max_dispatched_per_turn=0 rejected_overload=0 "
              "rejected_protocol=0 unimplemented=0 deadline_before_start=0 deadline_after_start=0 "
              "cancelled_before_start=0 aborted_after_start=0 refused_on_shutdown=0 unavailable_on_shutdown=0 "
              "late_finish=0 dropped=0 dropped_off_loop=0 codes= methods={}");
}

// ── 拦截钩子(ChannelOptions::unaryInterceptor / ServerOptions::dispatchInterceptor)──────────────
// 传输层只管「怎么包」,不认识键;engine 拿它们接链路追踪(docs/design/distributed-tracing.md §5.3)。

// 出站钩子:追加的 metadata 到达服务端;包装后的回调恰好执行一次,并且在原回调之前。
TEST_F(ServerLoopbackTest, UnaryInterceptorAppendsMetadataAndWrapsTheCompletion)
{
    ASSERT_NE(StartServer(), nullptr);
    const auto order = std::make_shared<std::vector<std::string>>();
    muduo_grpc::ChannelOptions channelOptions;
    channelOptions.unaryInterceptor = [order](Metadata &metadata, muduo_grpc::UnaryCallback done) -> muduo_grpc::UnaryCallback
    {
        metadata.emplace_back("x-hook", "on");
        return [order, done = std::move(done)](UnaryCallResult &&result) mutable
        {
            order->push_back("wrapper");
            done(std::move(result));
        };
    };
    const auto channel = Channel::Create(loop_, "127.0.0.1", port_, std::move(channelOptions));
    ASSERT_NE(channel, nullptr);

    const auto outcome = std::make_shared<CallOutcome>();
    UnaryCallRequest request;
    request.method = kEchoPath;
    request.message = "ping";
    request.deadline = std::chrono::steady_clock::now() + 5s;
    channel->StartUnaryCall(std::move(request), [order, outcome](UnaryCallResult &&result)
                            {
        order->push_back("done");
        ++outcome->count;
        outcome->result = std::move(result); });
    ASSERT_TRUE(RunUntil([&] { return outcome->count > 0; }, 5s));
    RunFor(100ms);

    EXPECT_EQ(outcome->count, 1);
    ASSERT_TRUE(outcome->result.status.ok()) << outcome->result.status.ToString();
    EXPECT_EQ(*order, (std::vector<std::string>{"wrapper", "done"}));
    const std::string *hook = muduo_grpc::FindMetadata(state_->lastMetadata, "x-hook");
    ASSERT_NE(hook, nullptr);
    EXPECT_EQ(*hook, "on");
}

// 出站钩子排在通道的所有本地检查之前:截止已过的本地拒绝同样走包装后的回调。
TEST_F(ServerLoopbackTest, UnaryInterceptorAlsoWrapsLocalRejections)
{
    const auto wrapped = std::make_shared<int>(0);
    muduo_grpc::ChannelOptions channelOptions;
    channelOptions.unaryInterceptor = [wrapped](Metadata &, muduo_grpc::UnaryCallback done) -> muduo_grpc::UnaryCallback
    {
        return [wrapped, done = std::move(done)](UnaryCallResult &&result) mutable
        {
            ++*wrapped;
            done(std::move(result));
        };
    };
    const auto channel = Channel::Create(loop_, "127.0.0.1", port_, std::move(channelOptions));
    ASSERT_NE(channel, nullptr);

    const auto outcome = StartCall(*channel, kEchoPath, "ping", -1ms); // 截止已过:本地拒绝,不发出
    ASSERT_TRUE(RunUntil([&] { return outcome->count > 0; }, 5s));
    EXPECT_EQ(outcome->count, 1);
    EXPECT_EQ(outcome->result.status.code(), StatusCode::kDeadlineExceeded) << outcome->result.status.ToString();
    EXPECT_EQ(*wrapped, 1);
}

// 派发钩子:每次派发恰好经过钩子一次,读得到本次调用的 metadata;钩子误调 invoke 两次,handler 仍只执行一次。
TEST_F(ServerLoopbackTest, DispatchInterceptorRunsOncePerCallAndExtraInvokesAreIgnored)
{
    const auto hookCalls = std::make_shared<int>(0);
    const auto sawMetadata = std::make_shared<bool>(false);
    ServerOptions options = QuietOptions();
    options.dispatchInterceptor = [hookCalls, sawMetadata](const Metadata &metadata, const std::function<void()> &invoke)
    {
        ++*hookCalls;
        *sawMetadata = muduo_grpc::FindMetadata(metadata, "x-hook") != nullptr;
        invoke();
        invoke(); // 误调第二次:必须被忽略
    };
    ASSERT_NE(StartServer(std::move(options)), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto outcome = StartCall(*channel, kEchoPath, "ping", 5000ms, {{"x-hook", "on"}});
    ASSERT_TRUE(RunUntil([&] { return outcome->count > 0; }, 5s));
    RunFor(100ms);
    ASSERT_TRUE(outcome->result.status.ok()) << outcome->result.status.ToString();
    EXPECT_EQ(outcome->count, 1);
    EXPECT_EQ(*hookCalls, 1);
    EXPECT_TRUE(*sawMetadata);
    EXPECT_EQ(state_->invocations, 1u);
}

// 派发钩子一次都没调 invoke:服务端补调一次(打 ERROR),调用照常完成。
TEST_F(ServerLoopbackTest, DispatchInterceptorThatNeverInvokesStillDispatchesOnce)
{
    ServerOptions options = QuietOptions();
    options.dispatchInterceptor = [](const Metadata &, const std::function<void()> &) {};
    ASSERT_NE(StartServer(std::move(options)), nullptr);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto outcome = StartCall(*channel, kEchoPath, "ping");
    ASSERT_TRUE(RunUntil([&] { return outcome->count > 0; }, 5s));
    RunFor(100ms);
    ASSERT_TRUE(outcome->result.status.ok()) << outcome->result.status.ToString();
    EXPECT_EQ(outcome->result.response, "echo:ping");
    EXPECT_EQ(outcome->count, 1);
    EXPECT_EQ(state_->invocations, 1u);
}

} // namespace
