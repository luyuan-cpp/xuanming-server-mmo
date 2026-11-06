#include <functional>
#include <optional>
#include <thread>
#include <vector>

#include <gtest/gtest.h>
#include "engine/core/async/coroutine_callback.h"
#include "engine/core/async/loop_task_scope.h"
#include "core/system/scene_task_scope.h"
#include <boost/cobalt/task.hpp>

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
}
