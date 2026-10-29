#include <gtest/gtest.h>

#include "muduo/base/Atomic.h"
#include "muduo/base/Condition.h"
#include "muduo/base/CurrentThread.h"
#include "muduo/base/Thread.h"
#include "muduo/base/Timestamp.h"

#include <array>
#include <chrono>
#include <concepts>
#include <cstdint>
#include <future>
#include <limits>
#include <memory>
#include <string>
#include <thread>
#include <type_traits>

namespace
{
using namespace std::chrono_literals;

template <typename Timestamp>
concept OriginalTimestampComparisons = requires(const Timestamp& left, const Timestamp& right)
{
    { left == right } -> std::same_as<bool>;
    { left != right } -> std::same_as<bool>;
    { left < right } -> std::same_as<bool>;
    { left > right } -> std::same_as<bool>;
    { left <= right } -> std::same_as<bool>;
    { left >= right } -> std::same_as<bool>;
};

template <typename Timestamp>
void CheckTimestampComparisonsAndLayout()
{
    EXPECT_EQ(sizeof(Timestamp), sizeof(int64_t)) << "Timestamp 必须保留原版八字节值布局";
    EXPECT_EQ(alignof(Timestamp), alignof(int64_t));
    if constexpr (OriginalTimestampComparisons<Timestamp>)
    {
        // 比较只取决于原始微秒值，覆盖负值、相等及整数边界，不读取真实时钟。
        const std::array<int64_t, 5> values{
            std::numeric_limits<int64_t>::min(), -1, 0, 1, std::numeric_limits<int64_t>::max()};
        for (const int64_t leftValue : values)
        {
            for (const int64_t rightValue : values)
            {
                SCOPED_TRACE("左值=" + std::to_string(leftValue) + "，右值=" + std::to_string(rightValue));
                const Timestamp left(leftValue);
                const Timestamp right(rightValue);
                EXPECT_EQ(left == right, leftValue == rightValue);
                EXPECT_EQ(left != right, leftValue != rightValue);
                EXPECT_EQ(left < right, leftValue < rightValue);
                EXPECT_EQ(left > right, leftValue > rightValue);
                EXPECT_EQ(left <= right, leftValue <= rightValue);
                EXPECT_EQ(left >= right, leftValue >= rightValue);
            }
        }
    }
    else
    {
        ADD_FAILURE() << "缺少原版 Timestamp 的完整六种比较运算";
    }
}

template <typename Condition>
concept OriginalCondition = std::is_constructible_v<Condition, muduo::MutexLock&> &&
    requires(Condition& condition)
    {
        condition.wait();
        { condition.waitForSeconds(0.1) } -> std::same_as<bool>;
    };

template <typename Condition>
void CheckConditionTimeout()
{
    if constexpr (OriginalCondition<Condition>)
    {
        muduo::MutexLock mutex;
        Condition condition(mutex);
        muduo::MutexLockGuard lock(mutex);
        const auto started = std::chrono::steady_clock::now();
        const auto deadline = started + 2s;
        bool timedOut = false;
        // 原版允许虚假唤醒;false 时继续等,但实际超时不能早于小数秒预算。
        while (!timedOut && std::chrono::steady_clock::now() < deadline)
            timedOut = condition.waitForSeconds(0.075);
        const auto elapsed = std::chrono::steady_clock::now() - started;
        EXPECT_TRUE(timedOut);
        EXPECT_GE(elapsed, 50ms) << "小数秒等待不能被截断为零秒";
    }
    else
    {
        ADD_FAILURE() << "缺少原版 Condition(MutexLock&)、wait() 或 waitForSeconds(double) 接口";
    }
}

template <typename Condition>
void CheckConditionNotification(bool timedWait)
{
    if constexpr (OriginalCondition<Condition>)
    {
        struct State
        {
            muduo::MutexLock mutex;
            Condition condition{mutex};
            bool released = false;
            std::promise<void> ready;
            std::promise<bool> finished;
        };
        auto state = std::make_shared<State>();
        auto ready = state->ready.get_future();
        auto finished = state->finished.get_future();
        std::thread waiter([state, timedWait]
        {
            muduo::MutexLockGuard lock(state->mutex);
            state->ready.set_value();
            bool timedOut = false;
            while (!state->released && !timedOut)
            {
                if (timedWait)
                    timedOut = state->condition.waitForSeconds(2.0);
                else
                    state->condition.wait();
            }
            state->finished.set_value(timedOut);
        });

        // 就绪通知与 wait 共用一把锁，通知方拿到锁时等待已经原子释放锁。
        const bool becameReady = ready.wait_for(2s) == std::future_status::ready;
        EXPECT_TRUE(becameReady);
        if (becameReady)
        {
            muduo::MutexLockGuard lock(state->mutex);
            state->released = true;
            state->condition.notify();
        }

        const bool completed = finished.wait_for(3s) == std::future_status::ready;
        EXPECT_TRUE(completed) << "收到通知后等待线程必须结束";
        if (completed)
        {
            EXPECT_FALSE(finished.get()) << "收到通知必须返回 false，而非超时";
            waiter.join();
        }
        else
        {
            // 失败路径不阻塞测试进程，线程捕获共享状态保证对象仍然有效。
            waiter.detach();
        }
    }
    else
    {
        ADD_FAILURE() << "缺少原版 Condition(MutexLock&)、wait() 或 waitForSeconds(double) 接口";
    }
}

template <typename Thread>
void CheckThreadStartAndJoin()
{
    auto entered = std::make_shared<std::promise<int>>();
    auto enteredFuture = entered->get_future();
    auto release = std::make_shared<std::promise<void>>();
    auto releaseFuture = release->get_future().share();
    Thread thread([entered, releaseFuture]
    {
        entered->set_value(muduo::CurrentThread::tid());
        releaseFuture.wait();
    });
    EXPECT_FALSE(thread.started());
    EXPECT_EQ(thread.name(), "Thread" + std::to_string(Thread::numCreated()));
    thread.start();
    EXPECT_TRUE(thread.started());
    if constexpr (requires(const Thread& value) { value.tid(); })
    {
        EXPECT_GT(thread.tid(), 0) << "start 返回前必须发布工作线程的 tid";
        const bool becameReady = enteredFuture.wait_for(2s) == std::future_status::ready;
        EXPECT_TRUE(becameReady);
        if (becameReady)
            EXPECT_EQ(thread.tid(), enteredFuture.get());
    }
    else
    {
        ADD_FAILURE() << "缺少原版 Thread::tid() 接口";
    }
    release->set_value();
    EXPECT_EQ(thread.join(), 0);
    EXPECT_TRUE(thread.started()) << "started 表示曾经启动，join 后仍应为 true";
}
} // namespace

TEST(MuduoBaseContractTest, ConditionPreservesFractionalTimeoutAndTimeoutResult)
{
    CheckConditionTimeout<muduo::Condition>();
}

TEST(MuduoBaseContractTest, TimestampPreservesAllComparisonsAndValueLayout)
{
    CheckTimestampComparisonsAndLayout<muduo::Timestamp>();
}

TEST(MuduoBaseContractTest, AtomicOperationsReturnThePromisedOldOrNewValue)
{
    muduo::AtomicInt32 value;
    EXPECT_EQ(value.get(), 0);
    EXPECT_EQ(value.getAndAdd(2), 0);
    EXPECT_EQ(value.get(), 2);
    EXPECT_EQ(value.addAndGet(3), 5);
    EXPECT_EQ(value.incrementAndGet(), 6);
    EXPECT_EQ(value.decrementAndGet(), 5);
    EXPECT_EQ(value.getAndSet(9), 5);
    EXPECT_EQ(value.get(), 9);
}

TEST(MuduoBaseContractTest, ConditionTimedWaitReturnsFalseOnNotification)
{
    CheckConditionNotification<muduo::Condition>(true);
}

TEST(MuduoBaseContractTest, ConditionWaitUsesConstructorMutex)
{
    CheckConditionNotification<muduo::Condition>(false);
}

TEST(MuduoBaseContractTest, ThreadStartPublishesIdentityAndJoinPreservesStarted)
{
    CheckThreadStartAndJoin<muduo::Thread>();
}

TEST(MuduoBaseContractTest, ThreadDestructorDoesNotWaitForUnjoinedWorker)
{
    struct State
    {
        std::promise<void> entered;
        std::promise<void> release;
        std::shared_future<void> releaseFuture{release.get_future().share()};
        std::promise<void> finished;
        std::promise<void> destroyed;
    };
    auto state = std::make_shared<State>();
    auto entered = state->entered.get_future();
    auto finished = state->finished.get_future();
    auto destroyed = state->destroyed.get_future();
    std::thread owner([state]
    {
        {
            muduo::Thread thread([state]
            {
                state->entered.set_value();
                state->releaseFuture.wait();
                state->finished.set_value();
            });
            thread.start();
        }
        state->destroyed.set_value();
    });

    const bool becameReady = entered.wait_for(2s) == std::future_status::ready;
    EXPECT_TRUE(becameReady);
    EXPECT_EQ(destroyed.wait_for(500ms), std::future_status::ready)
        << "未 join 的线程析构应 detach，不能等待工作函数返回";

    // 无论断言结果如何都释放工作线程，让旧版自动 join 的失败路径也能收尾。
    state->release.set_value();
    EXPECT_EQ(finished.wait_for(2s), std::future_status::ready);
    if (destroyed.wait_for(2s) == std::future_status::ready)
        owner.join();
    else
    {
        ADD_FAILURE() << "释放工作线程后仍未完成析构";
        owner.detach();
    }
}
