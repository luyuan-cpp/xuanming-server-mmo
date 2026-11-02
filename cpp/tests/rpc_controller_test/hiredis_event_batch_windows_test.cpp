#ifdef _WIN32

// hiredis_event_batch_windows_test —— Hiredis 适配层在 Windows 上的两条同批生命周期回归:
//  1. 同一轮里先释放、后有事件:本轮较早派发的另一个 Channel 撤掉 Hiredis 的最后一个 owner,
//     而这条连接本轮也有已就绪的 I/O。修复前(裸 this 回调、同步析构),同步析构会在
//     EventLoop::removeChannel 撞 Debug 断言「本轮仍排着事件的 Channel 不得注销」;Release 下随后
//     派发的旧 Channel 回调拿已析构的 this。修复后:weak 回调与 tie 让旧事件失效,Hiredis::Create 的
//     删除器把整个对象(context 与 socket)推迟到本轮 pending 阶段析构。
//  2. 对端半关闭:对端只 shutdown(SD_SEND),WSAPoll 可能只报 POLLHUP。断线必须被察觉,挂起命令恰好以
//     空应答结束一次;所有者在断线回调里放掉最后一个引用时,对象要活到这次事件分发返回。保活主要靠 Channel
//     回调里 weakSelf.lock() 拿到的局部强引用;Channel::tie 是纵深防御,本文件没有单独锁住它的用例。
//
// Linux 同一机制由 cpp/tests/redis_lifecycle_test/hiredis_event_batch_test.cpp 覆盖(eventfd + MUDUO_USE_POLL);
// 本文件只登记进 Windows 的 rpc_controller_test.vcxproj。Windows muduo 的 PollPoller 按注册顺序收集
// activeChannels_,所以「前置通道先于受害连接派发」可以确定地构造。
// loop 在测试线程上跑,所有等待都由 2 秒看门狗定时器或 WSAPoll 超时兜底,不做无限期等待。
// 每条用例给自己的连接传独立的 Hiredis::MaintenanceState(同 hiredis_command_lifecycle_test.cpp),不碰进程级账本;
// 用例末尾 Close() 后断言 Joined():连接对象和 removeChannel 排队保活的 Channel 引用都已释放、命令都已结束。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main;链接桩已由 rpc_client_lifecycle_test.cpp 定义。

#include <gtest/gtest.h>

#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")

#include <cstdint>
#include <memory>
#include <string>

#include "muduo/base/Timestamp.h"
#include "muduo/net/Channel.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/contrib/hiredis/Hiredis.h"   // 它包含 <hiredis/hiredis.h>,提供 REDIS_OK / redisReply

namespace
{

constexpr double kWatchdogSec = 2.0;
constexpr int kSocketWaitMs = 1500;

struct SocketLease
{
    SOCKET value{INVALID_SOCKET};
    SocketLease() = default;
    SocketLease(const SocketLease&) = delete;
    SocketLease& operator=(const SocketLease&) = delete;
    ~SocketLease()
    {
        if (value != INVALID_SOCKET)
        {
            ::closesocket(value);
        }
    }
};

bool ListenLoopback(SocketLease& listener, uint16_t& port)
{
    listener.value = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (listener.value == INVALID_SOCKET)
    {
        return false;
    }
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    int size = sizeof(address);
    if (::bind(listener.value, reinterpret_cast<sockaddr*>(&address), size) != 0 ||
        ::getsockname(listener.value, reinterpret_cast<sockaddr*>(&address), &size) != 0 ||
        ::listen(listener.value, 4) != 0)
    {
        return false;
    }
    port = ntohs(address.sin_port);
    return port != 0;
}

bool WaitReadable(SOCKET socket)
{
    WSAPOLLFD descriptor{};
    descriptor.fd = socket;
    descriptor.events = POLLRDNORM;
    return ::WSAPoll(&descriptor, 1, kSocketWaitMs) == 1 && (descriptor.revents & (POLLRDNORM | POLLHUP)) != 0;
}

// 等对端发起的连接到达后 accept。Hiredis 的非阻塞 connect 在 connect() 返回前已发出 SYN。
bool AcceptPeer(const SocketLease& listener, SocketLease& peer)
{
    if (!WaitReadable(listener.value))
    {
        return false;
    }
    peer.value = ::accept(listener.value, nullptr, nullptr);
    return peer.value != INVALID_SOCKET;
}

// 本机回环 UDP:向自己发一个字节就让它在下一轮 poll 里可读,用作「同一轮里较早派发的那个 Channel」。
bool OpenUdpLoopback(SocketLease& udp, sockaddr_in& self)
{
    udp.value = ::socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (udp.value == INVALID_SOCKET)
    {
        return false;
    }
    self = sockaddr_in{};
    self.sin_family = AF_INET;
    self.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    int size = sizeof(self);
    u_long nonBlocking = 1;
    return ::bind(udp.value, reinterpret_cast<sockaddr*>(&self), size) == 0 &&
           ::getsockname(udp.value, reinterpret_cast<sockaddr*>(&self), &size) == 0 &&
           ::ioctlsocket(udp.value, FIONBIO, &nonBlocking) == 0;
}

bool SendAll(SOCKET socket, const std::string& data)
{
    return ::send(socket, data.data(), static_cast<int>(data.size()), 0) == static_cast<int>(data.size());
}

// 在 loop 线程(即测试线程)上跑到有回调 quit,或看门狗到点;返回 false 表示是看门狗结束的。
bool RunUntilQuit(muduo::net::EventLoop& loop)
{
    bool watchdogExpired = false;
    const muduo::net::TimerId watchdog = loop.runAfter(kWatchdogSec, [&] {
        watchdogExpired = true;
        loop.quit();
    });
    loop.loop();
    loop.cancel(watchdog);
    return !watchdogExpired;
}

// 排空 removeChannel 排队持有 Channel 的 dummy 等 pending functor,再让 loop 停下(AGENTS.md §11.7 先排空再 quit)。
// 在 loop 线程上 queueInLoop 不会唤醒 poll,Windows 没有定时器时 poll 要等满 kPollTimeMs(100ms,见 EventLoop.cc
// 与 TimerQueue::nextTimeoutMs);零延时定时器让 nextTimeoutMs 返回 0,这次 poll 立刻返回。
void DrainPending(muduo::net::EventLoop& loop)
{
    loop.queueInLoop([&loop] { loop.quit(); });
    loop.runAfter(0, [] {});
    loop.loop();
}

// 清掉连接 / 断线回调后释放连接;loop 没在跑时是同步析构,挂起命令此刻以空应答回调。
void ReleaseConnection(std::shared_ptr<hiredis::Hiredis>& redis)
{
    if (!redis)
    {
        return;
    }
    redis->setConnectCallback({});
    redis->setDisconnectCallback({});
    redis.reset();
}

// 作用域收尾。用例里的 Hiredis 回调以引用捕获测试栈上的计数;ASSERT 失败提前返回时,连接若仍有 owner,
// 它的析构会同步回调挂起命令(和断线回调),写进已经析构的局部变量。所以收尾守卫声明在全部被捕获的局部
// 变量之后:它最先执行,在那些变量还活着时释放连接并排空 pending。正常路径上同样由它收尾。
template <typename F>
struct ScopeExit
{
    F onExit;
    ~ScopeExit() { onExit(); }
};
template <typename F>
ScopeExit(F) -> ScopeExit<F>;

// 用例中途 ASSERT 失败提前返回时也先注销 Channel:仍注册着的 Channel 析构会断言,带崩整个测试进程。
struct ChannelRegistration
{
    muduo::net::Channel& channel;
    ~ChannelRegistration()
    {
        channel.disableAll();
        channel.remove();
    }
};

class HiredisEventBatchWindows : public ::testing::Test
{
protected:
    void SetUp() override
    {
        static const int status = [] {
            WSADATA data{};
            return ::WSAStartup(MAKEWORD(2, 2), &data);
        }();
        ASSERT_EQ(status, 0);
    }
};

using MaintenanceStatePtr = std::shared_ptr<hiredis::Hiredis::MaintenanceState>;

// 在给定 loop 上建一条连到 port 的 Hiredis 并跑到连接回调;同时 accept 对端。
std::shared_ptr<hiredis::Hiredis> ConnectVictim(muduo::net::EventLoop& loop, uint16_t port,
                                                 const MaintenanceStatePtr& maintenance,
                                                 const SocketLease& listener, SocketLease& peer, int& connectCalls)
{
    auto redis = hiredis::Hiredis::Create(&loop, muduo::net::InetAddress("127.0.0.1", port), maintenance);
    // 回调只捕获测试栈上的 loop 与计数:它们比 redis 活得久(用例末尾先释放 redis 再排空 loop)。
    redis->setConnectCallback([&loop, &connectCalls](hiredis::Hiredis*, int status) {
        EXPECT_EQ(status, REDIS_OK);
        ++connectCalls;
        loop.queueInLoop([&loop] { loop.quit(); });
    });
    redis->connect();
    if (!AcceptPeer(listener, peer))
    {
        ADD_FAILURE() << "fake Redis never saw the Hiredis connection";
        return redis;
    }
    EXPECT_TRUE(RunUntilQuit(loop)) << "Hiredis connect callback did not run within the watchdog";
    redis->setConnectCallback({});
    return redis;
}

} // namespace

// ---------------------------------------------------------------------------
// 1. 同一轮里先释放、后有事件。activeChannels_ 的顺序 = 注册顺序:前置 UDP 通道先注册,受害 Hiredis 建连后
//    重建的 Channel 排在它后面。两者在同一次 WSAPoll 里同时就绪(UDP 可读;受害连接上有 +PONG 可读、
//    挂起的 PING 可写)。前置通道的回调撤掉受害连接唯一的 owner,随后同一批里受害连接的事件必须被丢弃。
// ---------------------------------------------------------------------------

TEST_F(HiredisEventBatchWindows, EarlierChannelReleasesLastOwnerOfReadyConnectionInSameBatch)
{
    muduo::net::EventLoop loop;
    SocketLease listener;
    uint16_t port = 0;
    ASSERT_TRUE(ListenLoopback(listener, port));

    // 前置通道必须先于受害连接注册;Channel 在 loop 之后声明、先于 loop 析构,由 attackerRegistration 注销。
    SocketLease udp;
    sockaddr_in udpSelf{};
    ASSERT_TRUE(OpenUdpLoopback(udp, udpSelf));
    ASSERT_LE(udp.value, static_cast<SOCKET>(INT32_MAX));
    muduo::net::Channel attacker(&loop, static_cast<int>(udp.value));
    attacker.enableReading();
    const ChannelRegistration attackerRegistration{attacker};

    // 受害连接与替代连接共用本用例自己的账本,声明在两者之前。
    const MaintenanceStatePtr maintenance = std::make_shared<hiredis::Hiredis::MaintenanceState>();
    SocketLease victimPeer;
    int victimConnects = 0;
    auto victim = ConnectVictim(loop, port, maintenance, listener, victimPeer, victimConnects);
    ASSERT_EQ(victimConnects, 1);
    ASSERT_TRUE(victim->connected());
    const std::weak_ptr<hiredis::Hiredis> victimWeak = victim;

    int nullReplies = 0;
    int nonNullReplies = 0;
    std::int64_t nullReplyIteration = -1;
    bool nullReplyInPending = false;
    std::weak_ptr<int> commandCapture;
    {
        auto capture = std::make_shared<int>(0);
        commandCapture = capture;
        ASSERT_EQ(victim->command([&, capture](hiredis::Hiredis*, redisReply* reply) {
            if (reply != nullptr)
            {
                ++nonNullReplies;
                return;
            }
            ++nullReplies;
            nullReplyIteration = loop.iteration();
            nullReplyInPending = !loop.eventHandling();
        }, "PING"), REDIS_OK);
    }

    int attackerCalls = 0;
    std::int64_t attackerIteration = -1;
    bool victimExpiredInAttacker = false;
    std::shared_ptr<hiredis::Hiredis> replacement;
    int replacementConnects = 0;
    int replacementReplies = 0;
    // 见 ScopeExit 注释:声明在全部被回调捕获的局部变量之后。先停掉前置通道,排空时不再派发它的回调;
    // 前置通道本身由更早声明的 attackerRegistration 注销,晚于这里、早于 loop 析构。
    const ScopeExit releaseBeforeCaptures{[&] {
        attacker.disableAll();
        ReleaseConnection(victim);
        ReleaseConnection(replacement);
        DrainPending(loop);
    }};
    attacker.setReadCallback([&](muduo::Timestamp) {
        char byte = 0;
        EXPECT_EQ(::recvfrom(udp.value, &byte, 1, 0, nullptr, nullptr), 1);
        ++attackerCalls;
        attackerIteration = loop.iteration();
        EXPECT_TRUE(loop.eventHandling()) << "precondition: the release must happen during I/O dispatch";
        victim.reset();
        victimExpiredInAttacker = victimWeak.expired();
        // 同一轮里立刻建一条新连接:旧连接的 socket 要留到 pending 阶段才关,新 socket 不会撞上旧的 Poller 注册。
        replacement = hiredis::Hiredis::Create(&loop, muduo::net::InetAddress("127.0.0.1", port), maintenance);
        replacement->setConnectCallback([&](hiredis::Hiredis* me, int status) {
            EXPECT_EQ(status, REDIS_OK);
            ++replacementConnects;
            EXPECT_EQ(me->command([&](hiredis::Hiredis*, redisReply* reply) {
                if (reply != nullptr)
                {
                    ++replacementReplies;
                }
                loop.queueInLoop([&loop] { loop.quit(); });
            }, "PING"), REDIS_OK);
        });
        replacement->connect();
        loop.queueInLoop([&loop] { loop.quit(); });
    });

    // 先让受害连接可读,再让前置通道可读;loop 此刻没在跑,下一次 WSAPoll 会在同一批里同时报告二者。
    ASSERT_TRUE(SendAll(victimPeer.value, "+PONG\r\n"));
    const char trigger = 'x';
    ASSERT_EQ(::sendto(udp.value, &trigger, 1, 0, reinterpret_cast<const sockaddr*>(&udpSelf), sizeof(udpSelf)), 1);
    ASSERT_TRUE(WaitReadable(udp.value)) << "precondition: the earlier channel must be readable before polling";
    ASSERT_TRUE(RunUntilQuit(loop)) << "the batch never completed";

    EXPECT_EQ(attackerCalls, 1);
    EXPECT_TRUE(victimExpiredInAttacker) << "after the last owner is gone the weak callbacks must no longer lock it";
    EXPECT_EQ(nonNullReplies, 0) << "the victim's ready reply must not be dispatched after its owner released it earlier in the batch";
    EXPECT_EQ(nullReplies, 1) << "the pending command must complete exactly once, with a null reply, on destruction";
    EXPECT_TRUE(nullReplyInPending) << "destruction must be deferred until the I/O dispatch of the batch is over";
    EXPECT_EQ(nullReplyIteration, attackerIteration) << "the deferred destruction must run in the same iteration's pending phase";
    EXPECT_TRUE(commandCapture.expired()) << "the pending command callback and its captures must be released";

    // 新连接必须真的完成一条命令,而不只是「没崩」:证明同批建连没有污染 Poller 的 fd 注册。
    SocketLease replacementPeer;
    ASSERT_TRUE(AcceptPeer(listener, replacementPeer));
    ASSERT_TRUE(SendAll(replacementPeer.value, "+PONG\r\n"));
    EXPECT_TRUE(RunUntilQuit(loop)) << "the replacement connection never completed its PING";
    EXPECT_EQ(replacementConnects, 1);
    EXPECT_EQ(replacementReplies, 1);

    // loop 外同步释放替代连接并排空 Channel 保活任务,再核对整批连接对象、排队的 Channel 引用和两条 PING 都已了结。
    // 提前返回的路径由 releaseBeforeCaptures 收尾,它在这里之后再跑一遍也只是空操作。
    ReleaseConnection(replacement);
    DrainPending(loop);
    maintenance->Close();
    EXPECT_TRUE(maintenance->Joined())
        << "every wrapper and every queued Channel keep-alive must be released, and both PINGs completed";
}

// ---------------------------------------------------------------------------
// 2. 对端半关闭(只 shutdown(SD_SEND),纯 FIN)。WSAPoll 可能只报 POLLHUP:断线必须经 Hiredis 的关闭回调被察觉,
//    挂起命令恰好以空应答结束一次。所有者在断线回调里放掉最后一个引用:回调持有的局部强引用(加 Channel::tie)
//    保住对象到本次事件分发返回,之后整个对象在 pending 阶段释放(回调与捕获最终都被释放)。
// ---------------------------------------------------------------------------

TEST_F(HiredisEventBatchWindows, PeerHalfCloseDisconnectsAndOwnerMayReleaseInDisconnectCallback)
{
    muduo::net::EventLoop loop;
    SocketLease listener;
    uint16_t port = 0;
    ASSERT_TRUE(ListenLoopback(listener, port));

    const MaintenanceStatePtr maintenance = std::make_shared<hiredis::Hiredis::MaintenanceState>();
    SocketLease peer;
    int connects = 0;
    auto redis = ConnectVictim(loop, port, maintenance, listener, peer, connects);
    ASSERT_EQ(connects, 1);
    ASSERT_TRUE(redis->connected());
    const std::weak_ptr<hiredis::Hiredis> redisWeak = redis;

    int nullReplies = 0;
    int nonNullReplies = 0;
    ASSERT_EQ(redis->command([&](hiredis::Hiredis*, redisReply* reply) {
        if (reply != nullptr)
        {
            ++nonNullReplies;
        }
        else
        {
            ++nullReplies;
        }
    }, "PING"), REDIS_OK);

    int disconnects = 0;
    int disconnectStatus = 12345;
    bool aliveAfterOwnerReleased = false;
    bool connectedAfterOwnerReleased = true;
    std::weak_ptr<int> disconnectCapture;
    {
        auto capture = std::make_shared<int>(0);
        disconnectCapture = capture;
        redis->setDisconnectCallback([&, capture](hiredis::Hiredis* me, int status) {
            ++disconnects;
            disconnectStatus = status;
            redis.reset();
            aliveAfterOwnerReleased = !redisWeak.expired();
            connectedAfterOwnerReleased = me->connected();
            EXPECT_FALSE(disconnectCapture.expired()) << "the running callback's captures must stay alive";
            loop.queueInLoop([&loop] { loop.quit(); });
        });
    }
    // 见 ScopeExit 注释:声明在全部被回调捕获的局部变量之后;正常路径上 redis 已在断线回调里释放,只剩排空。
    const ScopeExit releaseBeforeCaptures{[&] {
        ReleaseConnection(redis);
        DrainPending(loop);
    }};

    ASSERT_EQ(::shutdown(peer.value, SD_SEND), 0);
    ASSERT_TRUE(RunUntilQuit(loop)) << "a pure FIN from the peer was never noticed (POLLHUP-only path)";

    EXPECT_EQ(disconnects, 1);
    EXPECT_EQ(disconnectStatus, REDIS_ERR) << "EOF is an error disconnect, not an explicit one";
    EXPECT_EQ(nullReplies, 1) << "the command pending at the half-close must complete exactly once, with a null reply";
    EXPECT_EQ(nonNullReplies, 0);
    EXPECT_TRUE(aliveAfterOwnerReleased)
        << "the callback's locked self (plus Channel::tie) must keep the connection alive until the event returns";
    EXPECT_FALSE(connectedAfterOwnerReleased);

    DrainPending(loop);
    EXPECT_TRUE(redisWeak.expired());
    EXPECT_TRUE(disconnectCapture.expired()) << "the deferred destruction must release the stored callbacks";
    maintenance->Close();
    EXPECT_TRUE(maintenance->Joined())
        << "the wrapper and every queued Channel keep-alive must be released, and the PING completed";
}

#endif // _WIN32
