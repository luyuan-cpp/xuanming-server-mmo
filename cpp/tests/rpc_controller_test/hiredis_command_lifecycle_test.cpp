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
// 与 rpc_controller_test.cpp 同一工程、同一个 main;InitReply 等链接桩已由 rpc_client_lifecycle_test.cpp
// 定义,这里不要重复定义。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#endif

#include <chrono>
#include <cstdint>
#include <future>
#include <memory>
#include <optional>

#include "muduo/base/CountDownLatch.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpServer.h"
#include "muduo/contrib/hiredis/Hiredis.h"   // 它包含 <hiredis/hiredis.h>,提供 REDIS_OK / REDIS_ERR

using namespace std::chrono_literals;

namespace
{

// 同 rpc_client_lifecycle_test.cpp;两份都在各自 TU 的匿名命名空间里,不构成 ODR 冲突。
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

// 真实的 hiredis::Hiredis 连到一个只 accept、从不回 RESP 的假 Redis 上:已入队的命令会一直挂着,
// 直到 ~Hiredis → redisAsyncFree 用空应答回调它 —— 这正是生产上 FREEING 窗口的形状。
class HiredisCommandLifecycle : public ::testing::Test
{
protected:
    void SetUp() override
    {
        loop_ = loopThread_.startLoop();
        const uint16_t port = FindFreeLoopbackPort();
        ASSERT_NE(port, 0);

        auto connected = connectStatus_.get_future();
        RunInLoopAndWait([this, port] {
            // 假 Redis:TcpServer 的默认 messageCallback 只吞字节(retrieveAll),从不回 RESP,
            // 所以已入队的命令会一直挂着,直到 ~Hiredis 用空应答回调它。
            server_ = std::make_unique<muduo::net::TcpServer>(
                loop_, muduo::net::InetAddress(port, /*loopbackOnly=*/true), "fake_redis");
            server_->start();   // 在 loop 线程上 runInLoop 即时 listen,下面的 connect 不会撞拒连

            redis_ = std::make_unique<hiredis::Hiredis>(loop_, muduo::net::InetAddress("127.0.0.1", port));
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
            redis_.reset();
            server_.reset();
        });
        // 再转一圈:让 removeChannel 排队的 dummy(持有 Channel)等 functor 在 loop 停下前跑完,
        // 而不是留给 ~EventLoop 随 pendingFunctors_ 一起丢弃(AGENTS.md §11.7 先排空再 quit)。
        RunInLoopAndWait([] {});
    }

    template <typename F>
    void RunInLoopAndWait(F&& fn)
    {
        muduo::CountDownLatch done(1);
        loop_->runInLoop([&] { fn(); done.countDown(); });
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

    // unique_ptr::reset 先把 redis_ 置空再析构,所以析构期间的回调只能用参数里的 Hiredis*。
    void DestroyRedis()
    {
        RunInLoopAndWait([this] { redis_.reset(); });
    }

    // loopThread_ 必须排第一:最后析构,保证其余成员(含 redis_ / server_)析构时 loop 仍在。
    muduo::net::EventLoopThread loopThread_;
    muduo::net::EventLoop* loop_ = nullptr;
    std::unique_ptr<muduo::net::TcpServer> server_;
    std::unique_ptr<hiredis::Hiredis> redis_;
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
