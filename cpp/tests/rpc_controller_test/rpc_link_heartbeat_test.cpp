// rpc_link_heartbeat_test —— 节点链路心跳(docs/design/muduo-rpc-channel.md §21)。
//
// 被锁定的契约:
//   * 判定(RpcLinkLiveness,纯逻辑):
//     - 写空闲满 interval 恰好发一次心跳,早一纳秒不发;任何发送都推迟心跳;
//     - 读空闲满 idleTimeout 才判死,早一纳秒不判;任何收到都重新计时;
//     - 迟到的检查(本端 loop 卡过)只推迟判死,心跳照发;阈值是相邻两次检查相隔超过 2 个节拍;
//     - 参数校验与生产取值。
//   * 接线(RpcChannel / RpcClient / RpcServer):
//     - 写空闲时线上恰好一帧 HEARTBEAT(type=5,id、message_id、载荷都为空);
//     - 读空闲判死只关传输:IsReady 当场为假,在途调用经 DOWN 以 kDisconnected 失败一次,enableRetry 重连出新 channel;
//     - 对端的一帧 HEARTBEAT 让判死整体后移;收到 HEARTBEAT 不派发、不回包、不计 unknown_type;
//     - 真身 RpcServer 的入站连接同样发心跳、同样判死,DOWN 照常清掉 context;
//     - 没开心跳的 channel 什么都不做;Close(关机阶段 0)之后心跳照发;
//     - 汇总行带 link_idle_closed / link_idle_deferred(与 stress_summarize.ps1 的契约)。
//
// 判据不依赖墙钟(AGENTS §11.4):
//   * 纯逻辑用例的时刻全是合成的,按 1s 步进模拟生产节拍。
//   * 回环用例的心跳取 kTestHeartbeat(checkPeriod 1h):真实检查节拍在用例寿命内不会触发,检查一律由
//     CheckLinkLiveness 的合成时刻驱动。建连前后各取一次 t0 / t1,channel 的真实创建时刻必在 [t0, t1] 之间,
//     所以 t0 + X - 1ns 必不到期、t1 + X 必已到期(同 SweepTimeouts 的测法)。相邻两次检查不超过 2 × checkPeriod,
//     否则会被当成「本端 loop 卡过」而推迟判死。
//   * 等待一律以 5s 作活性上限,不作判据;「没有多发」用屏障帧证明(同一连接 FIFO)。
//     RpcChannelStats 是进程级计数,一律比较前后差值。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main;消息号注册表由 rpc_channel_loopback_test.cpp 的全局 Environment
// 填充一次,链接桩(InitPlayerService / InitServiceHandler)在 rpc_client_lifecycle_test.cpp,这里都不重复定义。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#endif

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <mutex>
#include <optional>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#include <boost/any.hpp>

#include "muduo/base/CountDownLatch.h"
#include "muduo/base/Timestamp.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"

#include "network/rpc_channel.h"
#include "network/rpc_channel_stats.h"
#include "network/rpc_client.h"
#include "network/rpc_link_liveness.h"
#include "network/rpc_server.h"
#include "network/rpc_status.h"
#include "proto/common/base/rpc_message.pb.h"
#include "proto/scene/scene.pb.h"
#include "rpc/service_metadata/scene_service_metadata.h"

using namespace std::chrono_literals;

namespace
{

using LinkClock = RpcLinkLiveness::Clock;

constexpr auto kLiveness = 5s;   // 只作活性上限,不作判据

// 纯逻辑用例用生产取值。
constexpr auto kInterval = kNodeLinkHeartbeat.interval;
constexpr auto kIdleTimeout = kNodeLinkHeartbeat.idleTimeout;
constexpr auto kCheckPeriod = kNodeLinkHeartbeat.checkPeriod;
// 纯逻辑用例的合成原点:任意取值,不读真实时钟。
const LinkClock::time_point kOrigin = LinkClock::time_point{} + std::chrono::hours(24);

// 回环用例的心跳:checkPeriod 1h,真实节拍在用例寿命内不会触发。三项按比例放大,仍满足 IsValidLinkHeartbeat。
constexpr RpcLinkHeartbeat kTestHeartbeat{std::chrono::hours(1), std::chrono::hours(4), std::chrono::hours(1)};
static_assert(IsValidLinkHeartbeat(kTestHeartbeat));
// 调用超时:用例寿命内不会到期,真实的超时 tick 永远不会让它失败。
constexpr std::chrono::milliseconds kLongTimeout = std::chrono::hours(1);

constexpr uint32_t kProcessId = SceneProcessClientPlayerMessageMessageId;
// 手工构造请求帧的调用号:远离被测 channel 自己从 1 起的号。
constexpr uint64_t kBarrierId = 4'200'000;
constexpr uint32_t kBarrierSession = 4242;

uint64_t Load(const std::atomic<uint64_t>& counter)
{
    return counter.load(std::memory_order_relaxed);
}

// 向 OS 借一个空闲回环端口。muduo 的 TcpServer 不暴露实际绑定端口,只能这样拿。
uint16_t FindFreeLoopbackPort()
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
    const uint16_t port = ntohs(addr.sin_port);
    ::closesocket(s);
    return port;
}

GameRpcMessage MakeHeartbeatFrame()
{
    GameRpcMessage frame;
    frame.set_type(HEARTBEAT);
    return frame;
}

GameRpcMessage MakeRequestFrame(uint64_t id, uint32_t messageId, const ProtobufMessage& body)
{
    GameRpcMessage frame;
    frame.set_type(REQUEST);
    frame.set_id(id);
    frame.set_message_id(messageId);
    frame.set_request(body.SerializeAsString());
    return frame;
}

ProcessClientPlayerMessageRequest ProcessRequest(uint32_t sessionId)
{
    ProcessClientPlayerMessageRequest request;
    request.set_session_id(sessionId);
    return request;
}

std::string Describe(const std::vector<GameRpcMessage>& frames)
{
    if (frames.empty())
    {
        return "<none>";
    }
    std::ostringstream out;
    for (const auto& frame : frames)
    {
        out << "{type=" << static_cast<int>(frame.type()) << " id=" << frame.id()
            << " message_id=" << frame.message_id() << " error=" << static_cast<int>(frame.error()) << "}";
    }
    return out.str();
}

// 记录从对端收到的 RPC0 帧(RpcCodec 解码)。帧在 loop 线程上写入,测试线程等待并读取。
class FrameRecorder
{
public:
    FrameRecorder()
        : codec_([this](const TcpConnectionPtr&, const RpcMessagePtr& frame, muduo::Timestamp) { Push(*frame); })
    {
    }

    FrameRecorder(const FrameRecorder&) = delete;
    FrameRecorder& operator=(const FrameRecorder&) = delete;

    // loop 线程:作为连接的消息回调,直接消费输入。
    void OnMessage(const TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
    {
        codec_.onMessage(conn, buffer, receiveTime);
    }

    // loop 线程:按线上格式发一帧。
    void Send(const TcpConnectionPtr& conn, const GameRpcMessage& frame) { codec_.send(conn, frame); }

    std::vector<GameRpcMessage> Frames() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return frames_;
    }

    bool WaitForCount(std::size_t count) const
    {
        std::unique_lock<std::mutex> lock(mutex_);
        return cv_.wait_for(lock, kLiveness, [&] { return frames_.size() >= count; });
    }

private:
    void Push(const GameRpcMessage& frame)
    {
        {
            std::lock_guard<std::mutex> lock(mutex_);
            frames_.push_back(frame);
        }
        cv_.notify_all();
    }

    mutable std::mutex mutex_;
    mutable std::condition_variable cv_;
    std::vector<GameRpcMessage> frames_;
    RpcCodec codec_;   // 解码回调绑 this:codec_ 是本对象的成员,寿命相同
};

// 调用的完成状态。done 在 loop 线程上写,测试线程等待并读取。
class StatusLog
{
public:
    void Push(const RpcStatus& status)
    {
        {
            std::lock_guard<std::mutex> lock(mutex_);
            statuses_.push_back(status);
        }
        cv_.notify_all();
    }

    bool WaitForCount(std::size_t count) const
    {
        std::unique_lock<std::mutex> lock(mutex_);
        return cv_.wait_for(lock, kLiveness, [&] { return statuses_.size() >= count; });
    }

    std::size_t Count() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return statuses_.size();
    }

    std::vector<RpcStatus> Statuses() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return statuses_;
    }

private:
    mutable std::mutex mutex_;
    mutable std::condition_variable cv_;
    std::vector<RpcStatus> statuses_;
};

ClientDoneCallback RecordInto(const std::shared_ptr<StatusLog>& log)
{
    return [log](const RpcStatus& status, const MessagePtr&) { log->Push(status); };
}

// ---------------------------------------------------------------------------
// 回环夹具
// ---------------------------------------------------------------------------
//
// 两种拓扑,都跑在同一个 EventLoopThread 上:
//   * 出站:原始 TcpServer(peer_)作对端,只记录被测 RpcClient 发来的帧、按需手工发帧;
//   * 入站:真身 muduo::net::RpcServer(rpcServer_)作被测端,原始 TcpClient(rawClient_)作对端,
//     经 RpcServer::setConnectionObserver 拿到入站连接,再从连接的 context 取 RpcServer 建的 channel。
class RpcLinkHeartbeatLoopback : public ::testing::Test
{
protected:
    void SetUp() override
    {
        loop_.emplace(*loopThread_.startLoop());
        port_ = FindFreeLoopbackPort();
        ASSERT_NE(port_, 0);
        ASSERT_TRUE(IsDispatchableMessageId(kProcessId))
            << "gRpcMethodRegistry is empty: the global RpcRegistryEnvironment (rpc_channel_loopback_test.cpp) did not run";
    }

    void TearDown() override
    {
        if (!loop_)
        {
            return;
        }
        // 先拆主动连接的一侧(各自连接的唯一持有者 → forceClose → 对端 DOWN),等对端每条连接都报过 DOWN,
        // 再在 loop 线程销毁两个服务端(~TcpServer 断言 in-loop)。
        RunInLoopAndWait([this] {
            client_.reset();
            rawClient_.reset();
        });
        PumpLoop(3);
        EXPECT_TRUE(WaitUntil([this] {
            return peerDowns_.load() == peerUps_.load() && inboundDowns_.load() == inboundUps_.load();
        })) << "a server side did not see DOWN for every connection: peer ups=" << peerUps_.load()
            << " downs=" << peerDowns_.load() << ", inbound ups=" << inboundUps_.load()
            << " downs=" << inboundDowns_.load();
        RunInLoopAndWait([this] {
            peer_.reset();
            rpcServer_.reset();
        });
        PumpLoop(2);
        // loopThread_ 析构 = quit + join。
    }

    muduo::net::EventLoop& Loop() { return loop_->get(); }

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

    // 把 loop 转几圈,让排队的 functor(批量失败、forceCloseInLoop、connectDestroyed……)跑完。
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

    // ---- 出站 ----

    void StartPeer()
    {
        RunInLoopAndWait([this] {
            peer_ = std::make_unique<muduo::net::TcpServer>(
                &Loop(), muduo::net::InetAddress(port_, /*loopbackOnly=*/true), "rpc_link_heartbeat_peer");
            // 回调捕获夹具 this:peer_ 在 TearDown 里先于夹具销毁(AGENTS §11.7 允许的同寿命例外)。
            peer_->setConnectionCallback([this](const TcpConnectionPtr& conn) {
                if (conn->connected())
                {
                    peerConn_ = conn;
                    conn->setMessageCallback(
                        [this](const TcpConnectionPtr& c, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime) {
                            peerRecorder_.OnMessage(c, buffer, receiveTime);
                        });
                    ++peerUps_;
                    return;
                }
                ++peerDowns_;
            });
            peer_->start();   // 在 loop 线程上即时 listen,随后的 connect 不会撞拒连
        });
    }

    // heartbeat 为真时与节点装配一样开启链路心跳(取测试值 kTestHeartbeat)。
    void ConnectClient(bool heartbeat)
    {
        RunInLoopAndWait([this, heartbeat] {
            client_ = std::make_shared<RpcClient>(&Loop(), muduo::net::InetAddress("127.0.0.1", port_));
            if (heartbeat)
            {
                client_->setLinkHeartbeat(kTestHeartbeat);
            }
            client_->connect();
        });
        ASSERT_TRUE(WaitUntil([this] { return client_->connected() && !peerConn_.expired(); }))
            << "RpcClient never connected";
    }

    // loop 线程:由对端发一帧。
    void SendFromPeer(const GameRpcMessage& frame)
    {
        const auto conn = peerConn_.lock();
        if (!conn)
        {
            ADD_FAILURE() << "peer connection is gone";
            return;
        }
        peerRecorder_.Send(conn, frame);
    }

    // ---- 入站 ----

    void StartRpcServer()
    {
        RunInLoopAndWait([this] {
            rpcServer_ = std::make_unique<muduo::net::RpcServer>(
                &Loop(), muduo::net::InetAddress(port_, /*loopbackOnly=*/true));
            rpcServer_->setLinkHeartbeat(kTestHeartbeat);
            // 同寿命例外:rpcServer_ 在 TearDown 里先于夹具销毁。
            rpcServer_->setConnectionObserver([this](const TcpConnectionPtr& conn) {
                if (conn->connected())
                {
                    inboundConn_ = conn;
                    ++inboundUps_;
                    return;
                }
                ++inboundDowns_;
            });
            rpcServer_->start();
        });
    }

    void ConnectRawClient()
    {
        RunInLoopAndWait([this] {
            rawClient_ = std::make_unique<muduo::net::TcpClient>(
                &Loop(), muduo::net::InetAddress("127.0.0.1", port_), "rpc_link_heartbeat_raw");
            // 同寿命例外:rawClient_ 在 TearDown 里先于夹具销毁;没有 enableRetry,断了不重连。
            rawClient_->setConnectionCallback([this](const TcpConnectionPtr& conn) {
                if (!conn->connected())
                {
                    ++rawDowns_;
                }
            });
            rawClient_->setMessageCallback(
                [this](const TcpConnectionPtr& c, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime) {
                    rawRecorder_.OnMessage(c, buffer, receiveTime);
                });
            rawClient_->connect();
        });
        ASSERT_TRUE(WaitUntil([this] { return rawClient_->connection() != nullptr && !inboundConn_.expired(); }))
            << "raw client never connected to the RpcServer";
    }

    // loop 线程:RpcServer 给入站连接建的 channel(存在连接的 context 里);连接已断或 context 已清时为空。
    RpcChannelPtr InboundChannel() const
    {
        const auto conn = inboundConn_.lock();
        if (!conn)
        {
            return nullptr;
        }
        const auto* channel = boost::any_cast<RpcChannelPtr>(&conn->getContext());
        return channel != nullptr ? *channel : nullptr;
    }

    // loopThread_ 排第一:最后析构,保证其余成员析构时 loop 仍在。
    muduo::net::EventLoopThread loopThread_;
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop_;
    uint16_t port_{0};

    // 出站拓扑。两个 weak 只在 loop 线程上读写。
    std::unique_ptr<muduo::net::TcpServer> peer_;
    FrameRecorder peerRecorder_;
    std::weak_ptr<muduo::net::TcpConnection> peerConn_;
    std::atomic<int> peerUps_{0};
    std::atomic<int> peerDowns_{0};
    RpcClientPtr client_;

    // 入站拓扑。
    std::unique_ptr<muduo::net::RpcServer> rpcServer_;
    std::weak_ptr<muduo::net::TcpConnection> inboundConn_;
    std::atomic<int> inboundUps_{0};
    std::atomic<int> inboundDowns_{0};
    std::unique_ptr<muduo::net::TcpClient> rawClient_;
    FrameRecorder rawRecorder_;
    std::atomic<int> rawDowns_{0};
};

} // namespace

// ===========================================================================
// A 纯逻辑:RpcLinkLiveness(生产取值;合成时刻,按 1s 步进模拟节拍)
// ===========================================================================

TEST(RpcLinkLiveness, ProductionValuesMatchTheDesign)
{
    // 推导见 docs/design/muduo-rpc-channel.md §21.4,与 Java NodeLink(5s / 15s)同值。改这里要同步改文档与 Java 版。
    EXPECT_EQ(kNodeLinkHeartbeat.interval, std::chrono::milliseconds(5000));
    EXPECT_EQ(kNodeLinkHeartbeat.idleTimeout, std::chrono::milliseconds(15000));
    EXPECT_EQ(kNodeLinkHeartbeat.checkPeriod, std::chrono::milliseconds(1000));
    EXPECT_TRUE(IsValidLinkHeartbeat(kNodeLinkHeartbeat));
}

TEST(RpcLinkLiveness, RejectsInvalidOptions)
{
    EXPECT_FALSE(IsValidLinkHeartbeat({0s, 15s, 1s})) << "interval must be positive";
    EXPECT_FALSE(IsValidLinkHeartbeat({5s, 15s, 0s})) << "check period must be positive";
    EXPECT_FALSE(IsValidLinkHeartbeat({5s, 15s, 6s})) << "check period must not exceed the interval";
    // 健康链路上最长的静默是 interval + 2 × checkPeriod = 7s,idleTimeout 必须严格大于它。
    EXPECT_FALSE(IsValidLinkHeartbeat({5s, 7s, 1s}));
    EXPECT_TRUE(IsValidLinkHeartbeat({5s, 7001ms, 1s}));
}

TEST(RpcLinkLiveness, HeartbeatIsDueAfterExactlyOneWriteIdleInterval)
{
    RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
    // 对端每拍都有帧到,只看写方向。
    for (auto now = kOrigin + kCheckPeriod; now < kOrigin + kInterval; now += kCheckPeriod)
    {
        liveness.OnReceived();
        EXPECT_FALSE(liveness.Check(now).sendHeartbeat);
    }
    liveness.OnReceived();
    EXPECT_FALSE(liveness.Check(kOrigin + kInterval - 1ns).sendHeartbeat) << "one nanosecond early";
    liveness.OnReceived();
    EXPECT_TRUE(liveness.Check(kOrigin + kInterval).sendHeartbeat);

    // 刚发过一帧:下一帧要再空闲满一个 interval。
    const auto sentAt = kOrigin + kInterval;
    for (auto now = sentAt + kCheckPeriod; now < sentAt + kInterval; now += kCheckPeriod)
    {
        liveness.OnReceived();
        EXPECT_FALSE(liveness.Check(now).sendHeartbeat);
    }
    liveness.OnReceived();
    EXPECT_TRUE(liveness.Check(sentAt + kInterval).sendHeartbeat);
}

TEST(RpcLinkLiveness, AnySentFramePostponesTheHeartbeat)
{
    RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
    // 每拍都有业务帧发出(对端也每拍都有帧来):忙的链路一帧心跳都不发。
    const auto busyUntil = kOrigin + 3 * kInterval;
    for (auto now = kOrigin + kCheckPeriod; now <= busyUntil; now += kCheckPeriod)
    {
        liveness.OnSent();
        liveness.OnReceived();
        EXPECT_FALSE(liveness.Check(now).sendHeartbeat);
    }
    // 最后一次发送记在 busyUntil 那次检查上:从那里起空闲满 interval 才发。
    for (auto now = busyUntil + kCheckPeriod; now < busyUntil + kInterval; now += kCheckPeriod)
    {
        liveness.OnReceived();
        EXPECT_FALSE(liveness.Check(now).sendHeartbeat);
    }
    liveness.OnReceived();
    EXPECT_TRUE(liveness.Check(busyUntil + kInterval).sendHeartbeat);
}

TEST(RpcLinkLiveness, IdleTimeoutClosesExactlyAtTheThreshold)
{
    RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
    int heartbeats = 0;
    // 对端一个字节都不发;本端照常每个 interval 发一帧心跳。
    for (auto now = kOrigin + kCheckPeriod; now < kOrigin + kIdleTimeout; now += kCheckPeriod)
    {
        const auto verdict = liveness.Check(now);
        EXPECT_FALSE(verdict.closeIdle);
        EXPECT_FALSE(verdict.idleDeferred);
        heartbeats += verdict.sendHeartbeat ? 1 : 0;
    }
    EXPECT_EQ(heartbeats, 2) << "heartbeats are due at interval and at 2 x interval";
    EXPECT_FALSE(liveness.Check(kOrigin + kIdleTimeout - 1ns).closeIdle) << "one nanosecond early";
    const auto verdict = liveness.Check(kOrigin + kIdleTimeout);
    EXPECT_TRUE(verdict.closeIdle);
    EXPECT_FALSE(verdict.sendHeartbeat) << "no heartbeat on a link that is being closed";
}

TEST(RpcLinkLiveness, AnyInboundByteRestartsTheIdleClock)
{
    RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
    const auto seenAt = kOrigin + 10s;
    for (auto now = kOrigin + kCheckPeriod; now < seenAt; now += kCheckPeriod)
    {
        EXPECT_FALSE(liveness.Check(now).inboundSeen);
    }
    liveness.OnReceived();
    const auto seen = liveness.Check(seenAt);
    EXPECT_TRUE(seen.inboundSeen);
    EXPECT_FALSE(seen.closeIdle);
    // 不重新计时的话,kOrigin + idleTimeout(= seenAt + 5s)就会判死。
    for (auto now = seenAt + kCheckPeriod; now < seenAt + kIdleTimeout; now += kCheckPeriod)
    {
        EXPECT_FALSE(liveness.Check(now).closeIdle);
    }
    EXPECT_FALSE(liveness.Check(seenAt + kIdleTimeout - 1ns).closeIdle);
    EXPECT_TRUE(liveness.Check(seenAt + kIdleTimeout).closeIdle);
}

TEST(RpcLinkLiveness, LateCheckDefersTheIdleVerdictToTheNextCheck)
{
    RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
    // 本端 loop 卡了 30s,这期间一次检查都没跑。醒来后的第一次检查迟到,对端的帧可能还在接收缓冲里没读。
    const auto resumedAt = kOrigin + 30s;
    const auto late = liveness.Check(resumedAt);
    EXPECT_TRUE(late.checkGap == 30s);
    EXPECT_TRUE(late.idleDeferred);
    EXPECT_FALSE(late.closeIdle);
    EXPECT_TRUE(late.sendHeartbeat) << "a late check still sends the heartbeat";
    // 下一次检查准时,仍没读到任何字节:这次才判死。
    const auto next = liveness.Check(resumedAt + kCheckPeriod);
    EXPECT_TRUE(next.closeIdle);
    EXPECT_FALSE(next.idleDeferred);
}

TEST(RpcLinkLiveness, LateThresholdIsTwoCheckPeriods)
{
    // 相邻两次检查相隔恰好 2 个节拍(漏了一拍)不算卡顿,照常判死;超过 2 个节拍才推迟。
    const auto lastOnTime = kOrigin + kIdleTimeout - 2 * kCheckPeriod;
    {
        RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
        for (auto now = kOrigin + kCheckPeriod; now <= lastOnTime; now += kCheckPeriod)
        {
            liveness.Check(now);
        }
        const auto verdict = liveness.Check(lastOnTime + 2 * kCheckPeriod);
        EXPECT_TRUE(verdict.closeIdle);
        EXPECT_FALSE(verdict.idleDeferred);
    }
    {
        RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
        for (auto now = kOrigin + kCheckPeriod; now <= lastOnTime; now += kCheckPeriod)
        {
            liveness.Check(now);
        }
        const auto verdict = liveness.Check(lastOnTime + 2 * kCheckPeriod + 1ns);
        EXPECT_FALSE(verdict.closeIdle);
        EXPECT_TRUE(verdict.idleDeferred);
    }
}

TEST(RpcLinkLiveness, InputReadAroundALateCheckKeepsTheLinkAlive)
{
    {
        // 卡顿期间的帧在迟到那次检查之前读到了:不算空闲,也不必推迟。
        RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
        liveness.OnReceived();
        const auto verdict = liveness.Check(kOrigin + 30s);
        EXPECT_TRUE(verdict.inboundSeen);
        EXPECT_FALSE(verdict.idleDeferred);
        EXPECT_FALSE(verdict.closeIdle);
    }
    {
        // 迟到那次检查推迟了判死,积压的帧在下一次检查之前读到了:链路保住。
        RpcLinkLiveness liveness(kNodeLinkHeartbeat, kOrigin);
        EXPECT_TRUE(liveness.Check(kOrigin + 30s).idleDeferred);
        liveness.OnReceived();
        const auto next = liveness.Check(kOrigin + 30s + kCheckPeriod);
        EXPECT_TRUE(next.inboundSeen);
        EXPECT_FALSE(next.closeIdle);
        EXPECT_FALSE(next.idleDeferred);
    }
}

// ===========================================================================
// B 接线:真实回环 + 合成时刻
// ===========================================================================

TEST_F(RpcLinkHeartbeatLoopback, OutboundSendsOneHeartbeatFrameWhenWriteIdle)
{
    ASSERT_NO_FATAL_FAILURE(StartPeer());
    const auto t0 = LinkClock::now();
    ASSERT_NO_FATAL_FAILURE(ConnectClient(/*heartbeat=*/true));
    const auto t1 = LinkClock::now();

    bool enabled = false;
    RpcLinkLiveness::Verdict early;
    RpcLinkLiveness::Verdict due;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = client_->channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        enabled = channel->HasLinkHeartbeat();
        early = channel->CheckLinkLiveness(t0 + kTestHeartbeat.interval - 1ns);
        due = channel->CheckLinkLiveness(t1 + kTestHeartbeat.interval);
        // 屏障:同一连接 FIFO,对端在它之前收到的就是之前发出的全部帧。
        EXPECT_TRUE(client_->SendRequest(kProcessId, ProcessRequest(kBarrierSession)));
    });
    EXPECT_TRUE(enabled) << "setLinkHeartbeat did not reach the channel created on connect";
    EXPECT_FALSE(early.sendHeartbeat) << "heartbeat sent before a full write-idle interval";
    EXPECT_TRUE(due.sendHeartbeat);
    EXPECT_FALSE(due.closeIdle);

    ASSERT_TRUE(peerRecorder_.WaitForCount(2));
    const auto frames = peerRecorder_.Frames();
    const auto& heartbeat = frames[0];
    EXPECT_EQ(heartbeat.type(), HEARTBEAT) << Describe(frames);
    EXPECT_EQ(heartbeat.id(), 0u);
    EXPECT_EQ(heartbeat.message_id(), 0u);
    EXPECT_EQ(heartbeat.error(), RPC_NO_ERROR);
    EXPECT_TRUE(heartbeat.request().empty());
    EXPECT_TRUE(heartbeat.response().empty());
    EXPECT_EQ(frames[1].type(), REQUEST) << "exactly one heartbeat must precede the barrier: " << Describe(frames);
    EXPECT_EQ(frames[1].message_id(), kProcessId);
}

TEST_F(RpcLinkHeartbeatLoopback, OutboundIdleTimeoutClosesTheLinkAndTheClientReconnects)
{
    ASSERT_NO_FATAL_FAILURE(StartPeer());
    const auto t0 = LinkClock::now();
    ASSERT_NO_FATAL_FAILURE(ConnectClient(/*heartbeat=*/true));
    const auto t1 = LinkClock::now();
    auto& stats = GetRpcChannelStats();
    const auto closedBefore = Load(stats.linkIdleClosed);
    const auto disconnectedBefore = Load(stats.disconnected);
    auto calls = std::make_shared<StatusLog>();

    std::weak_ptr<RpcChannel> oldChannel;
    std::vector<RpcLinkLiveness::Verdict> beforeTimeout;
    RpcLinkLiveness::Verdict atTimeout;
    bool readyAfterVerdict = true;
    std::size_t completionsAtVerdict = 99;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = client_->channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        oldChannel = channel;
        // 一次在途调用:对端从不回包,判死之后由既有的 DOWN 路径以 kDisconnected 失败。
        client_->CallMethod(kProcessId, ProcessRequest(1), RecordInto(calls), RpcCallOptions{kLongTimeout});
        // 对端一个字节都不发。相邻两次检查不超过 2 × checkPeriod,否则按本端卡顿推迟判死。
        for (int hour = 1; hour <= 3; ++hour)
        {
            beforeTimeout.push_back(channel->CheckLinkLiveness(t1 + std::chrono::hours(hour)));
        }
        beforeTimeout.push_back(channel->CheckLinkLiveness(t0 + kTestHeartbeat.idleTimeout - 1ns));
        atTimeout = channel->CheckLinkLiveness(t1 + kTestHeartbeat.idleTimeout);
        readyAfterVerdict = client_->IsReady();
        completionsAtVerdict = calls->Count();
    });
    for (const auto& verdict : beforeTimeout)
    {
        EXPECT_FALSE(verdict.closeIdle) << "closed before a full idle timeout";
        EXPECT_FALSE(verdict.idleDeferred);
    }
    EXPECT_TRUE(atTimeout.closeIdle);
    EXPECT_FALSE(atTimeout.sendHeartbeat);
    // forceClose 当场把连接置为 kDisconnecting:同一栈上业务已经看到链路不可用。
    EXPECT_FALSE(readyAfterVerdict);
    EXPECT_EQ(completionsAtVerdict, 0u) << "in-flight calls must fail through the DOWN path, not on the check stack";
    EXPECT_EQ(Load(stats.linkIdleClosed) - closedBefore, 1u);

    ASSERT_TRUE(calls->WaitForCount(1));
    const auto status = calls->Statuses().front();
    EXPECT_EQ(status.code(), RpcStatusCode::kDisconnected) << status.name();
    EXPECT_TRUE(status.OutcomeUnknown());
    EXPECT_EQ(Load(stats.disconnected) - disconnectedBefore, 1u);
    ASSERT_TRUE(WaitUntil([this] { return peerDowns_.load() >= 1; })) << "the peer never saw the forced close";

    // enableRetry 重连:新连接上是新 channel,同样带心跳(setLinkHeartbeat 跨重连保留)。
    ASSERT_TRUE(WaitUntil([this] { return peerUps_.load() >= 2 && client_->connected(); }))
        << "RpcClient did not reconnect";
    bool oldExpired = false;
    bool newHasHeartbeat = false;
    RunInLoopAndWait([&] {
        oldExpired = oldChannel.expired();
        newHasHeartbeat = client_->channel() != nullptr && client_->channel()->HasLinkHeartbeat();
    });
    EXPECT_TRUE(oldExpired) << "the closed link's channel must be dropped on DOWN";
    EXPECT_TRUE(newHasHeartbeat);
    EXPECT_EQ(calls->Count(), 1u) << "the in-flight call completed more than once";
}

TEST_F(RpcLinkHeartbeatLoopback, HeartbeatFromThePeerRestartsTheIdleClock)
{
    ASSERT_NO_FATAL_FAILURE(StartPeer());
    ASSERT_NO_FATAL_FAILURE(ConnectClient(/*heartbeat=*/true));
    const auto t1 = LinkClock::now();

    RunInLoopAndWait([&] { SendFromPeer(MakeHeartbeatFrame()); });
    // 用同一个合成时刻反复检查,直到某次检查看到了这帧(5s 活性上限)。seenAt 取建连后一个 interval:
    // 写方向顺带发一帧心跳,无妨;读方向离判死还差 3 个 interval。
    const auto seenAt = t1 + kTestHeartbeat.interval;
    ASSERT_TRUE(WaitUntil([&] {
        const RpcChannelPtr channel = client_->channel();
        return channel != nullptr && channel->CheckLinkLiveness(seenAt).inboundSeen;
    })) << "the peer's heartbeat never reached the channel";

    std::vector<RpcLinkLiveness::Verdict> beforeTimeout;
    RpcLinkLiveness::Verdict atTimeout;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = client_->channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        // 没有这帧的话,读空闲在建连后满 4h 判死,不晚于 seenAt + 3h;收到之后要等到 seenAt + 4h。
        for (int hour = 1; hour <= 3; ++hour)
        {
            beforeTimeout.push_back(channel->CheckLinkLiveness(seenAt + std::chrono::hours(hour)));
        }
        beforeTimeout.push_back(channel->CheckLinkLiveness(seenAt + kTestHeartbeat.idleTimeout - 1ns));
        atTimeout = channel->CheckLinkLiveness(seenAt + kTestHeartbeat.idleTimeout);
    });
    for (const auto& verdict : beforeTimeout)
    {
        EXPECT_FALSE(verdict.closeIdle) << "the heartbeat from the peer did not restart the idle clock";
    }
    EXPECT_TRUE(atTimeout.closeIdle);
}

TEST_F(RpcLinkHeartbeatLoopback, HeartbeatFrameIsNeitherDispatchedNorAnswered)
{
    ASSERT_NO_FATAL_FAILURE(StartPeer());
    ASSERT_NO_FATAL_FAILURE(ConnectClient(/*heartbeat=*/true));
    auto& stats = GetRpcChannelStats();
    const auto unknownBefore = Load(stats.unknownType);
    const auto rejectedBefore = Load(stats.requestsRejected);
    const auto dispatchedBefore = Load(stats.requestsDispatched);

    RunInLoopAndWait([&] {
        SendFromPeer(MakeHeartbeatFrame());
        // 屏障:id≠0 的请求。RpcClient 没装服务,框架回 RPC_ERROR(NO_SERVICE) 并回显这个 id。
        SendFromPeer(MakeRequestFrame(kBarrierId, kProcessId, ProcessRequest(kBarrierSession)));
    });
    ASSERT_TRUE(peerRecorder_.WaitForCount(1));
    const auto frames = peerRecorder_.Frames();
    EXPECT_EQ(frames.front().id(), kBarrierId) << "something answered the heartbeat: " << Describe(frames);
    EXPECT_EQ(frames.front().type(), RPC_ERROR);
    EXPECT_EQ(frames.front().error(), NO_SERVICE);
    EXPECT_EQ(Load(stats.unknownType) - unknownBefore, 0u) << "HEARTBEAT fell through to the unknown-type branch";
    EXPECT_EQ(Load(stats.requestsRejected) - rejectedBefore, 1u) << "only the barrier should have been rejected";
    EXPECT_EQ(Load(stats.requestsDispatched) - dispatchedBefore, 0u);
    bool stillConnected = false;
    RunInLoopAndWait([&] { stillConnected = client_->connected(); });
    EXPECT_TRUE(stillConnected);
}

TEST_F(RpcLinkHeartbeatLoopback, InboundRpcServerHeartbeatsAndClosesIdleLinks)
{
    ASSERT_NO_FATAL_FAILURE(StartRpcServer());
    const auto t0 = LinkClock::now();
    ASSERT_NO_FATAL_FAILURE(ConnectRawClient());
    const auto t1 = LinkClock::now();
    const auto closedBefore = Load(GetRpcChannelStats().linkIdleClosed);

    bool enabled = false;
    RpcLinkLiveness::Verdict early;
    std::vector<RpcLinkLiveness::Verdict> beforeTimeout;
    RpcLinkLiveness::Verdict atTimeout;
    int heartbeatsSent = 0;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = InboundChannel();
        if (!channel)
        {
            ADD_FAILURE() << "the inbound connection carries no RpcChannel";
            return;
        }
        enabled = channel->HasLinkHeartbeat();
        early = channel->CheckLinkLiveness(t0 + kTestHeartbeat.interval - 1ns);
        // 对端(原始客户端)一个字节都不发:每个 interval 发一帧心跳,满 idleTimeout 判死。
        for (int hour = 1; hour <= 3; ++hour)
        {
            const auto verdict = channel->CheckLinkLiveness(t1 + std::chrono::hours(hour));
            heartbeatsSent += verdict.sendHeartbeat ? 1 : 0;
            beforeTimeout.push_back(verdict);
        }
        beforeTimeout.push_back(channel->CheckLinkLiveness(t0 + kTestHeartbeat.idleTimeout - 1ns));
        atTimeout = channel->CheckLinkLiveness(t1 + kTestHeartbeat.idleTimeout);
    });
    EXPECT_TRUE(enabled) << "RpcServer::setLinkHeartbeat did not reach the inbound channel";
    EXPECT_FALSE(early.sendHeartbeat);
    EXPECT_EQ(heartbeatsSent, 3) << "one heartbeat per write-idle interval";
    for (const auto& verdict : beforeTimeout)
    {
        EXPECT_FALSE(verdict.closeIdle) << "closed before a full idle timeout";
    }
    EXPECT_TRUE(atTimeout.closeIdle);
    EXPECT_EQ(Load(GetRpcChannelStats().linkIdleClosed) - closedBefore, 1u);

    // 判死只关传输:原始客户端看到 DOWN,RpcServer 的 DOWN 分支照常执行、清掉连接的 context。
    ASSERT_TRUE(WaitUntil([this] { return rawDowns_.load() >= 1 && inboundDowns_.load() >= 1; }))
        << "the idle inbound link was not closed";
    bool contextCleared = false;
    RunInLoopAndWait([&] { contextCleared = InboundChannel() == nullptr; });
    EXPECT_TRUE(contextCleared);
    // 判死之前发出的心跳都已在线上(输出缓冲为空时 send 直接写 socket),原始客户端在 DOWN 之前全部读到。
    const auto frames = rawRecorder_.Frames();
    EXPECT_EQ(frames.size(), 3u) << Describe(frames);
    for (const auto& frame : frames)
    {
        EXPECT_EQ(frame.type(), HEARTBEAT) << Describe(frames);
    }
}

TEST_F(RpcLinkHeartbeatLoopback, ChannelsWithoutHeartbeatNeverSendOrClose)
{
    ASSERT_NO_FATAL_FAILURE(StartPeer());
    ASSERT_NO_FATAL_FAILURE(ConnectClient(/*heartbeat=*/false));
    const auto t1 = LinkClock::now();

    bool enabled = true;
    RpcLinkLiveness::Verdict verdict;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = client_->channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        enabled = channel->HasLinkHeartbeat();
        verdict = channel->CheckLinkLiveness(t1 + std::chrono::hours(24 * 365));
        EXPECT_TRUE(client_->SendRequest(kProcessId, ProcessRequest(kBarrierSession)));
    });
    EXPECT_FALSE(enabled);
    EXPECT_FALSE(verdict.sendHeartbeat);
    EXPECT_FALSE(verdict.closeIdle);
    EXPECT_FALSE(verdict.idleDeferred);
    ASSERT_TRUE(peerRecorder_.WaitForCount(1));
    EXPECT_EQ(peerRecorder_.Frames().front().type(), REQUEST) << "the barrier must be the first frame";
    bool stillConnected = false;
    RunInLoopAndWait([&] { stillConnected = client_->connected(); });
    EXPECT_TRUE(stillConnected);
}

TEST_F(RpcLinkHeartbeatLoopback, HeartbeatKeepsRunningAfterTheChannelIsClosed)
{
    ASSERT_NO_FATAL_FAILURE(StartPeer());
    ASSERT_NO_FATAL_FAILURE(ConnectClient(/*heartbeat=*/true));
    const auto t1 = LinkClock::now();

    bool closed = false;
    RpcLinkLiveness::Verdict due;
    RunInLoopAndWait([&] {
        client_->CloseChannel();   // 节点关机阶段 0:拒新调用,链路仍在
        const RpcChannelPtr channel = client_->channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        closed = channel->IsClosed();
        due = channel->CheckLinkLiveness(t1 + kTestHeartbeat.interval);
    });
    EXPECT_TRUE(closed);
    EXPECT_TRUE(due.sendHeartbeat) << "Close() must not stop heartbeats, or the peer kills the link while we drain";
    ASSERT_TRUE(peerRecorder_.WaitForCount(1));
    EXPECT_EQ(peerRecorder_.Frames().front().type(), HEARTBEAT);
}

// ===========================================================================
// C 汇总行字段(与 stress_summarize.ps1 的契约)
// ===========================================================================

TEST(RpcLinkHeartbeatStats, SummaryLineCarriesTheIdleCounters)
{
    RpcChannelStats stats;
    stats.linkIdleClosed.store(3, std::memory_order_relaxed);
    stats.linkIdleDeferred.store(4, std::memory_order_relaxed);
    const std::string line = FormatRpcChannelStats(stats);
    // 字段名是与 tools/scripts/stress_summarize.ps1 的契约,改名要两边一起改。
    EXPECT_NE(line.find(" link_idle_closed=3"), std::string::npos) << line;
    EXPECT_NE(line.find(" link_idle_deferred=4"), std::string::npos) << line;
}
