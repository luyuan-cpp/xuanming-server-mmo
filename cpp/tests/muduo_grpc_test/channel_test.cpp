// muduo_grpc::Channel 回环端到端单测(docs/design/grpc-muduo-transport.md §4.4–§6)。
//
// 测试线程自己建 muduo EventLoop;同一个 loop 上跑一个 muduo TcpServer,每条连接挂一个内存 nghttp2 服务端
// (TestH2Peer),按用例脚本应答。通道、连接、服务端全在这一个线程上,用 RunLoopUntil(loop.runEvery + quit)
// 驱动,不起线程、不死循环。
//
// 生命周期(AGENTS.md §11.7):服务端只存 weak_ptr<TcpConnection>;绑进 TcpServer 回调的是 weak_ptr<服务端>。
// 完成回调只捕获 shared_ptr<CallRecord>(用例体结束后 TearDown 还会转 loop,回调不能引用用例体的局部变量)。
// TearDown 先转空 loop(让排队的完成回调、forceClose、connectDestroyed 跑完)再停服务端,EventLoop 最后析构。

#include <gtest/gtest.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <deque>
#include <functional>
#include <map>
#include <memory>
#include <optional>
#include <random>
#include <set>
#include <string>
#include <string_view>
#include <unordered_map>
#include <utility>
#include <vector>

#include "muduo_test_loop.h"
#include "test_h2_peer.h"

// muduo 头放在所有标准库头之后(它的 Windows 适配头 #define 了 off_t / pid_t 等名字)。
#include "muduo_grpc/channel.h"
#include "muduo_grpc/grpc_timeout.h"
#include "muduo_grpc/metadata.h"
#include "muduo_grpc/status.h"

#include "muduo/base/Logging.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"

namespace
{

using muduo_grpc::Channel;
using muduo_grpc::ChannelOptions;
using muduo_grpc::ConnectivityState;
using muduo_grpc::Metadata;
using muduo_grpc::StatusCode;
using muduo_grpc::UnaryCallRequest;
using muduo_grpc::UnaryCallResult;
using muduo_grpc_test::HeaderList;
using muduo_grpc_test::ReceivedStream;
using muduo_grpc_test::TestH2Peer;
using namespace std::chrono_literals;
namespace h2 = muduo_grpc_test::h2;

constexpr char kMethod[] = "/client_rpc_router.ClientRpcRouter/Forward";
// gate 的 x-session-detail-bin 应用层值与它在线上的样子(见 codec_utils_test 的两层 base64 回归)。
constexpr char kSessionAppValue[] = "CgQIARABEgIIAQ==";
constexpr char kSessionWireValue[] = "Q2dRSUFSQUJFZ0lJQVE9PQ";

// ── 回环服务端 ──────────────────────────────────────────────────────────

class LoopbackH2Server : public std::enable_shared_from_this<LoopbackH2Server>
{
public:
    struct Connection
    {
        size_t index{0};                               // 第几条连接(从 0 起)
        std::weak_ptr<muduo::net::TcpConnection> tcp;  // §11.7:只存 weak,所有权在 TcpServer
        TestH2Peer peer;
        bool disconnected{false};
    };
    // 某个流的请求收完(END_STREAM)时调用;在 loop 线程上、nghttp2 回调栈之外。
    using RequestHandler = std::function<void(LoopbackH2Server &server, Connection &connection, int32_t streamId)>;
    // 每次收到客户端字节(已喂给 peer、已派发收完的请求)之后、刷出应答之前调用:双向流用例在这里
    // 按已收到的消息逐条应答,不必等请求 END_STREAM。
    using ActivityHandler = std::function<void(LoopbackH2Server &server, Connection &connection)>;

    LoopbackH2Server(muduo::net::EventLoop &loop, uint16_t port) : loop_(loop), port_(port) {}
    ~LoopbackH2Server() { Stop(); }
    LoopbackH2Server(const LoopbackH2Server &) = delete;
    LoopbackH2Server &operator=(const LoopbackH2Server &) = delete;

    // 必须已由 shared_ptr 持有。
    void Start(RequestHandler handler)
    {
        handler_ = std::move(handler);
        server_ = std::make_unique<muduo::net::TcpServer>(&loop_, muduo::net::InetAddress(port_, true),
                                                          "muduo_grpc_test_server");
        const std::weak_ptr<LoopbackH2Server> weakSelf = weak_from_this();
        server_->setConnectionCallback([weakSelf](const muduo::net::TcpConnectionPtr &conn)
                                       {
            if (const auto self = weakSelf.lock())
            {
                self->HandleConnection(conn);
            } });
        server_->setMessageCallback(
            [weakSelf](const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *buffer, muduo::Timestamp)
            {
                if (const auto self = weakSelf.lock())
                {
                    self->HandleMessage(conn, buffer);
                    return;
                }
                buffer->retrieveAll();
            });
        server_->start();
    }

    void SetHandler(RequestHandler handler) { handler_ = std::move(handler); }
    void SetActivityHandler(ActivityHandler handler) { activityHandler_ = std::move(handler); }

    // 析构 TcpServer:它在 loop 线程上同步 connectDestroyed 所有连接。
    void Stop()
    {
        handler_ = nullptr;
        activityHandler_ = nullptr;
        server_.reset();
    }

    // 把 peer 里待发的帧写进 TCP。
    void Flush(Connection &connection)
    {
        const auto conn = connection.tcp.lock();
        if (!conn || !conn->connected())
        {
            return;
        }
        const std::string out = connection.peer.TakeOutput();
        if (!out.empty())
        {
            conn->send(out.data(), static_cast<int>(out.size()));
        }
    }

    // 服务端主动断开(forceClose 排队到下一轮)。
    void Disconnect(Connection &connection)
    {
        if (const auto conn = connection.tcp.lock())
        {
            conn->forceClose();
        }
    }

    size_t connectionCount() const { return connections_.size(); }
    Connection &connection(size_t index) { return *connections_.at(index); }
    // 所有连接上收完的请求数(含透明重试的那一次)。
    size_t requestCount() const { return requestCount_; }
    const std::vector<std::string> &errors() const { return errors_; }

private:
    void HandleConnection(const muduo::net::TcpConnectionPtr &conn)
    {
        if (conn->connected())
        {
            auto connection = std::make_unique<Connection>();
            connection->index = connections_.size();
            connection->tcp = conn;
            indexByName_[conn->name()] = connection->index;
            conn->setTcpNoDelay(true);
            connections_.push_back(std::move(connection));
            Flush(*connections_.back()); // 服务端 SETTINGS
            return;
        }
        if (Connection *connection = Find(conn->name()))
        {
            connection->disconnected = true;
            connection->tcp.reset();
        }
    }

    void HandleMessage(const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *buffer)
    {
        Connection *connection = Find(conn->name());
        if (connection == nullptr || connection->disconnected)
        {
            buffer->retrieveAll();
            return;
        }
        const bool received = connection->peer.Receive(std::string_view(buffer->peek(), buffer->readableBytes()));
        buffer->retrieveAll();
        if (!received)
        {
            errors_.push_back("connection #" + std::to_string(connection->index) + ": " +
                              connection->peer.lastError());
            Disconnect(*connection);
            return;
        }
        for (const int32_t streamId : connection->peer.TakeCompletedRequests())
        {
            ++requestCount_;
            if (handler_)
            {
                handler_(*this, *connection, streamId);
            }
        }
        if (activityHandler_)
        {
            activityHandler_(*this, *connection);
        }
        Flush(*connection); // SETTINGS ACK / WINDOW_UPDATE / PING ACK / 应答
    }

    Connection *Find(const std::string &name)
    {
        const auto it = indexByName_.find(name);
        return it == indexByName_.end() ? nullptr : connections_.at(it->second).get();
    }

    muduo::net::EventLoop &loop_;
    const uint16_t port_;
    RequestHandler handler_;
    ActivityHandler activityHandler_;
    std::vector<std::unique_ptr<Connection>> connections_; // 断开后保留,供用例检查
    std::map<std::string, size_t> indexByName_;
    size_t requestCount_{0};
    std::vector<std::string> errors_;
    std::unique_ptr<muduo::net::TcpServer> server_; // 最后声明:析构时最先拆,其余成员仍有效
};

using Connection = LoopbackH2Server::Connection;

const std::string &HeaderOf(const ReceivedStream &stream, std::string_view name)
{
    static const std::string kMissing = "<missing>";
    const std::string *value = stream.FindHeader(name);
    return value != nullptr ? *value : kMissing;
}

// 回显:消息体 "echo:" + 请求负载;像路由服一样把 x-session-detail-bin 的线上原值写回响应头。
void RespondEcho(LoopbackH2Server &, Connection &connection, int32_t streamId)
{
    const ReceivedStream *stream = connection.peer.FindStream(streamId);
    ASSERT_NE(stream, nullptr);
    HeaderList initial;
    if (const std::string *session = stream->FindHeader("x-session-detail-bin"))
    {
        initial.emplace_back("x-session-detail-bin", *session);
    }
    const std::string payload = stream->body.size() >= 5 ? stream->body.substr(5) : std::string();
    EXPECT_TRUE(connection.peer.SendUnaryOk(streamId, "echo:" + payload, initial)) << connection.peer.lastError();
}

void RespondSmallOk(LoopbackH2Server &, Connection &connection, int32_t streamId)
{
    EXPECT_TRUE(connection.peer.SendUnaryOk(streamId, "ok")) << connection.peer.lastError();
}

void IgnoreRequest(LoopbackH2Server &, Connection &, int32_t)
{
}

// ── 调用结果记录 ────────────────────────────────────────────────────────

struct CallRecord
{
    int count{0};
    UnaryCallResult result;
    std::chrono::steady_clock::time_point completedAt{};
    bool channelAliveAtCompletion{false};
};
using CallRecordPtr = std::shared_ptr<CallRecord>;

// 回调只持有 shared_ptr<CallRecord>;channel 只以 weak_ptr 观察(判断回调是否在通道析构之后执行)。
muduo_grpc::UnaryCallback Recorder(const CallRecordPtr &record, std::weak_ptr<Channel> channel = {})
{
    return [record, channel](UnaryCallResult &&result)
    {
        ++record->count;
        record->result = std::move(result);
        record->completedAt = std::chrono::steady_clock::now();
        record->channelAliveAtCompletion = !channel.expired();
    };
}

UnaryCallRequest MakeRequest(std::string message, std::chrono::milliseconds timeout, Metadata metadata = {})
{
    UnaryCallRequest request;
    request.method = kMethod;
    request.message = std::move(message);
    request.metadata = std::move(metadata);
    request.deadline = std::chrono::steady_clock::now() + timeout;
    return request;
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

// ── 夹具 ────────────────────────────────────────────────────────────────

class ChannelLoopbackTest : public ::testing::Test
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
        // 用例体里的通道此时已析构:先转空 loop,让排队的完成回调、连接释放、forceClose、connectDestroyed 跑完。
        if (server_)
        {
            server_->SetHandler(nullptr);
            server_->SetActivityHandler(nullptr);
        }
        muduo_grpc_test::RunLoopFor(loop_, drainAfterTest_);
        if (server_)
        {
            for (const auto &error : server_->errors())
            {
                ADD_FAILURE() << "server side: " << error;
            }
            server_->Stop();
            muduo_grpc_test::RunLoopFor(loop_, 100ms);
            server_.reset();
        }
    }

    void StartServer(LoopbackH2Server::RequestHandler handler)
    {
        server_ = std::make_shared<LoopbackH2Server>(loop_, port_);
        server_->Start(std::move(handler));
    }

    std::shared_ptr<Channel> MakeChannel(ChannelOptions options = {})
    {
        auto channel = Channel::Create(loop_, "127.0.0.1", port_, std::move(options));
        EXPECT_NE(channel, nullptr);
        return channel;
    }

    bool RunUntil(const std::function<bool()> &done, std::chrono::milliseconds timeout)
    {
        return muduo_grpc_test::RunLoopUntil(loop_, done, timeout);
    }

    void RunFor(std::chrono::milliseconds duration) { muduo_grpc_test::RunLoopFor(loop_, duration); }

    // 第 index 条连接上第 n 个流;没有返回 nullptr。
    const ReceivedStream *ServerStream(size_t index, size_t n)
    {
        if (!server_ || server_->connectionCount() <= index)
        {
            return nullptr;
        }
        const TestH2Peer &peer = server_->connection(index).peer;
        return n < peer.StreamIds().size() ? peer.FindStream(peer.StreamIds()[n]) : nullptr;
    }

    // 声明顺序 = 析构逆序:EventLoop 最先构造、最后析构。
    muduo::net::EventLoop loop_;
    uint16_t port_{0};
    std::chrono::milliseconds drainAfterTest_{300};
    std::shared_ptr<LoopbackH2Server> server_;
};

// ── 用例 ────────────────────────────────────────────────────────────────

TEST_F(ChannelLoopbackTest, SuccessfulCallCompletesExactlyOnceAndNeverInsideStartUnaryCall)
{
    StartServer(&RespondEcho);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kIdle);
    EXPECT_EQ(channel->target(), "127.0.0.1:" + std::to_string(port_));

    const auto record = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("ping", 5s, {{"x-session-detail-bin", kSessionAppValue}}), Recorder(record));
    EXPECT_EQ(record->count, 0) << "完成回调不得在 StartUnaryCall 内同步执行";
    EXPECT_EQ(channel->outstandingCalls(), 1u);

    ASSERT_TRUE(RunUntil([&] { return record->count > 0; }, 5s));
    RunFor(200ms);
    EXPECT_EQ(record->count, 1) << "完成回调恰好一次";

    const UnaryCallResult &result = record->result;
    ASSERT_TRUE(result.status.ok()) << result.status.ToString();
    EXPECT_EQ(result.response, "echo:ping");
    const std::string *echoed = muduo_grpc::FindMetadata(result.initialMetadata, "x-session-detail-bin");
    ASSERT_NE(echoed, nullptr);
    EXPECT_EQ(*echoed, kSessionAppValue) << "回程只解线上那一层";
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kReady);
    EXPECT_EQ(channel->outstandingCalls(), 0u);

    // 服务端看到的请求。
    ASSERT_EQ(server_->connectionCount(), 1u);
    const ReceivedStream *stream = ServerStream(0, 0);
    ASSERT_NE(stream, nullptr);
    EXPECT_EQ(HeaderOf(*stream, ":path"), kMethod);
    EXPECT_EQ(HeaderOf(*stream, ":authority"), "127.0.0.1:" + std::to_string(port_));
    EXPECT_EQ(HeaderOf(*stream, "user-agent"), ChannelOptions{}.userAgent);
    EXPECT_EQ(HeaderOf(*stream, "x-session-detail-bin"), kSessionWireValue);
    const auto timeout = muduo_grpc::ParseGrpcTimeout(HeaderOf(*stream, "grpc-timeout"));
    ASSERT_TRUE(timeout.has_value()) << HeaderOf(*stream, "grpc-timeout");
    EXPECT_GT(*timeout, std::chrono::nanoseconds(0));
    EXPECT_LE(*timeout, std::chrono::nanoseconds(5s));
    EXPECT_EQ(stream->body, TestH2Peer::Frame("ping"));
}

TEST_F(ChannelLoopbackTest, LocalFailuresAreAlsoDeliveredAsynchronously)
{
    StartServer(&RespondEcho);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto expired = std::make_shared<CallRecord>();
    UnaryCallRequest late = MakeRequest("late", 5s);
    late.deadline = std::chrono::steady_clock::now() - 1ms;
    channel->StartUnaryCall(std::move(late), Recorder(expired));
    EXPECT_EQ(expired->count, 0);

    const auto badKey = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("x", 5s, {{"Bad-Key", "v"}}), Recorder(badKey));
    EXPECT_EQ(badKey->count, 0);

    ASSERT_TRUE(RunUntil([&] { return expired->count > 0 && badKey->count > 0; }, 2s));
    RunFor(100ms);
    EXPECT_EQ(expired->count, 1);
    EXPECT_EQ(badKey->count, 1);
    EXPECT_EQ(expired->result.status.code(), StatusCode::kDeadlineExceeded) << expired->result.status.ToString();
    EXPECT_EQ(badKey->result.status.code(), StatusCode::kInternal) << badKey->result.status.ToString();
    EXPECT_EQ(server_->connectionCount(), 0u) << "本地就失败的调用不应触发建连";
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kIdle);
}

TEST_F(ChannelLoopbackTest, GetStateWithTryToConnectReachesReadyOnPeerSettings)
{
    StartServer(&RespondEcho);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kIdle);
    EXPECT_EQ(channel->GetState(true), ConnectivityState::kConnecting);
    ASSERT_TRUE(RunUntil([&] { return channel->GetState(false) == ConnectivityState::kReady; }, 5s));
    EXPECT_EQ(server_->connectionCount(), 1u);
}

TEST_F(ChannelLoopbackTest, DeadlineExceededResetsStreamWithCancel)
{
    StartServer(&IgnoreRequest);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto record = std::make_shared<CallRecord>();
    const UnaryCallRequest request = MakeRequest("slow", 300ms);
    const auto deadline = request.deadline;
    channel->StartUnaryCall(request, Recorder(record));

    const auto streamReset = [&]
    {
        const ReceivedStream *stream = ServerStream(0, 0);
        return stream != nullptr && stream->rstReceived;
    };
    ASSERT_TRUE(RunUntil([&] { return record->count > 0 && streamReset(); }, 5s));
    EXPECT_EQ(record->result.status.code(), StatusCode::kDeadlineExceeded) << record->result.status.ToString();
    EXPECT_GE(record->completedAt, deadline) << "截止回调不得早于截止时间(steady_clock 复核)";
    const ReceivedStream *stream = ServerStream(0, 0);
    ASSERT_NE(stream, nullptr);
    EXPECT_EQ(stream->rstErrorCode, h2::kCancel);

    RunFor(200ms);
    EXPECT_EQ(record->count, 1);
    // §4.4:截止时若连接一帧没收到,至多发一次存活探测。
    EXPECT_LE(server_->connection(0).peer.pingsReceived(), 1u);
}

TEST_F(ChannelLoopbackTest, UnreachableTargetFailsFastInTransientFailure)
{
    // 不起服务端:port_ 是刚释放的空闲端口,没人监听。
    ChannelOptions options;
    options.connectTimeout = 300ms;
    options.backoff.initial = 10s; // 用例期间不触发后台重连
    const auto channel = MakeChannel(options);
    ASSERT_NE(channel, nullptr);
    drainAfterTest_ = 1200ms; // 让 ~TcpClient 挂的 1s removeConnector 定时器也走完

    const auto first = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("x", 5s), Recorder(first));
    ASSERT_TRUE(RunUntil([&] { return first->count > 0; }, 4s));
    EXPECT_EQ(first->result.status.code(), StatusCode::kUnavailable) << first->result.status.ToString();
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kTransientFailure);
    EXPECT_EQ(channel->GetState(true), ConnectivityState::kTransientFailure) << "退避期间 tryToConnect 不改变状态";

    // fail-fast:紧接着的调用立即 UNAVAILABLE,不再等一次 connect_timeout。
    const auto second = std::make_shared<CallRecord>();
    const auto secondStart = std::chrono::steady_clock::now();
    channel->StartUnaryCall(MakeRequest("y", 5s), Recorder(second));
    EXPECT_EQ(second->count, 0);
    ASSERT_TRUE(RunUntil([&] { return second->count > 0; }, 1s));
    EXPECT_EQ(second->result.status.code(), StatusCode::kUnavailable) << second->result.status.ToString();
    EXPECT_LT(second->completedAt - secondStart, std::chrono::steady_clock::duration(250ms));
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kTransientFailure);
}

TEST_F(ChannelLoopbackTest, RefusedConnectionEntersTransientFailureWithoutWaitingForConnectTimeout)
{
    // 不起服务端:port_ 是刚释放的空闲端口,对端回 RST。connectTimeout 与调用截止都设得远大于断言窗口,
    // 能在窗口内失败只可能来自 muduo Connector 的连接失败回调(§5),而不是 connect_timeout 或调用截止。
    ChannelOptions options;
    options.connectTimeout = 20s;
    options.backoff.initial = 10s; // 用例期间不触发后台重连
    const auto channel = MakeChannel(options);
    ASSERT_NE(channel, nullptr);
    drainAfterTest_ = 1200ms; // 让 ~TcpClient 挂的 1s removeConnector 定时器也走完

    const auto record = std::make_shared<CallRecord>();
    const auto start = std::chrono::steady_clock::now();
    const int64_t iterationsBefore = loop_.iteration();
    channel->StartUnaryCall(MakeRequest("x", 30s), Recorder(record));
    EXPECT_EQ(record->count, 0) << "完成回调不得在 StartUnaryCall 内同步执行";

    // Linux 回环拒连是即时的;Windows 协议栈收到 RST 后还会重试 SYN,拒连通常要 1~2s 才报给 WSAPoll。
    // 5s 的窗口留足余量,同时远小于 20s 的 connectTimeout。
    ASSERT_TRUE(RunUntil([&] { return record->count > 0; }, 8s)) << "拒连没有在 connectTimeout 之前报出";
    const auto elapsed = record->completedAt - start;
    EXPECT_LT(elapsed, std::chrono::steady_clock::duration(5s));
    EXPECT_EQ(record->result.status.code(), StatusCode::kUnavailable) << record->result.status.ToString();
    EXPECT_EQ(record->result.status.message().find("connect timeout"), std::string::npos)
        << record->result.status.ToString();
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kTransientFailure);

    // 等待期间 loop 不得空转:Windows 版 Channel 曾在 POLLHUP 时提前返回,吞掉 Connector 的错误回调,
    // 水平触发的 WSAPoll 会每轮立即返回。正常情况下只有 RunLoopUntil 的 5ms 检查定时器在推动循环
    // (约每毫秒 0.2 轮),这里放宽到每毫秒 1 轮再加 500 的余量;空转会多出几个数量级。
    const int64_t elapsedMs = std::chrono::duration_cast<std::chrono::milliseconds>(elapsed).count();
    EXPECT_LT(loop_.iteration() - iterationsBefore, 500 + elapsedMs);

    RunFor(200ms);
    EXPECT_EQ(record->count, 1) << "完成回调恰好一次";
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kTransientFailure);
}

TEST_F(ChannelLoopbackTest, PeerDisconnectFailsInFlightCallAndNextCallReconnects)
{
    StartServer(
        [](LoopbackH2Server &server, Connection &connection, int32_t streamId)
        {
            if (connection.index == 0)
            {
                server.Disconnect(connection); // 第一条连接:收到请求就断
                return;
            }
            RespondEcho(server, connection, streamId);
        });
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto first = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("first", 5s), Recorder(first));
    ASSERT_TRUE(RunUntil([&] { return first->count > 0; }, 5s));
    EXPECT_EQ(first->result.status.code(), StatusCode::kUnavailable) << first->result.status.ToString();
    EXPECT_EQ(server_->requestCount(), 1u) << "请求已交给对端:结果未知,不得透明重试";
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kIdle) << "READY 连接断开回 IDLE(pick_first)";

    const auto second = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("second", 5s), Recorder(second));
    ASSERT_TRUE(RunUntil([&] { return second->count > 0; }, 5s));
    ASSERT_TRUE(second->result.status.ok()) << second->result.status.ToString();
    EXPECT_EQ(second->result.response, "echo:second");
    EXPECT_EQ(server_->connectionCount(), 2u);
    EXPECT_TRUE(server_->connection(0).disconnected);
    EXPECT_EQ(first->count, 1);
}

TEST_F(ChannelLoopbackTest, TwoPhaseGoawayDrainsOldConnectionAndMovesNewCallsToNewConnection)
{
    StartServer(
        [](LoopbackH2Server &server, Connection &connection, int32_t streamId)
        {
            if (connection.index == 0)
            {
                return; // 老连接上的请求由用例脚本稍后应答
            }
            RespondEcho(server, connection, streamId);
        });
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto first = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("first", 10s), Recorder(first));
    ASSERT_TRUE(RunUntil([&] { return server_->requestCount() == 1; }, 5s));
    Connection &old = server_->connection(0);
    const int32_t firstStream = old.peer.StreamIds().at(0);

    // 第一段:last_stream_id = 2^31-1(go-zero MaxConnectionIdle 的优雅关闭)。
    ASSERT_TRUE(old.peer.SendShutdownNotice()) << old.peer.lastError();
    server_->Flush(old);
    ASSERT_TRUE(RunUntil([&] { return channel->GetState(false) == ConnectivityState::kIdle; }, 5s))
        << "收到 GOAWAY 即把连接标为 draining,通道回 IDLE";

    const auto second = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("second", 10s), Recorder(second));
    ASSERT_TRUE(RunUntil([&] { return second->count > 0; }, 5s));
    ASSERT_TRUE(second->result.status.ok()) << second->result.status.ToString();
    EXPECT_EQ(second->result.response, "echo:second");
    ASSERT_EQ(server_->connectionCount(), 2u) << "新调用必须走新连接";
    EXPECT_EQ(old.peer.StreamIds().size(), 1u) << "draining 连接上不得再开新流";
    EXPECT_EQ(server_->connection(1).peer.StreamIds().size(), 1u);
    EXPECT_EQ(first->count, 0) << "老连接上已接收的流照常等完";

    // 第二段:真实的 last_stream_id,然后把老流应答完。
    ASSERT_TRUE(old.peer.SendGoaway(firstStream, h2::kNoError, "graceful")) << old.peer.lastError();
    ASSERT_TRUE(old.peer.SendUnaryOk(firstStream, "late")) << old.peer.lastError();
    server_->Flush(old);
    ASSERT_TRUE(RunUntil([&] { return first->count > 0; }, 5s));
    ASSERT_TRUE(first->result.status.ok()) << first->result.status.ToString();
    EXPECT_EQ(first->result.response, "late");

    ASSERT_TRUE(RunUntil([&] { return server_->connection(0).disconnected; }, 5s))
        << "draining 连接的流清空后应由通道释放";
    EXPECT_FALSE(server_->connection(1).disconnected);
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kReady);
}

TEST_F(ChannelLoopbackTest, RefusedStreamIsRetriedTransparentlyOnce)
{
    StartServer(
        [](LoopbackH2Server &server, Connection &connection, int32_t streamId)
        {
            if (connection.peer.StreamIds().size() == 1)
            {
                EXPECT_TRUE(connection.peer.SendRstStream(streamId, h2::kRefusedStream));
                return;
            }
            RespondEcho(server, connection, streamId);
        });
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto record = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("retry-me", 5s), Recorder(record));
    ASSERT_TRUE(RunUntil([&] { return record->count > 0; }, 5s));
    ASSERT_TRUE(record->result.status.ok()) << record->result.status.ToString();
    EXPECT_EQ(record->result.response, "echo:retry-me");
    EXPECT_EQ(server_->connectionCount(), 1u);
    ASSERT_EQ(server_->requestCount(), 2u) << "原请求 + 一次透明重试";
    const ReceivedStream *original = ServerStream(0, 0);
    const ReceivedStream *retried = ServerStream(0, 1);
    ASSERT_NE(original, nullptr);
    ASSERT_NE(retried, nullptr);
    EXPECT_EQ(retried->body, original->body);
    EXPECT_EQ(HeaderOf(*retried, ":path"), HeaderOf(*original, ":path"));
}

TEST_F(ChannelLoopbackTest, RefusedTwiceFailsWithUnavailable)
{
    StartServer([](LoopbackH2Server &, Connection &connection, int32_t streamId)
                { EXPECT_TRUE(connection.peer.SendRstStream(streamId, h2::kRefusedStream)); });
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto record = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("refuse-me", 5s), Recorder(record));
    ASSERT_TRUE(RunUntil([&] { return record->count > 0; }, 5s));
    EXPECT_EQ(record->result.status.code(), StatusCode::kUnavailable) << record->result.status.ToString();
    RunFor(300ms);
    EXPECT_EQ(server_->requestCount(), 2u) << "透明重试只做一次";
    EXPECT_EQ(record->count, 1);
}

TEST_F(ChannelLoopbackTest, ChannelDestructionFailsInFlightCallsAfterDestruction)
{
    StartServer(&IgnoreRequest);
    auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto record = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("pending", 10s), Recorder(record, channel));
    ASSERT_TRUE(RunUntil([&] { return server_->requestCount() == 1; }, 5s));

    const std::weak_ptr<Channel> weak = channel;
    channel.reset();
    ASSERT_TRUE(weak.expired()) << "回调与连接都不应持有通道的强引用";
    EXPECT_EQ(record->count, 0) << "析构时不得同步执行回调";

    ASSERT_TRUE(RunUntil([&] { return record->count > 0; }, 2s));
    RunFor(200ms);
    EXPECT_EQ(record->count, 1) << "恰好一次";
    EXPECT_EQ(record->result.status.code(), StatusCode::kUnavailable);
    EXPECT_EQ(record->result.status.message(), "channel shut down");
    EXPECT_FALSE(record->channelAliveAtCompletion) << "回调在通道析构之后执行";
    ASSERT_TRUE(RunUntil([&] { return server_->connection(0).disconnected; }, 5s)) << "析构应关闭 TCP 连接";
}

TEST_F(ChannelLoopbackTest, OutstandingCallLimitIsResourceExhausted)
{
    StartServer(&RespondEcho);
    ChannelOptions options;
    options.maxOutstandingCalls = 2;
    const auto channel = MakeChannel(options);
    ASSERT_NE(channel, nullptr);

    std::vector<CallRecordPtr> records;
    for (int i = 0; i < 3; ++i)
    {
        records.push_back(std::make_shared<CallRecord>());
        channel->StartUnaryCall(MakeRequest("call-" + std::to_string(i), 5s), Recorder(records.back()));
    }
    ASSERT_TRUE(RunUntil([&] { return records[0]->count > 0 && records[1]->count > 0 && records[2]->count > 0; },
                         5s));
    EXPECT_TRUE(records[0]->result.status.ok()) << records[0]->result.status.ToString();
    EXPECT_TRUE(records[1]->result.status.ok()) << records[1]->result.status.ToString();
    EXPECT_EQ(records[2]->result.status.code(), StatusCode::kResourceExhausted)
        << records[2]->result.status.ToString();
    EXPECT_EQ(server_->requestCount(), 2u) << "超限的调用不发出";

    // 上限按「在途」计,不是累计:前两个结束后可以再发。
    const auto later = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("later", 5s), Recorder(later));
    ASSERT_TRUE(RunUntil([&] { return later->count > 0; }, 5s));
    EXPECT_TRUE(later->result.status.ok()) << later->result.status.ToString();
}

TEST_F(ChannelLoopbackTest, OversizedRequestIsRejectedLocally)
{
    StartServer(&RespondSmallOk);
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    constexpr size_t kLimit = 4 * 1024 * 1024; // 默认 maxSendMessageBytes:grpc-go 服务端接收上限

    const auto tooBig = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest(std::string(kLimit + 1, 'x'), 5s), Recorder(tooBig));
    ASSERT_TRUE(RunUntil([&] { return tooBig->count > 0; }, 2s));
    EXPECT_EQ(tooBig->result.status.code(), StatusCode::kResourceExhausted) << tooBig->result.status.ToString();
    EXPECT_EQ(server_->connectionCount(), 0u) << "超限请求一个字节都不发";
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kIdle);

    // 恰好等于上限的请求能发出(多帧 + 流控 + 输出缓冲背压一并走一遍)。
    const auto atLimit = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest(std::string(kLimit, 'y'), 10s), Recorder(atLimit));
    ASSERT_TRUE(RunUntil([&] { return atLimit->count > 0; }, 10s));
    ASSERT_TRUE(atLimit->result.status.ok()) << atLimit->result.status.ToString();
    EXPECT_EQ(atLimit->result.response, "ok");
    const ReceivedStream *stream = ServerStream(0, 0);
    ASSERT_NE(stream, nullptr);
    EXPECT_EQ(stream->body.size(), kLimit + 5);
}

// §0 / §6:新模块不创建任何线程。快照比较的是线程 id 集合:不得出现新线程;
// 数量只要求不增加(操作系统自己的线程池工作线程可能在这期间退出,数量用相等会偶发误报)。
TEST_F(ChannelLoopbackTest, NoThreadIsCreatedForConnectionsOrCalls)
{
    StartServer(&RespondEcho);
    // 预热一轮(建连、调用、拆通道):Winsock 等系统组件的惰性初始化发生在快照之前。
    {
        const auto warmup = MakeChannel();
        ASSERT_NE(warmup, nullptr);
        const auto record = std::make_shared<CallRecord>();
        warmup->StartUnaryCall(MakeRequest("warmup", 5s), Recorder(record));
        ASSERT_TRUE(RunUntil([&] { return record->count > 0; }, 5s));
        ASSERT_TRUE(record->result.status.ok()) << record->result.status.ToString();
    }
    RunFor(300ms);

    const std::set<uint64_t> before = muduo_grpc_test::SnapshotThreadIds();
    ASSERT_FALSE(before.empty());

    auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);
    constexpr int kCalls = 50;
    std::vector<CallRecordPtr> records;
    for (int i = 0; i < kCalls; ++i)
    {
        records.push_back(std::make_shared<CallRecord>());
        channel->StartUnaryCall(MakeRequest("call-" + std::to_string(i), 10s), Recorder(records.back()));
    }
    ASSERT_TRUE(RunUntil([&] { return server_->requestCount() > 1; }, 5s)); // 第二条连接已建好、调用在途
    const std::set<uint64_t> during = muduo_grpc_test::SnapshotThreadIds();

    const auto allDone = [&]
    {
        for (const auto &record : records)
        {
            if (record->count == 0)
            {
                return false;
            }
        }
        return true;
    };
    ASSERT_TRUE(RunUntil(allDone, 10s));
    for (int i = 0; i < kCalls; ++i)
    {
        EXPECT_TRUE(records[i]->result.status.ok()) << i << ": " << records[i]->result.status.ToString();
        EXPECT_EQ(records[i]->result.response, "echo:call-" + std::to_string(i));
    }
    channel.reset();
    RunFor(300ms);
    const std::set<uint64_t> after = muduo_grpc_test::SnapshotThreadIds();

    EXPECT_TRUE(NewThreadIds(before, during).empty()) << NewThreadIds(before, during).size() << " new thread(s) while calls were in flight";
    EXPECT_TRUE(NewThreadIds(before, after).empty()) << NewThreadIds(before, after).size() << " new thread(s) after the channel was destroyed";
    EXPECT_LE(after.size(), before.size());
}

// ── 双向流与周期 keepalive(§13.2 / §13.3)─────────────────────────────────

constexpr char kStreamMethod[] = "/etcdserverpb.Watch/Watch";

// 流回调的记录。回调只捕获 shared_ptr(TearDown 之后队列里的回调仍可能执行)。
struct StreamEvents
{
    int headers{0};
    std::vector<std::string> messages;
    int closes{0};
    muduo_grpc::Status status;
    Metadata trailing;
};
using StreamEventsPtr = std::shared_ptr<StreamEvents>;

muduo_grpc::StreamCallbacks StreamRecorder(const StreamEventsPtr &events)
{
    muduo_grpc::StreamCallbacks callbacks;
    callbacks.onHeaders = [events](const Metadata &) { ++events->headers; };
    callbacks.onMessage = [events](std::string &&message) { events->messages.push_back(std::move(message)); };
    callbacks.onClose = [events](const muduo_grpc::Status &status, const Metadata &trailing)
    {
        ++events->closes;
        events->status = status;
        events->trailing = trailing;
    };
    return callbacks;
}

// 服务端脚本:每个流上已收齐的消息逐条回显("echo:" + 负载,首条之前先发响应头);
// 客户端半关闭且消息都回显完后回 trailers(grpc-status 0)。
struct EchoStreamState
{
    std::map<int32_t, size_t> consumed; // 每个流的 body 已回显到哪个偏移
    std::set<int32_t> headersSent;
    std::set<int32_t> finished;
};

LoopbackH2Server::ActivityHandler EchoStreams(const std::shared_ptr<EchoStreamState> &state)
{
    return [state](LoopbackH2Server &, Connection &connection)
    {
        for (const int32_t streamId : connection.peer.StreamIds())
        {
            const ReceivedStream *stream = connection.peer.FindStream(streamId);
            if (stream == nullptr || stream->closed || stream->rstReceived || state->finished.count(streamId) != 0)
            {
                continue;
            }
            size_t &offset = state->consumed[streamId];
            while (stream->body.size() - offset >= 5)
            {
                const auto *prefix = reinterpret_cast<const unsigned char *>(stream->body.data() + offset);
                const size_t length = (static_cast<size_t>(prefix[1]) << 24) | (static_cast<size_t>(prefix[2]) << 16) |
                                      (static_cast<size_t>(prefix[3]) << 8) | static_cast<size_t>(prefix[4]);
                if (stream->body.size() - offset - 5 < length)
                {
                    break;
                }
                const std::string payload = stream->body.substr(offset + 5, length);
                offset += 5 + length;
                if (state->headersSent.insert(streamId).second)
                {
                    EXPECT_TRUE(connection.peer.SendHeaders(
                        streamId, {{":status", "200"}, {"content-type", "application/grpc"}}, false))
                        << connection.peer.lastError();
                }
                EXPECT_TRUE(connection.peer.SendData(streamId, TestH2Peer::Frame("echo:" + payload), false))
                    << connection.peer.lastError();
            }
            if (stream->requestEnded && offset == stream->body.size())
            {
                state->finished.insert(streamId);
                if (state->headersSent.insert(streamId).second)
                {
                    EXPECT_TRUE(connection.peer.SendTrailersOnly(streamId, 0, "")) << connection.peer.lastError();
                }
                else
                {
                    EXPECT_TRUE(connection.peer.SendHeaders(streamId, {{"grpc-status", "0"}, {"grpc-message", ""}}, true))
                        << connection.peer.lastError();
                }
            }
        }
    };
}

TEST_F(ChannelLoopbackTest, BidiStreamDeliversEachMessageInOrderAndClosesExactlyOnce)
{
    StartServer(&IgnoreRequest);
    server_->SetActivityHandler(EchoStreams(std::make_shared<EchoStreamState>()));
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {{"x-trace", "t1"}}, StreamRecorder(events));
    ASSERT_NE(stream, nullptr);
    // 连上之前写:本端缓冲,开流后按序发出。
    EXPECT_TRUE(stream->Write("a"));
    EXPECT_TRUE(stream->Write("b"));
    EXPECT_FALSE(stream->closed());
    EXPECT_EQ(events->closes, 0) << "回调不得在 StartStream / Write 内同步执行";
    EXPECT_TRUE(events->messages.empty());

    ASSERT_TRUE(RunUntil([&] { return events->messages.size() >= 2; }, 5s));
    // 开流之后再写:流的 DATA 已挂起(DEFERRED),这次写要把它唤醒。
    EXPECT_TRUE(stream->Write("c"));
    ASSERT_TRUE(RunUntil([&] { return events->messages.size() >= 3; }, 5s));
    stream->WritesDone();
    EXPECT_FALSE(stream->Write("late")) << "半关闭之后不能再写";
    ASSERT_TRUE(RunUntil([&] { return events->closes > 0; }, 5s));
    RunFor(200ms);

    EXPECT_EQ(events->closes, 1) << "onClose 恰好一次";
    EXPECT_TRUE(events->status.ok()) << events->status.ToString();
    EXPECT_EQ(events->headers, 1);
    EXPECT_EQ(events->messages, (std::vector<std::string>{"echo:a", "echo:b", "echo:c"}));
    EXPECT_TRUE(stream->closed());

    const ReceivedStream *serverStream = ServerStream(0, 0);
    ASSERT_NE(serverStream, nullptr);
    EXPECT_EQ(HeaderOf(*serverStream, ":path"), kStreamMethod);
    EXPECT_EQ(HeaderOf(*serverStream, "x-trace"), "t1");
    EXPECT_EQ(serverStream->FindHeader("grpc-timeout"), nullptr) << "流没有截止时间";
    EXPECT_EQ(serverStream->body, TestH2Peer::Frame("a") + TestH2Peer::Frame("b") + TestH2Peer::Frame("c"));
    EXPECT_TRUE(serverStream->requestEnded);
}

TEST_F(ChannelLoopbackTest, CancelledStreamIsResetWithCancelAndNeverCallsBackAgain)
{
    StartServer(&IgnoreRequest);
    server_->SetActivityHandler(EchoStreams(std::make_shared<EchoStreamState>()));
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {}, StreamRecorder(events));
    EXPECT_TRUE(stream->Write("first"));
    ASSERT_TRUE(RunUntil([&] { return !events->messages.empty(); }, 5s));

    stream->Cancel();
    stream->Cancel(); // 幂等
    EXPECT_TRUE(stream->closed());
    EXPECT_FALSE(stream->Write("after-cancel"));
    ASSERT_TRUE(RunUntil(
        [&]
        {
            const ReceivedStream *serverStream = ServerStream(0, 0);
            return serverStream != nullptr && serverStream->rstReceived;
        },
        5s));
    RunFor(200ms);
    EXPECT_EQ(ServerStream(0, 0)->rstErrorCode, h2::kCancel);
    EXPECT_EQ(events->closes, 0) << "Cancel 之后不再有任何回调";
    EXPECT_EQ(events->messages.size(), 1u);

    // 取消只影响该流:同一连接上照样能开新流。
    const auto second = std::make_shared<StreamEvents>();
    const auto again = channel->StartStream(kStreamMethod, {}, StreamRecorder(second));
    EXPECT_TRUE(again->Write("second"));
    ASSERT_TRUE(RunUntil([&] { return !second->messages.empty(); }, 5s));
    EXPECT_EQ(second->messages.front(), "echo:second");
    EXPECT_EQ(server_->connectionCount(), 1u);
}

TEST_F(ChannelLoopbackTest, ChannelDestructionClosesStreamsWithUnavailableUnlessCancelled)
{
    StartServer(&IgnoreRequest); // 收下流,不应答
    auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto kept = std::make_shared<StreamEvents>();
    const auto cancelled = std::make_shared<StreamEvents>();
    const auto keptStream = channel->StartStream(kStreamMethod, {}, StreamRecorder(kept));
    const auto cancelledStream = channel->StartStream(kStreamMethod, {}, StreamRecorder(cancelled));
    EXPECT_TRUE(keptStream->Write("x"));
    EXPECT_TRUE(cancelledStream->Write("y"));
    ASSERT_TRUE(RunUntil([&] { return ServerStream(0, 1) != nullptr; }, 5s));

    channel.reset();
    // 通道析构时两个 onClose 都已排进队列;此刻再 Cancel 必须把其中一个压住(句柄只剩对流记录的弱引用)。
    cancelledStream->Cancel();
    ASSERT_TRUE(RunUntil([&] { return kept->closes > 0; }, 2s));
    RunFor(200ms);

    EXPECT_EQ(kept->closes, 1);
    EXPECT_EQ(kept->status.code(), StatusCode::kUnavailable) << kept->status.ToString();
    EXPECT_EQ(cancelled->closes, 0);
    EXPECT_TRUE(keptStream->closed());
    EXPECT_FALSE(keptStream->Write("after")) << "通道没了,句柄的操作一律失败、不崩";
}

TEST_F(ChannelLoopbackTest, StreamToUnreachableTargetClosesWithUnavailable)
{
    // 不起服务端:port_ 是刚释放的空闲端口,没人监听。
    ChannelOptions options;
    options.connectTimeout = 300ms;
    options.backoff.initial = 10s; // 用例期间不触发后台重连
    const auto channel = MakeChannel(options);
    ASSERT_NE(channel, nullptr);
    drainAfterTest_ = 1200ms;

    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {}, StreamRecorder(events));
    EXPECT_TRUE(stream->Write("buffered")) << "未连上时先在本端缓冲";
    ASSERT_TRUE(RunUntil([&] { return events->closes > 0; }, 4s));
    RunFor(100ms);
    EXPECT_EQ(events->closes, 1);
    EXPECT_EQ(events->status.code(), StatusCode::kUnavailable) << events->status.ToString();
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kTransientFailure);

    // TRANSIENT_FAILURE 期间新开的流:不等连接,下一轮就以 UNAVAILABLE 结束(仍是异步)。
    const auto late = std::make_shared<StreamEvents>();
    const auto lateStream = channel->StartStream(kStreamMethod, {}, StreamRecorder(late));
    EXPECT_EQ(late->closes, 0);
    EXPECT_TRUE(lateStream->closed());
    ASSERT_TRUE(RunUntil([&] { return late->closes > 0; }, 1s));
    EXPECT_EQ(late->status.code(), StatusCode::kUnavailable) << late->status.ToString();
}

TEST_F(ChannelLoopbackTest, OversizedStreamWriteEndsTheStreamWithResourceExhausted)
{
    StartServer(&IgnoreRequest);
    ChannelOptions options;
    options.maxSendMessageBytes = 16;
    const auto channel = MakeChannel(options);
    ASSERT_NE(channel, nullptr);

    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {}, StreamRecorder(events));
    EXPECT_TRUE(stream->Write(std::string(16, 'x')));
    EXPECT_FALSE(stream->Write(std::string(17, 'x')));
    EXPECT_TRUE(stream->closed());
    ASSERT_TRUE(RunUntil([&] { return events->closes > 0; }, 2s));
    RunFor(100ms);
    EXPECT_EQ(events->closes, 1);
    EXPECT_EQ(events->status.code(), StatusCode::kResourceExhausted) << events->status.ToString();
}

TEST_F(ChannelLoopbackTest, PeriodicKeepalivePingsOnlyWhenEnabledAndAStreamIsActive)
{
    StartServer(&IgnoreRequest); // 流常开、服务端一字不回:连接上只有 PING 能证明对端还活着
    ChannelOptions options;
    options.keepaliveTime = 200ms;
    options.keepaliveTimeout = 2s;
    const auto channel = MakeChannel(options);
    ASSERT_NE(channel, nullptr);

    ASSERT_EQ(channel->GetState(true), ConnectivityState::kConnecting);
    ASSERT_TRUE(RunUntil([&] { return channel->GetState(false) == ConnectivityState::kReady; }, 5s));
    RunFor(600ms);
    ASSERT_EQ(server_->connectionCount(), 1u);
    EXPECT_EQ(server_->connection(0).peer.pingsReceived(), 0u) << "没有活跃流时不发周期 PING(grpc-go 不放行无流 PING)";

    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {}, StreamRecorder(events));
    EXPECT_TRUE(stream->Write("open"));
    ASSERT_TRUE(RunUntil([&] { return server_->connection(0).peer.pingsReceived() >= 2; }, 5s));
    // 服务端自动回 PING ACK:连接不因探测超时被判死,流仍开着。
    RunFor(300ms);
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kReady);
    EXPECT_EQ(events->closes, 0);
    EXPECT_FALSE(stream->closed());
    EXPECT_EQ(server_->connectionCount(), 1u);
}

TEST_F(ChannelLoopbackTest, NoPeriodicPingWhenKeepaliveIsDisabledEvenWithAnActiveStream)
{
    StartServer(&IgnoreRequest);
    const auto channel = MakeChannel(); // keepaliveTime 默认 0:Go 业务服务通道的配置
    ASSERT_NE(channel, nullptr);

    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {}, StreamRecorder(events));
    EXPECT_TRUE(stream->Write("open"));
    ASSERT_TRUE(RunUntil([&] { return ServerStream(0, 0) != nullptr; }, 5s));
    RunFor(800ms);
    EXPECT_EQ(server_->connection(0).peer.pingsReceived(), 0u)
        << "关掉周期 keepalive 的通道只在调用截止时探测(§4.4),流上不发 PING";
    EXPECT_EQ(events->closes, 0);
}

TEST_F(ChannelLoopbackTest, PeerTrailersWithoutRstStillCloseTheStreamWithThePeerStatus)
{
    // 服务端回完 trailers(END_STREAM)就停,不补 RST_STREAM;客户端还没 WritesDone,本端方向开着。
    // 不处理的话 nghttp2 只把流置为 half-closed(remote),onClose 永远不来。
    StartServer(&IgnoreRequest);
    const auto answered = std::make_shared<std::set<int32_t>>(); // 状态随用例走,--gtest_repeat 也不串
    server_->SetActivityHandler(
        [answered](LoopbackH2Server &, Connection &connection)
        {
            for (const int32_t streamId : connection.peer.StreamIds())
            {
                const ReceivedStream *stream = connection.peer.FindStream(streamId);
                if (stream == nullptr || stream->closed || stream->requestEnded || stream->body.size() < 5 + 1 ||
                    !answered->insert(streamId).second)
                {
                    continue;
                }
                EXPECT_TRUE(connection.peer.SendHeaders(streamId, {{":status", "200"}, {"content-type", "application/grpc"}},
                                                        false));
                EXPECT_TRUE(connection.peer.SendData(streamId, TestH2Peer::Frame("echo:x"), false));
                EXPECT_TRUE(connection.peer.SendHeaders(streamId, {{"grpc-status", "5"}, {"grpc-message", "gone"}}, true));
            }
        });
    const auto channel = MakeChannel();
    ASSERT_NE(channel, nullptr);

    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {}, StreamRecorder(events));
    EXPECT_TRUE(stream->Write("x"));
    ASSERT_TRUE(RunUntil([&] { return events->closes > 0; }, 5s)) << "对端 trailers 之后 onClose 必须到达";
    RunFor(200ms);

    EXPECT_EQ(events->closes, 1);
    EXPECT_EQ(events->status.code(), StatusCode::kNotFound) << events->status.ToString();
    EXPECT_EQ(events->status.message(), "gone");
    EXPECT_EQ(events->messages, (std::vector<std::string>{"echo:x"}));
    EXPECT_FALSE(stream->Write("after")) << "流已结束";
    const ReceivedStream *serverStream = ServerStream(0, 0);
    ASSERT_NE(serverStream, nullptr);
    EXPECT_TRUE(serverStream->rstReceived) << "客户端补 RST_STREAM 关掉自己这一侧";
    EXPECT_EQ(serverStream->rstErrorCode, h2::kNoError);
    EXPECT_FALSE(serverStream->requestEnded);
}

TEST_F(ChannelLoopbackTest, SingleLargeMessageFitsAnEmptyStreamBufferButABacklogIsCapped)
{
    StartServer(&IgnoreRequest);
    ChannelOptions options;
    options.maxStreamPendingSendBytes = 64;
    options.maxSendMessageBytes = 1024;
    const auto channel = MakeChannel(options);
    ASSERT_NE(channel, nullptr);

    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {}, StreamRecorder(events));
    // 还没连上:消息在本端缓冲。缓冲为空时总放行一条,哪怕它大于待发上限(单条只受 maxSendMessageBytes 约束)。
    EXPECT_TRUE(stream->Write(std::string(200, 'a')));
    // 已有积压再写就超限:以 RESOURCE_EXHAUSTED 结束这个流。
    EXPECT_FALSE(stream->Write("b"));
    EXPECT_TRUE(stream->closed());
    ASSERT_TRUE(RunUntil([&] { return events->closes > 0; }, 2s));
    RunFor(100ms);
    EXPECT_EQ(events->closes, 1);
    EXPECT_EQ(events->status.code(), StatusCode::kResourceExhausted) << events->status.ToString();
}

TEST_F(ChannelLoopbackTest, MaintenanceWaitsForSuccessfulUnaryCallbackToReturn)
{
    StartServer(&RespondEcho);
    const auto maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
    ChannelOptions options;
    options.maintenance = maintenance;
    const auto channel = MakeChannel(options);
    const auto record = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("maintenance", 5s), [maintenance, record](UnaryCallResult &&result)
    {
        std::string frontier;
        EXPECT_FALSE(maintenance->Joined());
        EXPECT_FALSE(maintenance->Frontier(frontier));
        ++record->count;
        record->result = std::move(result);
    });
    maintenance->Close();
    EXPECT_FALSE(maintenance->Joined());
    ASSERT_TRUE(RunUntil([maintenance] { return maintenance->Joined(); }, 5s));
    ASSERT_EQ(record->count, 1);
    EXPECT_TRUE(record->result.status.ok());
    std::string frontier;
    EXPECT_TRUE(maintenance->Frontier(frontier));
    EXPECT_FALSE(frontier.empty());
}

TEST_F(ChannelLoopbackTest, MaintenanceTracksRejectedCallbacksAndTheirNestedCallsAfterClose)
{
    const auto maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
    maintenance->Close();
    ChannelOptions options;
    options.maintenance = maintenance;
    const auto channel = MakeChannel(options);
    const auto callbacks = std::make_shared<int>(0);
    const std::weak_ptr<Channel> weakChannel = channel;
    channel->StartUnaryCall(MakeRequest("rejected", 5s), [maintenance, callbacks, weakChannel](UnaryCallResult &&result)
    {
        EXPECT_EQ(result.status.code(), StatusCode::kUnavailable);
        EXPECT_FALSE(maintenance->Joined());
        ++*callbacks;
        if (const auto current = weakChannel.lock())
        {
            current->StartUnaryCall(MakeRequest("nested", 5s), [maintenance, callbacks](UnaryCallResult &&nested)
            {
                EXPECT_EQ(nested.status.code(), StatusCode::kUnavailable);
                EXPECT_FALSE(maintenance->Joined());
                ++*callbacks;
            });
        }
    });
    EXPECT_EQ(*callbacks, 0);
    EXPECT_EQ(channel->outstandingCalls(), 0u);
    EXPECT_FALSE(maintenance->Joined());
    std::string frontier;
    EXPECT_FALSE(maintenance->Frontier(frontier));
    ASSERT_TRUE(RunUntil([maintenance] { return maintenance->Joined(); }, 2s));
    EXPECT_EQ(*callbacks, 2);
    EXPECT_EQ(channel->GetState(false), ConnectivityState::kIdle);
    EXPECT_TRUE(maintenance->Frontier(frontier)) << "本地拒绝已返回，不冒充远端未知提交";
}

TEST_F(ChannelLoopbackTest, MaintenanceTracksRejectedStreamCloseCallback)
{
    const auto maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
    maintenance->Close();
    ChannelOptions options;
    options.maintenance = maintenance;
    const auto channel = MakeChannel(options);
    const auto events = std::make_shared<StreamEvents>();
    auto callbacks = StreamRecorder(events);
    auto close = std::move(callbacks.onClose);
    callbacks.onClose = [maintenance, close = std::move(close)](const muduo_grpc::Status &status, const Metadata &trailing)
    {
        EXPECT_FALSE(maintenance->Joined());
        close(status, trailing);
    };
    const auto stream = channel->StartStream(kStreamMethod, {}, std::move(callbacks));
    EXPECT_TRUE(stream->closed());
    EXPECT_FALSE(stream->Write("rejected"));
    EXPECT_EQ(events->closes, 0);
    EXPECT_FALSE(maintenance->Joined());
    ASSERT_TRUE(RunUntil([maintenance] { return maintenance->Joined(); }, 2s));
    EXPECT_EQ(events->closes, 1);
    EXPECT_EQ(events->status.code(), StatusCode::kUnavailable);
    std::string frontier;
    EXPECT_TRUE(maintenance->Frontier(frontier));
}

TEST_F(ChannelLoopbackTest, MaintenanceSeparatesDestroyedCallJoinFromUnknownRemoteOutcome)
{
    const auto maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
    ChannelOptions options;
    options.maintenance = maintenance;
    auto channel = MakeChannel(options);
    const auto record = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("in-flight", 5s), Recorder(record));
    maintenance->Close();
    channel.reset();
    EXPECT_EQ(record->count, 0);
    EXPECT_FALSE(maintenance->Joined());
    ASSERT_TRUE(RunUntil([maintenance] { return maintenance->Joined(); }, 2s));
    EXPECT_EQ(record->count, 1);
    EXPECT_EQ(record->result.status.code(), StatusCode::kUnavailable);
    std::string frontier;
    EXPECT_FALSE(maintenance->Frontier(frontier));
}

TEST_F(ChannelLoopbackTest, MaintenanceSeparatesCancelledStreamJoinFromUnknownRemoteOutcome)
{
    const auto maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
    ChannelOptions options;
    options.maintenance = maintenance;
    const auto channel = MakeChannel(options);
    const auto events = std::make_shared<StreamEvents>();
    const auto stream = channel->StartStream(kStreamMethod, {}, StreamRecorder(events));
    maintenance->Close();
    EXPECT_FALSE(maintenance->Joined());
    stream->Cancel();
    ASSERT_TRUE(RunUntil([maintenance] { return maintenance->Joined(); }, 2s));
    EXPECT_EQ(events->closes, 0) << "保留原Cancel不再回调的合同";
    std::string frontier;
    EXPECT_FALSE(maintenance->Frontier(frontier));
}

TEST_F(ChannelLoopbackTest, MaintenanceStreamFrontierWaitsForAllSuccessfulCallbacks)
{
    StartServer(&IgnoreRequest);
    server_->SetActivityHandler(EchoStreams(std::make_shared<EchoStreamState>()));
    const auto maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
    ChannelOptions options;
    options.maintenance = maintenance;
    const auto channel = MakeChannel(options);
    const auto events = std::make_shared<StreamEvents>();
    auto callbacks = StreamRecorder(events);
    auto onMessage = std::move(callbacks.onMessage);
    callbacks.onMessage = [maintenance, onMessage = std::move(onMessage)](std::string &&message)
    {
        EXPECT_FALSE(maintenance->Joined());
        onMessage(std::move(message));
    };
    auto onClose = std::move(callbacks.onClose);
    callbacks.onClose = [maintenance, onClose = std::move(onClose)](const muduo_grpc::Status &status, const Metadata &trailing)
    {
        EXPECT_FALSE(maintenance->Joined());
        onClose(status, trailing);
    };
    const auto stream = channel->StartStream(kStreamMethod, {}, std::move(callbacks));
    EXPECT_TRUE(stream->Write("accepted"));
    stream->WritesDone();
    maintenance->Close();
    ASSERT_TRUE(RunUntil([maintenance] { return maintenance->Joined(); }, 5s));
    EXPECT_EQ(events->messages, (std::vector<std::string>{"echo:accepted"}));
    EXPECT_EQ(events->headers, 1);
    EXPECT_EQ(events->closes, 1);
    EXPECT_TRUE(events->status.ok());
    std::string frontier;
    EXPECT_TRUE(maintenance->Frontier(frontier));
}

TEST(ChannelMaintenanceTest, DroppedLocalCallbackJoinsButNeverProducesAClosure)
{
    muduo_grpc_test::EnsureSocketsInitialized();
    const auto maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
    maintenance->Close();
    const auto called = std::make_shared<bool>(false);
    {
        muduo::net::EventLoop loop;
        ChannelOptions options;
        options.maintenance = maintenance;
        auto channel = Channel::Create(loop, "127.0.0.1", 1, options);
        ASSERT_NE(channel, nullptr);
        channel->StartUnaryCall(MakeRequest("not-dispatched", 5s), [called](UnaryCallResult &&) { *called = true; });
        EXPECT_FALSE(maintenance->Joined());
        channel.reset();
        // 此用例不运行loop：析构只释放排队闭包，不能把没有调用的业务回调写成成功。
    }
    EXPECT_FALSE(*called);
    EXPECT_TRUE(maintenance->Joined());
    std::string frontier;
    EXPECT_FALSE(maintenance->Frontier(frontier));
}


TEST_F(ChannelLoopbackTest, MaintenanceWaitsForCallbackTargetDestructionAndItsDerivedRejection)
{
    const auto state = std::make_shared<muduo_grpc::MaintenanceState>();
    state->Close();
    ChannelOptions options; options.maintenance = state;
    auto channel = MakeChannel(options);
    const std::weak_ptr<Channel> weak = channel;
    auto calls = std::make_shared<int>(0);
    auto target = std::shared_ptr<int>(new int(0), [state, weak, calls](int* value) {
        delete value;
        EXPECT_FALSE(state->Joined()) << "target 析构必须早于完成票据";
        if (const auto locked = weak.lock()) locked->StartUnaryCall(MakeRequest("derived-on-destroy", 5s), [state, calls](UnaryCallResult&& result) {
            EXPECT_FALSE(state->Joined()); EXPECT_EQ(result.status.code(), StatusCode::kUnavailable); ++*calls;
        });
    });
    channel->StartUnaryCall(MakeRequest("closed", 5s), [target, calls](UnaryCallResult&& result) { EXPECT_EQ(result.status.code(), StatusCode::kUnavailable); ++*calls; });
    target.reset();
    ASSERT_TRUE(RunUntil([state] { return state->Joined(); }, 2s));
    EXPECT_EQ(*calls, 2);
    std::string frontier; EXPECT_TRUE(state->Frontier(frontier));
}

TEST_F(ChannelLoopbackTest, MaintenanceNativeJoinWaitsForOriginalLoopTransportRelease)
{
    StartServer(&RespondEcho);
    const auto state = std::make_shared<muduo_grpc::MaintenanceState>();
    ChannelOptions options; options.maintenance = state;
    auto channel = MakeChannel(options);
    const auto call = std::make_shared<CallRecord>();
    channel->StartUnaryCall(MakeRequest("before-close", 5s), Recorder(call));
    ASSERT_TRUE(RunUntil([call] { return call->count == 1; }, 5s));
    ASSERT_TRUE(call->result.status.ok());
    std::string frontier; EXPECT_TRUE(state->SettledFrontier(frontier));
    EXPECT_FALSE(state->Closed()) << "P1 快照不应关闭 relay";
    state->Close();
    EXPECT_FALSE(state->NativeJoined());
    channel.reset();
    EXPECT_FALSE(state->NativeJoined()) << "析构返回仍有原 loop 的 forceClose/connectDestroyed";
    ASSERT_TRUE(RunUntil([state] { return state->NativeJoined(); }, 2s));
    EXPECT_TRUE(state->Joined());
    EXPECT_TRUE(state->Frontier(frontier));
}

} // namespace
