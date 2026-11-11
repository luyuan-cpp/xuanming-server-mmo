#include <cstdio>
#include <exception>
#include <functional>
#include <optional>
#include <stdexcept>
#include <thread>
#include <vector>

#include <gtest/gtest.h>
#include "engine/core/async/coroutine_callback.h"
#include "engine/core/async/loop_task_scope.h"
#include "core/system/scene_task_scope.h"
#include "thread_context/trace_context_tls.h"
#include "muduo/base/Logging.h"
#include <boost/asio/error.hpp>
#include <boost/cobalt/task.hpp>
#include <boost/system/system_error.hpp>

namespace
{
using Reply = std::function<void(int)>;
using Submit = std::function<bool(Reply)>;

boost::cobalt::task<void> ReadTwice(muduo::net::EventLoop& loop, Submit submit,
    std::vector<int>& events, std::vector<std::thread::id>& threads)
{
    threads.push_back(std::this_thread::get_id());
    events.push_back(0);
    auto first = co_await loop_async::AwaitCallback<int>(loop, submit, -1);
    threads.push_back(std::this_thread::get_id());
    if (!first) { events.push_back(-2); co_return; }
    events.push_back(*first);
    if (*first < 0) co_return;
    auto second = co_await loop_async::AwaitCallback<int>(loop, submit, -1);
    threads.push_back(std::this_thread::get_id());
    events.push_back(second.value_or(-2));
}

void Drain(muduo::net::EventLoop& loop)
{
    loop.queueInLoop([&loop] { loop.quit(); });
    loop.loop();
}

struct Permit
{
    int& releases;
    ~Permit() { ++releases; }
};
using PermitReply = std::function<void(std::unique_ptr<Permit>)>;

boost::cobalt::task<void> UsePermit(muduo::net::EventLoop& loop,
    std::function<bool(PermitReply)> submit, int& actions)
{
    auto result = co_await loop_async::AwaitCallback<std::unique_ptr<Permit>>(loop, std::move(submit), nullptr);
    if (!result || !*result ||
        (co_await boost::cobalt::this_coro::cancelled) != boost::asio::cancellation_type::none) co_return;
    ++actions;
}

TEST(LoopTaskScope, StoppedOwnerReleasesLateMoveOnlyPermitWithoutBusiness)
{
    muduo::net::EventLoop loop;
    PermitReply reply;
    int actions = 0, releases = 0;
    {
        LoopTaskScope tasks(loop);
        ASSERT_TRUE(tasks.Start(UsePermit(loop, [&](PermitReply done) {
            reply = std::move(done);
            return true;
        }, actions), "许可停止"));
        tasks.Stop();
    }
    reply(std::unique_ptr<Permit>(new Permit{releases}));
    EXPECT_EQ(actions, 0);
    EXPECT_EQ(releases, 1);
    Drain(loop);
}

TEST(LoopTaskScope, ReentrantStopAfterPermitResultPreventsBusinessAndReleasesPermit)
{
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    int actions = 0, releases = 0;
    ASSERT_TRUE(tasks.Start(UsePermit(loop, [&](PermitReply done) {
        done(std::unique_ptr<Permit>(new Permit{releases}));
        tasks.Stop();
        return true;
    }, actions), "许可重入停止"));
    EXPECT_EQ(actions, 0);
    EXPECT_EQ(releases, 1);
    Drain(loop);
}

TEST(LoopTaskScope, HeldReplyAllowsLoopProgressAndInlineSameThreadResumption)
{
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<Reply> replies;
    std::vector<int> events;
    std::vector<std::thread::id> threads;
    ASSERT_TRUE(tasks.Start(ReadTwice(loop, [&](Reply done) { replies.push_back(std::move(done)); return true; }, events, threads), "测试两跳"));
    ASSERT_EQ(replies.size(), 1u);
    loop.queueInLoop([&] {
        events.push_back(7);
        replies[0](1);
        ASSERT_EQ(replies.size(), 2u);
        events.push_back(8);
        replies[1](2);
        events.push_back(9);
        loop.quit();
    });
    loop.loop();
    EXPECT_EQ(events, (std::vector<int>{0, 7, 1, 8, 2, 9}));
    EXPECT_EQ(threads, (std::vector<std::thread::id>(3, std::this_thread::get_id())));
}

TEST(LoopTaskScope, RejectionCompletesWithoutCallback)
{
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<int> events;
    std::vector<std::thread::id> threads;
    ASSERT_TRUE(tasks.Start(ReadTwice(loop, [](Reply) { return false; }, events, threads), "测试拒绝"));
    EXPECT_EQ(events, (std::vector<int>{0, -1}));
    Drain(loop);
}

TEST(LoopTaskScope, ReentrantReplyWaitsForSubmissionReturnAndCompletesOnce)
{
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<int> events;
    std::vector<std::thread::id> threads;
    int calls = 0;
    ASSERT_TRUE(tasks.Start(ReadTwice(loop, [&](Reply done) {
        const int value = ++calls;
        done(value);
        done(99);
        events.push_back(10 + value);
        return true;
    }, events, threads), "测试重入"));
    EXPECT_EQ(events, (std::vector<int>{0, 11, 1, 12, 2}));
    Drain(loop);
}

TEST(LoopTaskScope, StopWinsAndLateReplyCannotResumeDestroyedTask)
{
    muduo::net::EventLoop loop;
    std::vector<int> events;
    std::vector<std::thread::id> threads;
    Reply reply;
    {
        LoopTaskScope tasks(loop);
        ASSERT_TRUE(tasks.Start(ReadTwice(loop, [&](Reply done) { reply = std::move(done); return true; }, events, threads), "测试停止"));
        tasks.Stop();
        tasks.Stop();
        EXPECT_FALSE(tasks.Start(ReadTwice(loop, {}, events, threads), "拒绝新任务"));
        EXPECT_EQ(events, (std::vector<int>{0, -2}));
    }
    reply(1);
    reply(2);
    EXPECT_EQ(events, (std::vector<int>{0, -2}));
    Drain(loop);
}

TEST(LoopTaskScope, ResultWinsBeforeStopAndCloseAllowsAcceptedWorkToFinish)
{
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<int> events;
    std::vector<std::thread::id> threads;
    Reply reply;
    ASSERT_TRUE(tasks.Start(ReadTwice(loop, [&](Reply done) { reply = std::move(done); return true; }, events, threads), "测试关闭"));
    tasks.Close();
    EXPECT_FALSE(tasks.IsAccepting());
    auto first = reply;
    first(1);
    reply(2);
    tasks.Stop();
    EXPECT_EQ(events, (std::vector<int>{0, 1, 2}));
    Drain(loop);
}

TEST(LoopTaskScope, StopDuringSubmissionIsDeliveredOnceAfterSubmitReturns)
{
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<int> events;
    std::vector<std::thread::id> threads;
    ASSERT_TRUE(tasks.Start(ReadTwice(loop, [&](Reply done) {
        tasks.Stop();
        done(1);
        events.push_back(8);
        return true;
    }, events, threads), "测试重入停止"));
    EXPECT_EQ(events, (std::vector<int>{0, 8, -2}));
    Drain(loop);
}

TEST(LoopTaskScope, SceneShutdownSeparatesGameplayFromDurableDrain)
{
    muduo::net::EventLoop loop;
    SceneTaskScope tasks(loop);
    std::vector<int> gameplay, durable;
    std::vector<std::thread::id> threads;
    Reply gameplayReply, durableReply;
    ASSERT_TRUE(SceneTaskScope::TryStart(ReadTwice(loop, [&](Reply done) { gameplayReply = std::move(done); return true; }, gameplay, threads), "瞬态"));
    ASSERT_TRUE(SceneTaskScope::TryStartDurable(ReadTwice(loop, [&](Reply done) { durableReply = std::move(done); return true; }, durable, threads), "持久"));
    tasks.Stop();
    EXPECT_EQ(gameplay, (std::vector<int>{0, -2}));
    EXPECT_EQ(durable, (std::vector<int>{0}));
    auto first = durableReply;
    first(1);
    tasks.StopAll();
    durableReply(2);
    gameplayReply(1);
    EXPECT_EQ(durable, (std::vector<int>{0, 1, -2}));
    Drain(loop);
}

// 链路追踪(docs/design/distributed-tracing.md §5.6):协程挂起时记下 trace,完成回调里装回。
// 回包可能在另一条 trace 里到达,也可能像 Redis、etcd 回包那样根本没有 trace;协程都应回到自己的 trace。
boost::cobalt::task<void> RecordTracesAroundAwait(muduo::net::EventLoop& loop, Submit submit,
    std::vector<tracing::TraceContext>& seen)
{
    seen.push_back(tracing::Current());
    const auto result = co_await loop_async::AwaitCallback<int>(loop, submit, -1);
    (void)result;
    seen.push_back(tracing::Current());
}

TEST(LoopTaskScope, CoroutineResumesOnTheTraceItSuspendedWith)
{
    tracing::Set(tracing::TraceContext{});
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<Reply> replies;
    std::vector<tracing::TraceContext> seen;
    tracing::TraceContext request;
    {
        tracing::ScopedRoot requestTrace;
        request = tracing::Current();
        ASSERT_TRUE(tasks.Start(RecordTracesAroundAwait(loop, [&](Reply done) { replies.push_back(std::move(done)); return true; }, seen), "trace-resume"));
    }
    ASSERT_EQ(replies.size(), 1u);
    ASSERT_EQ(seen.size(), 1u);
    EXPECT_EQ(seen[0].trace_id.bytes, request.trace_id.bytes);
    {
        tracing::ScopedRoot unrelated;   // 回包在另一条 trace 里到达
        replies[0](1);
    }
    ASSERT_EQ(seen.size(), 2u);
    EXPECT_EQ(seen[1].trace_id.bytes, request.trace_id.bytes);
    EXPECT_FALSE(tracing::Current().IsValid());
    Drain(loop);
}

TEST(LoopTaskScope, CoroutineWithoutTraceDoesNotPickUpTheWakersTrace)
{
    tracing::Set(tracing::TraceContext{});
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<Reply> replies;
    std::vector<tracing::TraceContext> seen;
    ASSERT_TRUE(tasks.Start(RecordTracesAroundAwait(loop, [&](Reply done) { replies.push_back(std::move(done)); return true; }, seen), "trace-none"));
    ASSERT_EQ(replies.size(), 1u);
    {
        tracing::ScopedRoot waker;
        replies[0](1);
    }
    ASSERT_EQ(seen.size(), 2u);
    EXPECT_FALSE(seen[0].IsValid());
    EXPECT_FALSE(seen[1].IsValid());   // 宁可丢,不可错:不接上唤醒方的 trace
    Drain(loop);
}

TEST(LoopTaskScope, StoppedCoroutineFinishesOnItsOwnTrace)
{
    tracing::Set(tracing::TraceContext{});
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<Reply> replies;
    std::vector<tracing::TraceContext> seen;
    tracing::TraceContext request;
    {
        tracing::ScopedRoot requestTrace;
        request = tracing::Current();
        ASSERT_TRUE(tasks.Start(RecordTracesAroundAwait(loop, [&](Reply done) { replies.push_back(std::move(done)); return true; }, seen), "trace-stop"));
    }
    {
        tracing::ScopedRoot shutdown;   // 发起停止的一方
        tasks.Stop();
    }
    ASSERT_EQ(seen.size(), 2u);
    EXPECT_EQ(seen[1].trace_id.bytes, request.trace_id.bytes);   // 收尾日志挂回原请求
    Drain(loop);
}

// 拿到真实结果之后被重入停止，再 co_await：Cobalt 在挂起前抛 operation_aborted，第二次提交不会发生，
// 抛点之后的代码也不执行。根完成把它当作停止的正常结局，只记警告；误判成故障的话，LOG_FATAL 会让整个测试进程中止。
TEST(LoopTaskScope, AwaitAfterReentrantStopEndsTaskWithoutRunningLaterCode)
{
    muduo::net::EventLoop loop;
    LoopTaskScope tasks(loop);
    std::vector<int> events;
    std::vector<std::thread::id> threads;
    int submits = 0;
    ASSERT_TRUE(tasks.Start(ReadTwice(loop, [&](Reply done) {
        ++submits;
        done(1);
        tasks.Stop();
        return true;
    }, events, threads), "await-after-stop"));
    EXPECT_EQ(submits, 1);
    EXPECT_EQ(events, (std::vector<int>{0, 1}));
    Drain(loop);
}

#if GTEST_HAS_DEATH_TEST
boost::cobalt::task<void> ThrowAfterReply(muduo::net::EventLoop& loop, Submit submit, std::exception_ptr failure)
{
    const auto reply = co_await loop_async::AwaitCallback<int>(loop, submit, -1);
    (void)reply;
    std::rethrow_exception(failure);
}

// muduo 日志默认写 stdout，EXPECT_DEATH 只匹配 stderr，所以在子进程里先把日志改写到 stderr。
void RouteMuduoLogToStderr()
{
    muduo::Logger::setOutput([](const char* msg, int len) {
        std::fwrite(msg, 1, static_cast<std::size_t>(len), stderr);
        std::fflush(stderr);
    });
}

// 任务体抛出的真实异常不能吞。改造前它在回调里逸出 EventLoop，进程终止；现在带任务名和 what() 经 LOG_FATAL 终止。
// 修复前这里只打一行不带 what() 的 ERROR，进程继续运行，子进程不死，用例红。
TEST(LoopTaskScopeDeathTest, TaskExceptionIsFatalWithNameAndWhat)
{
    EXPECT_DEATH(
        {
            RouteMuduoLogToStderr();
            muduo::net::EventLoop loop;
            LoopTaskScope tasks(loop);
            Reply reply;
            (void)tasks.Start(ThrowAfterReply(loop, [&](Reply done) { reply = std::move(done); return true; },
                std::make_exception_ptr(std::runtime_error("ledger invariant broken"))), "death-probe");
            reply(1);
            Drain(loop);
        },
        "death-probe task failed with an exception: ledger invariant broken");
}

// 只有 Stop 发过取消，operation_aborted 才算停止的结局。只 Close(封准入、不取消)时任务自己抛出的同一个错误码照样终止，
// 不能把 operation_aborted 一律当作停止。
TEST(LoopTaskScopeDeathTest, OperationAbortedWithoutStopIsFatal)
{
    EXPECT_DEATH(
        {
            RouteMuduoLogToStderr();
            muduo::net::EventLoop loop;
            LoopTaskScope tasks(loop);
            Reply reply;
            (void)tasks.Start(ThrowAfterReply(loop, [&](Reply done) { reply = std::move(done); return true; },
                std::make_exception_ptr(boost::system::system_error(boost::asio::error::operation_aborted))), "close-probe");
            tasks.Close();
            reply(1);
            Drain(loop);
        },
        "close-probe task failed with an exception");
}
#endif // GTEST_HAS_DEATH_TEST
}
