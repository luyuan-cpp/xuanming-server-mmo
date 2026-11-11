#include <gtest/gtest.h>

#include "muduo/contrib/hiredis/Hiredis.h"
#include "muduo/net/Channel.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"

#include <arpa/inet.h>
#include <dirent.h>
#include <poll.h>
#include <sys/eventfd.h>
#include <sys/socket.h>
#include <unistd.h>

#include <cerrno>
#include <charconv>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <memory>
#include <optional>
#include <string>
#include <string_view>

namespace
{
using Clock = std::chrono::steady_clock;
using namespace std::chrono_literals;

class OwnedFd
{
public:
    explicit OwnedFd(int value) : value_(value) {}
    ~OwnedFd() { if (value_ >= 0) ::close(value_); }
    OwnedFd(const OwnedFd&) = delete;
    OwnedFd& operator=(const OwnedFd&) = delete;
    int Get() const { return value_; }
private:
    int value_;
};

bool WaitForFd(int fd, short events)
{
    const auto deadline = Clock::now() + 1s;
    while (Clock::now() < deadline)
    {
        const auto remaining = std::chrono::ceil<std::chrono::milliseconds>(deadline - Clock::now());
        pollfd item{fd, events, 0};
        const int result = ::poll(&item, 1, static_cast<int>(remaining.count()));
        if (result > 0) return (item.revents & events) != 0;
        if (result < 0 && errno != EINTR) return false;
    }
    return false;
}

std::unique_ptr<muduo::net::EventLoop> MakeOrderedLoop()
{
    const char* previous = ::getenv("MUDUO_USE_POLL");
    const std::optional<std::string> saved = previous ? std::optional<std::string>(previous) : std::nullopt;
    if (::setenv("MUDUO_USE_POLL", "1", 1) != 0) return {};
    auto result = std::make_unique<muduo::net::EventLoop>();
    if (saved) ::setenv("MUDUO_USE_POLL", saved->c_str(), 1);
    else ::unsetenv("MUDUO_USE_POLL");
    return result;
}

// IPv4 本地端口;fd 已关闭、调用失败或不是 IPv4 socket 时返回 0。fd 编号被关掉后又被新连接复用时,
// 本地端口会变成新连接的临时端口,所以能区分「还是原来那条连接」和「编号被复用」。
std::uint16_t LocalPort(int fd)
{
    sockaddr_in local{};
    socklen_t size = sizeof(local);
    if (::getsockname(fd, reinterpret_cast<sockaddr*>(&local), &size) != 0 || local.sin_family != AF_INET)
    {
        return 0;
    }
    return ntohs(local.sin_port);
}

// 使用公开的 socket 边界确认旧 fd 在当前批次里仍然占用，不依赖 Hiredis 的私有成员。
int FindConnectedSocket(std::uint16_t port)
{
    struct CloseDirectory { void operator()(DIR* value) const { ::closedir(value); } };
    const std::unique_ptr<DIR, CloseDirectory> directory(::opendir("/proc/self/fd"));
    if (!directory) return -1;
    while (const dirent* entry = ::readdir(directory.get()))
    {
        const std::string_view text(entry->d_name);
        int fd = -1;
        const auto parsed = std::from_chars(text.data(), text.data() + text.size(), fd);
        if (parsed.ec != std::errc() || parsed.ptr != text.data() + text.size()) continue;
        sockaddr_in peer{};
        socklen_t size = sizeof(peer);
        if (::getpeername(fd, reinterpret_cast<sockaddr*>(&peer), &size) == 0 &&
            peer.sin_family == AF_INET && ntohs(peer.sin_port) == port) return fd;
    }
    return -1;
}

TEST(HiredisEventBatch, OtherChannelReleasesReadyVictimAndCreatesReplacementWithoutReusingItsFd)
{
    const auto loopOwner = MakeOrderedLoop();
    ASSERT_NE(loopOwner, nullptr);
    auto& loop = *loopOwner;
    const OwnedFd listener(::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC | SOCK_NONBLOCK, 0));
    ASSERT_GE(listener.Get(), 0);
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    ASSERT_EQ(::bind(listener.Get(), reinterpret_cast<sockaddr*>(&address), sizeof(address)), 0);
    ASSERT_EQ(::listen(listener.Get(), 4), 0);
    socklen_t addressSize = sizeof(address);
    ASSERT_EQ(::getsockname(listener.Get(), reinterpret_cast<sockaddr*>(&address), &addressSize), 0);
    const std::uint16_t port = ntohs(address.sin_port);
    const muduo::net::InetAddress serverAddress("127.0.0.1", port);

    // PollPoller 按注册顺序收集 activeChannels；这个前置通道必须早于 victim 注册。
    const OwnedFd attackerFd(::eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC));
    ASSERT_GE(attackerFd.Get(), 0);
    muduo::net::Channel attacker(&loop, attackerFd.Get());
    attacker.enableReading();
    auto victim = hiredis::Hiredis::Create(&loop, serverAddress);
    std::shared_ptr<hiredis::Hiredis> replacement;
    std::weak_ptr<hiredis::Hiredis> victimWeak = victim;
    int connectCalls = 0;
    bool watchdogExpired = false;
    victim->setConnectCallback([&](hiredis::Hiredis*, int status) {
        EXPECT_EQ(status, REDIS_OK);
        ++connectCalls;
        loop.queueInLoop([&] { loop.quit(); });
    });
    victim->connect();
    ASSERT_TRUE(WaitForFd(listener.Get(), POLLIN));
    const OwnedFd originalPeer(::accept4(listener.Get(), nullptr, nullptr, SOCK_CLOEXEC | SOCK_NONBLOCK));
    ASSERT_GE(originalPeer.Get(), 0);
    auto watchdog = loop.runAfter(2, [&] { watchdogExpired = true; loop.quit(); });
    loop.loop();
    loop.cancel(watchdog);
    ASSERT_EQ(connectCalls, 1);
    ASSERT_FALSE(watchdogExpired);
    const int victimFd = FindConnectedSocket(port);
    ASSERT_GE(victimFd, 0);
    const std::uint16_t victimLocalPort = LocalPort(victimFd);
    ASSERT_NE(victimLocalPort, 0);

    int nullReplies = 0;
    int nonNullReplies = 0;
    int attackerCalls = 0;
    int replacementConnects = 0;
    int replacementReplies = 0;
    bool victimExpiredInAttacker = false;
    bool oldFdHeldThroughBatch = false;
    bool replacementUsesDifferentFd = false;
    std::int64_t attackerIteration = -1;
    std::int64_t nullReplyIteration = -1;
    bool nullReplyInPending = false;
    std::weak_ptr<int> callbackCapture;
    {
        auto capture = std::make_shared<int>(0);
        callbackCapture = capture;
        ASSERT_EQ(victim->command([&, capture](hiredis::Hiredis*, redisReply* reply) {
            if (reply) ++nonNullReplies;
            else
            {
                ++nullReplies;
                nullReplyIteration = loop.iteration();
                nullReplyInPending = !loop.eventHandling();
            }
        }, "PING"), REDIS_OK);
    }
    attacker.setReadCallback([&](muduo::Timestamp) {
        std::uint64_t value = 0;
        EXPECT_EQ(::read(attackerFd.Get(), &value, sizeof(value)), sizeof(value));
        ++attackerCalls;
        attackerIteration = loop.iteration();
        victim.reset();
        victimExpiredInAttacker = victimWeak.expired();
        sockaddr_in peer{};
        socklen_t peerSize = sizeof(peer);
        oldFdHeldThroughBatch = ::getpeername(victimFd, reinterpret_cast<sockaddr*>(&peer), &peerSize) == 0;
        replacement = hiredis::Hiredis::Create(&loop, serverAddress);
        replacement->setConnectCallback([&](hiredis::Hiredis* me, int status) {
            EXPECT_EQ(status, REDIS_OK);
            ++replacementConnects;
            EXPECT_EQ(me->command([&](hiredis::Hiredis*, redisReply* reply) {
                if (reply) ++replacementReplies;
                loop.queueInLoop([&] { loop.quit(); });
            }, "PING"), REDIS_OK);
        });
        replacement->connect();
        // 旧 fd 仍指向原连接（本地端口没变），新的客户端不能借用该编号；旧 Poller 注册因而不会撞新连接。
        // 只看 getpeername 分不出来：新连接连的是同一个端口，复用了旧编号同样取得到对端。
        replacementUsesDifferentFd = oldFdHeldThroughBatch && LocalPort(victimFd) == victimLocalPort;
        loop.queueInLoop([&] { loop.quit(); });
    });
    const std::uint64_t ready = 1;
    ASSERT_EQ(::write(attackerFd.Get(), &ready, sizeof(ready)), sizeof(ready));
    ASSERT_EQ(::send(originalPeer.Get(), "+PONG\r\n", 7, MSG_NOSIGNAL), 7);
    ASSERT_TRUE(WaitForFd(victimFd, POLLIN)) << "victim 必须在下一轮 poll 前真正可读";
    watchdog = loop.runAfter(2, [&] { watchdogExpired = true; loop.quit(); });
    loop.loop();
    loop.cancel(watchdog);

    EXPECT_EQ(attackerCalls, 1);
    EXPECT_TRUE(victimExpiredInAttacker) << "外部 owner 撤掉后 weak 必须立即失效，同批旧 Channel 不得锁回对象";
    EXPECT_TRUE(oldFdHeldThroughBatch) << "同批 I/O 分发结束前必须保留 context/socket";
    EXPECT_TRUE(replacementUsesDifferentFd);
    EXPECT_EQ(nonNullReplies, 0) << "victim 虽然已有应答可读，同批较早撤销 owner 后不得再分发旧应答";
    EXPECT_EQ(nullReplies, 1) << "析构必须使挂起命令恰好空回复一次";
    EXPECT_TRUE(nullReplyInPending) << "context 释放必须延至本批 I/O 分发之后";
    EXPECT_EQ(nullReplyIteration, attackerIteration) << "释放应在当前批次的 pending 阶段完成";
    EXPECT_TRUE(callbackCapture.expired()) << "挂起命令和捕获都应清理完毕";

    ASSERT_TRUE(WaitForFd(listener.Get(), POLLIN));
    const OwnedFd replacementPeer(::accept4(listener.Get(), nullptr, nullptr, SOCK_CLOEXEC | SOCK_NONBLOCK));
    ASSERT_GE(replacementPeer.Get(), 0);
    // 新连接必须真的完成一次命令，不能只通过没有崩溃来判断 fd 注册未被污染。
    ASSERT_EQ(::send(replacementPeer.Get(), "+PONG\r\n", 7, MSG_NOSIGNAL), 7);
    watchdog = loop.runAfter(2, [&] { watchdogExpired = true; loop.quit(); });
    loop.loop();
    loop.cancel(watchdog);
    EXPECT_EQ(replacementConnects, 1);
    EXPECT_EQ(replacementReplies, 1);
    EXPECT_FALSE(watchdogExpired);

    replacement.reset();
    victim.reset();
    attacker.disableAll();
    attacker.remove();
    // removeChannel 的 Channel 保活任务也要在退出前排空。
    loop.queueInLoop([&] { loop.quit(); });
    loop.runAfter(0, [] {});
    loop.loop();
}

// 同一轮里先请求断开、后有事件:较早的 eventfd 通道对一条没有在途命令、本轮已有 EOF 可读的连接调 disconnect()。
// 修复前 hiredis 当场拆除(没有在途命令就不等),在前置通道的回调里注销它的 Channel,Debug 撞
// EventLoop::removeChannel 的断言;修复后推迟到本轮 pending:EOF 照常分发,断线回调恰好一次、是出错断线,
// 推迟的那次断开发现连接已断,什么也不做。
TEST(HiredisEventBatch, OtherChannelDisconnectsIdleVictimWithReadyEofInSameBatch)
{
    const auto loopOwner = MakeOrderedLoop();
    ASSERT_NE(loopOwner, nullptr);
    auto& loop = *loopOwner;
    const OwnedFd listener(::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC | SOCK_NONBLOCK, 0));
    ASSERT_GE(listener.Get(), 0);
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    ASSERT_EQ(::bind(listener.Get(), reinterpret_cast<sockaddr*>(&address), sizeof(address)), 0);
    ASSERT_EQ(::listen(listener.Get(), 4), 0);
    socklen_t addressSize = sizeof(address);
    ASSERT_EQ(::getsockname(listener.Get(), reinterpret_cast<sockaddr*>(&address), &addressSize), 0);
    const std::uint16_t port = ntohs(address.sin_port);

    // PollPoller 按注册顺序收集 activeChannels；这个前置通道必须早于 victim 注册。
    const OwnedFd attackerFd(::eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC));
    ASSERT_GE(attackerFd.Get(), 0);
    muduo::net::Channel attacker(&loop, attackerFd.Get());
    attacker.enableReading();
    // 回调以引用捕获的计数都声明在 victim 之前:用例中途失败提前返回时,victim 析构触发的回调不会写到已析构的变量。
    int connectCalls = 0;
    int attackerCalls = 0;
    int disconnects = 0;
    int disconnectStatus = 12345;
    bool disconnectDuringDispatch = false;
    bool connectedAfterRequest = false;
    bool watchdogExpired = false;
    auto victim = hiredis::Hiredis::Create(&loop, muduo::net::InetAddress("127.0.0.1", port));
    victim->setConnectCallback([&](hiredis::Hiredis*, int status) {
        EXPECT_EQ(status, REDIS_OK);
        ++connectCalls;
        loop.queueInLoop([&] { loop.quit(); });
    });
    victim->connect();
    ASSERT_TRUE(WaitForFd(listener.Get(), POLLIN));
    const OwnedFd peer(::accept4(listener.Get(), nullptr, nullptr, SOCK_CLOEXEC | SOCK_NONBLOCK));
    ASSERT_GE(peer.Get(), 0);
    auto watchdog = loop.runAfter(2, [&] { watchdogExpired = true; loop.quit(); });
    loop.loop();
    loop.cancel(watchdog);
    ASSERT_EQ(connectCalls, 1);
    ASSERT_FALSE(watchdogExpired);
    const int victimFd = victim->fd();

    victim->setDisconnectCallback([&](hiredis::Hiredis*, int status) {
        ++disconnects;
        disconnectStatus = status;
        disconnectDuringDispatch = loop.eventHandling();
        loop.queueInLoop([&] { loop.quit(); });
    });
    attacker.setReadCallback([&](muduo::Timestamp) {
        std::uint64_t value = 0;
        EXPECT_EQ(::read(attackerFd.Get(), &value, sizeof(value)), sizeof(value));
        ++attackerCalls;
        victim->disconnect();
        connectedAfterRequest = victim->connected();
    });
    // 对端只关写方向:victim 有 EOF 可读,它又没有在途命令,修复前的 disconnect() 会当场拆除。
    ASSERT_EQ(::shutdown(peer.Get(), SHUT_WR), 0);
    ASSERT_TRUE(WaitForFd(victimFd, POLLIN)) << "precondition: the victim's EOF must be readable before polling";
    const std::uint64_t ready = 1;
    ASSERT_EQ(::write(attackerFd.Get(), &ready, sizeof(ready)), sizeof(ready));
    watchdog = loop.runAfter(2, [&] { watchdogExpired = true; loop.quit(); });
    loop.loop();
    loop.cancel(watchdog);

    EXPECT_FALSE(watchdogExpired);
    EXPECT_EQ(attackerCalls, 1);
    EXPECT_TRUE(connectedAfterRequest)
        << "a disconnect requested during I/O dispatch must not tear the connection down on the spot";
    EXPECT_EQ(disconnects, 1);
    EXPECT_EQ(disconnectStatus, REDIS_ERR) << "the victim's own EOF in the same batch must be dispatched first";
    EXPECT_TRUE(disconnectDuringDispatch) << "the EOF disconnect must come from the victim's own event dispatch";

    victim.reset();
    attacker.disableAll();
    attacker.remove();
    // 推迟的断开与 removeChannel 的 Channel 保活任务都要在退出前排空。
    loop.queueInLoop([&] { loop.quit(); });
    loop.runAfter(0, [] {});
    loop.loop();
    EXPECT_EQ(disconnects, 1) << "the deferred disconnect must find the connection gone and do nothing";
}
} // namespace
