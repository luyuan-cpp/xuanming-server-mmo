// hiredis_command_lifecycle_test —— 回归测试,对应 docs/design/cross-zone-scene-travel.md §12.3『Hiredis 泄漏』。
//
// 判据:Hiredis::command() 返回非 REDIS_OK 时,调用方回调的那份拷贝(command() 里 new 出来的
// CommandCallback)必须在 command() 返回前释放、且永不被调用;返回 OK 时恰好被调用一次(连接释放时以空应答)。
// 做法:回调按值捕获一个 shared_ptr 哨兵,command() 返回后看 weak_ptr 的 use_count / expired ——
// 泄漏的拷贝会一直攥着哨兵。
//
// 修复前前三个用例红(use_count==2 / 哨兵未释放),对照组绿。取红态时只构建本测试工程、链接修复前的
// lib/muduo.lib,不重建 muduo。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main;链接桩(InitPlayerService /
// InitServiceHandler)已由 rpc_client_lifecycle_test.cpp 定义,这里不要重复定义。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#else
#include <sys/socket.h>
#include <netinet/in.h>
#include <unistd.h>
#endif

#include <atomic>
#include <chrono>
#include <cstdint>
#include <functional>
#include <future>
#include <memory>
#include <optional>
#include <thread>

#include "muduo/base/CountDownLatch.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "muduo/contrib/hiredis/Hiredis.h"   // 它包含 <hiredis/hiredis.h>,提供 REDIS_OK / REDIS_ERR

using namespace std::chrono_literals;

namespace
{

// 同 rpc_client_lifecycle_test.cpp;两份都在各自 TU 的匿名命名空间里,不构成 ODR 冲突。
uint16_t FindFreeLoopbackPort()
{
#ifdef _WIN32
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
#else
    const int s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    EXPECT_NE(s, -1);
    if (s == -1)
    {
        return 0;
    }
    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = 0;
    const int bindResult = ::bind(s, reinterpret_cast<sockaddr*>(&addr), sizeof(addr));
    EXPECT_EQ(bindResult, 0);
    if (bindResult != 0)
    {
        ::close(s);
        return 0;
    }
    socklen_t len = sizeof(addr);
    const int nameResult = ::getsockname(s, reinterpret_cast<sockaddr*>(&addr), &len);
    EXPECT_EQ(nameResult, 0);
    if (nameResult != 0)
    {
        ::close(s);
        return 0;
    }
    const uint16_t port = ntohs(addr.sin_port);
    ::close(s);
    return port;
#endif
}

// 真实的 hiredis::Hiredis 连到一个只 accept、从不回 RESP 的假 Redis 上:已入队的命令会一直挂着,
// 直到 ~Hiredis → redisAsyncFree 用空应答回调它 —— 这正是生产上 FREEING 窗口的形状。
class HiredisCommandLifecycle : public ::testing::Test
{
protected:
    void SetUp() override
    {
        loop_.emplace(*loopThread_.startLoop());
        const uint16_t port = FindFreeLoopbackPort();
        ASSERT_NE(port, 0);

        auto connected = connectStatus_.get_future();
        RunInLoopAndWait([this, port] {
            // 假 Redis:TcpServer 的默认 messageCallback 只吞字节(retrieveAll),从不回 RESP,
            // 所以已入队的命令会一直挂着,直到 ~Hiredis 用空应答回调它。
            server_ = std::make_unique<muduo::net::TcpServer>(
                &Loop(), muduo::net::InetAddress(port, /*loopbackOnly=*/true), "fake_redis");
            // TcpServer 只在 accept 时复制 messageCallback；间接调用允许单个用例控制已连接 socket。
            server_->setMessageCallback([this](const muduo::net::TcpConnectionPtr& connection,
                                                muduo::net::Buffer* input, muduo::Timestamp received) {
                if (serverMessageCallback_) serverMessageCallback_(connection, input, received);
                else input->retrieveAll();
            });
            server_->start();   // 在 loop 线程上 runInLoop 即时 listen,下面的 connect 不会撞拒连

            redis_ = hiredis::Hiredis::Create(&Loop(), muduo::net::InetAddress("127.0.0.1", port), maintenance_);
            // 各回调只捕获夹具 this 与值:redis_ 在 TearDown / DestroyRedis 里先于夹具销毁,
            // 属于 AGENTS.md §11.7 允许的同寿命例外。
            redis_->setConnectCallback([this](hiredis::Hiredis*, int status) {
                if (!connectSignalled_)
                {
                    connectSignalled_ = true;
                    connectStatus_.set_value(status);
                }
            });
            redis_->connect();
        });
        ASSERT_EQ(connected.wait_for(5s), std::future_status::ready) << "hiredis never connected to the fake server";
        ASSERT_EQ(connected.get(), REDIS_OK);
    }

    void TearDown() override
    {
        RunInLoopAndWait([this] {
            serverMessageCallback_ = {};
            redis_.reset();
            server_.reset();
        });
        // 再转一圈:让 removeChannel 排队的 dummy(持有 Channel)等 functor 在 loop 停下前跑完,
        // 而不是留给 ~EventLoop 随 pendingFunctors_ 一起丢弃(AGENTS.md §11.7 先排空再 quit)。
        RunInLoopAndWait([] {});
    }

    muduo::net::EventLoop& Loop() { return loop_->get(); }

    template <typename F>
    void RunInLoopAndWait(F&& fn)
    {
        muduo::CountDownLatch done(1);
        Loop().runInLoop([&] {
            fn();
            // 第一跳和 removeChannel 的清理可能同批；第二跳才保证该批捕获已析构。
            // 只等函数体返回不足以证明 MaintenanceState 的本机引用已经释放。
            Loop().queueInLoop([this, &done] {
                Loop().queueInLoop([&done] { done.countDown(); });
            });
        });
        done.wait();
    }

    // 只在 loop 线程调用。函数返回后局部 sentinel 已释放,返回的回调就是哨兵的唯一持有者;
    // 被拒的命令永远不该调用它,所以体内只记 rejectedRuns_。
    hiredis::Hiredis::CommandCallback MakeSentinelCallback()
    {
        auto sentinel = std::make_shared<int>(0);
        sentinelWeak_ = sentinel;
        return [held = sentinel, this](hiredis::Hiredis*, redisReply*) {
            (void)held;
            ++rejectedRuns_;
        };
    }

    // reset 先清业务 owner；同步析构期间的回调只能用参数里的 Hiredis*。
    void DestroyRedis()
    {
        RunInLoopAndWait([this] { redis_.reset(); });
    }

    // loopThread_ 必须排第一:最后析构,保证其余成员(含 redis_ / server_)析构时 loop 仍在。
    muduo::net::EventLoopThread loopThread_;
    // 每例独立关闭域，不污染同进程的 ProcessMaintenance。
    std::shared_ptr<hiredis::Hiredis::MaintenanceState> maintenance_ =
        std::make_shared<hiredis::Hiredis::MaintenanceState>();
    // 引用而非裸指针成员(裸指针成员门禁覆盖测试工程);SetUp 里绑定。
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop_;
    std::unique_ptr<muduo::net::TcpServer> server_;
    std::shared_ptr<hiredis::Hiredis> redis_;
    muduo::net::MessageCallback serverMessageCallback_;
    std::promise<int> connectStatus_;
    bool connectSignalled_ = false;

    // 以下只在 loop 线程上写;测试线程在 RunInLoopAndWait / DestroyRedis 返回后读(latch 提供 happens-before)。
    int rejectedRuns_ = 0;            // 被拒命令的回调被调用次数,必须恒为 0
    int acceptedNullReplies_ = 0;
    int acceptedNonNullReplies_ = 0;
    std::weak_ptr<int> sentinelWeak_;
    std::optional<int> nestedRet_;    // FREEING 用例:空应答回调里再发命令的返回值
    long nestedUseCount_ = -1;
    bool nestedConnected_ = false;
};

} // namespace

// ---------------------------------------------------------------------------
// 1. 格式串非法:hiredis.c 的 fmt_invalid → redisvFormatCommand 返回 -2(不消费 va_arg),
//    redisvAsyncCommand 在调 __redisAsyncCommand 之前就返回 REDIS_ERR,privdata 从未登记。
// ---------------------------------------------------------------------------

TEST_F(HiredisCommandLifecycle, FormatErrorReleasesCallbackCopy)
{
    std::optional<int> ret;
    long useCount = -1;
    RunInLoopAndWait([&] {
        ASSERT_TRUE(redis_->connected());
        const hiredis::Hiredis::CommandCallback cb = MakeSentinelCallback();
        ret = redis_->command(cb, "SET k %Z");
        useCount = sentinelWeak_.use_count();
    });   // 退出 lambda 时 cb 析构

    ASSERT_TRUE(ret.has_value());
    EXPECT_EQ(*ret, REDIS_ERR);
    EXPECT_EQ(useCount, 1) << "只剩调用方那一份;修复前为 2(command() 里 new 的拷贝泄漏)";
    EXPECT_TRUE(sentinelWeak_.expired()) << "调用方的 cb 已析构,哨兵仍被泄漏的回调拷贝攥着";

    DestroyRedis();
    EXPECT_EQ(rejectedRuns_, 0) << "没入队的回调在 redisAsyncFree 时也不得被调用";
}

// ---------------------------------------------------------------------------
// 2. FREEING(生产上的典型触发):~Hiredis → redisAsyncFree 先用空应答回调挂起命令,
//    回调里用参数 Hiredis* me 再发命令。那一刻 connected() 仍为真,command() 会走到 new,
//    随后 __redisAsyncCommand 因 REDIS_FREEING 拒收。
// ---------------------------------------------------------------------------

TEST_F(HiredisCommandLifecycle, CommandFromNullReplyCallbackDuringFreeReleasesCallbackCopy)
{
    RunInLoopAndWait([this] {
        ASSERT_TRUE(redis_->connected());
        const hiredis::Hiredis::CommandCallback nested = MakeSentinelCallback();
        const hiredis::Hiredis::CommandCallback outer = [this, nested](hiredis::Hiredis* me, redisReply* r) {
            if (r != nullptr)
            {
                ++acceptedNonNullReplies_;
                return;
            }
            ++acceptedNullReplies_;
            // 这一刻处在 ~Hiredis → redisAsyncFree 之内(FREEING)。async.c 的 __redisAsyncFree 先跑
            // 挂起回调、之后才 _EL_CLEANUP,所以 channel_ 与 REDIS_CONNECTED 都还在。
            nestedConnected_ = me->connected();
            nestedRet_ = me->command(nested, "PING");
            nestedUseCount_ = sentinelWeak_.use_count();
        };
        ASSERT_EQ(REDIS_OK, redis_->command(outer, "PING"));
    });   // 局部 nested / outer 析构:此后哨兵只由队列里那份外层回调捕获的 nested 持有

    DestroyRedis();

    EXPECT_EQ(acceptedNullReplies_, 1);
    EXPECT_EQ(acceptedNonNullReplies_, 0);
    EXPECT_TRUE(nestedConnected_) << "前提:FREEING 期间 connected() 仍为真,command() 会走到 new";
    ASSERT_TRUE(nestedRet_.has_value());
    EXPECT_EQ(*nestedRet_, REDIS_ERR);
    EXPECT_EQ(nestedUseCount_, 1) << "只剩外层回调捕获的那一份;修复前为 2";
    EXPECT_TRUE(sentinelWeak_.expired()) << "外层回调已被 delete,哨兵仍被泄漏的回调拷贝攥着";
    EXPECT_EQ(rejectedRuns_, 0);
}

// ---------------------------------------------------------------------------
// 3. DISCONNECTING:显式 disconnect()(async.c redisAsyncDisconnect)置 REDIS_DISCONNECTING;
//    有挂起应答时不立即断开,connected() 仍为真。生产上没有调用点,但它是 __redisAsyncCommand
//    拒收的另一个标志位,顺手覆盖。
// ---------------------------------------------------------------------------

TEST_F(HiredisCommandLifecycle, CommandWhileDisconnectingReleasesCallbackCopy)
{
    std::optional<int> ret;
    long useCount = -1;
    RunInLoopAndWait([&] {
        // 先挂一条永远等不到应答的命令(假服务器从不回),让 disconnect() 只置标志、不立即断开。
        const hiredis::Hiredis::CommandCallback pending = [this](hiredis::Hiredis*, redisReply* r) {
            if (r != nullptr) ++acceptedNonNullReplies_;
            else ++acceptedNullReplies_;
        };
        ASSERT_EQ(REDIS_OK, redis_->command(pending, "PING"));
        redis_->disconnect();
        ASSERT_TRUE(redis_->connected()) << "前提:有挂起应答时 disconnect() 只置 DISCONNECTING,连接仍在";

        const hiredis::Hiredis::CommandCallback cb = MakeSentinelCallback();
        ret = redis_->command(cb, "PING");
        useCount = sentinelWeak_.use_count();
    });

    ASSERT_TRUE(ret.has_value());
    EXPECT_EQ(*ret, REDIS_ERR);
    EXPECT_EQ(useCount, 1) << "只剩调用方那一份;修复前为 2";
    EXPECT_TRUE(sentinelWeak_.expired()) << "调用方的 cb 已析构,哨兵仍被泄漏的回调拷贝攥着";

    DestroyRedis();
    EXPECT_EQ(acceptedNullReplies_, 1) << "挂起的那条必须恰好以空应答回调一次";
    EXPECT_EQ(acceptedNonNullReplies_, 0);
    EXPECT_EQ(rejectedRuns_, 0);
}

// ---------------------------------------------------------------------------
// 4. 对照组:入队成功的命令,回调拷贝归应答队列所有,由 commandCallback 调用恰好一次再 delete。
//    防止修法把成功路径也误删成二次释放;修复前后都应为绿。
// ---------------------------------------------------------------------------

TEST_F(HiredisCommandLifecycle, AcceptedCommandCallbackRunsExactlyOnceWithNullReplyOnFree)
{
    std::optional<int> ret;
    long useCountQueued = -1;
    RunInLoopAndWait([&] {
        auto sentinel = std::make_shared<int>(0);
        sentinelWeak_ = sentinel;
        const hiredis::Hiredis::CommandCallback cb = [sentinel, this](hiredis::Hiredis*, redisReply* r) {
            (void)sentinel;
            if (r != nullptr) ++acceptedNonNullReplies_;
            else ++acceptedNullReplies_;
        };
        sentinel.reset();
        ret = redis_->command(cb, "PING");
        useCountQueued = sentinelWeak_.use_count();
    });

    ASSERT_TRUE(ret.has_value());
    EXPECT_EQ(*ret, REDIS_OK);
    EXPECT_EQ(useCountQueued, 2) << "调用方一份 + 应答队列一份";
    EXPECT_FALSE(sentinelWeak_.expired()) << "队列里那份必须活到应答(或连接释放)为止";

    DestroyRedis();
    EXPECT_EQ(acceptedNullReplies_, 1) << "连接释放时挂起命令必须恰好以空应答回调一次";
    EXPECT_EQ(acceptedNonNullReplies_, 0);
    EXPECT_TRUE(sentinelWeak_.expired()) << "回调跑完后 commandCallback 必须 delete 那份拷贝";
}

// 用户回调可以同步撤掉最后一个业务 owner；一次 Channel 分发必须保住整个 wrapper/context，
// 而不仅是 Channel。回调返回后再排空两轮，确认保活最终释放，没有把对象钉在事件循环里。
TEST_F(HiredisCommandLifecycle, ConnectCallbackCanReleaseLastOwner)
{
    const auto address = redis_->serverAddress();
    DestroyRedis();
    std::promise<void> callbackDone;
    auto completed = callbackDone.get_future();
    int calls = 0;
    bool connectedAfterReset = false;
    std::weak_ptr<int> captured;
    RunInLoopAndWait([&] {
        auto sentinel = std::make_shared<int>(0);
        captured = sentinel;
        redis_ = hiredis::Hiredis::Create(&Loop(), address, maintenance_);
        redis_->setConnectCallback([&, sentinel](hiredis::Hiredis* me, int status) {
            ++calls;
            EXPECT_EQ(status, REDIS_OK);
            redis_.reset();
            // 回调参数在当前分发栈内仍有效；旧裸 this 版本在这里稳定触发 ASan UAF。
            connectedAfterReset = me->connected();
            maintenance_->Close();
            EXPECT_FALSE(maintenance_->Joined()) << "连接回调及当前 Channel 分发仍未返回";
            EXPECT_FALSE(captured.expired()) << "正在执行的回调捕获必须仍然存活";
            callbackDone.set_value();
        });
        redis_->connect();
    });
    ASSERT_EQ(completed.wait_for(5s), std::future_status::ready) << "建连回调未按期完成";
    RunInLoopAndWait([] {});
    RunInLoopAndWait([] {});
    EXPECT_EQ(calls, 1);
    EXPECT_TRUE(connectedAfterReset) << "释放业务 owner 不得提前破坏正在分发的连接";
    EXPECT_TRUE(captured.expired()) << "当前批次结束后必须释放回调捕获";
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_TRUE(maintenance_->Frontier(frontier));
}

TEST_F(HiredisCommandLifecycle, ReplyCallbackCanReleaseLastOwnerAndCompletesPendingCommandOnce)
{
    std::promise<void> released;
    auto completed = released.get_future();
    int replyCalls = 0;
    int pendingNullReplies = 0;
    int pendingReplies = 0;
    bool connectedAfterReset = false;
    bool responded = false;
    std::weak_ptr<int> captured;
    RunInLoopAndWait([&] {
        serverMessageCallback_ = [&](const muduo::net::TcpConnectionPtr& connection,
                                      muduo::net::Buffer* input, muduo::Timestamp) {
            input->retrieveAll();
            if (!responded)
            {
                responded = true;
                connection->send("+PONG\r\n");
            }
        };
        auto sentinel = std::make_shared<int>(0);
        captured = sentinel;
        ASSERT_EQ(redis_->command([&, sentinel](hiredis::Hiredis* me, redisReply* reply) {
            ++replyCalls;
            EXPECT_NE(reply, nullptr);
            redis_.reset();
            connectedAfterReset = me->connected();
            EXPECT_FALSE(captured.expired());
        }, "PING"), REDIS_OK);
        ASSERT_EQ(redis_->command([&, sentinel](hiredis::Hiredis*, redisReply* reply) {
            if (reply) ++pendingReplies;
            else ++pendingNullReplies;
            released.set_value();
        }, "PING"), REDIS_OK);
    });
    ASSERT_EQ(completed.wait_for(5s), std::future_status::ready) << "挂起命令未在销毁时完成";
    RunInLoopAndWait([] {});
    RunInLoopAndWait([] {});
    EXPECT_EQ(replyCalls, 1);
    EXPECT_TRUE(connectedAfterReset);
    EXPECT_EQ(pendingNullReplies, 1) << "未收到应答的另一条命令必须恰好以空应答完成一次";
    EXPECT_EQ(pendingReplies, 0);
    EXPECT_TRUE(captured.expired()) << "两个命令回调的捕获都必须释放";
}

TEST_F(HiredisCommandLifecycle, DisconnectCallbackCanReleaseLastOwner)
{
    std::promise<void> callbackDone;
    auto completed = callbackDone.get_future();
    int disconnectCalls = 0;
    int nullReplies = 0;
    bool disconnectedAfterReset = false;
    std::weak_ptr<int> captured;
    RunInLoopAndWait([&] {
        serverMessageCallback_ = [](const muduo::net::TcpConnectionPtr& connection,
                                      muduo::net::Buffer* input, muduo::Timestamp) {
            input->retrieveAll();
            connection->forceClose();
        };
        auto sentinel = std::make_shared<int>(0);
        captured = sentinel;
        redis_->setDisconnectCallback([&, sentinel](hiredis::Hiredis* me, int) {
            ++disconnectCalls;
            redis_.reset();
            disconnectedAfterReset = !me->connected();
            EXPECT_FALSE(captured.expired()) << "断线回调不能销毁自己正在使用的捕获";
            callbackDone.set_value();
        });
        ASSERT_EQ(redis_->command([&, sentinel](hiredis::Hiredis*, redisReply* reply) {
            EXPECT_EQ(reply, nullptr);
            if (!reply) ++nullReplies;
        }, "PING"), REDIS_OK);
    });
    ASSERT_EQ(completed.wait_for(5s), std::future_status::ready) << "真实 EOF 未触发断线回调";
    RunInLoopAndWait([] {});
    RunInLoopAndWait([] {});
    EXPECT_EQ(disconnectCalls, 1);
    EXPECT_EQ(nullReplies, 1);
    EXPECT_TRUE(disconnectedAfterReset);
    EXPECT_TRUE(captured.expired()) << "EOF 分发和销毁结束后必须释放全部回调捕获";
}

TEST_F(HiredisCommandLifecycle, SynchronousDisconnectCallbackCanReleaseLastOwner)
{
    int disconnectCalls = 0;
    bool parameterStillValid = false;
    std::weak_ptr<int> captured;
    RunInLoopAndWait([&] {
        ASSERT_FALSE(Loop().eventHandling());
        auto sentinel = std::make_shared<int>(0);
        captured = sentinel;
        redis_->setDisconnectCallback([&, sentinel](hiredis::Hiredis* me, int status) {
            EXPECT_EQ(status, REDIS_OK);
            ++disconnectCalls;
            redis_.reset();
            parameterStillValid = !me->connected();
            EXPECT_FALSE(captured.expired());
        });
        // 无挂起命令时 C API 当场执行断线回调，不能依赖 Channel::tie 保活。
        redis_->disconnect();
        EXPECT_EQ(redis_, nullptr);
    });
    RunInLoopAndWait([] {});
    EXPECT_EQ(disconnectCalls, 1);
    EXPECT_TRUE(parameterStillValid);
    EXPECT_TRUE(captured.expired());
}

TEST(HiredisMaintenanceState, CloseAndAdmissionAreLinearizedAndUnknownStillJoins)
{
    auto state = std::make_shared<hiredis::Hiredis::MaintenanceState>();
    ASSERT_TRUE(state->Begin());
    state->Close();
    EXPECT_FALSE(state->Begin());
    EXPECT_FALSE(state->Joined());
    std::string frontier;
    EXPECT_FALSE(state->Frontier(frontier));
    state->Complete(false);
    EXPECT_TRUE(state->Joined()) << "UNKNOWN 不等于本机回调仍在运行";
    EXPECT_FALSE(state->Frontier(frontier)) << "本机 join 不得抹掉 Redis UNKNOWN";
}

TEST(HiredisMaintenanceState, ConcurrentCloseRejectsEveryLaterAdmission)
{
    auto state = std::make_shared<hiredis::Hiredis::MaintenanceState>();
    std::promise<void> start;
    auto ready = start.get_future();
    bool admitted = false;
    std::thread caller([&] { ready.wait(); admitted = state->Begin(); });
    start.set_value();
    state->Close();
    caller.join();
    if (admitted) state->Complete(true);
    EXPECT_FALSE(state->Begin());
    EXPECT_TRUE(state->Joined());
    std::string frontier;
    EXPECT_TRUE(state->Frontier(frontier));
}

TEST_F(HiredisCommandLifecycle, FrontierWaitsForReplyCallbackAndLoopReferences)
{
    std::promise<void> returned;
    auto done = returned.get_future();
    int rejected = 0;
    bool replied = false;
    RunInLoopAndWait([&] {
        serverMessageCallback_ = [&](const muduo::net::TcpConnectionPtr& connection,
                                     muduo::net::Buffer* input, muduo::Timestamp) {
            input->retrieveAll();
            if (!replied) { replied = true; connection->send("+PONG\r\n"); }
        };
        ASSERT_EQ(redis_->command([&](hiredis::Hiredis* me, redisReply* reply) {
            ASSERT_NE(reply, nullptr);
            maintenance_->Close();
            std::string frontier;
            EXPECT_FALSE(maintenance_->Joined());
            EXPECT_FALSE(maintenance_->Frontier(frontier));
            EXPECT_EQ(me->command([&](hiredis::Hiredis*, redisReply*) { ++rejected; }, "PING"), REDIS_ERR);
            redis_.reset();
            EXPECT_FALSE(maintenance_->Joined()) << "用户回调及其 native 分发栈还没有返回";
            returned.set_value();
        }, "PING"), REDIS_OK);
    });
    ASSERT_EQ(done.wait_for(5s), std::future_status::ready);
    RunInLoopAndWait([] {});
    RunInLoopAndWait([] {});
    EXPECT_EQ(rejected, 0);
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_TRUE(maintenance_->Frontier(frontier));
}

TEST_F(HiredisCommandLifecycle, NativeFormatRejectionIsKnownAndHasNoCallback)
{
    int callbacks = 0;
    RunInLoopAndWait([&] {
        EXPECT_EQ(redis_->command([&](hiredis::Hiredis*, redisReply*) { ++callbacks; }, "SET k %Z"), REDIS_ERR);
        maintenance_->Close();
    });
    DestroyRedis();
    RunInLoopAndWait([] {});
    EXPECT_EQ(callbacks, 0);
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_TRUE(maintenance_->Frontier(frontier));
}

TEST_F(HiredisCommandLifecycle, NullReplyOnDestructionJoinsButKeepsUnknown)
{
    int callbacks = 0;
    RunInLoopAndWait([&] {
        ASSERT_EQ(redis_->command([&](hiredis::Hiredis* me, redisReply* reply) {
            ++callbacks;
            EXPECT_EQ(reply, nullptr);
            EXPECT_FALSE(maintenance_->Joined());
            EXPECT_EQ(me->command([](hiredis::Hiredis*, redisReply*) {}, "PING"), REDIS_ERR);
        }, "PING"), REDIS_OK);
        maintenance_->Close();
    });
    DestroyRedis();
    RunInLoopAndWait([] {});
    EXPECT_EQ(callbacks, 1);
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_FALSE(maintenance_->Frontier(frontier));
}

TEST_F(HiredisCommandLifecycle, ErrorReplyDoesNotBecomeSuccessfulFrontier)
{
    std::promise<void> replied;
    auto done = replied.get_future();
    RunInLoopAndWait([&] {
        serverMessageCallback_ = [](const muduo::net::TcpConnectionPtr& connection,
                                    muduo::net::Buffer* input, muduo::Timestamp) {
            input->retrieveAll(); connection->send("-ERR maintenance-test\r\n");
        };
        ASSERT_EQ(redis_->command([&](hiredis::Hiredis*, redisReply* reply) {
            ASSERT_NE(reply, nullptr);
            EXPECT_EQ(reply->type, REDIS_REPLY_ERROR);
            replied.set_value();
        }, "PING"), REDIS_OK);
    });
    ASSERT_EQ(done.wait_for(5s), std::future_status::ready);
    maintenance_->Close();
    DestroyRedis();
    RunInLoopAndWait([] {});
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_FALSE(maintenance_->Frontier(frontier));
}

TEST_F(HiredisCommandLifecycle, DisconnectCallbackCannotReconnectAfterClose)
{
    int callbacks = 0;
    RunInLoopAndWait([&] {
        redis_->setDisconnectCallback([&](hiredis::Hiredis* me, int status) {
            EXPECT_EQ(status, REDIS_OK);
            ++callbacks;
            maintenance_->Close();
            EXPECT_FALSE(maintenance_->Joined());
            me->connect();
            EXPECT_FALSE(me->connected());
            EXPECT_EQ(me->command([](hiredis::Hiredis*, redisReply*) {}, "PING"), REDIS_ERR);
            redis_.reset();
        });
        redis_->disconnect();
    });
    RunInLoopAndWait([] {});
    EXPECT_EQ(callbacks, 1);
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_TRUE(maintenance_->Frontier(frontier));
}

TEST_F(HiredisCommandLifecycle, ClosedStateConnectIsANativeNoop)
{
    const auto address = redis_->serverAddress();
    DestroyRedis();
    RunInLoopAndWait([] {});
    maintenance_->Close();
    int callbacks = 0;
    RunInLoopAndWait([&] {
        redis_ = hiredis::Hiredis::Create(&Loop(), address, maintenance_);
        redis_->setConnectCallback([&](hiredis::Hiredis*, int) { ++callbacks; });
        redis_->connect();
        EXPECT_FALSE(redis_->connected());
        EXPECT_EQ(redis_->command([](hiredis::Hiredis*, redisReply*) {}, "PING"), REDIS_ERR);
        redis_.reset();
    });
    EXPECT_EQ(callbacks, 0);
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_TRUE(maintenance_->Frontier(frontier));
}
TEST_F(HiredisCommandLifecycle, FailedConnectCallbackCannotReenterNativeConnect)
{
    std::promise<void> failed;
    auto done = failed.get_future();
    int calls = 0;
    const uint16_t unusedPort = FindFreeLoopbackPort();
    ASSERT_NE(unusedPort, 0);
    std::shared_ptr<hiredis::Hiredis> unavailable;
    RunInLoopAndWait([&] {
        unavailable = hiredis::Hiredis::Create(&Loop(), muduo::net::InetAddress("127.0.0.1", unusedPort), maintenance_);
        unavailable->setConnectCallback([&](hiredis::Hiredis* me, int status) {
            EXPECT_EQ(status, REDIS_ERR);
            EXPECT_FALSE(me->connected());
            EXPECT_NE(me->errstr(), nullptr);
            ++calls;
            me->connect(); // 旧 context 尚待 native cleanup，必须零 native 重入。
            unavailable.reset();
            EXPECT_FALSE(maintenance_->Joined());
            if (calls == 1) failed.set_value();
        });
        unavailable->connect();
    });
    ASSERT_EQ(done.wait_for(5s), std::future_status::ready);
    maintenance_->Close();
    DestroyRedis();
    RunInLoopAndWait([] {});
    RunInLoopAndWait([] {});
    EXPECT_EQ(calls, 1);
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_FALSE(maintenance_->Frontier(frontier));
}

TEST_F(HiredisCommandLifecycle, ReplyCallbackDerivedCommandRemainsInAcceptedSet)
{
    std::promise<void> terminal;
    auto done = terminal.get_future();
    bool responded = false;
    int originalReplies = 0;
    int derivedNullReplies = 0;
    RunInLoopAndWait([&] {
        serverMessageCallback_ = [&](const muduo::net::TcpConnectionPtr& connection,
                                     muduo::net::Buffer* input, muduo::Timestamp) {
            input->retrieveAll();
            if (!responded) { responded = true; connection->send("+PONG\r\n"); }
        };
        ASSERT_EQ(redis_->command([&](hiredis::Hiredis* me, redisReply* reply) {
            ASSERT_NE(reply, nullptr);
            ++originalReplies;
            EXPECT_EQ(me->command([&](hiredis::Hiredis*, redisReply* derived) {
                EXPECT_EQ(derived, nullptr);
                if (!derived) ++derivedNullReplies;
                EXPECT_FALSE(maintenance_->Joined());
                terminal.set_value();
            }, "PING"), REDIS_OK);
            maintenance_->Close();
            redis_.reset();
        }, "PING"), REDIS_OK);
    });
    ASSERT_EQ(done.wait_for(5s), std::future_status::ready);
    RunInLoopAndWait([] {});
    RunInLoopAndWait([] {});
    EXPECT_EQ(originalReplies, 1);
    EXPECT_EQ(derivedNullReplies, 1);
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_FALSE(maintenance_->Frontier(frontier));
}
TEST_F(HiredisCommandLifecycle, NullMaintenanceCannotOpenNativeConnection)
{
    const auto address = redis_->serverAddress();
    int callbacks = 0;
    RunInLoopAndWait([&] {
        auto untracked = hiredis::Hiredis::Create(&Loop(), address, nullptr);
        untracked->setConnectCallback([&](hiredis::Hiredis*, int) { ++callbacks; });
        untracked->connect();
        EXPECT_FALSE(untracked->connected());
        EXPECT_EQ(untracked->command([](hiredis::Hiredis*, redisReply*) {}, "PING"), REDIS_ERR);
    });
    EXPECT_EQ(callbacks, 0);
}

TEST_F(HiredisCommandLifecycle, DisconnectCallbackTargetDestructorCannotReenterConnect)
{
    std::promise<void> targetDestroyed;
    auto completed = targetDestroyed.get_future();
    int targetsDestroyed = 0;
    RunInLoopAndWait([&] {
        const std::weak_ptr<hiredis::Hiredis> weak = redis_;
        auto target = std::shared_ptr<int>(new int(0), [&, weak](int* value) {
            delete value;
            ++targetsDestroyed;
            if (const auto client = weak.lock()) {
                // target 析构也在 native callback 栈内。若先撤 phase，第一次会建新 context，
                // 第二次就撞已有 context 断言；修复后两次均是零 native 重入。
                client->connect();
                client->connect();
                EXPECT_FALSE(client->connected());
            }
            targetDestroyed.set_value();
        });
        redis_->setDisconnectCallback([target](hiredis::Hiredis* client, int status) {
            EXPECT_EQ(status, REDIS_OK);
            client->setDisconnectCallback({}); // 原回调释放，只剩实际分发栈的那份副本。
        });
        target.reset();
        redis_->disconnect();
    });
    ASSERT_EQ(completed.wait_for(5s), std::future_status::ready);
    maintenance_->Close();
    DestroyRedis();
    RunInLoopAndWait([] {});
    RunInLoopAndWait([] {});
    EXPECT_EQ(targetsDestroyed, 1);
    EXPECT_TRUE(maintenance_->Joined());
    std::string frontier;
    EXPECT_TRUE(maintenance_->Frontier(frontier));
}


TEST(HiredisMaintenanceState, SettledSnapshotKeepsRelayAdmissionOpen)
{
    auto state = std::make_shared<hiredis::Hiredis::MaintenanceState>();
    ASSERT_TRUE(state->Begin()); std::string frontier;
    EXPECT_FALSE(state->SettledFrontier(frontier));
    state->Complete(true);
    EXPECT_TRUE(state->SettledFrontier(frontier));
    EXPECT_TRUE(state->Begin()) << "P1 快照不能提前关闭 relay";
    state->Complete(false);
    EXPECT_FALSE(state->SettledFrontier(frontier));
    state->Close(); EXPECT_TRUE(state->Joined()); EXPECT_FALSE(state->Frontier(frontier));
}
