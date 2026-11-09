#include <gtest/gtest.h>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "thread_context/redis_manager.h"

#include <arpa/inet.h>
#include <dirent.h>
#include <poll.h>
#include <sys/socket.h>
#include <unistd.h>

#include <array>
#include <atomic>
#include <cerrno>
#include <charconv>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <functional>
#include <future>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <utility>

namespace
{
using namespace std::chrono_literals;
using Clock = std::chrono::steady_clock;

// 仅用于本地假服务端的测试数据，任何失败信息和日志都不输出命令正文。
constexpr char kFakePassword[] = "event-batch-test-password";
constexpr std::string_view kPing = "*1\r\n$4\r\nPING\r\n";
constexpr double kHandshakeDeadlineSec = 0.1;

class ScopedFd
{
public:
    explicit ScopedFd(int fd = -1) : fd_(fd) {}
    ~ScopedFd() { if (fd_ >= 0) ::close(fd_); }
    ScopedFd(const ScopedFd&) = delete;
    ScopedFd& operator=(const ScopedFd&) = delete;
    int Get() const { return fd_; }
private:
    int fd_;
};

bool EventsBefore(int fd, short events, Clock::time_point deadline)
{
    for (;;)
    {
        const auto now = Clock::now();
        if (now >= deadline) return false;
        const auto remaining = std::chrono::ceil<std::chrono::milliseconds>(deadline - now);
        pollfd item{fd, events, 0};
        const int result = ::poll(&item, 1, static_cast<int>(remaining.count()));
        if (result > 0) return (item.revents & events) != 0;
        if (result < 0 && errno != EINTR) return false;
    }
}

bool ReadableBefore(int fd, Clock::time_point deadline)
{
    return EventsBefore(fd, POLLIN, deadline);
}

bool SendBefore(int fd, std::string_view bytes, Clock::time_point deadline)
{
    while (!bytes.empty() && Clock::now() < deadline)
    {
        const ssize_t count = ::send(fd, bytes.data(), bytes.size(), MSG_NOSIGNAL | MSG_DONTWAIT);
        if (count > 0)
        {
            bytes.remove_prefix(static_cast<std::size_t>(count));
            continue;
        }
        if (count < 0 && errno != EINTR && errno != EAGAIN && errno != EWOULDBLOCK) return false;
        pollfd item{fd, POLLOUT, 0};
        ::poll(&item, 1, 5);
    }
    return bytes.empty();
}

// 通过 Linux 的文件描述符边界找到真实 timerfd 和本次客户端 socket，不窥探 Hiredis 的私有状态。
struct BatchFds
{
    int timer = -1;
    int client = -1;
    int timerCount = 0;
    int clientCount = 0;
};

struct CloseDirectory
{
    void operator()(DIR* directory) const { ::closedir(directory); }
};

BatchFds FindBatchFds(std::uint16_t serverPort)
{
    BatchFds result;
    const std::unique_ptr<DIR, CloseDirectory> directory(::opendir("/proc/self/fd"));
    if (!directory) return result;
    while (const dirent* entry = ::readdir(directory.get()))
    {
        const std::string_view name(entry->d_name);
        int fd = -1;
        const auto parsed = std::from_chars(name.data(), name.data() + name.size(), fd);
        if (parsed.ec != std::errc() || parsed.ptr != name.data() + name.size()) continue;
        const std::string path = "/proc/self/fd/" + std::string(name);
        std::array<char, 128> target{};
        const ssize_t length = ::readlink(path.c_str(), target.data(), target.size());
        if (length > 0 && std::string_view(target.data(), static_cast<std::size_t>(length)) == "anon_inode:[timerfd]")
        {
            result.timer = fd;
            ++result.timerCount;
        }
        sockaddr_in peer{};
        socklen_t size = sizeof(peer);
        if (::getpeername(fd, reinterpret_cast<sockaddr*>(&peer), &size) == 0 &&
            peer.sin_family == AF_INET && ntohs(peer.sin_port) == serverPort)
        {
            result.client = fd;
            ++result.clientCount;
        }
    }
    return result;
}

struct BatchObservation
{
    bool prepared = false;
    bool timerReady = false;
    bool socketReady = false;
    bool preparationFailed = false;
    bool watchdogExpired = false;
    bool passwordInLog = false;
    std::int64_t preparedIteration = -1;
    std::int64_t timerIteration = -1;
    std::int64_t promotionIteration = -1;
    std::int64_t connectedIteration = -1;
    int promotions = 0;
    int reconnectAttempts = 0;
    int pingReplies = 0;
    int pingSubmitResult = REDIS_ERR;
    bool pingWasPong = false;
    bool originalConnectionHeld = false;
    std::function<void()> onConnectFailed;
};

// 本程序仅运行这一组串行用例；日志和观测都由 loop 所在线程访问，后台假服务端不写 muduo 日志。
BatchObservation* gObservation = nullptr;
muduo::net::EventLoop* gObservedLoop = nullptr;

void ObserveLog(const char* message, int size)
{
    const std::string_view line(message, static_cast<std::size_t>(size));
    if (gObservation)
    {
        if (line.find(kFakePassword) != std::string_view::npos) gObservation->passwordInLog = true;
        if (line.find("Attempting Redis reconnect") != std::string_view::npos) ++gObservation->reconnectAttempts;
        if (line.find("Redis connect attempt failed") != std::string_view::npos && gObservation->onConnectFailed)
            gObservation->onConnectFailed();
        if (gObservation->prepared && line.find("Redis connected to") != std::string_view::npos)
            gObservation->connectedIteration = gObservedLoop->iteration();
        if (gObservation->prepared && gObservation->timerIteration < 0 &&
            line.find("TimerQueue::handleRead()") != std::string_view::npos)
        {
            gObservation->timerIteration = gObservedLoop->iteration();
        }
    }
    // 保留断言前的生产日志作为红态证据，测试口令即使意外泄漏也不继续输出。
    if (line.find(kFakePassword) == std::string_view::npos)
    {
        std::fwrite(message, 1, static_cast<std::size_t>(size), stdout);
        std::fflush(stdout);
    }
}

void StdoutLog(const char* message, int size)
{
    std::fwrite(message, 1, static_cast<std::size_t>(size), stdout);
}

class ScopedLogObservation
{
public:
    ScopedLogObservation(BatchObservation& observation, muduo::net::EventLoop& loop)
        : oldLevel_(muduo::Logger::logLevel())
    {
        gObservation = &observation;
        gObservedLoop = &loop;
        muduo::Logger::setLogLevel(muduo::Logger::TRACE);
        muduo::Logger::setOutput(ObserveLog);
    }
    ~ScopedLogObservation()
    {
        muduo::Logger::setOutput(StdoutLog);
        muduo::Logger::setLogLevel(oldLevel_);
        gObservation = nullptr;
        gObservedLoop = nullptr;
    }
private:
    muduo::Logger::LogLevel oldLevel_;
};

class FakeRedis : public std::enable_shared_from_this<FakeRedis>
{
public:
    FakeRedis(muduo::net::EventLoop& loop, BatchObservation& observation, bool listenNow = true)
        : loop_(loop), observation_(observation),
          listener_(::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC | SOCK_NONBLOCK, 0))
    {
        sockaddr_in address{};
        address.sin_family = AF_INET;
        address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        if (listener_.Get() < 0 || ::bind(listener_.Get(), reinterpret_cast<sockaddr*>(&address), sizeof(address)) != 0 ||
            (listenNow && !Listen())) return;
        socklen_t size = sizeof(address);
        if (::getsockname(listener_.Get(), reinterpret_cast<sockaddr*>(&address), &size) == 0)
            port_ = ntohs(address.sin_port);
    }
    ~FakeRedis() { Stop(); }

    void Start() { worker_ = std::thread([this] { Serve(); }); }
    void Stop()
    {
        stopping_ = true;
        if (worker_.joinable()) worker_.join();
    }
    std::uint16_t Port() const { return port_; }
    int Accepted() const { return accepted_.load(); }
    int AuthCommands() const { return authCommands_.load(); }
    bool WorkerFailed() const { return workerFailed_.load(); }
    bool Listen() { return ::listen(listener_.Get(), 4) == 0; }
    bool WaitAccepted() { return acceptedFuture_.wait_for(1s) == std::future_status::ready; }
    void SetImmediateAuth() { immediateAuth_ = true; }
    void SetDeadlineReadyAction(std::function<void()> action) { deadlineReadyAction_ = std::move(action); }

private:
    void Serve()
    {
        const auto deadline = Clock::now() + 2500ms;
        if (!ReadableBefore(listener_.Get(), deadline)) { workerFailed_ = true; return; }
        acceptedSocket_ = std::make_unique<ScopedFd>(::accept4(listener_.Get(), nullptr, nullptr, SOCK_CLOEXEC | SOCK_NONBLOCK));
        const int accepted = acceptedSocket_->Get();
        if (accepted < 0) { workerFailed_ = true; return; }
        ++accepted_;
        acceptedSignal_.set_value();
        const std::string expected = "*2\r\n$4\r\nAUTH\r\n$" + std::to_string(sizeof(kFakePassword) - 1) +
            "\r\n" + kFakePassword + "\r\n";
        std::string request;
        while (request.size() < expected.size() && ReadableBefore(accepted, deadline))
        {
            std::array<char, 256> bytes{};
            const ssize_t count = ::recv(accepted, bytes.data(), bytes.size(), 0);
            if (count <= 0) { workerFailed_ = true; return; }
            request.append(bytes.data(), static_cast<std::size_t>(count));
        }
        if (request != expected) { workerFailed_ = true; return; }
        ++authCommands_;
        if (immediateAuth_)
        {
            if (!SendBefore(accepted, "+OK\r\n", deadline)) { workerFailed_ = true; return; }
        }
        else
        {
            loop_.queueInLoop([weak = weak_from_this()]
            {
                if (const auto self = weak.lock()) self->PrepareBatch();
            });
        }
        request.clear();
        while (!stopping_ && Clock::now() < deadline)
        {
            pollfd items[]{{listener_.Get(), POLLIN, 0}, {accepted, POLLIN, 0}};
            if (::poll(items, 2, 10) < 0 && errno != EINTR) { workerFailed_ = true; return; }
            if ((items[0].revents & POLLIN) != 0)
            {
                const ScopedFd unexpected(::accept4(listener_.Get(), nullptr, nullptr, SOCK_CLOEXEC | SOCK_NONBLOCK));
                if (unexpected.Get() >= 0) ++accepted_;
            }
            if ((items[1].revents & POLLIN) == 0) continue;
            std::array<char, 256> bytes{};
            const ssize_t count = ::recv(accepted, bytes.data(), bytes.size(), 0);
            if (count <= 0) return;
            request.append(bytes.data(), static_cast<std::size_t>(count));
            if (request.size() < kPing.size()) continue;
            if (request != kPing || !SendBefore(accepted, "+PONG\r\n", deadline))
            {
                workerFailed_ = true;
                return;
            }
            request.clear();
        }
        // 即使后台线程超时或收到 EOF，socket 仍由 FakeRedis 成员持有至析构；排队准备任务不会拿到已关闭的 fd。
    }

    void PrepareBatch()
    {
        const BatchFds fds = FindBatchFds(port_);
        if (fds.timerCount != 1 || fds.clientCount != 1)
        {
            observation_.preparationFailed = true;
            return;
        }
        // 在 pending 阶段有界等待真正的握手截止 timerfd；不读取它，让下一轮 poll 同时看到两个 POLLIN。
        observation_.timerReady = ReadableBefore(fds.timer, Clock::now() + 1s);
        if (observation_.timerReady && deadlineReadyAction_)
        {
            observation_.preparedIteration = loop_.iteration();
            observation_.prepared = true;
            deadlineReadyAction_();
            return;
        }
        if (!observation_.timerReady || !SendBefore(acceptedSocket_->Get(), "+OK\r\n", Clock::now() + 500ms))
        {
            observation_.preparationFailed = true;
            return;
        }
        observation_.socketReady = ReadableBefore(fds.client, Clock::now() + 500ms);
        observation_.preparationFailed = !observation_.socketReady;
        observation_.preparedIteration = loop_.iteration();
        observation_.prepared = !observation_.preparationFailed;
    }

    muduo::net::EventLoop& loop_;
    BatchObservation& observation_;
    ScopedFd listener_;
    std::uint16_t port_ = 0;
    // accept 写入先于入队锁及 promise 的通知；准备任务读取时已有同步关系，Stop 后也继续持有 fd。
    std::unique_ptr<ScopedFd> acceptedSocket_;
    std::promise<void> acceptedSignal_;
    std::future<void> acceptedFuture_{acceptedSignal_.get_future()};
    bool immediateAuth_ = false;
    std::function<void()> deadlineReadyAction_;
    std::thread worker_;
    std::atomic<bool> stopping_{false};
    std::atomic<bool> workerFailed_{false};
    std::atomic<int> accepted_{0};
    std::atomic<int> authCommands_{0};
};

std::unique_ptr<muduo::net::EventLoop> MakePollLoop()
{
    const char* previous = ::getenv("MUDUO_USE_POLL");
    const std::optional<std::string> previousPoll = previous ? std::optional<std::string>(previous) : std::nullopt;
    if (::setenv("MUDUO_USE_POLL", "1", 1) != 0) return {};
    auto loop = std::make_unique<muduo::net::EventLoop>();
    if (previousPoll) ::setenv("MUDUO_USE_POLL", previousPoll->c_str(), 1);
    else ::unsetenv("MUDUO_USE_POLL");
    return loop;
}

void DrainPending(muduo::net::EventLoop& loop)
{
    loop.runAfter(0.0001, [&] { loop.queueInLoop([&] { loop.quit(); }); });
    loop.loop();
}

TEST(RedisManagerEventBatch, AuthSuccessAfterDeadlineTickInSameBatchKeepsOriginalConnection)
{
    // PollPoller 按注册顺序分发，EventLoop 构造时 timerfd 先于 Hiredis socket 注册。
    const char* previous = ::getenv("MUDUO_USE_POLL");
    const std::optional<std::string> previousPoll = previous ? std::optional<std::string>(previous) : std::nullopt;
    ASSERT_EQ(::setenv("MUDUO_USE_POLL", "1", 1), 0) << "无法选择确定事件顺序的 poll 后端";
    muduo::net::EventLoop loop;
    if (previousPoll) ::setenv("MUDUO_USE_POLL", previousPoll->c_str(), 1);
    else ::unsetenv("MUDUO_USE_POLL");

    BatchObservation observation;
    ScopedLogObservation logs(observation, loop);
    RedisManager manager;
    const auto server = std::make_shared<FakeRedis>(loop, observation);
    ASSERT_NE(server->Port(), 0) << "本地假 Redis 无法监听回环临时端口";
    server->Start();

    manager.SetReconnectCallback([&]
    {
        ++observation.promotions;
        observation.promotionIteration = loop.iteration();
        const auto original = manager.GetConnection().get();
        observation.pingSubmitResult = original->command(
            [&, original](hiredis::Hiredis* connection, redisReply* reply)
            {
                if (!reply) return;
                ++observation.pingReplies;
                observation.pingWasPong = reply->type == REDIS_REPLY_STATUS && reply->str &&
                    std::string_view(reply->str, reply->len) == "PONG";
                observation.originalConnectionHeld = connection == original && manager.GetConnection().get() == original;
                // 再观察一个以上重连周期，确认旧 tick 没有在稍后拆掉已认证连接。
                loop.runAfter(0.15, [&] { loop.queueInLoop([&] { loop.quit(); }); });
            }, "PING");
    });
    RedisConnectOptions options;
    options.password = kFakePassword;
    options.reconnectIntervalSec = kHandshakeDeadlineSec;
    options.livenessProbeIntervalSec = 2.0;
    options.livenessTimeoutSec = 2.0;
    manager.Connect(&loop, muduo::net::InetAddress("127.0.0.1", server->Port()), std::move(options));
    loop.runAfter(2.5, [&]
    {
        observation.watchdogExpired = true;
        loop.queueInLoop([&] { loop.quit(); });
    });
    loop.loop();

    const bool stillConnected = manager.GetConnection() && manager.GetConnection()->connected();
    server->Stop();
    // loop 外安全释放，随后排空适配器持有 Channel 的 pending functor，最后才析构 EventLoop。
    manager.Shutdown();
    loop.runAfter(0.0001, [&] { loop.queueInLoop([&] { loop.quit(); }); });
    loop.loop();

    EXPECT_FALSE(observation.preparationFailed) << "未能准备 timerfd 和 AUTH 应答同时可读的事件批次";
    EXPECT_TRUE(observation.timerReady) << "真实握手截止 timerfd 必须已经到期";
    EXPECT_TRUE(observation.socketReady) << "下一轮 poll 前 AUTH 应答必须已经可读";
    EXPECT_FALSE(observation.watchdogExpired) << "握手及原连接上的 PING 必须在有界时间内完成";
    EXPECT_EQ(observation.timerIteration, observation.preparedIteration + 1) << "截止事件必须在准备后的下一批分发";
    EXPECT_EQ(observation.promotionIteration, observation.timerIteration) << "AUTH 成功必须紧随同一批中先执行的截止事件";
    EXPECT_EQ(observation.promotions, 1) << "同一条认证成功的连接只能移交一次";
    EXPECT_EQ(server->Accepted(), 1) << "同批到达的 AUTH 成功不得触发多余重建";
    EXPECT_EQ(server->AuthCommands(), 1) << "原连接上只能进行一次 AUTH";
    EXPECT_EQ(observation.pingSubmitResult, REDIS_OK) << "移交后的原连接必须可提交命令";
    EXPECT_EQ(observation.pingReplies, 1) << "原连接上的业务命令必须恰好完成一次";
    EXPECT_TRUE(observation.pingWasPong) << "原连接必须返回正常 PONG";
    EXPECT_TRUE(observation.originalConnectionHeld) << "业务回包必须来自原先移交的连接";
    EXPECT_TRUE(stillConnected) << "观察窗口结束时原连接必须仍然可用";
    EXPECT_FALSE(server->WorkerFailed()) << "假 Redis 的有界协议交互失败";
    EXPECT_FALSE(observation.passwordInLog) << "测试口令不得出现在 muduo 日志中";
}

TEST(RedisManagerEventBatch, QueuedDeadlineTaskDoesNotAccessManagerDestroyedEarlierInPendingBatch)
{
    const auto loopOwner = MakePollLoop();
    ASSERT_NE(loopOwner, nullptr) << "无法选择确定事件顺序的 poll 后端";
    auto& loop = *loopOwner;
    BatchObservation observation;
    ScopedLogObservation logs(observation, loop);
    auto manager = std::make_unique<RedisManager>();
    const auto server = std::make_shared<FakeRedis>(loop, observation);
    ASSERT_NE(server->Port(), 0) << "本地假 Redis 无法监听回环临时端口";
    int destructions = 0;
    std::int64_t shutdownIteration = -1;
    server->SetDeadlineReadyAction([&]
    {
        // 当前处在前一轮 pending 中。先排 shutdown，下一轮 timerfd 再把旧重建任务排在它之后。
        loop.queueInLoop([&]
        {
            shutdownIteration = loop.iteration();
            manager->Shutdown();
            manager.reset();
            ++destructions;
            // quit 留到下一轮，使同一 pending 快照里的旧重建闭包确实有机会执行其失效判断。
            loop.queueInLoop([&] { loop.quit(); });
        });
    });
    server->Start();
    manager->SetReconnectCallback([&] { ++observation.promotions; });
    RedisConnectOptions options;
    options.password = kFakePassword;
    options.reconnectIntervalSec = kHandshakeDeadlineSec;
    options.livenessProbeIntervalSec = 2.0;
    options.livenessTimeoutSec = 2.0;
    manager->Connect(&loop, muduo::net::InetAddress("127.0.0.1", server->Port()), std::move(options));
    loop.runAfter(2.5, [&]
    {
        observation.watchdogExpired = true;
        loop.queueInLoop([&] { loop.quit(); });
    });
    loop.loop();
    server->Stop();
    if (manager)
    {
        manager->Shutdown();
        manager.reset();
    }
    DrainPending(loop);

    EXPECT_FALSE(observation.preparationFailed) << "必须成功准备真实握手截止事件";
    EXPECT_TRUE(observation.timerReady) << "预排停机前 timerfd 必须已经到期";
    EXPECT_FALSE(observation.watchdogExpired) << "停机及失效闭包必须在有界时间内完成";
    EXPECT_EQ(observation.timerIteration, observation.preparedIteration + 1) << "截止事件必须在准备后的下一批分发";
    EXPECT_EQ(shutdownIteration, observation.timerIteration) << "停机必须与旧重建任务处在同一轮 pending";
    EXPECT_EQ(destructions, 1) << "旧重建任务执行前管理器必须已经析构一次";
    EXPECT_EQ(observation.reconnectAttempts, 0) << "已析构管理器的旧重建任务必须失效";
    EXPECT_EQ(observation.promotions, 0) << "停机不得移交未经认证的连接";
    EXPECT_EQ(server->Accepted(), 1) << "停机后不得另建连接";
    EXPECT_EQ(server->AuthCommands(), 1) << "原握手之外不得再次认证";
    EXPECT_FALSE(server->WorkerFailed()) << "假 Redis 的有界协议交互失败";
    EXPECT_FALSE(observation.passwordInLog) << "测试口令不得出现在 muduo 日志中";
}

TEST(RedisManagerEventBatch, ConnectSuccessRearmingAuthDeadlineInvalidatesPreviousQueuedGeneration)
{
    const auto loopOwner = MakePollLoop();
    ASSERT_NE(loopOwner, nullptr) << "无法选择确定事件顺序的 poll 后端";
    auto& loop = *loopOwner;
    BatchObservation observation;
    ScopedLogObservation logs(observation, loop);
    RedisManager manager;
    // 先 bind 而不 listen：首个连接必然被拒，使用真实失败回调建立旧代次的重连定时器。
    const auto server = std::make_shared<FakeRedis>(loop, observation, false);
    ASSERT_NE(server->Port(), 0) << "本地假 Redis 无法绑定回环临时端口";
    bool firstConnectFailed = false;
    observation.onConnectFailed = [&]
    {
        firstConnectFailed = true;
        loop.queueInLoop([&] { loop.quit(); });
    };
    manager.SetReconnectCallback([&]
    {
        ++observation.promotions;
        observation.promotionIteration = loop.iteration();
        const auto original = manager.GetConnection().get();
        observation.pingSubmitResult = original->command(
            [&, original](hiredis::Hiredis* connection, redisReply* reply)
            {
                if (!reply) return;
                ++observation.pingReplies;
                observation.pingWasPong = reply->type == REDIS_REPLY_STATUS && reply->str &&
                    std::string_view(reply->str, reply->len) == "PONG";
                observation.originalConnectionHeld = connection == original && manager.GetConnection().get() == original;
                loop.runAfter(0.15, [&] { loop.queueInLoop([&] { loop.quit(); }); });
            }, "PING");
    });
    RedisConnectOptions options;
    options.password = kFakePassword;
    options.reconnectIntervalSec = kHandshakeDeadlineSec;
    options.livenessProbeIntervalSec = 2.0;
    options.livenessTimeoutSec = 2.0;
    manager.Connect(&loop, muduo::net::InetAddress("127.0.0.1", server->Port()), std::move(options));
    loop.runAfter(2.5, [&]
    {
        observation.watchdogExpired = true;
        loop.queueInLoop([&] { loop.quit(); });
    });
    loop.loop();
    observation.onConnectFailed = {};
    ASSERT_TRUE(firstConnectFailed) << "必须先经过真实拒连路径，建立旧代次的重连定时器";
    ASSERT_TRUE(server->Listen()) << "假 Redis 必须在同一端口开启监听";
    server->SetImmediateAuth();
    server->Start();
    const BatchFds initial = FindBatchFds(server->Port());
    ASSERT_EQ(initial.timerCount, 1) << "必须定位当前 EventLoop 的唯一 timerfd";
    ASSERT_TRUE(ReadableBefore(initial.timer, Clock::now() + 1s)) << "旧代次重连 timerfd 必须真正到期";

    std::int64_t markerIteration = -1;
    // 旧 tick 已过期，零延时 marker 的时间戳晚于它；同一 TimerQueue 批次先排重建、后排准备任务。
    loop.runAfter(0, [&]
    {
        markerIteration = loop.iteration();
        loop.queueInLoop([&]
        {
            // 前一个 pending 已创建新连接。先由 accept 的 promise 确认 TCP 握手，再定位公开 fd。
            if (!server->WaitAccepted()) { observation.preparationFailed = true; return; }
            const BatchFds fds = FindBatchFds(server->Port());
            if (fds.timerCount != 1 || fds.clientCount != 1)
            {
                observation.preparationFailed = true;
                return;
            }
            observation.timerReady = ReadableBefore(fds.timer, Clock::now() + 1s);
            observation.socketReady = EventsBefore(fds.client, POLLOUT, Clock::now() + 500ms);
            observation.preparationFailed = !observation.timerReady || !observation.socketReady;
            observation.preparedIteration = loop.iteration();
            observation.prepared = !observation.preparationFailed;
        });
    });
    loop.loop();
    const bool stillConnected = manager.GetConnection() && manager.GetConnection()->connected();
    server->Stop();
    manager.Shutdown();
    DrainPending(loop);

    EXPECT_FALSE(observation.preparationFailed) << "必须准备旧 tick 和新连接 POLLOUT 同批就绪";
    EXPECT_TRUE(observation.timerReady) << "旧代次的下一跳 timerfd 必须已经到期";
    EXPECT_TRUE(observation.socketReady) << "新连接的建连完成事件必须已经可写";
    EXPECT_EQ(markerIteration, observation.preparedIteration) << "marker 排队的准备任务必须紧随首次重建";
    EXPECT_EQ(observation.timerIteration, observation.preparedIteration + 1) << "旧截止 tick 必须在准备后的下一批先执行";
    EXPECT_EQ(observation.connectedIteration, observation.timerIteration) << "同批建连成功必须取消旧代次并重新挂 AUTH 截止";
    EXPECT_EQ(observation.reconnectAttempts, 1) << "只允许首次失败后的必要重建，旧排队任务不得拆掉新 AUTH";
    EXPECT_EQ(server->Accepted(), 1) << "新认证定时器虽处于 active，旧代次仍必须失效";
    EXPECT_EQ(server->AuthCommands(), 1) << "新连接不得被旧代次重复认证或重建";
    EXPECT_EQ(observation.promotions, 1) << "认证成功只能移交一次";
    EXPECT_EQ(observation.pingSubmitResult, REDIS_OK) << "被移交的原连接必须可提交命令";
    EXPECT_EQ(observation.pingReplies, 1) << "原连接上的业务命令必须恰好完成一次";
    EXPECT_TRUE(observation.pingWasPong) << "原连接必须返回正常 PONG";
    EXPECT_TRUE(observation.originalConnectionHeld) << "业务回包必须来自原先移交的连接";
    EXPECT_TRUE(stillConnected) << "观察窗口结束时原连接必须仍然可用";
    EXPECT_FALSE(observation.watchdogExpired) << "重连、认证和 PING 必须在有界时间内完成";
    EXPECT_FALSE(server->WorkerFailed()) << "假 Redis 的有界协议交互失败";
    EXPECT_FALSE(observation.passwordInLog) << "测试口令不得出现在 muduo 日志中";
}
} // namespace
