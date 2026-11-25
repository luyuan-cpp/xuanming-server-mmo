// muduo EventLoop 唤醒的延迟用例(docs/handoff/grpc-muduo-batch3-5/README.md §3.5、survey-muduo.md)。
//
// Windows 版 muduo 以前的 wakeup() / handleRead() 是空操作:poll 超时取最近定时器的剩余时间,上限
// kPollTimeMs = 100ms,跨线程 queueInLoop 与 pending functor 里再排的任务都要等 poll 自己超时才执行。
// 原先 EtcdService 的 5ms CQ 轮询定时器把它压在约 5ms;第 3 批删掉那个定时器后,改由回环 UDP 唤醒 socket
// 真正唤醒 poll。下面每条用例的 loop 上只挂一个远期的兜底 quit 定时器(用例自己的收尾定时器除外),
// 让 poll 每次都按满上限阻塞:唤醒失效时每个样本约 100ms,生效时在 1ms 量级。
// Linux 版用真 eventfd,这几条用例在两边都应通过。
//
// 跨线程投递本身就是被测对象,所以第一、三条用例各起一个投递线程(不同于 muduo_test_loop.h「不起线程」的
// 通用约定);它只调用线程安全的 queueInLoop,等待一律带超时,loop() 返回后先 join 再断言(join 之前不放
// ASSERT,免得 std::thread 析构时 terminate)。回调只在 loop 线程上读写用例体的局部变量;loop 由夹具持有,
// 最先构造、最后析构。

#include <gtest/gtest.h>

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <future>
#include <memory>
#include <thread>
#include <vector>

#include "muduo_test_loop.h"

// muduo 头放在所有标准库头之后(它的 Windows 适配头 #define 了 off_t / pid_t 等名字)。
#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

namespace
{

using Clock = std::chrono::steady_clock;
using namespace std::chrono_literals;

// 唤醒失效时样本约 100ms(Windows 的 kPollTimeMs);阈值远低于它,同时给 CI 的调度抖动留余量。
constexpr Clock::duration kMaxWakeupLatency = 30ms;
// 兜底:用例出错时也能退出 loop。离得足够远,不会把 poll 超时压到 100ms 以下。
constexpr double kSafetyQuitSeconds = 5.0;
// 投递线程等某一轮执行完的上限;超过说明 loop 已被兜底定时器停掉。
constexpr auto kPosterWait = 2s;
constexpr int kRounds = 5;

// 空闲窗口:Windows 上 poll 每 100ms 超时一次,窗口里约 3~4 轮;Linux 由 timerfd 唤醒,约 1~2 轮。
// 唤醒数据报没读干净时,水平触发的 WSAPoll 每轮立刻返回:每次只读一个的实现,轮数接近积压的数据报个数
// (至多 kBurst + 1,受接收缓冲限制);完全不读则一直空转到窗口结束。
constexpr double kIdleSeconds = 0.3;
constexpr int64_t kMaxIdleIterations = 20;
constexpr int kBurst = 200;

double ToMilliseconds(Clock::duration duration)
{
    return std::chrono::duration<double, std::milli>(duration).count();
}

class EventLoopWakeupTest : public ::testing::Test
{
protected:
    static void SetUpTestSuite()
    {
        muduo::Logger::setLogLevel(muduo::Logger::WARN);
        muduo_grpc_test::EnsureSocketsInitialized();
    }

    void SetUp() override
    {
        loop.runAfter(kSafetyQuitSeconds, [this] { loop.quit(); });
    }

    muduo::net::EventLoop loop;
};

// 别的线程 queueInLoop 时 loop 正阻塞在 poll 里(或刚跑完上一轮的 pending functor、即将进入 poll):
// queueInLoop 跨线程必须 wakeup,任务应当立刻执行,而不是等 poll 超时。
TEST_F(EventLoopWakeupTest, CrossThreadQueueInLoopWakesBlockedPoll)
{
    std::vector<Clock::duration> latencies;  // 只在 loop 线程上写;loop() 返回、投递线程 join 之后才读
    muduo::net::EventLoop &loop = this->loop;

    std::thread poster([&loop, &latencies]
    {
        // 第 0 轮预热:等 loop 真正转起来,不计入样本。之后每一轮都在上一轮的任务执行完后才投递,
        // 投递时 loop 要么还在本轮 doPendingFunctors 里,要么已回到 poll,两种情况都只能靠 wakeup 及时执行。
        for (int round = 0; round <= kRounds; ++round)
        {
            auto executed = std::make_shared<std::promise<void>>();
            std::future<void> executedFuture = executed->get_future();
            const bool measured = round > 0;
            const Clock::time_point postedAt = Clock::now();
            loop.queueInLoop([executed, measured, postedAt, &latencies]
            {
                if (measured)
                {
                    latencies.push_back(Clock::now() - postedAt);
                }
                executed->set_value();
            });
            if (executedFuture.wait_for(kPosterWait) != std::future_status::ready)
            {
                break;  // loop 已停,样本不足由主线程断言
            }
        }
        loop.queueInLoop([&loop] { loop.quit(); });
    });
    loop.loop();
    poster.join();

    ASSERT_EQ(latencies.size(), static_cast<size_t>(kRounds)) << "loop stopped before every round ran";
    for (size_t i = 0; i < latencies.size(); ++i)
    {
        EXPECT_LT(latencies[i], kMaxWakeupLatency)
            << "round " << i + 1 << ": cross-thread queueInLoop waited " << ToMilliseconds(latencies[i])
            << " ms; poll was not woken up (Windows caps the poll timeout at 100 ms)";
    }
}

// pending functor 里再 queueInLoop:此时 callingPendingFunctors_ 为真,本轮已经 swap 过队列,
// 新任务只能在下一轮执行,queueInLoop 必须 wakeup 让下一轮 poll 立即返回。muduo_grpc 的完成回调、
// 两跳 quit、关机时 FinalizeTeardownInLoop 的自我重排都走这条路径。
TEST_F(EventLoopWakeupTest, ChainedQueueInLoopFromPendingFunctorRunsPromptly)
{
    std::vector<Clock::duration> latencies;
    muduo::net::EventLoop &loop = this->loop;

    std::function<void(int, Clock::time_point)> link;
    link = [&loop, &latencies, &link](int remaining, Clock::time_point queuedAt)
    {
        latencies.push_back(Clock::now() - queuedAt);
        if (remaining == 0)
        {
            loop.quit();
            return;
        }
        const Clock::time_point now = Clock::now();
        loop.queueInLoop([&link, remaining, now] { link(remaining - 1, now); });
    };
    // 起点放在立即到期的定时器里:定时器回调不在 pending functor 里,它排的第一环本轮就会执行;
    // 之后每一环都是「pending functor 里再排」。这个定时器触发一次就没了,之后 loop 上只剩兜底定时器。
    loop.runAfter(0.0, [&loop, &link]
    {
        const Clock::time_point now = Clock::now();
        loop.queueInLoop([&link, now] { link(kRounds, now); });
    });
    loop.loop();

    ASSERT_EQ(latencies.size(), static_cast<size_t>(kRounds + 1)) << "loop stopped before the chain finished";
    for (size_t i = 0; i < latencies.size(); ++i)
    {
        EXPECT_LT(latencies[i], kMaxWakeupLatency)
            << "link " << i << ": queueInLoop from a pending functor waited " << ToMilliseconds(latencies[i])
            << " ms; the next poll was not woken up";
    }
}

// 一阵密集的跨线程投递会在唤醒 socket 里积压很多个唤醒数据报。handleRead 必须一次读干净,
// 否则水平触发的 WSAPoll 在之后的空闲期里每轮立刻返回,loop 空转吃满一个核。
// 边投边读时积压多少取决于两个线程谁快,「每次只读一个」的实现可能恰好跟得上而漏检;所以投递期间先用一个
// 任务把 loop 线程挡住(等待带超时),让 kBurst 个唤醒确定堆在 socket 里,放行后的下一轮 handleRead 面对整批积压。
TEST_F(EventLoopWakeupTest, BurstOfWakeupsIsDrainedAndIdleLoopDoesNotSpin)
{
    int64_t idleStartIteration = -1;
    int64_t idleIterations = -1;
    muduo::net::EventLoop &loop = this->loop;

    const auto blockerStarted = std::make_shared<std::promise<void>>();
    std::future<void> blockerStartedFuture = blockerStarted->get_future();
    const auto release = std::make_shared<std::promise<void>>();
    const std::shared_future<void> releaseFuture = release->get_future().share();

    std::thread poster([&loop, &idleStartIteration, &idleIterations, &blockerStartedFuture, blockerStarted, release,
                        releaseFuture]
    {
        loop.queueInLoop([blockerStarted, releaseFuture]
        {
            blockerStarted->set_value();
            (void)releaseFuture.wait_for(kPosterWait);
        });
        // loop 没在时限内转起来也照投:积压不再确定,但断言照常进行,不会卡住。
        (void)blockerStartedFuture.wait_for(kPosterWait);
        for (int i = 0; i < kBurst; ++i)
        {
            loop.queueInLoop([] {});
        }
        // 最后一个任务在 loop 线程上开始计空闲窗口;窗口里没有任何投递,只有兜底定时器和收尾定时器。
        loop.queueInLoop([&loop, &idleStartIteration, &idleIterations]
        {
            idleStartIteration = loop.iteration();
            loop.runAfter(kIdleSeconds, [&loop, &idleStartIteration, &idleIterations]
            {
                idleIterations = loop.iteration() - idleStartIteration;
                loop.quit();
            });
        });
        release->set_value();
    });
    loop.loop();
    poster.join();

    ASSERT_GE(idleIterations, 0) << "idle window never finished";
    EXPECT_LT(idleIterations, kMaxIdleIterations)
        << "loop iterated " << idleIterations << " times in a " << kIdleSeconds * 1000
        << " ms idle window; the wakeup fd was left readable and poll kept returning at once";
}

} // namespace
