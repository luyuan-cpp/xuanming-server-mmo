// rpc_chain_test —— A→B→C 延后完成的端到端测试(docs/design/muduo-rpc-channel.md §5.3、§15 C)。
//
// 拓扑:一个 EventLoopThread;两个真身 muduo::net::RpcServer(B、C,各装一个派生自生成 ::Scene 的测试服务);
// 两个真实 RpcClient(A→B、B→C)。A 发起 Scene.ProcessClientPlayerMessage;B 的 handler 接管 done
// (RpcDoneCallback::Adopt)后经 B→C 调 Scene.PrepareBattle 并立即返回;C 的 handler 接管后把回复存起来,
// 由测试决定何时、以什么载荷完成。
//
// 被锁定的契约:
//   * 等待 C 期间 loop 不阻塞(另一个 functor 能跑完),B 的 handler 已返回,A / B 的回调都还没执行;
//   * C 完成后事件顺序是 [C.handler, B.cont(ok), A.done(ok)],A 的响应同时带 C 的载荷与 B 自己写的字段,
//     A 的回调拿到的是它发起那次调用时捕获的上下文(两条链交错完成也不串);
//   * B→C 超时 / 断连时 B 仍完成并填业务码:A 看到 ok + 业务码,而不是挂起到超时;C 迟到的回复被丢弃并计数;
//   * B 忘了 Run:最后一个副本随 B 的回调析构,A 立即以 kReplyDropped 失败(A 的超时是 1h 且从不扫描)。
//
// 判据只用事件与计数,等待一律以 5s 作活性上限(AGENTS §11.4);超时由 SweepTimeouts 的合成时刻驱动。
// 拆除顺序:先 reset 两个 RpcClient、转 loop、等两端 DOWN(客户端侧与服务端侧连接对象都已析构),
// 再在 loop 线程销毁两个 RpcServer(~TcpServer 断言 in-loop)。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main;消息号注册表由 rpc_channel_loopback_test.cpp 的全局
// Environment 填充一次;链接桩在 rpc_client_lifecycle_test.cpp,这里不要重复定义。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#endif

#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <functional>
#include <future>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include "muduo/base/CountDownLatch.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpConnection.h"

#include "network/rpc_channel.h"
#include "network/rpc_channel_stats.h"
#include "network/rpc_client.h"
#include "network/rpc_controller.h"
#include "network/rpc_done_callback.h"
#include "network/rpc_server.h"
#include "network/rpc_status.h"
#include "proto/common/base/empty.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/rpc_message.pb.h"
#include "proto/scene/scene.pb.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "table/proto/tip/common_error_tip.pb.h"   // kServiceUnavailable:B 在下游失败时写给 A 的业务码

using namespace std::chrono_literals;

namespace
{

// ---------------------------------------------------------------------------
// 常量与方法 tag
// ---------------------------------------------------------------------------

constexpr auto kLiveness = 5s;   // 只作活性上限,不作判据
// 用例超时:真实的超时 tick(100ms 一次)在用例寿命内永远不会让它到期,超时一律由 SweepTimeouts 的合成时刻驱动。
constexpr std::chrono::milliseconds kLongTimeout = std::chrono::hours(1);

constexpr uint32_t kProcessId = SceneProcessClientPlayerMessageMessageId;   // A→B:B 接管并转给 C
constexpr uint32_t kPrepareId = ScenePrepareBattleMessageId;                // B→C:C 接管,由测试完成
constexpr uint32_t kCreateId = SceneCreateSceneMessageId;                   // 同步方法:拆除前的 ping
constexpr uint32_t kDestroyId = SceneDestroySceneMessageId;                 // 同步方法(Empty 响应):微基准

constexpr char kLeafPayload[] = "payload-from-C";
// B 自己写进 A 响应的标记(MessageContent.id):与 A 请求里的任何值都不同,证明这个字段是 B 写的。
constexpr uint64_t kMiddleStamp = 0xB0B0;

// 方法 tag 的测试内替身(生成器的 <KeyName>Rpc 尚未产出时,形状与设计 §10 一致)。
struct ChainProcessRpc
{
    static constexpr uint32_t kMessageId = kProcessId;
    using Request = ::ProcessClientPlayerMessageRequest;
    using Response = ::ProcessClientPlayerMessageResponse;
};

struct ChainPrepareRpc
{
    static constexpr uint32_t kMessageId = kPrepareId;
    using Request = ::PrepareBattleRequest;
    using Response = ::PrepareBattleResponse;
};

uint64_t Load(const std::atomic<uint64_t>& counter)
{
    return counter.load(std::memory_order_relaxed);
}

// 一次借出 count 个互不相同的空闲回环端口:所有 socket 同时持有到最后才关,保证端口两两不同。
std::vector<uint16_t> FindFreeLoopbackPorts(std::size_t count)
{
    std::vector<SOCKET> sockets;
    std::vector<uint16_t> ports;
    for (std::size_t i = 0; i < count; ++i)
    {
        const SOCKET s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
        EXPECT_NE(s, INVALID_SOCKET);
        sockaddr_in addr{};
        addr.sin_family = AF_INET;
        addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        addr.sin_port = 0;
        EXPECT_EQ(::bind(s, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)), 0);
        int len = sizeof(addr);
        EXPECT_EQ(::getsockname(s, reinterpret_cast<sockaddr*>(&addr), &len), 0);
        ports.push_back(ntohs(addr.sin_port));
        sockets.push_back(s);
    }
    for (const SOCKET s : sockets)
    {
        ::closesocket(s);
    }
    return ports;
}

bool AllExpired(const std::vector<std::weak_ptr<muduo::net::TcpConnection>>& connections)
{
    return std::all_of(connections.begin(), connections.end(),
                       [](const std::weak_ptr<muduo::net::TcpConnection>& c) { return c.expired(); });
}

// ---------------------------------------------------------------------------
// 事件序列
// ---------------------------------------------------------------------------

// 各节点的回调在 loop 线程上写,测试线程等待并读取(锁 + 条件变量)。
// 写进别的结构(AResult)的数据先于 Push 写完:Push 释放锁、等待方拿到锁,happens-before 由这把锁提供。
class EventLog
{
public:
    void Push(std::string event)
    {
        {
            std::lock_guard<std::mutex> lock(mutex);
            events.push_back(std::move(event));
        }
        cv.notify_all();
    }

    std::vector<std::string> Events() const
    {
        std::lock_guard<std::mutex> lock(mutex);
        return events;
    }

    std::size_t CountWithPrefix(const std::string& prefix) const
    {
        std::lock_guard<std::mutex> lock(mutex);
        return CountIn(events, prefix);
    }

    bool WaitForCountWithPrefix(const std::string& prefix, std::size_t count) const
    {
        std::unique_lock<std::mutex> lock(mutex);
        return cv.wait_for(lock, kLiveness, [&] { return CountIn(events, prefix) >= count; });
    }

private:
    static std::size_t CountIn(const std::vector<std::string>& events, const std::string& prefix)
    {
        return static_cast<std::size_t>(std::count_if(events.begin(), events.end(),
                                                      [&](const std::string& e) { return e.starts_with(prefix); }));
    }

    mutable std::mutex mutex;
    mutable std::condition_variable cv;
    std::vector<std::string> events;
};

std::string EventOf(const char* node, const RpcStatus& status)
{
    return std::string(node) + "(" + status.name() + ")";
}

// A 的一次调用的结局。只在 A 的 done 里(loop 线程)写一次,写完才 Push 事件。
struct AResult
{
    bool completed{false};
    RpcStatus status;
    bool hasResponse{false};
    ::ProcessClientPlayerMessageResponse response;
    uint64_t contextToken{0};
    std::string contextLabel;
};

// ---------------------------------------------------------------------------
// B、C 的测试服务(派生自生成的 ::Scene;所有状态只在 loop 线程上读写)
// ---------------------------------------------------------------------------

enum class MiddleMode
{
    kRunAfterDownstream,   // 正确写法:下游完成(含失败)后继续自己的业务,再 Run
    kForgetRun,            // 缺陷形态:下游回来后既不 Run 也不 Fail
};

// B:中间节点。
class MiddleSceneService final : public ::Scene
{
public:
    explicit MiddleSceneService(std::shared_ptr<EventLog> log) : log(std::move(log)) {}

    std::weak_ptr<RpcClient> downstream;   // B→C;只存 weak,handler 里按次 lock
    MiddleMode mode{MiddleMode::kRunAfterDownstream};
    int handlerReturns{0};
    // B→C 调用发起之后取的时刻:真实截止时刻 ≤ 它 + 超时,超时变体据此推导 SweepTimeouts 的合成时刻。
    std::optional<RpcChannel::Clock::time_point> downstreamIssuedAfter;
    // 服务过请求的入站连接:拆除时等它们析构(== 服务端侧 DOWN 已处理完)。
    std::vector<std::weak_ptr<muduo::net::TcpConnection>> inbound;

    void ProcessClientPlayerMessage(::google::protobuf::RpcController* controller,
                                    const ::ProcessClientPlayerMessageRequest* request,
                                    ::ProcessClientPlayerMessageResponse* response,
                                    ::google::protobuf::Closure* done) override
    {
        inbound.push_back(::RpcController::From(controller).Conn());
        const auto client = downstream.lock();
        if (!client)
        {
            // 未接管:框架在返回后立即回包。
            response->mutable_message_content()->mutable_error_message()->set_id(kServiceUnavailable);
            ++handlerReturns;
            return;
        }

        const auto reply = RpcDoneCallback::Adopt(done);   // 返回后框架不再自动回包
        ::PrepareBattleRequest downstreamRequest;
        downstreamRequest.set_player_id(request->player_id());
        downstreamRequest.set_battle_id(request->message_content().id());
        // 回调只捕获值与 reply(内部只持 weak channel),不捕获 this / controller / request 指针(契约 b)。
        client->CallMethod<ChainPrepareRpc>(
            downstreamRequest,
            [reply, log = this->log, mode = mode, sessionId = request->session_id()](
                const RpcStatus& status, const std::shared_ptr<::PrepareBattleResponse>& fromLeaf)
            {
                log->Push(EventOf("B.cont", status));
                if (mode == MiddleMode::kForgetRun)
                {
                    return;   // 没有 Run / Fail:reply 的最后一个副本随本回调析构 → 框架回 REPLY_DROPPED
                }
                auto& out = reply.response<::ProcessClientPlayerMessageResponse>();
                out.set_session_id(sessionId);                        // B 自己的字段
                out.mutable_message_content()->set_id(kMiddleStamp);
                if (!status.ok())
                {
                    // C 失败 / 超时 / 断连也必须完成:先写业务码再 Run(直接 return 会让 A 只剩 REPLY_DROPPED)。
                    out.mutable_message_content()->mutable_error_message()->set_id(kServiceUnavailable);
                    reply.Run();
                    return;
                }
                out.mutable_message_content()->set_serialized_message(fromLeaf->table_fingerprint());   // C 的载荷
                reply.Run();
            },
            RpcCallOptions{kLongTimeout});
        downstreamIssuedAfter = RpcChannel::Clock::now();
        ++handlerReturns;
    }

    // 同步方法:拆除前的 ping。
    void CreateScene(::google::protobuf::RpcController* controller, const ::CreateSceneRequest*,
                     ::CreateSceneResponse*, ::google::protobuf::Closure*) override
    {
        inbound.push_back(::RpcController::From(controller).Conn());
    }

    // 同步方法(Empty 响应):微基准的往返。不记 inbound,免得 1e5 次往返在热路径上堆记录。
    void DestroyScene(::google::protobuf::RpcController*, const ::DestroySceneRequest*, ::Empty*,
                      ::google::protobuf::Closure*) override
    {
    }

private:
    std::shared_ptr<EventLog> log;
};

// C 暂存的一次回复;battleId 即 A 请求里的 message_content.id(B 原样转下来),测试据此挑选要完成的那一次。
struct ParkedReply
{
    uint64_t battleId{0};
    RpcDoneCallback reply;
};

// C:叶子节点。接管 done 后把回复存起来,由测试决定何时完成。
class LeafSceneService final : public ::Scene
{
public:
    explicit LeafSceneService(std::shared_ptr<EventLog> log) : log(std::move(log)) {}

    std::vector<ParkedReply> parked;
    std::vector<std::weak_ptr<muduo::net::TcpConnection>> inbound;

    void PrepareBattle(::google::protobuf::RpcController* controller, const ::PrepareBattleRequest* request,
                       ::PrepareBattleResponse*, ::google::protobuf::Closure* done) override
    {
        inbound.push_back(::RpcController::From(controller).Conn());
        parked.push_back(ParkedReply{request->battle_id(), RpcDoneCallback::Adopt(done)});
        log->Push("C.handler");
    }

    // 同步方法:拆除前的 ping。
    void CreateScene(::google::protobuf::RpcController* controller, const ::CreateSceneRequest*,
                     ::CreateSceneResponse*, ::google::protobuf::Closure*) override
    {
        inbound.push_back(::RpcController::From(controller).Conn());
    }

private:
    std::shared_ptr<EventLog> log;
};

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

class RpcChain : public ::testing::Test
{
protected:
    void SetUp() override
    {
        loop.emplace(*loopThread.startLoop());
        ASSERT_TRUE(IsDispatchableMessageId(kProcessId) && IsDispatchableMessageId(kPrepareId) &&
                    IsDispatchableMessageId(kCreateId) && IsDispatchableMessageId(kDestroyId))
            << "gRpcMethodRegistry is empty: the global RpcRegistryEnvironment (rpc_channel_loopback_test.cpp) did not run";
        const auto ports = FindFreeLoopbackPorts(2);
        ASSERT_EQ(ports.size(), 2u);
        ASSERT_NE(ports[0], 0);
        ASSERT_NE(ports[1], 0);
        const uint16_t portB = ports[0];
        const uint16_t portC = ports[1];

        RunInLoopAndWait([this, portB, portC] {
            serverC = std::make_unique<muduo::net::RpcServer>(&Loop(), muduo::net::InetAddress(portC, /*loopbackOnly=*/true));
            serverC->registerService(&leaf);
            serverC->start();   // 在 loop 线程上即时 listen,随后的 connect 不会撞拒连
            serverB = std::make_unique<muduo::net::RpcServer>(&Loop(), muduo::net::InetAddress(portB, /*loopbackOnly=*/true));
            serverB->registerService(&middle);
            serverB->start();

            bToC = std::make_shared<RpcClient>(&Loop(), muduo::net::InetAddress("127.0.0.1", portC));
            bToC->connect();
            aToB = std::make_shared<RpcClient>(&Loop(), muduo::net::InetAddress("127.0.0.1", portB));
            aToB->connect();
            middle.downstream = bToC;
        });
        ASSERT_TRUE(WaitUntil([this] { return aToB->connected() && bToC->connected(); }))
            << "chain links never connected";
    }

    void TearDown() override
    {
        if (!loop)
        {
            return;
        }
        // 1. 仍连着的链路各 ping 一次:让服务端把当前这条入站连接记进 inbound,下面才能等到它析构。
        PingIfConnected(aToB);
        PingIfConnected(bToC);

        // 2. 先拆两个 RpcClient(各自连接的唯一持有者 → forceClose → 对端 DOWN)。
        std::vector<std::weak_ptr<muduo::net::TcpConnection>> clientConnections;
        RunInLoopAndWait([&] {
            if (aToB)
            {
                clientConnections.push_back(aToB->GetConnection());
            }
            if (bToC)
            {
                clientConnections.push_back(bToC->GetConnection());
            }
            middle.downstream.reset();
            aToB.reset();
            bToC.reset();
        });
        PumpLoop(3);

        // 3. 等两端 DOWN:客户端侧与服务端侧的连接对象都已析构(connectDestroyed 跑完、没有人再持有它们)。
        EXPECT_TRUE(WaitUntil([&] {
            return AllExpired(clientConnections) && AllExpired(middle.inbound) && AllExpired(leaf.inbound);
        })) << "a chain connection did not finish DOWN";

        // 4. 暂存的回复在 loop 线程上释放(连接已断:只计 reply_channel_gone,不发帧),再在 loop 线程销毁两个 RpcServer。
        RunInLoopAndWait([this] {
            leaf.parked.clear();
            serverB.reset();
            serverC.reset();
        });
        PumpLoop(2);
        // loopThread_ 析构 = quit + join。
    }

    muduo::net::EventLoop& Loop() { return loop->get(); }

    template <typename F>
    void RunInLoopAndWait(F&& fn)
    {
        muduo::CountDownLatch done(1);
        Loop().runInLoop([&] {
            fn();
            done.countDown();
        });
        done.wait();
    }

    void PumpLoop(int rounds)
    {
        for (int i = 0; i < rounds; ++i)
        {
            RunInLoopAndWait([] {});
        }
    }

    // 在 loop 线程上反复求值 pred,成立即返回 true;5s 只是活性上限。
    template <typename Pred>
    bool WaitUntil(Pred pred)
    {
        const auto deadline = std::chrono::steady_clock::now() + kLiveness;
        for (;;)
        {
            bool satisfied = false;
            RunInLoopAndWait([&] { satisfied = pred(); });
            if (satisfied)
            {
                return true;
            }
            if (std::chrono::steady_clock::now() >= deadline)
            {
                return false;
            }
            std::this_thread::sleep_for(1ms);
        }
    }

    // 测试线程:经 client 调一次同步方法并等它完成;未连接时什么都不做。
    void PingIfConnected(const RpcClientPtr& client)
    {
        auto finishedPromise = std::make_shared<std::promise<void>>();
        auto finished = finishedPromise->get_future();
        bool issued = false;
        RunInLoopAndWait([&] {
            if (!client || !client->connected())
            {
                return;
            }
            issued = true;
            client->CallMethod(
                kCreateId, ::CreateSceneRequest{},
                [finishedPromise](const RpcStatus&, const MessagePtr&) { finishedPromise->set_value(); },
                RpcCallOptions{kLongTimeout});
        });
        if (issued)
        {
            EXPECT_EQ(finished.wait_for(kLiveness), std::future_status::ready) << "teardown ping never completed";
        }
    }

    // 测试线程:A 发起一次 A→B 调用。回调只捕获值(上下文)与 shared 结果槽,执行时写结果再记事件。
    void IssueFromA(const std::shared_ptr<AResult>& result, uint32_t sessionId, uint64_t contextToken,
                    const std::string& contextLabel)
    {
        RunInLoopAndWait([&] {
            ::ProcessClientPlayerMessageRequest request;
            request.set_session_id(sessionId);
            request.set_player_id(sessionId);
            request.mutable_message_content()->set_id(sessionId);   // B 转成 C 的 battle_id
            aToB->CallMethod<ChainProcessRpc>(
                request,
                [result, log = this->log, contextToken, contextLabel](
                    const RpcStatus& status, const std::shared_ptr<::ProcessClientPlayerMessageResponse>& response)
                {
                    result->status = status;
                    result->hasResponse = response != nullptr;
                    if (response)
                    {
                        result->response = *response;
                    }
                    result->contextToken = contextToken;
                    result->contextLabel = contextLabel;
                    result->completed = true;
                    log->Push(EventOf("A.done", status));
                },
                RpcCallOptions{kLongTimeout});
        });
    }

    // loop 线程:取出 C 为 battleId 暂存的回复,写载荷后 Run。找不到返回 false。
    bool CompleteLeafReply(uint64_t battleId, const std::string& payload)
    {
        const auto it = std::find_if(leaf.parked.begin(), leaf.parked.end(),
                                     [battleId](const ParkedReply& p) { return p.battleId == battleId; });
        if (it == leaf.parked.end())
        {
            return false;
        }
        const RpcDoneCallback reply = it->reply;
        leaf.parked.erase(it);
        reply.response<::PrepareBattleResponse>().set_table_fingerprint(payload);
        reply.Run();
        return true;
    }

    // loopThread_ 排第一:最后析构,保证其余成员析构时 loop 仍在。
    muduo::net::EventLoopThread loopThread;
    // 引用而非裸指针成员(裸指针成员门禁覆盖测试工程);SetUp 里绑定。
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop;
    std::shared_ptr<EventLog> log = std::make_shared<EventLog>();
    // 服务声明在服务器之前:每条入站连接的 channel 拷贝的服务表借用它们,它们必须后析构。
    LeafSceneService leaf{log};
    MiddleSceneService middle{log};
    std::unique_ptr<muduo::net::RpcServer> serverC;
    std::unique_ptr<muduo::net::RpcServer> serverB;
    RpcClientPtr bToC;
    RpcClientPtr aToB;
};

// 微基准共用同一套拓扑,只用 A→B 的同步方法。
class RpcChannelBench : public RpcChain
{
};

struct BenchState
{
    std::weak_ptr<RpcClient> client;
    int remaining{0};
    int failures{0};
    std::promise<void> finished;
};

// loop 线程:上一次往返的回调里发起下一次(串行往返,测的是单次往返延迟而不是吞吐)。
// 回调只捕获 shared 状态,状态只持 weak_ptr<RpcClient>,不形成环。
void IssueBenchRoundTrip(const std::shared_ptr<BenchState>& state)
{
    const auto client = state->client.lock();
    if (!client)
    {
        state->finished.set_value();
        return;
    }
    client->CallMethod(
        kDestroyId, ::DestroySceneRequest{},
        [state](const RpcStatus& status, const MessagePtr&)
        {
            if (!status.ok())
            {
                ++state->failures;
            }
            if (--state->remaining == 0)
            {
                state->finished.set_value();
                return;
            }
            IssueBenchRoundTrip(state);
        },
        RpcCallOptions{kLongTimeout});
}

} // namespace

// ---------------------------------------------------------------------------
// 主路径
// ---------------------------------------------------------------------------

TEST_F(RpcChain, MiddleNodeCompletesAfterItsDownstreamWithoutBlockingTheLoop)
{
    auto result = std::make_shared<AResult>();
    IssueFromA(result, /*sessionId=*/7, /*contextToken=*/0xA11CE, "ctx-A");
    ASSERT_TRUE(log->WaitForCountWithPrefix("C.handler", 1)) << "C was never called";

    // loop 没被 B 阻塞:另一个 functor 能跑完;B 的 handler 早已返回,A / B 的回调都还没执行。
    std::size_t middleContinuations = 99;
    std::size_t callerCompletions = 99;
    int middleHandlerReturns = -1;
    std::size_t parked = 0;
    RunInLoopAndWait([&] {
        middleContinuations = log->CountWithPrefix("B.cont");
        callerCompletions = log->CountWithPrefix("A.done");
        middleHandlerReturns = middle.handlerReturns;
        parked = leaf.parked.size();
    });
    EXPECT_EQ(middleContinuations, 0u);
    EXPECT_EQ(callerCompletions, 0u);
    EXPECT_EQ(middleHandlerReturns, 1) << "B's handler must have returned while C is still pending";
    EXPECT_EQ(parked, 1u);

    bool completed = false;
    RunInLoopAndWait([&] { completed = CompleteLeafReply(7, kLeafPayload); });
    ASSERT_TRUE(completed);
    ASSERT_TRUE(log->WaitForCountWithPrefix("A.done", 1)) << "A never completed";

    EXPECT_EQ(log->Events(), (std::vector<std::string>{"C.handler", "B.cont(ok)", "A.done(ok)"}));
    ASSERT_TRUE(result->completed);
    ASSERT_TRUE(result->status.ok()) << result->status.name();
    ASSERT_TRUE(result->hasResponse);
    EXPECT_EQ(result->response.message_content().serialized_message(), kLeafPayload) << "A did not get C's payload";
    EXPECT_EQ(result->response.message_content().id(), kMiddleStamp) << "A did not get the field B wrote";
    EXPECT_EQ(result->response.session_id(), 7u);
    EXPECT_FALSE(result->response.message_content().has_error_message());
    EXPECT_EQ(result->contextToken, 0xA11CEu) << "A's callback lost the context it captured";
    EXPECT_EQ(result->contextLabel, "ctx-A");
}

// 两条链交错:C 倒序完成,每个 A 的回调仍拿到自己那次调用的上下文与载荷(按调用号认领,不按 message_id)。
TEST_F(RpcChain, InterleavedChainsKeepTheirOwnContext)
{
    auto first = std::make_shared<AResult>();
    auto second = std::make_shared<AResult>();
    IssueFromA(first, 21, 0x21, "ctx-21");
    IssueFromA(second, 22, 0x22, "ctx-22");
    ASSERT_TRUE(log->WaitForCountWithPrefix("C.handler", 2));

    bool completedSecond = false;
    RunInLoopAndWait([&] { completedSecond = CompleteLeafReply(22, "C-22"); });
    ASSERT_TRUE(completedSecond);
    ASSERT_TRUE(log->WaitForCountWithPrefix("A.done", 1));
    bool firstStillPending = false;
    RunInLoopAndWait([&] { firstStillPending = !first->completed; });
    EXPECT_TRUE(firstStillPending) << "completing C's reply for call 22 completed call 21";
    EXPECT_TRUE(second->completed);

    bool completedFirst = false;
    RunInLoopAndWait([&] { completedFirst = CompleteLeafReply(21, "C-21"); });
    ASSERT_TRUE(completedFirst);
    ASSERT_TRUE(log->WaitForCountWithPrefix("A.done", 2));

    struct Expected
    {
        const AResult& result;
        uint32_t session;
        uint64_t token;
        std::string label;     // 不用 const char*:裸指针成员门禁同样覆盖测试里的局部结构
        std::string payload;
    };
    for (const Expected& e : {Expected{*first, 21, 0x21, "ctx-21", "C-21"}, Expected{*second, 22, 0x22, "ctx-22", "C-22"}})
    {
        ASSERT_TRUE(e.result.completed) << e.label;
        ASSERT_TRUE(e.result.status.ok()) << e.label << ": " << e.result.status.name();
        EXPECT_EQ(e.result.contextToken, e.token) << e.label;
        EXPECT_EQ(e.result.contextLabel, e.label);
        EXPECT_EQ(e.result.response.session_id(), e.session) << e.label;
        EXPECT_EQ(e.result.response.message_content().serialized_message(), e.payload) << e.label << " got another call's payload";
    }
    EXPECT_EQ(log->CountWithPrefix("B.cont(ok)"), 2u);
}

// ---------------------------------------------------------------------------
// 下游失败:B 仍完成并填业务码
// ---------------------------------------------------------------------------

TEST_F(RpcChain, MiddleNodeStillCompletesWhenItsDownstreamTimesOut)
{
    const auto lateBefore = Load(GetRpcChannelStats().lateReply);
    auto result = std::make_shared<AResult>();
    IssueFromA(result, /*sessionId=*/8, /*contextToken=*/0xBEEF, "ctx-timeout");
    ASSERT_TRUE(log->WaitForCountWithPrefix("C.handler", 1));

    std::size_t callerCompletionsDuringSweep = 99;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = bToC ? bToC->Channel() : RpcChannelPtr{};
        if (!channel || !middle.downstreamIssuedAfter)
        {
            ADD_FAILURE() << "B->C channel or its issue time is missing";
            return;
        }
        // B→C 调用的真实截止时刻 ≤ 发起之后取的时刻 + 超时:按它扫描必过期,不依赖墙钟。
        channel->SweepTimeouts(*middle.downstreamIssuedAfter + kLongTimeout);
        callerCompletionsDuringSweep = log->CountWithPrefix("A.done");
    });
    EXPECT_EQ(callerCompletionsDuringSweep, 0u) << "B's reply to A must travel over the wire, not complete A inline";
    ASSERT_TRUE(log->WaitForCountWithPrefix("A.done", 1));

    EXPECT_EQ(log->Events(), (std::vector<std::string>{"C.handler", "B.cont(timeout)", "A.done(ok)"}));
    ASSERT_TRUE(result->status.ok()) << result->status.name();
    ASSERT_TRUE(result->hasResponse);
    EXPECT_EQ(result->response.message_content().error_message().id(), static_cast<uint32_t>(kServiceUnavailable))
        << "B must fill a business code when its downstream fails";
    EXPECT_EQ(result->response.message_content().id(), kMiddleStamp);
    EXPECT_EQ(result->response.session_id(), 8u);
    EXPECT_TRUE(result->response.message_content().serialized_message().empty());
    EXPECT_EQ(result->contextToken, 0xBEEFu);
    EXPECT_EQ(result->contextLabel, "ctx-timeout");

    // C 现在才完成:回包到 B 时那次调用已因超时完成 —— 丢弃并计数,B 的回调不会再跑一次。
    bool completed = false;
    RunInLoopAndWait([&] { completed = CompleteLeafReply(8, kLeafPayload); });
    ASSERT_TRUE(completed);
    EXPECT_TRUE(WaitUntil([&] { return Load(GetRpcChannelStats().lateReply) - lateBefore >= 1; }))
        << "C's late reply never reached B";
    EXPECT_EQ(Load(GetRpcChannelStats().lateReply) - lateBefore, 1u);
    EXPECT_EQ(log->CountWithPrefix("B.cont"), 1u) << "the late reply from C completed B's call a second time";
}

TEST_F(RpcChain, MiddleNodeStillCompletesWhenItsDownstreamDisconnects)
{
    auto result = std::make_shared<AResult>();
    IssueFromA(result, /*sessionId=*/9, /*contextToken=*/0xD15C, "ctx-disconnect");
    ASSERT_TRUE(log->WaitForCountWithPrefix("C.handler", 1));

    RunInLoopAndWait([&] {
        const muduo::net::TcpConnectionPtr conn = bToC ? bToC->GetConnection() : muduo::net::TcpConnectionPtr{};
        if (!conn)
        {
            ADD_FAILURE() << "B->C link has no connection";
            return;
        }
        conn->forceClose();   // B→C 断开:B 的在途调用以 kDisconnected 失败(排队投递,不在 DOWN 栈上)
    });
    ASSERT_TRUE(log->WaitForCountWithPrefix("A.done", 1));

    EXPECT_EQ(log->Events(), (std::vector<std::string>{"C.handler", "B.cont(disconnected)", "A.done(ok)"}));
    ASSERT_TRUE(result->status.ok()) << result->status.name();
    ASSERT_TRUE(result->hasResponse);
    EXPECT_EQ(result->response.message_content().error_message().id(), static_cast<uint32_t>(kServiceUnavailable));
    EXPECT_EQ(result->response.message_content().id(), kMiddleStamp);
    EXPECT_EQ(result->response.session_id(), 9u);
    EXPECT_EQ(result->contextToken, 0xD15Cu);
    EXPECT_EQ(result->contextLabel, "ctx-disconnect");

    // C 那一侧的入站 channel 随连接 DOWN 析构:此时再完成 C 的回复只计 reply_channel_gone,不崩、不出帧。
    ASSERT_TRUE(WaitUntil([this] { return AllExpired(leaf.inbound); })) << "C never saw the B->C link go DOWN";
    const auto goneBefore = Load(GetRpcChannelStats().replyChannelGone);
    bool completed = false;
    RunInLoopAndWait([&] { completed = CompleteLeafReply(9, kLeafPayload); });
    ASSERT_TRUE(completed);
    EXPECT_EQ(Load(GetRpcChannelStats().replyChannelGone) - goneBefore, 1u);
    EXPECT_EQ(log->CountWithPrefix("B.cont"), 1u);

    // B→C 会自动重连(enableRetry):等它连上再拆,让 TearDown 的 ping 覆盖新连接。
    EXPECT_TRUE(WaitUntil([this] { return bToC && bToC->connected(); })) << "B->C link did not reconnect";
}

// ---------------------------------------------------------------------------
// B 忘了 Run
// ---------------------------------------------------------------------------

// A 的超时是 1h 且从不扫描,所以 A 能完成只可能来自 B 那边的 REPLY_DROPPED 兜底。
TEST_F(RpcChain, MiddleNodeThatForgetsToRunFailsTheCallerImmediately)
{
    const auto abandonedBefore = Load(GetRpcChannelStats().replyAbandoned);
    RunInLoopAndWait([this] { middle.mode = MiddleMode::kForgetRun; });
    auto result = std::make_shared<AResult>();
    IssueFromA(result, /*sessionId=*/10, /*contextToken=*/0xF0F0, "ctx-forgot");
    ASSERT_TRUE(log->WaitForCountWithPrefix("C.handler", 1));

    bool completed = false;
    RunInLoopAndWait([&] { completed = CompleteLeafReply(10, kLeafPayload); });
    ASSERT_TRUE(completed);
    ASSERT_TRUE(log->WaitForCountWithPrefix("A.done", 1));

    EXPECT_EQ(log->Events(), (std::vector<std::string>{"C.handler", "B.cont(ok)", "A.done(reply_dropped)"}));
    EXPECT_EQ(result->status.Code(), RpcStatusCode::kReplyDropped) << result->status.name();
    EXPECT_EQ(result->status.RemoteError(), REPLY_DROPPED);
    EXPECT_TRUE(result->status.OutcomeUnknown());
    EXPECT_FALSE(result->hasResponse);
    EXPECT_EQ(result->contextToken, 0xF0F0u);
    EXPECT_EQ(result->contextLabel, "ctx-forgot");
    EXPECT_EQ(Load(GetRpcChannelStats().replyAbandoned) - abandonedBefore, 1u);
}

// ---------------------------------------------------------------------------
// 微基准(默认不跑)
// ---------------------------------------------------------------------------

// 回环往返 1e5 次的 ns/op(A→B 同步 handler,Empty 响应,串行往返)。由 Codex 用
// --gtest_also_run_disabled_tests --gtest_filter=RpcChannelBench.* 在改动前后各跑一次出对比表,
// Linux 镜像另跑一份(AGENTS §6:没有对比表不声称性能结论)。
TEST_F(RpcChannelBench, DISABLED_LoopbackRoundTripNsPerOp)
{
    constexpr int kRoundTrips = 100'000;
    auto state = std::make_shared<BenchState>();
    state->client = aToB;
    state->remaining = kRoundTrips;
    auto finished = state->finished.get_future();

    const auto start = std::chrono::steady_clock::now();
    RunInLoopAndWait([state] { IssueBenchRoundTrip(state); });
    // 基准不是正确性判据:活性上限放宽到 120s,只防挂死。
    ASSERT_EQ(finished.wait_for(120s), std::future_status::ready) << "benchmark did not finish";
    const auto elapsed = std::chrono::steady_clock::now() - start;

    const double nsPerOp = std::chrono::duration<double, std::nano>(elapsed).count() / kRoundTrips;
    std::printf("[RpcChannelBench] loopback round trips=%d ns_per_op=%.1f\n", kRoundTrips, nsPerOp);
    RecordProperty("ns_per_op", static_cast<int>(nsPerOp));
    int failures = -1;
    RunInLoopAndWait([&] { failures = state->failures; });
    EXPECT_EQ(failures, 0);
}
